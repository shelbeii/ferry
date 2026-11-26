package mcpserver

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// confirmKey 交给编程软件的 MCP Elicitation 确认表单标识。
const confirmKey = "ferry-confirm"

// confirmTTL 编程软件里待确认状态的有效期（与桌面端审批超时一致）。
const confirmTTL = approve.Timeout

// minAnswer Elicitation 回答至少隔这么久才算人看过、点过；更快的是客户端未展示确认框就自动返回的。
var minAnswer = 1500 * time.Millisecond

// elicitHeartbeat 旧版 MCP 协议同步调用 ss.Elicit 时发送进度心跳的间隔，防止客户端工具调用超时（常见 60s）。
var elicitHeartbeat = 15 * time.Second

const (
	answerYes      = "yes"      // 用户明确同意
	answerNo       = "no"       // 用户明确拒绝或主动取消
	answerFallback = "fallback" // 客户端未展示确认框（秒回或无有效回答）：回退处理
)

var answerText = map[string]string{
	answerYes:      "在编程软件里同意",
	answerNo:       "在编程软件里拒绝",
	answerFallback: "编程软件未展示确认界面，回退确认",
}

// standardClientActions 常规运维改动（L2）：默认即可在编程软件中确认。
var standardClientActions = map[string]bool{
	"deploy":       true,
	"start":        true,
	"stop":         true,
	"restart":      true,
	"rollback":     true,
	"sw-start":     true,
	"sw-stop":      true,
	"sw-restart":   true,
	"run-job":      true,
	"backup-run":   true,
	"backup-fetch": true,
}

// IsStandardClientAction 该操作是否属于常规运维操作（L2）。
func IsStandardClientAction(action string) bool {
	return standardClientActions[action]
}

// AttachApprovalHandlers 把 MCP 审批校验逻辑挂到桌面端 Hub 上。
func AttachApprovalHandlers(h *approve.Hub) {
	h.Prepare = PrepareRequest
	h.PrepareClient = PrepareClientApproval
	h.VerifyCommit = VerifyClientCommit
}

// PrepareClientApproval 判断一个已预检通过的请求是否允许在编程软件里确认。
// 生产服务器和 aiChanges: false 已经在 PrepareRequest 里拦截；这里负责确认渠道分级与红线判断。
func PrepareClientApproval(r *approve.Request) (eligible bool, forceDesktopReason string, err error) {
	if r == nil {
		return false, "请求为空", nil
	}
	if r.Action == "secret" {
		return false, "填写密码只能在 Ferry 桌面端进行，不经过编程软件", nil
	}
	st, _ := settings.Load()
	if !st.MCP.ConfirmInClient() {
		return false, "当前设置为在 Ferry 桌面端确认", nil
	}
	if !IsStandardClientAction(r.Action) && !st.MCP.AllowAllInClient() {
		return false, fmt.Sprintf("「%s」不在常规运维范围内，当前设置需要在 Ferry 桌面端确认", cmp.Or(actionNames[r.Action], r.Action)), nil
	}
	if r.Danger != "" && st.MCP.DangerRequiresDesktop() {
		return false, fmt.Sprintf("检测到高危操作（%s），按设置需要在 Ferry 桌面端确认", r.Danger), nil
	}
	return true, "", nil
}

// VerifyClientCommit 桌面端在 Commit 客户端确认前再复核一次设置、范围、危险级别与确认渠道证据。
func VerifyClientCommit(r *approve.Request, ev approve.ConfirmEvidence) error {
	if r == nil {
		return errors.New("请求为空")
	}
	st, _ := settings.Load()
	if r.Action != "create" && !st.MCP.AllowChanges {
		return errors.New("Ferry 已关闭「允许编程软件请求改动」")
	}
	eligible, reason, err := PrepareClientApproval(r)
	if err != nil {
		return err
	}
	if !eligible {
		return errors.New(cmp.Or(reason, "此操作需要在 Ferry 桌面端确认"))
	}
	switch ev.Channel {
	case "mcp-app":
		return nil
	case "elicitation":
		if ev.Action != "accept" {
			return errors.New("客户端表单未同意执行")
		}
		if minAnswer > 0 && ev.TookMs < minAnswer.Milliseconds() {
			return fmt.Errorf("客户端表单回答过快（%d ms），不能确认是本人操作", ev.TookMs)
		}
		return nil
	case "two-step":
		if !st.MCP.FallbackTwoStep() {
			return errors.New("Ferry 未开启两段式确认，请在「设置 → 命令行与 MCP」里开启")
		}
		return nil
	default:
		return fmt.Errorf("不支持的客户端确认方式：%q", ev.Channel)
	}
}

