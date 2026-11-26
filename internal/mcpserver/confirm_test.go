package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func isAppOnlyTool(tl *mcp.Tool) bool {
	if tl == nil || tl.Meta == nil {
		return false
	}
	ui, ok := tl.Meta["ui"].(map[string]any)
	if !ok {
		return false
	}
	vis, ok := ui["visibility"].([]any)
	if !ok || len(vis) != 1 {
		return false
	}
	s, _ := vis[0].(string)
	return s == "app"
}

func setupConfirmEnv(t *testing.T) (shopPath string, approverPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	file := filepath.Join(t.TempDir(), "approver.json")
	oldFile := approverFile
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile = oldFile })

	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), shopYAML)
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}}
	st.MCP.AllowChanges = true
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
	return shop, file
}

func connectMCPApp(t *testing.T, dir string) *mcp.ClientSession {
	t.Helper()
	srv, closeSrv := New(Options{Version: "test", Dir: dir})
	t.Cleanup(closeSrv)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	caps := &mcp.ClientCapabilities{}
	caps.AddExtension(mcpAppExtension, map[string]any{"mimeTypes": []string{confirmAppMIME}})
	c := mcp.NewClient(&mcp.Implementation{Name: "app-ide", Version: "2.0"}, &mcp.ClientOptions{
		Capabilities: caps,
	})
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func connectElicitOpts(t *testing.T, dir string, handler func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
	t.Helper()
	srv, closeSrv := New(Options{Version: "test", Dir: dir})
	t.Cleanup(closeSrv)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "elicit-ide", Version: "1.5"}, &mcp.ClientOptions{
		ElicitationHandler: handler,
	})
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestConfirmPolicyMatrix(t *testing.T) {
	setupConfirmEnv(t)

	// 默认仅桌面端
	if ok, _, _ := PrepareClientApproval(&approve.Request{Action: "deploy"}); ok {
		t.Fatal("默认不应允许客户端确认")
	}

	// 开启客户端确认，默认范围 standard（L2）
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	settings.Save(st)

	for _, act := range []string{"deploy", "start", "stop", "restart", "rollback", "sw-restart", "run-job", "backup-run", "backup-fetch"} {
		if ok, reason, _ := PrepareClientApproval(&approve.Request{Action: act}); !ok {
			t.Errorf("常规运维 %s 应允许客户端确认：%s", act, reason)
		}
	}
	for _, act := range []string{"exec", "write-file", "config-edit", "db-exec", "create", "secret", "fw-enable", "port-open", "ssh-kick", "upload-file"} {
		if ok, _, _ := PrepareClientApproval(&approve.Request{Action: act}); ok {
			t.Errorf("standard 模式下 %s 不应在客户端确认", act)
		}
	}

	// 切到 all：除 secret 和高危外均允许
	st.MCP.ClientConfirmScope = "all"
	settings.Save(st)
	if ok, _, _ := PrepareClientApproval(&approve.Request{Action: "secret"}); ok {
		t.Fatal("secret 绝不能在客户端确认")
	}
	for _, act := range []string{"exec", "write-file", "config-edit", "db-exec", "create", "upload-file"} {
		if ok, reason, _ := PrepareClientApproval(&approve.Request{Action: act}); !ok {
			t.Errorf("all 模式下无高危的 %s 应允许客户端确认：%s", act, reason)
		}
	}
	// 默认高危强制回退桌面端
	if ok, reason, _ := PrepareClientApproval(&approve.Request{Action: "exec", Danger: "包含 rm -rf"}); ok || !strings.Contains(reason, "高危") {
		t.Fatalf("命中 Danger 默认应强制桌面端：%v %s", ok, reason)
	}
	// 关闭高危兜底后允许
	off := false
	st.MCP.ForceDesktopOnDanger = &off
	settings.Save(st)
	if ok, reason, _ := PrepareClientApproval(&approve.Request{Action: "exec", Danger: "包含 rm -rf"}); !ok {
		t.Fatalf("关闭高危兜底后应允许：%s", reason)
	}
}

