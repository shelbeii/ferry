package mcpserver

import (
	"cmp"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/safe"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed confirm_app.html
var confirmAppHTML string

const (
	// confirmAppURI MCP Apps 交互确认卡片的资源 URI（SEP-1865）。
	confirmAppURI = "ui://ferry/confirm.html"
	// confirmAppMIME MCP Apps 规范要求的 HTML MIME 类型。
	confirmAppMIME = "text/html;profile=mcp-app"
	// mcpAppExtension 客户端在 capabilities.extensions 里声明支持 MCP Apps 的键名。
	mcpAppExtension = "io.modelcontextprotocol/ui"
)

// appDecideWait 用户在卡片里点击「同意执行」后，mcp_app_decide 最多同步等待这么久（快速操作直接返回完成，长任务返回 running 由卡片轮询）。
var appDecideWait = 2 * time.Second

type pendingAppCard struct {
	req           approve.Request
	op            *op
	prepareToken  string
	appToken      string // 仅放在 StructuredContent 里供卡片使用，不放进模型可读的 Content 文本中
	desktopOnline bool
	createdAt     time.Time
	expiresAt     time.Time
	timer         *time.Timer
	decided       bool
	rejected      bool
	rejectMsg     string
}

type appDecideIn struct {
	ApprovalID string `json:"approvalId" jsonschema:"待确认卡片的 approvalId"`
	AppToken   string `json:"appToken" jsonschema:"卡片内部令牌"`
	Decision   string `json:"decision" jsonschema:"approve 或 reject"`
	Reason     string `json:"reason,omitempty" jsonschema:"拒绝时的可选原因说明"`
}

type appStatusIn struct {
	ApprovalID string `json:"approvalId" jsonschema:"待确认卡片的 approvalId"`
	AppToken   string `json:"appToken" jsonschema:"卡片内部令牌"`
}

// addMCPApp 注册 MCP Apps 确认卡片资源和仅供卡片调用（模型不可见）的专属工具。
func (s *server) addMCPApp(m *mcp.Server) {
	m.AddResource(&mcp.Resource{
		URI:         confirmAppURI,
		Name:        "Ferry 改动确认卡片",
		Title:       "Ferry 改动确认",
		Description: "在编程软件内展示 Ferry 改动预览、差异与危险提示，并直接同意或拒绝执行",
		MIMEType:    confirmAppMIME,
		Meta: mcp.Meta{
			"ui": map[string]any{"prefersBorder": false},
		},
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      confirmAppURI,
				MIMEType: confirmAppMIME,
				Text:     confirmAppHTML,
			}},
		}, nil
	})

	appOnly := mcp.Meta{"ui": map[string]any{"visibility": []string{"app"}}}

	// 仅供 MCP App 卡片内部调用，模型不可见也不能调用（不加入 s.tools 的对外工具列表）
	mcp.AddTool(m, &mcp.Tool{
		Name:        "mcp_app_decide",
		Title:       "确认卡片决定",
		Description: "仅供 Ferry MCP 确认卡片调用：提交用户在卡片里点的同意或拒绝",
		Meta:        appOnly,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appDecideIn) (*mcp.CallToolResult, any, error) {
		snap, err := func() (v any, err error) {
			defer safe.Recover("工具 mcp_app_decide", &err)
			return s.mcpAppDecide(ctx, req, in)
		}()
		return appToolResult(snap, err), nil, nil
	})

	mcp.AddTool(m, &mcp.Tool{
		Name:        "mcp_app_status",
		Title:       "确认卡片进度",
		Description: "仅供 Ferry MCP 确认卡片调用：轮询已同意操作的最新进展与结果",
		Meta:        appOnly,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appStatusIn) (*mcp.CallToolResult, any, error) {
		snap, err := func() (v any, err error) {
			defer safe.Recover("工具 mcp_app_status", &err)
			return s.mcpAppStatus(in)
		}()
		return appToolResult(snap, err), nil, nil
	})

	mcp.AddTool(m, &mcp.Tool{
		Name:        "mcp_app_cancel",
		Title:       "停止卡片任务",
		Description: "仅供 Ferry MCP 确认卡片调用：停止已同意且正在执行的操作",
		Meta:        appOnly,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appStatusIn) (*mcp.CallToolResult, any, error) {
		snap, err := func() (v any, err error) {
			defer safe.Recover("工具 mcp_app_cancel", &err)
			return s.mcpAppCancel(ctx, in)
		}()
		return appToolResult(snap, err), nil, nil
	})
}