// desktopOnlyExecReason 判断当 Ferry 桌面端未打开时，这个操作本身是否必须依赖桌面端才能执行。
// 返回空字符串表示在客户端确认后，可以由 ferry mcp 直接离线执行。
func desktopOnlyExecReason(r approve.Request) string {
	if r.Action == "secret" {
		return "填写密码只能在 Ferry 桌面端进行，且密码只保存在桌面端内存里"
	}
	if len(r.Input) > 0 {
		raw := string(r.Input)
		if len(ai.SecretNames(raw)) > 0 || strings.Contains(raw, "FERRY_SECRET_") {
			return "该操作引用了经 ask_secret 填写的密码（密码只保存在 Ferry 桌面端内存里），请先打开 Ferry 桌面版"
		}
	}
	if r.Action == "deploy" && len(r.Targets) > 1 {
		f, err := config.LoadWithLocal(r.Config)
		if err == nil {
			e := &engine.Engine{File: f, Rep: quiet{}}
			if e.RolloutCanary(rolloutOpts(r), len(r.Targets)) {
				return "多台服务器灰度部署在首批上线后需要弹窗确认是否继续，请先打开 Ferry 桌面版（或设置 canary: 0 不暂停）"
			}
		}
	}
	return ""
}

func desktopRequiredError(reason string) error {
	if reason == "" {
		return approve.ErrNoDesktop
	}
	return fmt.Errorf("%w（%s）", approve.ErrNoDesktop, reason)
}

// canElicitForm 编程软件是否声明支持 MCP 表单确认（elicitation form mode）。
func canElicitForm(req *mcp.CallToolRequest) bool {
	if req == nil || req.Session == nil {
		return false
	}
	p := req.Session.InitializeParams()
	if p == nil || p.Capabilities == nil || p.Capabilities.Elicitation == nil {
		return false
	}
	el := p.Capabilities.Elicitation
	return el.Form != nil || el.URL == nil // Form 和 URL 都没写时按兼容支持 form 算
}

// canMCPApp 编程软件是否声明支持 MCP Apps（io.modelcontextprotocol/ui）。
func canMCPApp(req *mcp.CallToolRequest) bool {
	if req == nil || req.Session == nil {
		return false
	}
	p := req.Session.InitializeParams()
	if p == nil || p.Capabilities == nil {
		return false
	}
	if p.Capabilities.Extensions != nil {
		if _, ok := p.Capabilities.Extensions[mcpAppExtension]; ok {
			return true
		}
	}
	if p.Capabilities.Experimental != nil {
		if _, ok := p.Capabilities.Experimental[mcpAppExtension]; ok {
			return true
		}
	}
	return false
}

// supportsMRTR 编程软件协商的 MCP 协议版本是否支持 SEP-2322 多轮往返（InputRequests）。
func supportsMRTR(req *mcp.CallToolRequest) bool {
	if req == nil || req.Session == nil {
		return false
	}
	p := req.Session.InitializeParams()
	return p != nil && p.ProtocolVersion >= "2026-07-28"
}

// selectClientChannel 按优先级为当前连接挑选客户端确认渠道：MCP App 卡片 > MCP Elicitation 表单 > 两段式工具确认。
func selectClientChannel(req *mcp.CallToolRequest, st settings.Settings) string {
	switch {
	case canMCPApp(req):
		return "mcp-app"
	case canElicitForm(req):
		return "elicitation"
	case st.MCP.FallbackTwoStep():
		return "two-step"
	default:
		return ""
	}
}

// pendingConfirm 等待编程软件 MRTR 表单确认回传的记录。
type pendingConfirm struct {
	req           approve.Request
	prepareToken  string
	desktopOnline bool
	at            time.Time
	expiresAt     time.Time
}

// pendingPlan 等待两段式 confirm_change 确认的预检计划。
type pendingPlan struct {
	req            approve.Request
	prepareToken   string
	desktopOnline  bool
	expectedTarget string
	expectedAction string
	expectedDanger string
	at             time.Time
	expiresAt      time.Time
}

func newSecretToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *server) sweepPendingLocked(now time.Time) {
	for k, c := range s.confirms {
		if !now.Before(c.expiresAt) {
			delete(s.confirms, k)
		}
	}
	for k, p := range s.plans {
		if !now.Before(p.expiresAt) {
			delete(s.plans, k)
		}
	}
	for k, a := range s.appCards {
		if !now.Before(a.expiresAt) {
			delete(s.appCards, k)
		}
	}
}

// targetSummary 给人看也给 confirm_change 强校验用的目标描述。
func targetSummary(r approve.Request) string {
	switch {
	case r.Action == "db-exec":
		db := r.Database
		if r.DBKind == "redis" {
			db = "db" + strings.TrimPrefix(strings.ToLower(orZero(r.Database)), "db")
		} else if db == "" {
			db = "默认库"
		}
		if r.Target != "" {
			return fmt.Sprintf("%s / %s（经 %s）", r.DBName, db, r.Target)
		}
		return fmt.Sprintf("%s / %s", r.DBName, db)
	case r.Action == "create" || r.Action == "config-edit":
		if r.Project != "" {
			return fmt.Sprintf("%s（%s）", r.Project, r.Config)
		}
		return r.Config
	case len(r.Targets) > 1:
		return fmt.Sprintf("%s/%s", r.Project, strings.Join(r.Targets, ","))
	case r.Target != "":
		if r.Host != "" {
			return fmt.Sprintf("%s/%s (%s)", r.Project, r.Target, r.Host)
		}
		return fmt.Sprintf("%s/%s", r.Project, r.Target)
	default:
		return r.Project
	}
}

// confirmForm 构造给 MCP 客户端展示的确认表单。
// 安全要求：默认勾选为 false 且必填，避免不支持交互却自动提交默认值的客户端误放行。
func confirmForm(r approve.Request, desktopOnline bool) *mcp.ElicitParams {
	var b strings.Builder
	fmt.Fprintf(&b, "Ferry：%s 请求%s\n", orDash(r.Client), opTitle(r))
	if t := targetSummary(r); t != "" {
		fmt.Fprintf(&b, "目标：%s\n", t)
	}
	if r.Danger != "" {
		fmt.Fprintf(&b, "⚠️ 危险：%s\n", r.Danger)
	}
	if r.DBEstimate != "" {
		fmt.Fprintf(&b, "预计影响行数（EXPLAIN 估算）：%s\n", r.DBEstimate)
	}
	for _, d := range r.Detail {
		b.WriteString(d + "\n")
	}
	if r.Reason != "" {
		fmt.Fprintf(&b, "理由：%s\n", r.Reason)
	}
	if desktopOnline {
		b.WriteString("同意后由 Ferry 桌面版在后台执行（任务面板里能看到、能停止）。看不明白要做什么时请拒绝。")
	} else {
		b.WriteString("Ferry 桌面版当前未打开，同意后将由 ferry mcp 直接执行并记入审计。看不明白要做什么时请拒绝。")
	}
	return &mcp.ElicitParams{
		Mode:    "form",
		Message: ai.Clip(b.String(), 8000),
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"approve": map[string]any{
					"type":        "boolean",
					"title":       "同意执行",
					"description": "勾选后提交表示同意执行；不勾选或取消则拒绝",
					"default":     false,
				},
			},
		},
	}
}

// judgeAnswer 判定编程软件返回的 Elicitation 回答：
//   - 耗时小于 minAnswer：一律视作客户端未真正给用户展示确认框，返回 answerFallback（不直接同意也不直接拒绝）；
//   - 耗时达标且 action=accept 且 approve==true：answerYes；
//   - 其余（approve==false、decline、cancel）：answerNo。
func judgeAnswer(res *mcp.ElicitResult, took time.Duration) (desc string, verdict string) {
	if res == nil {
		return fmt.Sprintf("没有回答，%.1f 秒", took.Seconds()), answerFallback
	}
	content, _ := json.Marshal(res.Content)
	desc = fmt.Sprintf("回答 %s %s，%.1f 秒", orDash(res.Action), content, took.Seconds())
	if minAnswer > 0 && took < minAnswer {
		return desc, answerFallback
	}
	switch res.Action {
	case "accept":
		if v, ok := res.Content["approve"].(bool); ok && v {
			return desc, answerYes
		}
		return desc, answerNo
	case "decline", "cancel":
		return desc, answerNo
	default:
		return desc, answerFallback
	}
}

