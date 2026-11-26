package mcpserver

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPAppDesktopRejectionPreservesReason(t *testing.T) {
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
	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "测桌面拒绝透传"}

	r1, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "app_control", Arguments: restart})
	if err != nil || r1.IsError {
		t.Fatalf("app_control 失败：%v %+v", err, r1)
	}
	sc := r1.StructuredContent.(map[string]any)
	opID := sc["opId"].(string)

	type statusOut struct {
		text  string
		isErr bool
	}
	waitCh := make(chan statusOut, 1)
	go func() {
		txt, isErr := callText(t, cs, "task_status", map[string]any{"id": opID, "wait": 10})
		waitCh <- statusOut{txt, isErr}
	}()
	time.Sleep(50 * time.Millisecond)

	// 模拟用户在卡片点同意后，桌面端因策略拒绝了该 Commit（例如会话被拉黑）
	go func() {
		r := <-reqs
		hub.Decide(r.ID, approve.Decision{Approve: false, Note: "桌面端测试拒绝原因"})
	}()

	dec, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": sc["approvalId"], "appToken": sc["appToken"], "decision": "approve"},
	})
	if err != nil || dec.IsError {
		t.Fatalf("mcp_app_decide 失败：%v %+v", err, dec)
	}
	decMap := dec.StructuredContent.(map[string]any)
	if decMap["state"] != "rejected" || !strings.Contains(decMap["message"].(string), "桌面端测试拒绝原因") {
		t.Fatalf("桌面端拒绝原因应完整透传而不是「执行结果未知」：%#v", decMap)
	}
	select {
	case out := <-waitCh:
		if !out.isErr || !strings.Contains(out.text, "桌面端测试拒绝原因") || strings.Contains(out.text, "结果未知") {
			t.Fatalf("task_status 应返回真实拒绝原因：%v %s", out.isErr, out.text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task_status 未在桌面端拒绝后唤醒")
	}
}

func TestCallLogRedactsAndClipsArgsAndMatchesOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	secretVal := "sk-ant-api03-SuperSecretTokenValue1234567890"
	huge := strings.Repeat("A", 12000)
	args := redactLogArgs(map[string]any{
		"path":    "/etc/demo.conf",
		"content": "token=" + secretVal + "\n" + huge,
	})
	if strings.Contains(string(args), secretVal) {
		t.Fatalf("calls.jsonl 入参不应包含明文密钥：%s", args)
	}
	if len(args) > 9000 {
		t.Fatalf("calls.jsonl 入参未截断：%d bytes", len(args))
	}

	writeLog(callEntry{Time: time.Now(), Client: "ide 1.0", Tool: "deploy（结果）", Op: "op-abc999", OK: true})
	pg := Calls(CallFilter{Keyword: "op-abc999"}, 0, 10)
	if pg.Total != 1 {
		t.Fatalf("按 op ID 筛选应命中 1 条记录：got %d", pg.Total)
	}
}

func TestConcurrentTaskStatusProgressNotifiers(t *testing.T) {
	o := &op{
		req:     approve.Request{ID: "op-prog", Action: "restart", Target: "test", Apps: []string{"api"}},
		started: time.Now(),
		state:   "running",
		done:    make(chan struct{}),
	}
	var got1, got2 []string
	o.mu.Lock()
	o.notifiers = map[int]func(string){
		0: func(s string) { got1 = append(got1, s) },
		1: func(s string) { got2 = append(got2, s) },
	}
	o.mu.Unlock()

	o.addProgress("step-1")
	o.mu.Lock()
	delete(o.notifiers, 0)
	o.mu.Unlock()
	o.addProgress("step-2")

	if len(got1) != 1 || got1[0] != "step-1" {
		t.Fatalf("订阅者 1 异常：%v", got1)
	}
	if len(got2) != 2 || got2[0] != "step-1" || got2[1] != "step-2" {
		t.Fatalf("订阅者 2 不应被订阅者 1 退出清零：%v", got2)
	}
}