func appToolResult(v any, err error) *mcp.CallToolResult {
	if err != nil {
		msg := ai.Redact(err.Error())
		payload := map[string]any{"state": "error", "error": msg}
		return &mcp.CallToolResult{
			IsError:           true,
			Content:           []mcp.Content{&mcp.TextContent{Text: msg}},
			StructuredContent: payload,
		}
	}
	redacted := redactTree(v)
	b, _ := json.Marshal(redacted)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: ai.Clip(string(b), resultLimit)}},
		StructuredContent: redacted,
	}
}

func redactTree(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var tree any
	if json.Unmarshal(b, &tree) != nil {
		return v
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			return ai.Redact(t)
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
		}
		return x
	}
	return walk(tree)
}

func (s *server) startMCPAppConfirm(r approve.Request, prepareToken string, desktopOnline bool) (*mcp.CallToolResult, error) {
	approvalID := newID() + newID()
	appToken := newSecretToken()
	now := time.Now()
	exp := now.Add(confirmTTL)

	o := &op{
		req:       r,
		started:   now,
		state:     "waiting",
		waitState: "等用户在确认卡片里操作",
		done:      make(chan struct{}),
	}
	card := &pendingAppCard{
		req:           r,
		op:            o,
		prepareToken:  prepareToken,
		appToken:      appToken,
		desktopOnline: desktopOnline,
		createdAt:     now,
		expiresAt:     exp,
	}
	card.timer = time.AfterFunc(confirmTTL, func() {
		s.mu.Lock()
		if card.decided {
			s.mu.Unlock()
			return
		}
		card.decided = true
		card.rejected = true
		card.rejectMsg = "超过 5 分钟没有操作，已自动取消"
		if cur, ok := s.appCards[approvalID]; ok && cur == card {
			delete(s.appCards, approvalID)
		}
		s.mu.Unlock()
		if desktopOnline {
			approve.Cancel(context.Background(), approverFile(), prepareToken, r.Session)
		}
		s.finishOp(o, r, approve.Decision{Approve: false, Note: "5 分钟内没有在确认卡片里操作，已自动取消"}, nil, nil)
	})

	s.mu.Lock()
	s.sweepPendingLocked(now)
	s.sweepOpsLocked(now)
	s.ops[r.ID] = o
	s.appCards[approvalID] = card
	s.mu.Unlock()

	title := opTitle(r)
	target := targetSummary(r)

	// 供模型阅读的文本（绝不包含 appToken）：引导模型立即调 task_status 等待用户点完卡片并拿到结果，不要提前结束回复
	modelSummary := map[string]any{
		"state":               "等用户在确认卡片里操作",
		"confirmationChannel": "mcp-app",
		"id":                  r.ID,
		"approvalId":          approvalID,
		"title":               title,
		"target":              target,
		"desktopOnline":       desktopOnline,
		"expiresInSeconds":    int(confirmTTL.Seconds()),
		"next":                fmt.Sprintf("确认卡片已经展示给用户。请不要提前结束回复，立即调用 task_status(id=%q, wait=40) 等待用户在卡片中点击「同意执行」或「拒绝」并拿到结果；如果返回仍在等待或执行中，请继续调用 task_status 直到完成、拒绝或超时，不要重复发起原改动工具。", r.ID),
	}
	if r.Danger != "" {
		modelSummary["danger"] = r.Danger
	}
	if r.DBEstimate != "" {
		modelSummary["dbEstimate"] = r.DBEstimate
	}

	// 供 MCP App 卡片渲染的结构化数据（含 appToken 和脱敏后的完整预览）
	cardData := map[string]any{
		"kind":          "ferry-confirm-card",
		"approvalId":    approvalID,
		"appToken":      appToken,
		"opId":          r.ID,
		"title":         title,
		"action":        r.Action,
		"actionLabel":   cmp.Or(actionNames[r.Action], r.Action),
		"project":       r.Project,
		"target":        r.Target,
		"host":          r.Host,
		"targetSummary": target,
		"dbName":        r.DBName,
		"dbKind":        r.DBKind,
		"database":      r.Database,
		"dbEstimate":    r.DBEstimate,
		"dbEstimateMax": r.DBEstimateMax,
		"config":        r.Config,
		"danger":        r.Danger,
		"detail":        r.Detail,
		"content":       r.Content,
		"reason":        r.Reason,
		"client":        r.Client,
		"desktopOnline": desktopOnline,
		"expiresAt":     exp.Format(time.RFC3339),
	}

	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: redactJSON(modelSummary)}},
		StructuredContent: redactTree(cardData),
	}, nil
}