// requestChange 所有改动工具（deploy/app_control/rollback/AI工具/db_execute/config_edit/create_project 等）统一入口。
// 按设置、风险等级、桌面端在线状态和客户端能力路由确认与执行。
func (s *server) requestChange(ctx context.Context, req *mcp.CallToolRequest, r approve.Request) (any, error) {
	if req != nil && req.Params != nil && req.Params.RequestState != "" {
		return s.resumeMRTR(ctx, req)
	}
	if r.ID == "" {
		r.ID = newID()
	}
	r.Session = s.session
	r.Client = clientName(req)
	r.ClientConfirmed = false
	r.ConfirmChannel = ""
	r.ConfirmNote = ""

	st, _ := loadSettingsCached()
	// 默认「在 Ferry 桌面端确认」模式：沿用单阶段 Ask，零额外往返，兼容旧桌面端
	if !st.MCP.ConfirmInClient() {
		return s.submit(r).wait(ctx, req, waitCap)
	}

	// 密码输入始终在 Ferry 桌面端完成（不经过客户端、也不能离线执行）
	if r.Action == "secret" {
		return s.submit(r).wait(ctx, req, waitCap)
	}

	// 阶段 1：优先向运行中的 Ferry 桌面端发起 Prepare（权威预览 + 快照锁定 + 会话拉黑检查）
	var (
		preparedReq   approve.Request
		prepareToken  string
		desktopOnline bool
		eligible      bool
		forceReason   string
	)
	prep, err := approve.Prepare(ctx, approverFile(), r)
	switch {
	case err == nil:
		desktopOnline = true
		preparedReq = prep.Request
		prepareToken = prep.Token
		eligible = prep.Eligible
		forceReason = prep.ForceDesktopReason
	case errors.Is(err, approve.ErrNoDesktop):
		// Ferry 桌面端未打开：本地运行同一套 PrepareRequest 与策略校验，判断是否允许客户端确认并直接执行
		desktopOnline = false
		if err := s.prepareLocal(&r); err != nil {
			return nil, err
		}
		eligible, forceReason, err = PrepareClientApproval(&r)
		if err != nil {
			return nil, err
		}
		if !eligible {
			return nil, desktopRequiredError(forceReason)
		}
		if reason := desktopOnlyExecReason(r); reason != "" {
			return nil, desktopRequiredError(reason)
		}
		preparedReq = r
	default:
		// 桌面端在线但拒绝了预检（权限/参数/拉黑会话/生产服务器等）
		return nil, err
	}

	// 桌面端在线但策略要求必须在桌面端弹窗确认（例如 L1 未切「全部」、命中高危 Danger）
	if !eligible {
		return s.submitCommit(prepareToken, preparedReq, approve.ConfirmEvidence{
			FallbackToDesktop: true,
			FallbackReason:    forceReason,
		}).wait(ctx, req, waitCap)
	}

	// 阶段 2：挑选客户端确认渠道
	channel := selectClientChannel(req, st)
	if channel == "" {
		if desktopOnline {
			return s.submitCommit(prepareToken, preparedReq, approve.ConfirmEvidence{
				FallbackToDesktop: true,
				FallbackReason:    fmt.Sprintf("%s 不支持卡片或表单确认，已改在 Ferry 桌面端确认", orDash(preparedReq.Client)),
			}).wait(ctx, req, waitCap)
		}
		return nil, desktopRequiredError("当前编程软件不支持卡片或表单确认，且未开启「两段式确认」")
	}

	switch channel {
	case "mcp-app":
		return s.startMCPAppConfirm(preparedReq, prepareToken, desktopOnline)
	case "elicitation":
		return s.startElicitationConfirm(ctx, req, st, preparedReq, prepareToken, desktopOnline)
	case "two-step":
		return s.startTwoStepConfirm(preparedReq, prepareToken, desktopOnline)
	default:
		return nil, desktopRequiredError("当前没有可用的客户端确认渠道")
	}
}