func TestElicitationApproveRejectFallbackAndSafeDefault(t *testing.T) {
	_, file := setupConfirmEnv(t)
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	settings.Save(st)

	hub := approve.NewHub(file)
	AttachApprovalHandlers(hub)
	reqs := make(chan approve.Request, 8)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		if kind == "request" {
			reqs <- v.(approve.Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	oldMin := minAnswer
	minAnswer = 40 * time.Millisecond
	t.Cleanup(func() { minAnswer = oldMin })

	var mode string
	var sawSchema map[string]any
	cs := connectElicitOpts(t, t.TempDir(), func(ctx context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		sawSchema, _ = r.Params.RequestedSchema.(map[string]any)
		switch mode {
		case "approve":
			time.Sleep(60 * time.Millisecond)
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
		case "unchecked":
			time.Sleep(60 * time.Millisecond)
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": false}}, nil
		case "empty":
			time.Sleep(60 * time.Millisecond)
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
		case "fast-accept":
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
		case "err":
			return nil, errors.New("elicitation ui crashed")
		default:
			time.Sleep(60 * time.Millisecond)
			return &mcp.ElicitResult{Action: "decline"}, nil
		}
	})

	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "验证表单"}

	// 1. 正常同意：桌面端收到 ClientConfirmed=true 且 ConfirmChannel="elicitation"
	mode = "approve"
	go func() {
		r := <-reqs
		if !r.ClientConfirmed || r.ConfirmChannel != "elicitation" {
			t.Errorf("应标记客户端表单确认：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true, Note: r.ConfirmNote})
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`{"ok":true}`)})
	}()
	if text, isErr := callText(t, cs, "app_control", restart); isErr || !strings.Contains(text, `"state": "完成"`) {
		t.Fatalf("表单同意应执行成功：%v %s", isErr, text)
	}
	// 验证安全默认值（绝不是 default: true）
	props, _ := sawSchema["properties"].(map[string]any)
	ap, _ := props["approve"].(map[string]any)
	if ap["default"] != false {
		t.Fatalf("approve 默认值必须为 false：%v", ap)
	}

	// 2. 取消勾选或空内容提交：按拒绝处理，不弹桌面端
	for _, m := range []string{"unchecked", "empty", "decline"} {
		mode = m
		if text, isErr := callText(t, cs, "app_control", restart); !isErr || !strings.Contains(text, "拒绝或取消") {
			t.Fatalf("模式 %s 应拒绝：%v %s", m, isErr, text)
		}
	}

	// 3. 瞬间秒回（< minAnswer）：即使返回 accept+true 也不能放行，必须回退 Ferry 桌面端弹窗！
	mode = "fast-accept"
	go func() {
		r := <-reqs
		if r.ClientConfirmed {
			t.Errorf("瞬间秒回绝不能置 ClientConfirmed=true：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Note: "桌面端拦截了秒回"})
	}()
	if text, isErr := callText(t, cs, "app_control", restart); !isErr || !strings.Contains(text, "桌面端拦截了秒回") {
		t.Fatalf("瞬间秒回未回退桌面端：%v %s", isErr, text)
	}
}

func TestTwoStepConfirmAndTamperProtection(t *testing.T) {
	_, file := setupConfirmEnv(t)
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	st.MCP.ClientConfirmFallback = "two_step"
	settings.Save(st)

	hub := approve.NewHub(file)
	AttachApprovalHandlers(hub)
	reqs := make(chan approve.Request, 4)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		if kind == "request" {
			reqs <- v.(approve.Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	// 普通客户端（无 Elicitation、无 MCP App）：走两段式 confirm_change
	cs := connect(t, t.TempDir())
	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "两段式测试"}
	text, isErr := callText(t, cs, "app_control", restart)
	if isErr {
		t.Fatalf("第一步预检失败：%s", text)
	}
	var step1 struct {
		State       string `json:"state"`
		ApprovalID  string `json:"approvalId"`
		ConfirmArgs struct {
			ApprovalID string `json:"approvalId"`
			Target     string `json:"target"`
			Action     string `json:"action"`
			Danger     string `json:"danger"`
		} `json:"confirmArgs"`
	}
	if err := json.Unmarshal([]byte(text), &step1); err != nil || step1.State != "pendingConfirmation" || step1.ApprovalID == "" {
		t.Fatalf("预检返回格式不对：%v %s", err, text)
	}

	// 篡改 target：严格拒绝且消费掉 approvalId
	if badText, badErr := callText(t, cs, "confirm_change", map[string]any{
		"approvalId": step1.ApprovalID,
		"target":     "shop/fake",
		"action":     step1.ConfirmArgs.Action,
	}); !badErr || !strings.Contains(badText, "不一致") {
		t.Fatalf("篡改 target 应被拦截：%v %s", badErr, badText)
	}
	// 已失效不可重放
	if badText, badErr := callText(t, cs, "confirm_change", map[string]any{
		"approvalId": step1.ApprovalID,
		"target":     step1.ConfirmArgs.Target,
		"action":     step1.ConfirmArgs.Action,
	}); !badErr || !strings.Contains(badText, "不存在") {
		t.Fatalf("消费后不应重放：%v %s", badErr, badText)
	}

	// 重新预检 + 原样参数确认：桌面端静默执行
	text, isErr = callText(t, cs, "app_control", restart)
	if isErr || json.Unmarshal([]byte(text), &step1) != nil {
		t.Fatalf("重新预检失败：%v %s", isErr, text)
	}
	go func() {
		r := <-reqs
		if !r.ClientConfirmed || r.ConfirmChannel != "two-step" {
			t.Errorf("应标记 two-step 确认：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true, Note: r.ConfirmNote})
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`{"restarted":true}`)})
	}()
	out, isErr := callText(t, cs, "confirm_change", map[string]any{
		"approvalId": step1.ConfirmArgs.ApprovalID,
		"target":     step1.ConfirmArgs.Target,
		"action":     step1.ConfirmArgs.Action,
		"danger":     step1.ConfirmArgs.Danger,
	})
	if isErr || !strings.Contains(out, `"restarted": true`) {
		t.Fatalf("confirm_change 应执行成功：%v %s", isErr, out)
	}
}

func TestMCPAppCardFlowAndSecurity(t *testing.T) {
	_, file := setupConfirmEnv(t)
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	settings.Save(st)

	hub := approve.NewHub(file)
	AttachApprovalHandlers(hub)
	reqs := make(chan approve.Request, 4)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		if kind == "request" {
			reqs <- v.(approve.Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	cs := connectMCPApp(t, t.TempDir())

	// 1. 验证 ui://ferry/confirm.html 资源可读取且 MIME 类型正确
	res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: confirmAppURI})
	if err != nil || len(res.Contents) != 1 || res.Contents[0].MIMEType != confirmAppMIME || !strings.Contains(res.Contents[0].Text, "Ferry") {
		t.Fatalf("MCP App 资源异常：%v %+v", err, res)
	}

	// 2. 发起改动：返回 StructuredContent（含 appToken）与 Content（绝不泄露 appToken）
	r1, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "app_control",
		Arguments: map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "卡片测试"},
	})
	if err != nil || r1.IsError {
		t.Fatalf("调用失败：%v %+v", err, r1)
	}
	sc, ok := r1.StructuredContent.(map[string]any)
	if !ok || sc["kind"] != "ferry-confirm-card" {
		t.Fatalf("缺少确认卡片结构化数据：%#v", r1.StructuredContent)
	}
	approvalID, _ := sc["approvalId"].(string)
	appToken, _ := sc["appToken"].(string)
	if approvalID == "" || appToken == "" {
		t.Fatalf("缺少 approvalId / appToken：%#v", sc)
	}
	modelText := r1.Content[0].(*mcp.TextContent).Text
	if strings.Contains(modelText, appToken) {
		t.Fatal("appToken 绝不能泄露到模型可读的 Content 文本中")
	}

	// 3. 错误 appToken 被拦截
	if bad, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": approvalID, "appToken": "wrong-token", "decision": "approve"},
	}); err != nil || !bad.IsError {
		t.Fatalf("错误 appToken 应被拒绝：%v %+v", err, bad)
	}

	// 4. 正确 appToken 同意执行 → 桌面端静默执行 + mcp_app_status 查到完成
	go func() {
		r := <-reqs
		if !r.ClientConfirmed || r.ConfirmChannel != "mcp-app" {
			t.Errorf("应标记 mcp-app 确认：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true, Note: r.ConfirmNote})
		hub.Progress(r.ID, "==> 重启 api")
		time.Sleep(40 * time.Millisecond)
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`{"status":"ok"}`), Log: []string{"重启完成"}})
	}()
	dec, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": approvalID, "appToken": appToken, "decision": "approve"},
	})
	if err != nil || dec.IsError {
		t.Fatalf("mcp_app_decide 失败：%v %+v", err, dec)
	}
	// 再次提交应被拦住（单次有效）
	if dup, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": approvalID, "appToken": appToken, "decision": "approve"},
	}); err != nil || !dup.IsError {
		t.Fatalf("同一张卡片不能重复决定：%v %+v", err, dup)
	}
	// 查询卡片状态
	stRes, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "mcp_app_status",
		Arguments: map[string]any{"approvalId": approvalID, "appToken": appToken},
	})
	if err != nil || stRes.IsError {
		t.Fatalf("mcp_app_status 失败：%v %+v", err, stRes)
	}
	stMap, _ := stRes.StructuredContent.(map[string]any)
	if stMap["state"] != "done" {
		t.Fatalf("期望状态 done：%#v", stMap)
	}
}