func TestUIMetaStrippedWhenDesktopConfirmMode(t *testing.T) {
	setupConfirmEnv(t)
	ctx := context.Background()
	cs := connectMCPApp(t, t.TempDir())

	// 默认桌面端确认模式：tools/list 不应给改动工具挂 _meta.ui（同时工具总数保持不变）
	listDesktop, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	desktopCount := len(listDesktop.Tools)
	for _, tool := range listDesktop.Tools {
		if tool.Name == "app_control" || tool.Name == "deploy" {
			if tool.Meta != nil && (tool.Meta["ui"] != nil || tool.Meta["ui/resourceUri"] != nil) {
				t.Fatalf("桌面端确认模式下 %s 不应附带 _meta.ui：%+v", tool.Name, tool.Meta)
			}
		}
	}

	// 切换为客户端确认模式：tools/list 立即带上 _meta.ui，且工具数量完全一致
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	settings.Save(st)
	listClient, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listClient.Tools) != desktopCount {
		t.Fatalf("切换确认模式不应改变工具总数：%d vs %d", len(listClient.Tools), desktopCount)
	}
	var foundUI bool
	for _, tool := range listClient.Tools {
		if tool.Name == "app_control" {
			if tool.Meta != nil && tool.Meta["ui"] != nil {
				foundUI = true
			}
		}
	}
	if !foundUI {
		t.Fatal("客户端确认模式下 app_control 必须携带 _meta.ui")
	}
}

func TestSafeCacheNeverMissesUpdatesOrFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	keyFile := filepath.Join(t.TempDir(), "id_ed25519")
	yaml1 := strings.Replace(shopYAML, "    runAs: shop\n", "    runAs: shop\n    identityFile: "+filepath.ToSlash(keyFile)+"\n", 1)
	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), yaml1)
	blogYAML := strings.Replace(shopYAML, "project: shop", "project: blog", 1)
	blog := writeProject(t, filepath.Join(t.TempDir(), "blog"), blogYAML)

	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{
		{Path: shop, Name: "商城"},
		{Path: blog, Name: "博客"},
	}
	settings.Save(st)

	s, _ := newServer(Options{Version: "test", Dir: t.TempDir()})
	defer s.pool.Close()

	// 1. 首次读取：私钥文件不存在时，list_projects 必须实时发现缺失项
	list1 := s.projects()
	if len(list1) != 2 || len(list1[0].Machine) == 0 {
		t.Fatalf("应返回 2 个项目且检测到缺失私钥：%+v", list1)
	}

	// 2. 精准按登记名("商城")和内部项目名("shop")都能完整解析
	f1, err := s.resolve("商城")
	if err != nil || f1.Project != "shop" {
		t.Fatalf("按登记名查找失败：%v %+v", err, f1)
	}
	f2, err := s.resolve("shop")
	if err != nil || f2.Project != "shop" {
		t.Fatalf("按内部 project 名查找失败：%v %+v", err, f2)
	}

	// 3. 立即原地修改 ferry.yaml（同毫秒内增加新应用 worker 与新目标 stage），绝不能读到旧缓存！
	yaml2 := yaml1 + "  - name: worker\n    type: static\n    artifact: {path: dist}\n"
	yaml2 = strings.Replace(yaml2, "  test:\n", "  stage:\n    host: 127.0.0.9\n    port: 1\n    root: /opt/shop\n    runAs: shop\n  test:\n", 1)
	if err := os.WriteFile(shop, []byte(yaml2), 0o644); err != nil {
		t.Fatal(err)
	}
	f3, err := s.resolve("商城")
	if err != nil || f3.App("worker") == nil || f3.Targets["stage"] == nil {
		t.Fatalf("修改 ferry.yaml 后必须立即读到新应用和新目标：%v apps=%v targets=%v", err, f3.Ordered(), f3.TargetNames())
	}

	// 4. 立即写入 local 本机设置覆盖，绝不能漏读！
	localKey := filepath.Join(t.TempDir(), "local_key")
	if err := os.WriteFile(localKey, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteLocal(f3, &config.Local{
		Targets: map[string]*config.LocalTarget{"test": {IdentityFile: localKey}},
	}); err != nil {
		t.Fatal(err)
	}
	f4, err := s.resolve("商城")
	if err != nil || f4.Targets["test"].IdentityFile != localKey {
		t.Fatalf("修改 local 设置后必须立即生效：%v got=%q want=%q", err, f4.Targets["test"].IdentityFile, localKey)
	}

	// 5. 补上原本缺失的私钥文件后，list_projects 的 Machine 必须立即刷新（绝不缓存旧缺失项）
	if err := os.WriteFile(keyFile, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}
	list2 := s.projects()
	if len(list2[0].Machine) != 0 {
		t.Fatalf("补齐私钥后 Machine 缺失项应立即消失：%+v", list2[0].Machine)
	}
}