// startElicitationConfirm 发起 MCP Elicitation Form 确认，兼容 MRTR（>= 2026-07-28）与旧版同步 Elicit（带进度保活与异常降级）。
func (s *server) startElicitationConfirm(ctx context.Context, req *mcp.CallToolRequest, st settings.Settings, r approve.Request, prepareToken string, desktopOnline bool) (any, error) {
	if supportsMRTR(req) {
		state := newSecretToken()
		now := time.Now()
		s.mu.Lock()
		s.sweepPendingLocked(now)
		s.confirms[state] = pendingConfirm{
			req:           r,
			prepareToken:  prepareToken,
			desktopOnline: desktopOnline,
			at:            now,
			expiresAt:     now.Add(confirmTTL),
		}
		s.mu.Unlock()
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{confirmKey: confirmForm(r, desktopOnline)},
			RequestState:  state,
		}, nil
	}

	// 旧版 MCP 协议：直接调用 ss.Elicit，等待期间定期发进度通知保活，出错自动回退而不是直接中断
	start := time.Now()
	res, err := s.elicitWithHeartbeat(ctx, req, confirmForm(r, desktopOnline))
	took := time.Since(start)
	if err != nil {
		writeClientConfirmLog(r, "elicitation", fmt.Sprintf("表单确认出错：%v", err), answerFallback, took)
		return s.handleClientFallback(ctx, req, st, r, prepareToken, desktopOnline,
			fmt.Sprintf("%s 的表单确认不可用（%v），已改在 Ferry 桌面端确认", orDash(r.Client), err))
	}
	return s.finishElicitationAnswer(ctx, req, st, r, prepareToken, desktopOnline, res, took)
}

func (s *server) elicitWithHeartbeat(ctx context.Context, req *mcp.CallToolRequest, form *mcp.ElicitParams) (*mcp.ElicitResult, error) {
	elicitCtx, cancel := context.WithTimeout(ctx, approve.Timeout)
	defer cancel()
	if req != nil && req.Session != nil && req.Params != nil {
		if tok := req.Params.GetProgressToken(); tok != nil && elicitHeartbeat > 0 {
			done := make(chan struct{})
			defer close(done)
			go func() {
				ticker := time.NewTicker(elicitHeartbeat)
				defer ticker.Stop()
				n := 0
				for {
					select {
					case <-done:
						return
					case <-elicitCtx.Done():
						return
					case <-ticker.C:
						n++
						_ = req.Session.NotifyProgress(elicitCtx, &mcp.ProgressNotificationParams{
							ProgressToken: tok,
							Message:       "等待你在编程软件中确认…",
							Progress:      float64(n),
						})
					}
				}
			}()
		}
	}
	return req.Session.Elicit(elicitCtx, form)
}

// resumeMRTR 收到 SEP-2322 MRTR 第二轮带 InputResponses 的回调。
func (s *server) resumeMRTR(ctx context.Context, req *mcp.CallToolRequest) (any, error) {
	p := req.Params
	now := time.Now()
	s.mu.Lock()
	s.sweepPendingLocked(now)
	c, ok := s.confirms[p.RequestState]
	delete(s.confirms, p.RequestState)
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("这次确认已经失效或超时（ferry mcp 重启过，或已回答过）：要做的话请重新发起")
	}
	res, _ := p.InputResponses[confirmKey].(*mcp.ElicitResult)
	took := time.Since(c.at)
	st, _ := settings.Load()
	return s.finishElicitationAnswer(ctx, req, st, c.req, c.prepareToken, c.desktopOnline, res, took)
}

func (s *server) finishElicitationAnswer(ctx context.Context, req *mcp.CallToolRequest, st settings.Settings, r approve.Request, prepareToken string, desktopOnline bool, res *mcp.ElicitResult, took time.Duration) (any, error) {
	desc, verdict := judgeAnswer(res, took)
	writeClientConfirmLog(r, "elicitation", desc, verdict, took)
	switch verdict {
	case answerYes:
		ev := approve.ConfirmEvidence{
			Channel: "elicitation",
			Client:  r.Client,
			Action:  "accept",
			TookMs:  took.Milliseconds(),
			Note:    fmt.Sprintf("用户已在 %s 里确认（MCP 表单，%.1f 秒）", orDash(r.Client), took.Seconds()),
		}
		return s.dispatchConfirmed(ctx, req, r, prepareToken, desktopOnline, ev)
	case answerNo:
		if desktopOnline {
			approve.Cancel(context.Background(), approverFile(), prepareToken, r.Session)
		}
		return nil, fmt.Errorf("没有执行%s：用户在 %s 里拒绝或取消了。不要换个办法做同样的事，先问用户怎么办", opTitle(r), orDash(r.Client))
	default:
		reason := fmt.Sprintf("%s 没有展示确认框（%s），改在 Ferry 桌面端确认", orDash(r.Client), desc)
		return s.handleClientFallback(ctx, req, st, r, prepareToken, desktopOnline, reason)
	}
}