func (s *server) lookupAppCard(approvalID, appToken string) (*pendingAppCard, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	card, ok := s.appCards[strings.TrimSpace(approvalID)]
	if !ok {
		return nil, errors.New("确认卡片不存在或已过期，请重新发起")
	}
	if subtle.ConstantTimeCompare([]byte(appToken), []byte(card.appToken)) != 1 {
		return nil, errors.New("确认卡片令牌无效")
	}
	if !card.decided && !now.Before(card.expiresAt) {
		delete(s.appCards, approvalID)
		return nil, errors.New("确认卡片已超过 5 分钟有效期，请重新发起")
	}
	return card, nil
}

func (s *server) mcpAppDecide(ctx context.Context, req *mcp.CallToolRequest, in appDecideIn) (any, error) {
	if in.Decision != "approve" && in.Decision != "reject" {
		return nil, fmt.Errorf("decision 只能是 approve 或 reject：%q", in.Decision)
	}
	now := time.Now()
	s.mu.Lock()
	approvalID := strings.TrimSpace(in.ApprovalID)
	card, ok := s.appCards[approvalID]
	if !ok {
		s.mu.Unlock()
		return nil, errors.New("确认卡片不存在或已过期，请重新发起")
	}
	if subtle.ConstantTimeCompare([]byte(in.AppToken), []byte(card.appToken)) != 1 {
		s.mu.Unlock()
		return nil, errors.New("确认卡片令牌无效")
	}
	if card.decided {
		s.mu.Unlock()
		return nil, errors.New("这张确认卡片已经处理过了")
	}
	if !now.Before(card.expiresAt) {
		delete(s.appCards, approvalID)
		s.mu.Unlock()
		return nil, errors.New("确认卡片已超过 5 分钟有效期，请重新发起")
	}
	card.decided = true
	if card.timer != nil {
		card.timer.Stop()
	}
	// 已处理后延长 10 分钟供卡片轮询任务进度，到期后自动释放
	card.expiresAt = now.Add(10 * time.Minute)
	card.timer = time.AfterFunc(10*time.Minute, func() {
		s.mu.Lock()
		if cur, ok := s.appCards[approvalID]; ok && cur == card {
			delete(s.appCards, approvalID)
		}
		s.mu.Unlock()
	})
	r := card.req
	o := card.op
	prepToken := card.prepareToken
	desktopOnline := card.desktopOnline
	took := time.Since(card.createdAt)
	rejectReason := strings.TrimSpace(in.Reason)
	if rs := []rune(rejectReason); len(rs) > 200 {
		rejectReason = string(rs[:200])
	}
	if in.Decision == "reject" {
		card.rejected = true
		if rejectReason != "" {
			card.rejectMsg = "已拒绝：" + rejectReason
		} else {
			card.rejectMsg = "已拒绝，未执行任何改动"
		}
	}
	s.mu.Unlock()

	if in.Decision == "reject" {
		logAnswer := fmt.Sprintf("卡片拒绝，%.1f 秒", took.Seconds())
		rejectNote := "用户在确认卡片中拒绝了"
		if rejectReason != "" {
			logAnswer = fmt.Sprintf("卡片拒绝（%s），%.1f 秒", rejectReason, took.Seconds())
			rejectNote = "用户在确认卡片中拒绝了：" + rejectReason
		}
		writeClientConfirmLog(r, "mcp-app", logAnswer, answerNo, took)
		if desktopOnline {
			approve.Cancel(context.Background(), approverFile(), prepToken, r.Session)
		}
		s.finishOp(o, r, approve.Decision{Approve: false, Note: rejectNote}, nil, nil)
		snap := opSnapshot(o)
		snap["message"] = card.rejectMsg
		return snap, nil
	}

	writeClientConfirmLog(r, "mcp-app", fmt.Sprintf("卡片同意，%.1f 秒", took.Seconds()), answerYes, took)
	client := cmp.Or(clientName(req), r.Client)
	ev := approve.ConfirmEvidence{
		Channel: "mcp-app",
		Client:  client,
		Action:  "accept",
		TookMs:  took.Milliseconds(),
		Note:    fmt.Sprintf("用户已在 %s 里确认（MCP 卡片，%.1f 秒）", orDash(client), took.Seconds()),
	}
	if err := VerifyClientCommit(&r, ev); err != nil {
		if desktopOnline {
			approve.Cancel(context.Background(), approverFile(), prepToken, r.Session)
		}
		s.finishOp(o, r, approve.Decision{Approve: false, Note: err.Error()}, nil, nil)
		return nil, err
	}
	if !desktopOnline {
		if reason := desktopOnlyExecReason(r); reason != "" {
			err := desktopRequiredError(reason)
			s.finishOp(o, r, approve.Decision{}, nil, err)
			return nil, err
		}
	}

	o.mu.Lock()
	o.state = "running"
	o.decision = &approve.Decision{Approve: true, Note: ev.Note}
	o.mu.Unlock()

	if desktopOnline {
		s.runCommitOnOp(o, prepToken, r, ev, true)
	} else {
		s.runDirectOnOp(o, r, ev)
	}

	select {
	case <-o.done:
	case <-time.After(appDecideWait):
	case <-ctx.Done():
	}

	return opSnapshot(o), nil
}