func TestOfflineDirectExecutionWhenDesktopClosed(t *testing.T) {
	shop, _ := setupConfirmEnv(t) // 注意：不启动 approve.Hub，模拟 Ferry 桌面版未打开！

	oldMin := minAnswer
	minAnswer = 0
	t.Cleanup(func() { minAnswer = oldMin })

	// 1. 默认设置（桌面端确认）+ 桌面端未打开 → 提示打开 Ferry 桌面版
	csPlain := connect(t, filepath.Dir(shop))
	if text, isErr := callText(t, csPlain, "config_edit", map[string]any{
		"edits":  []map[string]any{{"op": "set", "path": "apps.api.service.jvmArgs", "value": []string{"-Xmx256m"}}},
		"reason": "调内存",
	}); !isErr || !strings.Contains(text, "Ferry 桌面版") {
		t.Fatalf("桌面端模式未开桌面版应报错：%v %s", isErr, text)
	}

	// 2. 开启客户端确认但范围是 standard：L1（config_edit）仍必须打开桌面版
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	st.MCP.ClientConfirmFallback = "two_step"
	settings.Save(st)
	if text, isErr := callText(t, csPlain, "config_edit", map[string]any{
		"edits":  []map[string]any{{"op": "set", "path": "apps.api.service.jvmArgs", "value": []string{"-Xmx256m"}}},
		"reason": "调内存",
	}); !isErr || !strings.Contains(text, "Ferry 桌面版") || !strings.Contains(text, "常规运维") {
		t.Fatalf("standard 模式下离线 config_edit 应提示打开桌面版：%v %s", isErr, text)
	}

	// 3. 切到 scope=all：
	st.MCP.ClientConfirmScope = "all"
	settings.Save(st)

	// 3a. 密码（ask_secret）即便在 all 模式下也必须打开桌面版
	if text, isErr := callText(t, csPlain, "ask_secret", map[string]any{
		"target":  "test",
		"fields":  []map[string]any{{"name": "db_pw", "label": "密码"}},
		"purpose": "测离线密码",
	}); !isErr || !strings.Contains(text, "Ferry 桌面版") {
		t.Fatalf("离线 ask_secret 必须提示打开桌面版：%v %s", isErr, text)
	}

	// 3b. 命中高危（修改构建命令）且 ForceDesktopOnDanger 默认 true → 必须打开桌面版
	if text, isErr := callText(t, csPlain, "config_edit", map[string]any{
		"edits":  []map[string]any{{"op": "set", "path": "apps.api.build.command", "value": "echo hi"}},
		"reason": "改构建命令",
	}); !isErr || !strings.Contains(text, "Ferry 桌面版") || !strings.Contains(text, "高危") {
		t.Fatalf("离线高危操作应提示打开桌面版：%v %s", isErr, text)
	}

	// 3c. 多台灰度暂停部署（canary: 1）在桌面端未打开时应明确提示
	shopMulti := writeProject(t, filepath.Join(t.TempDir(), "multi"), strings.Replace(shopYAML, "  test:\n", "  test2:\n    host: 127.0.0.2\n    port: 1\n    root: /opt/shop\n    runAs: shop\n  test:\n", 1))
	csMulti := connect(t, filepath.Dir(shopMulti))
	if text, isErr := callText(t, csMulti, "deploy", map[string]any{
		"targets": []string{"test", "test2"}, "apps": []string{"api"}, "canary": 1, "skipBuild": true, "reason": "灰度",
	}); !isErr || !strings.Contains(text, "灰度") || !strings.Contains(text, "Ferry 桌面版") {
		t.Fatalf("离线多台灰度部署应提示打开桌面版：%v %s", isErr, text)
	}

	// 3d. 非高危 config_edit 通过两段式 confirm_change 离线直接执行！
	text, isErr := callText(t, csPlain, "config_edit", map[string]any{
		"edits":  []map[string]any{{"op": "set", "path": "apps.api.service.jvmArgs", "value": []string{"-Xmx256m"}}},
		"reason": "离线调 JVM 内存",
	})
	if isErr {
		t.Fatalf("离线两段式预检失败：%s", text)
	}
	var step1 struct {
		DesktopOnline bool `json:"desktopOnline"`
		ConfirmArgs   struct {
			ApprovalID string `json:"approvalId"`
			Target     string `json:"target"`
			Action     string `json:"action"`
		} `json:"confirmArgs"`
	}
	if err := json.Unmarshal([]byte(text), &step1); err != nil || step1.DesktopOnline || step1.ConfirmArgs.ApprovalID == "" {
		t.Fatalf("预检结果不对：%v %s", err, text)
	}
	out, isErr := callText(t, csPlain, "confirm_change", map[string]any{
		"approvalId": step1.ConfirmArgs.ApprovalID,
		"target":     step1.ConfirmArgs.Target,
		"action":     step1.ConfirmArgs.Action,
	})
	if isErr || !strings.Contains(out, `"state": "完成"`) {
		t.Fatalf("离线两段式执行失败：%v %s", isErr, out)
	}
	yamlBytes, _ := os.ReadFile(shop)
	if !strings.Contains(string(yamlBytes), "-Xmx256m") {
		t.Fatalf("ferry.yaml 未实际写入：\n%s", yamlBytes)
	}

	// 3e. TOCTOU 防护：预检后外部修改了 ferry.yaml，离线确认执行必须中止
	text, isErr = callText(t, csPlain, "config_edit", map[string]any{
		"edits":  []map[string]any{{"op": "set", "path": "apps.api.service.jvmArgs", "value": []string{"-Xmx512m"}}},
		"reason": "TOCTOU 测试",
	})
	if isErr || json.Unmarshal([]byte(text), &step1) != nil {
		t.Fatalf("预检失败：%v %s", isErr, text)
	}
	if err := os.WriteFile(shop, append(yamlBytes, []byte("\n# external edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, isErr := callText(t, csPlain, "confirm_change", map[string]any{
		"approvalId": step1.ConfirmArgs.ApprovalID,
		"target":     step1.ConfirmArgs.Target,
		"action":     step1.ConfirmArgs.Action,
	}); !isErr || !strings.Contains(out, "被改过") {
		t.Fatalf("TOCTOU 变更应阻止写入：%v %s", isErr, out)
	}

	// 3f. 通过 MCP App 卡片离线直接执行 create_project（生成 ferry.yaml 并写入设置项目列表）
	csApp := connectMCPApp(t, t.TempDir())
	newDir := t.TempDir()
	rCard, err := csApp.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_project",
		Arguments: map[string]any{
			"spec": map[string]any{
				"projectDir": newDir,
				"project":    "offline-demo",
				"targets":    []map[string]any{{"name": "test", "host": "10.0.0.9", "user": "root"}},
				"apps":       []map[string]any{{"name": "web", "type": "static", "artifact": "dist"}},
			},
			"reason": "离线建项目",
		},
	})
	if err != nil || rCard.IsError {
		t.Fatalf("离线卡片预检失败：%v %+v", err, rCard)
	}
	sc, _ := rCard.StructuredContent.(map[string]any)
	if sc["desktopOnline"] != false {
		t.Fatalf("应标明 desktopOnline=false：%#v", sc)
	}
	dec, err := csApp.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": sc["approvalId"], "appToken": sc["appToken"], "decision": "approve"},
	})
	if err != nil || dec.IsError {
		t.Fatalf("离线卡片执行失败：%v %+v", err, dec)
	}
	createdCfg := filepath.Join(newDir, "ferry.yaml")
	if b, err := os.ReadFile(createdCfg); err != nil || !strings.Contains(string(b), "project: offline-demo") {
		t.Fatalf("离线新建 ferry.yaml 失败：%v %s", err, b)
	}
	stAfter, _ := settings.Load()
	found := slices.ContainsFunc(stAfter.Desktop.Projects, func(p settings.Project) bool { return samePath(p.Path, createdCfg) })
	if !found {
		t.Fatalf("离线新建的项目未加入 settings.Desktop.Projects：%+v", stAfter.Desktop.Projects)
	}

	// 3g. 通过 Elicitation 离线直接执行 config_edit
	csEl := connectElicitOpts(t, filepath.Dir(shop), func(ctx context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	})
	if out, isErr := callText(t, csEl, "config_edit", map[string]any{
		"edits":  []map[string]any{{"op": "set", "path": "apps.api.service.jvmArgs", "value": []string{"-Xmx768m"}}},
		"reason": "表单离线改配置",
	}); isErr || !strings.Contains(out, `"state": "完成"`) {
		t.Fatalf("表单离线执行失败：%v %s", isErr, out)
	}
	yamlBytes, _ = os.ReadFile(shop)
	if !strings.Contains(string(yamlBytes), "-Xmx768m") {
		t.Fatalf("表单离线写入未生效：\n%s", yamlBytes)
	}
}