func (s *server) handleClientFallback(ctx context.Context, req *mcp.CallToolRequest, st settings.Settings, r approve.Request, prepareToken string, desktopOnline bool, reason string) (any, error) {
	// 如果桌面端没开，但用户开启了「两段式确认」，可无缝降级到两段式确认，避免被损坏的 Elicitation 卡死
	if !desktopOnline && st.MCP.FallbackTwoStep() {
		return s.startTwoStepConfirm(r, prepareToken, false)
	}
	if desktopOnline {
		return s.submitCommit(prepareToken, r, approve.ConfirmEvidence{
			FallbackToDesktop: true,
			FallbackReason:    reason,
		}).wait(ctx, req, waitCap)
	}
	return nil, desktopRequiredError(reason)
}

// dispatchConfirmed 客户端已经确认通过：优先交给运行中的桌面版执行（静默进任务面板）；
// 若桌面版未打开（或在确认间隙关闭且允许离线执行），则由 ferry mcp 直接执行。
func (s *server) dispatchConfirmed(ctx context.Context, req *mcp.CallToolRequest, r approve.Request, prepareToken string, desktopOnline bool, ev approve.ConfirmEvidence) (any, error) {
	if err := VerifyClientCommit(&r, ev); err != nil {
		if desktopOnline {
			approve.Cancel(context.Background(), approverFile(), prepareToken, r.Session)
		}
		return nil, err
	}
	if desktopOnline {
		o := s.submitCommit(prepareToken, r, ev)
		return o.wait(ctx, req, waitCap)
	}
	if reason := desktopOnlyExecReason(r); reason != "" {
		return nil, desktopRequiredError(reason)
	}
	return s.submitDirect(r, ev).wait(ctx, req, waitCap)
}

// ---------------------------------------------------------------- Phase 2：两段式预检 + confirm_change

type confirmChangeIn struct {
	ApprovalID string `json:"approvalId" jsonschema:"上一步预检返回的 approvalId（5 分钟内有效，一次性）"`
	Target     string `json:"target" jsonschema:"核对目标（必须与预检返回的 confirmArgs.target 完全一致）"`
	Action     string `json:"action" jsonschema:"核对操作（必须与预检返回的 confirmArgs.action 完全一致）"`
	Danger     string `json:"danger,omitempty" jsonschema:"危险提示（必须与预检返回的 confirmArgs.danger 完全一致，无危险时留空）"`
}