func (s *server) mcpAppStatus(in appStatusIn) (any, error) {
	card, err := s.lookupAppCard(in.ApprovalID, in.AppToken)
	if err != nil {
		return nil, err
	}
	if card.op != nil {
		snap := opSnapshot(card.op)
		if card.rejected && snap["message"] == nil {
			snap["message"] = card.rejectMsg
		}
		return snap, nil
	}
	return map[string]any{
		"state": "waiting",
		"id":    card.req.ID,
		"title": opTitle(card.req),
	}, nil
}

func (s *server) mcpAppCancel(ctx context.Context, in appStatusIn) (any, error) {
	card, err := s.lookupAppCard(in.ApprovalID, in.AppToken)
	if err != nil {
		return nil, err
	}
	if card.op == nil {
		return nil, errors.New("任务尚未开始")
	}
	if _, err := s.cancelOp(ctx, card.op.req.ID); err != nil {
		return nil, err
	}
	snap := opSnapshot(card.op)
	if snap["state"] == "running" {
		snap["message"] = "已发送停止请求，正在中止…"
	}
	return snap, nil
}

func opSnapshot(o *op) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	title := opTitle(o.req)
	snap := map[string]any{
		"id":             o.req.ID,
		"title":          title,
		"elapsed":        time.Since(o.started).Round(time.Second).String(),
		"modelListening": o.isModelListeningLocked(),
	}
	switch o.state {
	case "waiting":
		snap["state"] = "waiting"
		return snap
	case "running":
		snap["state"] = "running"
		snap["progress"] = slices.Clone(o.progress)
		return snap
	}
	if o.err != nil {
		snap["state"] = "error"
		snap["error"] = o.err.Error()
		return snap
	}
	if o.decision == nil || !o.decision.Approve {
		snap["state"] = "rejected"
		if o.decision != nil && o.decision.Note != "" {
			snap["message"] = o.decision.Note
		}
		return snap
	}
	if o.outcome == nil {
		snap["state"] = "error"
		snap["error"] = "执行结果未知"
		return snap
	}
	if len(o.outcome.Log) > 0 {
		snap["log"] = slices.Clone(o.outcome.Log)
	}
	if !o.outcome.OK {
		snap["state"] = "error"
		snap["error"] = cmp.Or(o.outcome.Error, "执行失败")
		return snap
	}
	snap["state"] = "done"
	if o.decision.Note != "" {
		snap["approval"] = o.decision.Note
	}
	var data any
	if json.Unmarshal(o.outcome.Data, &data) == nil {
		snap["result"] = data
	}
	return snap
}