func TestOpsEvictionAndAskSecretOfflineGuard(t *testing.T) {
	s, _ := newServer(Options{Version: "test", Dir: t.TempDir()})
	defer s.pool.Close()

	now := time.Now()
	s.ops["old-done"] = &op{state: "done", finishedAt: now.Add(-opRetainTTL - time.Minute)}
	s.ops["fresh-done"] = &op{state: "done", finishedAt: now.Add(-time.Minute)}
	s.ops["running"] = &op{state: "running", started: now.Add(-2 * time.Hour)}
	s.ops["waiting-polled"] = &op{state: "done", finishedAt: now.Add(-2 * time.Hour), waiters: 1}

	s.mu.Lock()
	s.sweepOpsLocked(now)
	s.mu.Unlock()

	if _, ok := s.ops["old-done"]; ok {
		t.Fatal("超时已完成的 op 应被淘汰")
	}
	if _, ok := s.ops["fresh-done"]; !ok {
		t.Fatal("刚完成的 op 不应被淘汰")
	}
	if _, ok := s.ops["running"]; !ok {
		t.Fatal("执行中的 op 不应被淘汰")
	}
	if _, ok := s.ops["waiting-polled"]; !ok {
		t.Fatal("还有 task_status 等待者的 op 不应被淘汰")
	}

	// 含 ask_secret 密码引用的请求在桌面端未打开时必须拦截
	if reason := desktopOnlyExecReason(approve.Request{
		Action: "write-file",
		Input:  json.RawMessage(`{"path":"/etc/app.env","content":"PASS={{secret:db_pw}}"}`),
	}); !strings.Contains(reason, "ask_secret") {
		t.Fatalf("包含 {{secret:...}} 的操作离线时应要求桌面版：%q", reason)
	}
	if reason := desktopOnlyExecReason(approve.Request{
		Action: "exec",
		Input:  json.RawMessage(`{"command":"mysql -p\"$FERRY_SECRET_DB_PW\""}`),
	}); !strings.Contains(reason, "ask_secret") {
		t.Fatalf("包含 $FERRY_SECRET_ 的操作离线时应要求桌面版：%q", reason)
	}
}

func TestTaskCancelAndMCPAppRejectWithReason(t *testing.T) {
	_, file := setupConfirmEnv(t)
	st, _ := settings.Load()
	st.MCP.ConfirmIn = "client"
	settings.Save(st)

	hub := approve.NewHub(file)
	AttachApprovalHandlers(hub)
	reqs := make(chan approve.Request, 4)
	cancels := make(chan string, 4)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		switch kind {
		case "request":
			reqs <- v.(approve.Request)
		case "cancel":
			if m, ok := v.(map[string]any); ok {
				cancels <- m["id"].(string)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	cs := connectMCPApp(t, t.TempDir())
	ctx := context.Background()
	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "取消与拒因测试"}

	// 1. 卡片带理由拒绝
	r1, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "app_control", Arguments: restart})
	if err != nil || r1.IsError {
		t.Fatalf("app_control 失败：%v %+v", err, r1)
	}
	sc1 := r1.StructuredContent.(map[string]any)
	dec1, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "mcp_app_decide",
		Arguments: map[string]any{
			"approvalId": sc1["approvalId"],
			"appToken":   sc1["appToken"],
			"decision":   "reject",
			"reason":     "先别重启，还在跑批任务",
		},
	})
	if err != nil || dec1.IsError {
		t.Fatalf("带理由拒绝失败：%v %+v", err, dec1)
	}
	dec1Map := dec1.StructuredContent.(map[string]any)
	if dec1Map["state"] != "rejected" || !strings.Contains(dec1Map["message"].(string), "先别重启") {
		t.Fatalf("拒绝原因未返回：%#v", dec1Map)
	}
	if out, isErr := callText(t, cs, "task_status", map[string]any{"id": sc1["opId"], "wait": 1}); !isErr || !strings.Contains(out, "先别重启") {
		t.Fatalf("task_status 应看到拒绝原因：%v %s", isErr, out)
	}

	// 2. 用 task_cancel 取消尚未点击的待确认卡片
	r2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "app_control", Arguments: restart})
	if err != nil || r2.IsError {
		t.Fatalf("第 2 次 app_control 失败：%v %+v", err, r2)
	}
	sc2 := r2.StructuredContent.(map[string]any)
	if out, isErr := callText(t, cs, "task_cancel", map[string]any{"id": sc2["opId"]}); isErr || !strings.Contains(out, "已取消") {
		t.Fatalf("task_cancel 取消待确认卡片失败：%v %s", isErr, out)
	}
	if decAfter, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": sc2["approvalId"], "appToken": sc2["appToken"], "decision": "approve"},
	}); err != nil || !decAfter.IsError {
		t.Fatalf("已取消的卡片不应允许再同意：%v %+v", err, decAfter)
	}

	// 3. 运行中的桌面端任务可通过 mcp_app_cancel / task_cancel 通知桌面端停止
	oldWait := appDecideWait
	appDecideWait = 40 * time.Millisecond
	t.Cleanup(func() { appDecideWait = oldWait })

	r3, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "app_control", Arguments: restart})
	if err != nil || r3.IsError {
		t.Fatalf("第 3 次 app_control 失败：%v %+v", err, r3)
	}
	sc3 := r3.StructuredContent.(map[string]any)
	go func() {
		r := <-reqs
		hub.Decide(r.ID, approve.Decision{Approve: true, Note: r.ConfirmNote})
		// 模拟收到取消信号后结束任务
		cid := <-cancels
		if cid == r.ID {
			hub.Done(r.ID, approve.Outcome{OK: false, Error: "已停止"})
		}
	}()
	dec3, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mcp_app_decide",
		Arguments: map[string]any{"approvalId": sc3["approvalId"], "appToken": sc3["appToken"], "decision": "approve"},
	})
	if err != nil || dec3.IsError {
		t.Fatalf("第 3 次 decide 失败：%v %+v", err, dec3)
	}
	can3, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mcp_app_cancel",
		Arguments: map[string]any{"approvalId": sc3["approvalId"], "appToken": sc3["appToken"]},
	})
	if err != nil || can3.IsError {
		t.Fatalf("mcp_app_cancel 失败：%v %+v", err, can3)
	}
	if out, isErr := callText(t, cs, "task_status", map[string]any{"id": sc3["opId"], "wait": 3}); !isErr || !strings.Contains(out, "已停止") {
		t.Fatalf("停止后 task_status 应收到已停止错误：%v %s", isErr, out)
	}
}