func (s *server) addConfirmTools(m *mcp.Server) {
	addTool(s, m, &mcp.Tool{
		Name:  "confirm_change",
		Title: "确认并执行待审批改动",
		Description: "两段式客户端确认的第二步：当改动工具返回 pendingConfirmation 和 approvalId 时，先把完整预览（目标、差异、风险）展示给用户，" +
			"再带着预检返回的 confirmArgs（approvalId、target、action、danger，必须一字不差）调用本工具触发编程软件原生的确认拦截并执行。" +
			"必须等用户明确同意或通过编程软件的工具确认栏放行；如果用户拒绝，不要重试。最多等 40 秒，没结束返回 id，再用 task_status 查。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, loggedReq(s, "confirm_change", s.confirmChange))
}

func (s *server) startTwoStepConfirm(r approve.Request, prepareToken string, desktopOnline bool) (any, error) {
	approvalID := newID() + newID()
	now := time.Now()
	exp := now.Add(confirmTTL)
	expTarget := targetSummary(r)
	expAction := opTitle(r)
	expDanger := r.Danger

	s.mu.Lock()
	s.sweepPendingLocked(now)
	s.plans[approvalID] = pendingPlan{
		req:            r,
		prepareToken:   prepareToken,
		desktopOnline:  desktopOnline,
		expectedTarget: expTarget,
		expectedAction: expAction,
		expectedDanger: expDanger,
		at:             now,
		expiresAt:      exp,
	}
	s.mu.Unlock()

	confirmArgs := map[string]any{
		"approvalId": approvalID,
		"target":     expTarget,
		"action":     expAction,
	}
	if expDanger != "" {
		confirmArgs["danger"] = expDanger
	}
	out := map[string]any{
		"state":               "pendingConfirmation",
		"confirmationChannel": "two-step",
		"approvalId":          approvalID,
		"expiresInSeconds":    int(confirmTTL.Seconds()),
		"desktopOnline":       desktopOnline,
		"title":               expAction,
		"target":              expTarget,
		"detail":              r.Detail,
		"reason":              r.Reason,
		"confirmArgs":         confirmArgs,
		"next":                "先把上面的目标、详情、差异和风险完整展示给用户；然后调用 confirm_change 并原样传入 confirmArgs 里的字段，由编程软件向用户确认后执行。不要修改或省略 target/action/danger。",
	}
	if expDanger != "" {
		out["danger"] = expDanger
	}
	if r.DBEstimate != "" {
		out["dbEstimate"] = r.DBEstimate
	}
	if r.Content != "" {
		out["content"] = r.Content
	}
	return out, nil
}

func (s *server) confirmChange(ctx context.Context, req *mcp.CallToolRequest, in confirmChangeIn) (any, error) {
	id := strings.TrimSpace(in.ApprovalID)
	if id == "" {
		return nil, errors.New("要提供 approvalId（来自上一步预检结果）")
	}
	now := time.Now()
	s.mu.Lock()
	s.sweepPendingLocked(now)
	plan, ok := s.plans[id]
	if ok {
		delete(s.plans, id)
	}
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("这个 approvalId 不存在、已使用或已过期（5 分钟有效），请重新发起改动")
	}
	if !now.Before(plan.expiresAt) {
		if plan.desktopOnline {
			approve.Cancel(context.Background(), approverFile(), plan.prepareToken, plan.req.Session)
		}
		return nil, errors.New("这个确认已超过 5 分钟有效期，请重新发起改动")
	}
	if plan.req.Session != "" && plan.req.Session != s.session {
		return nil, errors.New("这个 approvalId 不属于当前 MCP 会话")
	}
	// 严格核对摘要参数：防止模型在编程软件原生工具确认卡片里篡改目标、操作名或隐藏危险标记
	if strings.TrimSpace(in.Target) != plan.expectedTarget ||
		strings.TrimSpace(in.Action) != plan.expectedAction ||
		strings.TrimSpace(in.Danger) != plan.expectedDanger {
		if plan.desktopOnline {
			approve.Cancel(context.Background(), approverFile(), plan.prepareToken, plan.req.Session)
		}
		return nil, fmt.Errorf("核对参数与预检计划不一致（期望 target=%q, action=%q, danger=%q），已中止执行，请重新发起",
			plan.expectedTarget, plan.expectedAction, plan.expectedDanger)
	}

	took := time.Since(plan.at)
	writeClientConfirmLog(plan.req, "two-step", fmt.Sprintf("两段式 confirm_change，%.1f 秒", took.Seconds()), answerYes, took)
	ev := approve.ConfirmEvidence{
		Channel: "two-step",
		Client:  cmp.Or(clientName(req), plan.req.Client),
		Action:  "accept",
		TookMs:  took.Milliseconds(),
		Note:    fmt.Sprintf("用户已在 %s 里确认（两段式工具确认）", orDash(cmp.Or(clientName(req), plan.req.Client))),
	}
	if err := VerifyClientCommit(&plan.req, ev); err != nil {
		if plan.desktopOnline {
			approve.Cancel(context.Background(), approverFile(), plan.prepareToken, plan.req.Session)
		}
		return nil, err
	}
	if plan.desktopOnline {
		o := s.submitCommit(plan.prepareToken, plan.req, ev)
		// 如果在两步之间桌面版被关闭了，只要该操作支持离线直接执行就无感衔接
		res, err := o.wait(ctx, req, waitCap)
		if errors.Is(err, approve.ErrNoDesktop) && desktopOnlyExecReason(plan.req) == "" {
			return s.submitDirect(plan.req, ev).wait(ctx, req, waitCap)
		}
		return res, err
	}
	if reason := desktopOnlyExecReason(plan.req); reason != "" {
		return nil, desktopRequiredError(reason)
	}
	return s.submitDirect(plan.req, ev).wait(ctx, req, waitCap)
}

func writeClientConfirmLog(r approve.Request, channel, answer, verdict string, took time.Duration) {
	args, _ := json.Marshal(map[string]any{
		"project": r.Project,
		"target":  r.Target,
		"action":  cmp.Or(actionNames[r.Action], r.Action),
		"channel": channel,
		"answer":  answer,
	})
	writeLog(callEntry{
		Time:     time.Now(),
		Client:   r.Client,
		Tool:     r.Action + "（编程软件里确认）",
		Op:       r.ID,
		Args:     args,
		OK:       verdict == answerYes,
		Decision: answerText[verdict],
		Ms:       took.Milliseconds(),
	})
}

// submitCommit 把预检令牌 Commit 给运行中的桌面版，返回可追踪和轮询的 op。
func (s *server) submitCommit(prepareToken string, r approve.Request, ev approve.ConfirmEvidence) *op {
	state := "waiting"
	if !ev.FallbackToDesktop {
		state = "running"
	}
	now := time.Now()
	o := &op{req: r, started: now, state: state, done: make(chan struct{})}
	s.mu.Lock()
	s.sweepOpsLocked(now)
	s.ops[r.ID] = o
	s.mu.Unlock()
	s.runCommitOnOp(o, prepareToken, r, ev, false)
	return o
}

// runCommitOnOp 在已注册的 op 上执行桌面端 Commit；fallbackDirect 为 true 且桌面版在确认间隙关闭时自动转本地直接执行。
func (s *server) runCommitOnOp(o *op, prepareToken string, r approve.Request, ev approve.ConfirmEvidence, fallbackDirect bool) {
	o.mu.Lock()
	o.cancelFn = func() {
		_ = approve.CancelRun(context.Background(), approverFile(), r.ID, r.Session)
	}
	o.mu.Unlock()
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				o.mu.Lock()
				alreadyDone := o.state == "done"
				if !alreadyDone {
					o.state = "done"
					o.finishedAt = time.Now()
					o.err = fmt.Errorf("内部异常：%v", rec)
				}
				o.mu.Unlock()
				if !alreadyDone {
					o.closeDone()
				}
			}
		}()
		d, out, err := approve.Commit(context.Background(), approverFile(), prepareToken, r.Session, ev,
			func(d approve.Decision) {
				o.mu.Lock()
				o.decision = &d
				if d.Approve {
					o.state = "running"
				}
				o.mu.Unlock()
			},
			o.addProgress)
		if fallbackDirect && errors.Is(err, approve.ErrNoDesktop) && desktopOnlyExecReason(r) == "" {
			s.execDirectIntoOp(o, r, ev)
			return
		}
		s.finishOp(o, r, d, out, err)
	}()
}