func TestLegacyProtocolElicitationHeartbeatAndErrorFallback(t *testing.T) {
	_, file := setupConfirmEnv(t)
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	settings.Save(st)

	hub := approve.NewHub(file)
	AttachApprovalHandlers(hub)
	reqs := make(chan approve.Request, 4)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		if kind == "request" {
			reqs <- v.(approve.Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	oldMin, oldHB := minAnswer, elicitHeartbeat
	minAnswer = 40 * time.Millisecond
	elicitHeartbeat = 20 * time.Millisecond
	t.Cleanup(func() { minAnswer, elicitHeartbeat = oldMin, oldHB })

	var mode string
	progressCh := make(chan string, 16)
	srv, closeSrv := New(Options{Version: "test", Dir: t.TempDir()})
	t.Cleanup(closeSrv)
	ct, stTr := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, stTr, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "legacy-ide", Version: "1.0"}, &mcp.ClientOptions{
		ElicitationHandler: func(ctx context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if mode == "err" {
				return nil, errors.New("method not implemented")
			}
			time.Sleep(85 * time.Millisecond)
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			select {
			case progressCh <- req.Params.Message:
			default:
			}
		},
	})
	cs, err := c.Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "旧协议测试"}

	// 1. 旧协议同步 Elicit 等待期间收到心跳进度通知，同意后成功执行
	mode = "ok"
	go func() {
		r := <-reqs
		if !r.ClientConfirmed || r.ConfirmChannel != "elicitation" {
			t.Errorf("旧协议表单同意应置 ClientConfirmed=true：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true, Note: r.ConfirmNote})
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`{"legacy":"ok"}`)})
	}()
	params := &mcp.CallToolParams{Name: "app_control", Arguments: restart}
	params.SetProgressToken("tok-1")
	res, err := cs.CallTool(ctx, params)
	if err != nil || res.IsError {
		t.Fatalf("旧协议表单调用失败：%v %+v", err, res)
	}
	select {
	case msg := <-progressCh:
		if !strings.Contains(msg, "确认") {
			t.Fatalf("心跳消息不对：%q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("旧协议同步 Elicit 等待期间未收到进度心跳")
	}

	// 2. Elicit 报错回退桌面端
	mode = "err"
	go func() {
		r := <-reqs
		if r.ClientConfirmed || len(r.Detail) == 0 || !strings.Contains(r.Detail[0], "不可用") {
			t.Errorf("Elicit 报错应回退桌面端并带原因：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Note: "桌面端已接管"})
	}()
	if text, isErr := callText(t, cs, "app_control", restart); !isErr || !strings.Contains(text, "桌面端已接管") {
		t.Fatalf("Elicit 报错未顺利回退桌面端：%v %s", isErr, text)
	}
}

func TestMCPAppActiveWaitAndAutoResumeFlag(t *testing.T) {
	_, file := setupConfirmEnv(t)
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	settings.Save(st)

	hub := approve.NewHub(file)
	AttachApprovalHandlers(hub)
	reqs := make(chan approve.Request, 4)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		if kind == "request" {
			reqs <- v.(approve.Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	cs := connectMCPApp(t, t.TempDir())
	ctx := context.Background()
	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "双轨等待测试"}

	// 1. 轨道 A（同意）：模型拿到确认卡片后立即调 task_status 阻塞等待，用户点同意后 task_status 立即被唤醒并拿到完成结果，且 modelListening=true
	r1, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "app_control", Arguments: restart})
	if err != nil || r1.IsError {
		t.Fatalf("app_control 失败：%v %+v", err, r1)
	}
	modelText := r1.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(modelText, "task_status") {
		t.Fatalf("返回给模型的文本应引导立即调用 task_status 等待：%s", modelText)
	}
	sc := r1.StructuredContent.(map[string]any)
	opID := sc["opId"].(string)
	approvalID := sc["approvalId"].(string)
	appToken := sc["appToken"].(string)

	type statusOut struct {
		text  string
		isErr bool
	}
	waitCh := make(chan statusOut, 1)
	go func() {
		txt, isErr := callText(t, cs, "task_status", map[string]any{"id": opID, "wait": 10})
		waitCh <- statusOut{txt, isErr}
	}()
	// 给 task_status 一点时间进入阻塞等待
	time.Sleep(60 * time.Millisecond)

	go func() {
		r := <-reqs
		hub.Decide(r.ID, approve.Decision{Approve: true, Note: r.ConfirmNote})
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`{"restarted":true}`)})
	}()
	dec, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": approvalID, "appToken": appToken, "decision": "approve"},
	})
	if err != nil || dec.IsError {
		t.Fatalf("mcp_app_decide 失败：%v %+v", err, dec)
	}
	decMap := dec.StructuredContent.(map[string]any)
	if decMap["state"] != "done" || decMap["modelListening"] != true {
		t.Fatalf("有 task_status 等待时应返回 state=done 且 modelListening=true：%#v", decMap)
	}
	select {
	case out := <-waitCh:
		if out.isErr || !strings.Contains(out.text, `"state": "完成"`) || !strings.Contains(out.text, `"restarted": true`) {
			t.Fatalf("task_status 应立即唤醒并返回完成结果：%v %s", out.isErr, out.text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task_status 未在同意执行完成后及时唤醒")
	}

	// 2. 轨道 A（拒绝）：模型在 task_status 等待中，用户点拒绝 → task_status 立即唤醒返回拒绝错误，且 modelListening=true
	r2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "app_control", Arguments: restart})
	if err != nil || r2.IsError {
		t.Fatalf("第 2 次 app_control 失败：%v %+v", err, r2)
	}
	sc2 := r2.StructuredContent.(map[string]any)
	waitCh2 := make(chan statusOut, 1)
	go func() {
		txt, isErr := callText(t, cs, "task_status", map[string]any{"id": sc2["opId"], "wait": 10})
		waitCh2 <- statusOut{txt, isErr}
	}()
	time.Sleep(60 * time.Millisecond)

	dec2, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": sc2["approvalId"], "appToken": sc2["appToken"], "decision": "reject"},
	})
	if err != nil || dec2.IsError {
		t.Fatalf("mcp_app_decide 拒绝失败：%v %+v", err, dec2)
	}
	dec2Map := dec2.StructuredContent.(map[string]any)
	if dec2Map["state"] != "rejected" || dec2Map["modelListening"] != true {
		t.Fatalf("有 task_status 等待时拒绝应返回 state=rejected 且 modelListening=true：%#v", dec2Map)
	}
	select {
	case out := <-waitCh2:
		if !out.isErr || !strings.Contains(out.text, "拒绝") {
			t.Fatalf("task_status 应返回拒绝错误：%v %s", out.isErr, out.text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task_status 未在拒绝后及时唤醒")
	}

	// 3. 轨道 B（模型未等待）：模型没有调 task_status 就结束了回复，用户点同意 → modelListening=false，卡片可据此用 sendMessage 自动唤醒模型
	r3, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "app_control", Arguments: restart})
	if err != nil || r3.IsError {
		t.Fatalf("第 3 次 app_control 失败：%v %+v", err, r3)
	}
	sc3 := r3.StructuredContent.(map[string]any)
	go func() {
		r := <-reqs
		hub.Decide(r.ID, approve.Decision{Approve: true, Note: r.ConfirmNote})
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`{"ok":true}`)})
	}()
	dec3, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": sc3["approvalId"], "appToken": sc3["appToken"], "decision": "approve"},
	})
	if err != nil || dec3.IsError {
		t.Fatalf("第 3 次 mcp_app_decide 失败：%v %+v", err, dec3)
	}
	dec3Map := dec3.StructuredContent.(map[string]any)
	if dec3Map["state"] != "done" || dec3Map["modelListening"] != false {
		t.Fatalf("模型未调 task_status 时 modelListening 应为 false（由卡片自动 sendMessage 唤醒）：%#v", dec3Map)
	}
	// 如果之后模型补调了一次 task_status，modelDelivered 变为 true，再查 mcp_app_status 时 modelListening 变为 true
	if out, isErr := callText(t, cs, "task_status", map[string]any{"id": sc3["opId"], "wait": 1}); isErr || !strings.Contains(out, `"state": "完成"`) {
		t.Fatalf("补调 task_status 应直接拿到完成结果：%v %s", isErr, out)
	}
	st3, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mcp_app_status",
		Arguments: map[string]any{"approvalId": sc3["approvalId"], "appToken": sc3["appToken"]},
	})
	if err != nil || st3.IsError {
		t.Fatalf("mcp_app_status 失败：%v %+v", err, st3)
	}
	if st3.StructuredContent.(map[string]any)["modelListening"] != true {
		t.Fatalf("模型通过 task_status 拿到终态结果后 modelListening 应为 true：%#v", st3.StructuredContent)
	}
}