func TestMCPRootsResourcePromptsAndCompletion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	rootDir := filepath.Join(t.TempDir(), "ws-shop")
	writeProject(t, rootDir, shopYAML)

	srv, closeSrv := New(Options{Version: "test", Dir: t.TempDir()})
	t.Cleanup(closeSrv)
	ct, stTr := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, stTr, nil); err != nil {
		t.Fatal(err)
	}
	rootURI := pathToFileURI(rootDir)
	c := mcp.NewClient(&mcp.Implementation{Name: "roots-ide", Version: "2.0"}, nil)
	c.AddRoots(&mcp.Root{URI: rootURI, Name: "ws-shop"})
	// Roots 协议请求在 <= 2025-11-25 握手协议中生效（2026-07-28 起由 SEP-2577 废弃服务端主动拉取）
	cs, err := c.Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	// 等待 onInitialized 拉取 Roots 完成
	deadline := time.Now().Add(2 * time.Second)
	for {
		txt, isErr := callText(t, cs, "list_projects", map[string]any{})
		if !isErr && strings.Contains(txt, `"currentDir": true`) && strings.Contains(txt, `"project": "shop"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("MCP Roots 未识别出当前目录项目：%v %s", isErr, txt)
		}
		time.Sleep(30 * time.Millisecond)
	}

	// 读取 ferry://projects 只读资源
	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: projectsResourceURI})
	if err != nil || len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, `"project": "shop"`) {
		t.Fatalf("ferry://projects 资源异常：%v %+v", err, res)
	}

	// 验证新增 Prompts 与参数自动补全
	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotNames := map[string]bool{}
	for _, p := range prompts.Prompts {
		gotNames[p.Name] = true
	}
	for _, want := range []string{"diagnose", "check_after_deploy", "pre_deploy_review", "db_diagnose", "server_audit"} {
		if !gotNames[want] {
			t.Fatalf("缺少 Prompt %s，现有：%v", want, gotNames)
		}
	}

	comp, err := cs.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "diagnose"},
		Argument: mcp.CompleteParamsArgument{Name: "target", Value: "te"},
		Context:  &mcp.CompleteContext{Arguments: map[string]string{"project": "shop"}},
	})
	if err != nil || len(comp.Completion.Values) == 0 || comp.Completion.Values[0] != "test" {
		t.Fatalf("Prompt target 自动补全异常：%v %+v", err, comp)
	}
}

func pathToFileURI(dir string) string {
	slash := filepath.ToSlash(filepath.Clean(dir))
	if runtime.GOOS == "windows" && !strings.HasPrefix(slash, "/") {
		slash = "/" + slash
	}
	u := url.URL{Scheme: "file", Path: slash}
	return u.String()
}