func (s *server) finishOp(o *op, r approve.Request, d approve.Decision, out *approve.Outcome, err error) {
	args, _ := json.Marshal(map[string]any{
		"project": r.Project,
		"target":  r.Target,
		"apps":    r.Apps,
		"name":    cmp.Or(r.Software, r.Backup),
		"port":    r.Port,
		"action":  cmp.Or(actionNames[r.Action], r.Action),
		"channel": r.ConfirmChannel,
	})
	entry := callEntry{
		Time:   time.Now(),
		Client: r.Client,
		Tool:   r.Action + ResultTool,
		Op:     r.ID,
		Args:   args,
		Ms:     time.Since(o.started).Milliseconds(),
	}
	switch {
	case err != nil:
		entry.Error = errText(err)
	case !d.Approve:
		entry.Decision, entry.Error = "拒绝", d.Note
	default:
		entry.Decision = "同意"
		if d.Note != "" {
			entry.Decision = d.Note
		}
		entry.OK = out != nil && out.OK
		if out != nil && !out.OK {
			entry.Error = errText(errors.New(out.Error))
		}
	}
	writeLog(entry)
	now := time.Now()
	o.mu.Lock()
	alreadyDone := o.state == "done"
	o.state, o.outcome, o.err = "done", out, err
	o.finishedAt = now
	if err == nil {
		o.decision = &d
	}
	o.mu.Unlock()
	if !alreadyDone {
		o.closeDone()
	}
}
