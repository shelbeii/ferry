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
	"github.com/captain/ferry/internal/engine"
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

func TestExecuteRequestUnifiedCreateAndConfigEdit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	st := settings.Defaults()
	st.MCP.AllowChanges = true
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}

	// 1. DesktopOnline=false 的 create：生成 ferry.yaml 并自动登记到 settings.json
	offlineDir := t.TempDir()
	offlineCfg := filepath.Join(offlineDir, "ferry.yaml")
	specRaw := json.RawMessage(`{"projectDir":"` + filepath.ToSlash(offlineDir) + `","project":"offlineproj","targets":[{"name":"test","host":"127.0.0.1","port":22,"user":"root","root":"/opt/offlineproj","runAs":"demo"}],"apps":[{"name":"web","type":"static","artifact":"dist"}]}`)
	res, err := ExecuteRequest(context.Background(), approve.Request{
		Action:  "create",
		Project: "offlineproj",
		Config:  offlineCfg,
		Spec:    specRaw,
	}, ExecOptions{DesktopOnline: false})
	if err != nil {
		t.Fatalf("离线 create 失败：%v", err)
	}
	if _, err := os.Stat(offlineCfg); err != nil {
		t.Fatalf("离线 create 未生成文件：%v", err)
	}
	loaded, _ := settings.Load()
	found := false
	for _, p := range loaded.Desktop.Projects {
		if samePath(p.Path, offlineCfg) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("离线 create 应写入 settings.json 项目列表：%#v", loaded.Desktop.Projects)
	}
	if m, ok := res.(map[string]any); !ok || m["project"] != "offlineproj" {
		t.Fatalf("返回结果异常：%#v", res)
	}

	// 2. DesktopOnline=true 的 create：生成 ferry.yaml，不直接改写 settings.json（由桌面端 addProject 负责）
	desktopDir := t.TempDir()
	desktopCfg := filepath.Join(desktopDir, "ferry.yaml")
	if _, err := ExecuteRequest(context.Background(), approve.Request{
		Action:  "create",
		Project: "desktopproj",
		Config:  desktopCfg,
		Spec:    json.RawMessage(`{"projectDir":"` + filepath.ToSlash(desktopDir) + `","project":"desktopproj","targets":[{"name":"test","host":"127.0.0.1","port":22,"user":"root","root":"/opt/desktopproj","runAs":"demo"}],"apps":[{"name":"web","type":"static","artifact":"dist"}]}`),
	}, ExecOptions{DesktopOnline: true}); err != nil {
		t.Fatalf("桌面端 create 失败：%v", err)
	}
	if _, err := os.Stat(desktopCfg); err != nil {
		t.Fatalf("桌面端 create 未生成文件：%v", err)
	}
	loaded2, _ := settings.Load()
	for _, p := range loaded2.Desktop.Projects {
		if samePath(p.Path, desktopCfg) {
			t.Fatalf("桌面端 create 不应绕过桌面端状态直接写 settings.json")
		}
	}

	// 3. config-edit 正常写入与 Base 不一致（TOCTOU）拦截
	editInput := engine.ConfigEditInput{
		Edits: []config.Edit{{Op: "set", Path: "targets.test.host", Value: json.RawMessage(`"10.0.0.88"`)}},
	}
	plan, err := engine.PlanConfigEdit(desktopCfg, editInput)
	if err != nil {
		t.Fatal(err)
	}
	editInput.Base = plan.Base
	inBytes, _ := json.Marshal(editInput)
	if _, err := ExecuteRequest(context.Background(), approve.Request{
		Action: "config-edit",
		Config: desktopCfg,
		Input:  inBytes,
	}, ExecOptions{DesktopOnline: true}); err != nil {
		t.Fatalf("统一 config-edit 失败：%v", err)
	}
	b, _ := os.ReadFile(desktopCfg)
	if !strings.Contains(string(b), "10.0.0.88") {
		t.Fatalf("config-edit 未生效：%s", b)
	}
	// 用旧 Base 尝试另一处修改，必须因 TOCTOU 被拒绝
	staleInput := engine.ConfigEditInput{
		Base:  plan.Base,
		Edits: []config.Edit{{Op: "set", Path: "targets.test.host", Value: json.RawMessage(`"10.0.0.77"`)}},
	}
	staleBytes, _ := json.Marshal(staleInput)
	if _, err := ExecuteRequest(context.Background(), approve.Request{
		Action: "config-edit",
		Config: desktopCfg,
		Input:  staleBytes,
	}, ExecOptions{DesktopOnline: true}); err == nil || !strings.Contains(err.Error(), "确认之后被改过了") {
		t.Fatalf("旧 Base 应被 TOCTOU 拦截，got：%v", err)
	}
}

func TestDownloadsQuotaAndCleanup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	base := DownloadsDir()
	mkBatch := func(name string, size int, ageDays int) string {
		d := filepath.Join(base, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "data.bin"), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().AddDate(0, 0, -ageDays)
		if err := os.Chtimes(d, ts, ts); err != nil {
			t.Fatal(err)
		}
		return d
	}

	oldDir := mkBatch("20250101-000001", 4096, 45)
	midDir := mkBatch("20260101-000002", 4096, 5)
	newDir := mkBatch("20260102-000003", 4096, 1)

	cnt, sz := DownloadsUsage()
	if cnt != 3 || sz != 4096*3 {
		t.Fatalf("初始占用异常：count=%d size=%d", cnt, sz)
	}

	// 按天数清理应只删 oldDir
	if removed := CleanDownloads(30, 10<<20, ""); removed != 1 {
		t.Fatalf("按天数应删 1 批，got %d", removed)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("过期下载目录应被删除")
	}

	// 按容量上限清理：保留 keepDir（newDir），删掉更旧的 midDir
	if removed := CleanDownloads(30, 5000, newDir); removed != 1 {
		t.Fatalf("按容量应删 1 批，got %d", removed)
	}
	if _, err := os.Stat(midDir); !os.IsNotExist(err) {
		t.Fatal("超配额的旧目录应被删除")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatal("受保护的本次下载目录 keepDir 不应被删除")
	}

	// 全部清理
	if removed := CleanAllDownloads(); removed != 1 {
		t.Fatalf("全部清理应删 1 批，got %d", removed)
	}
	if cnt, sz := DownloadsUsage(); cnt != 0 || sz != 0 {
		t.Fatalf("全部清理后应为 0：count=%d size=%d", cnt, sz)
	}
}

func TestCallFilterChannelAndLegacyDesktop(t *testing.T) {
	logHome(t)
	now := time.Now()
	entries := []callEntry{
		{Time: now.Add(-4 * time.Minute), Client: "ide", Tool: "deploy（结果）", Op: "op-d", Decision: "同意", OK: true}, // 旧记录：无 channel 字段，按桌面弹窗识别
		{Time: now.Add(-3 * time.Minute), Client: "ide", Tool: "restart（编程软件里确认）", Op: "op-a", Args: json.RawMessage(`{"channel":"mcp-app"}`), Decision: "在编程软件里同意", OK: true},
		{Time: now.Add(-2 * time.Minute), Client: "ide", Tool: "restart（结果）", Op: "op-a", Args: json.RawMessage(`{"channel":"mcp-app"}`), Decision: "同意", OK: true},
		{Time: now.Add(-1 * time.Minute), Client: "ide", Tool: "write-file（结果）", Op: "op-e", Args: json.RawMessage(`{"channel":"elicitation"}`), Decision: "同意", OK: true},
		{Time: now, Client: "ide", Tool: "restart（结果）", Op: "op-t", Args: json.RawMessage(`{"channel":"two-step"}`), Decision: "同意", OK: true},
	}
	for _, e := range entries {
		writeLog(e)
	}

	if pg := Calls(CallFilter{Channel: "desktop"}, 0, 10); pg.Total != 1 {
		t.Fatalf("desktop 渠道筛选应兼容旧记录（1 条），got %d", pg.Total)
	}
	if pg := Calls(CallFilter{Channel: "mcp-app"}, 0, 10); pg.Total != 2 {
		t.Fatalf("mcp-app 渠道应命中 2 条（确认+结果），got %d", pg.Total)
	}
	if pg := Calls(CallFilter{Channel: "elicitation"}, 0, 10); pg.Total != 1 {
		t.Fatalf("elicitation 渠道应命中 1 条，got %d", pg.Total)
	}
	if pg := Calls(CallFilter{Channel: "two-step"}, 0, 10); pg.Total != 1 {
		t.Fatalf("two-step 渠道应命中 1 条，got %d", pg.Total)
	}
}

func TestResourceTemplatesAndCompletion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	secretYAML := shopYAML + "# token=sk-ant-api03-ResourceTemplateSecret1234567890\n"
	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), secretYAML)
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}}
	settings.Save(st)

	cs := connect(t, t.TempDir())
	ctx := context.Background()

	tmpls, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotURIs := map[string]bool{}
	for _, rt := range tmpls.ResourceTemplates {
		gotURIs[rt.URITemplate] = true
	}
	if !gotURIs[projectConfigTemplate] || !gotURIs[projectTargetStatusTmpl] {
		t.Fatalf("缺少参数化资源模板：%v", gotURIs)
	}

	// 读取 ferry://projects/{project}/config：返回脱敏后的 YAML
	cfgRes, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ferry://projects/shop/config"})
	if err != nil || len(cfgRes.Contents) != 1 {
		t.Fatalf("读取项目配置资源失败：%v %+v", err, cfgRes)
	}
	if cfgRes.Contents[0].MIMEType != "text/yaml" || !strings.Contains(cfgRes.Contents[0].Text, "project: shop") {
		t.Fatalf("配置资源内容异常：%+v", cfgRes.Contents[0])
	}
	if strings.Contains(cfgRes.Contents[0].Text, "ResourceTemplateSecret1234567890") {
		t.Fatalf("配置资源未脱敏：%s", cfgRes.Contents[0].Text)
	}

	// 支持中文登记名 URL 编码读取
	encodedURI := "ferry://projects/" + url.PathEscape("商城") + "/config"
	if resCN, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: encodedURI}); err != nil || len(resCN.Contents) != 1 {
		t.Fatalf("中文名 URL 编码读取失败：%v %+v", err, resCN)
	}

	// ref/resource 补全（project 与 target）
	compProj, err := cs.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/resource", URI: projectConfigTemplate},
		Argument: mcp.CompleteParamsArgument{Name: "project", Value: "sh"},
	})
	if err != nil || len(compProj.Completion.Values) == 0 || compProj.Completion.Values[0] != "shop" {
		t.Fatalf("资源模板 project 补全失败：%v %+v", err, compProj)
	}
	compTarget, err := cs.Complete(ctx, &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/resource", URI: projectTargetStatusTmpl},
		Argument: mcp.CompleteParamsArgument{Name: "target", Value: "te"},
		Context:  &mcp.CompleteContext{Arguments: map[string]string{"project": url.PathEscape("商城")}},
	})
	if err != nil || len(compTarget.Completion.Values) == 0 || compTarget.Completion.Values[0] != "test" {
		t.Fatalf("资源模板 target 补全失败：%v %+v", err, compTarget)
	}
}

func TestReadTimeoutAndAppCardExpiryDeletionAndPrecheckReuse(t *testing.T) {
	// 1. withReadTimeout：无截止时间时注入兜底超时；已有截止时间时不缩短
	oldRT := defaultReadTimeout
	defaultReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { defaultReadTimeout = oldRT })

	ctx1, cancel1 := withReadTimeout(context.Background())
	defer cancel1()
	if dl, ok := ctx1.Deadline(); !ok || time.Until(dl) > 200*time.Millisecond {
		t.Fatalf("未设置截止时间时应注入 defaultReadTimeout：%v", ok)
	}
	longCtx, longCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer longCancel()
	ctx2, cancel2 := withReadTimeout(longCtx)
	defer cancel2()
	if dl, ok := ctx2.Deadline(); !ok || time.Until(dl) < 4*time.Minute {
		t.Fatalf("调用方已设长超时时不应被 defaultReadTimeout 截断：%v", time.Until(dl))
	}

	// 2. prepareLocal 复用 extra.go 预检出的 Detail 时不重复发起 SSH 连接
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), shopYAML)
	st := settings.Defaults()
	st.MCP.AllowChanges = true
	settings.Save(st)
	s, _ := newServer(Options{Version: "test", Dir: t.TempDir()})
	defer s.pool.Close()

	reqWithDetail := approve.Request{
		Action: "fw-enable",
		Config: shop,
		Target: "test",
		Allow:  []string{"80/tcp"},
		Detail: []string{"防火墙：打开 firewalld", "放行：80/tcp"},
	}
	if err := s.prepareLocal(&reqWithDetail); err != nil {
		t.Fatalf("prepareLocal 失败：%v", err)
	}
	if len(reqWithDetail.Detail) != 2 || reqWithDetail.Detail[0] != "防火墙：打开 firewalld" {
		t.Fatalf("prepareLocal 应复用已预检好的 Detail，避免重复 SSH：%v", reqWithDetail.Detail)
	}

	// 3. MCP App 卡片到期定时器触发后应直接从 s.appCards 删除，防止内存滞留
	card := &pendingAppCard{
		req:       approve.Request{ID: "op-exp", Action: "restart", Target: "test"},
		op:        &op{req: approve.Request{ID: "op-exp", Action: "restart", Target: "test"}, started: time.Now(), state: "waiting", done: make(chan struct{})},
		appToken:  "tok",
		createdAt: time.Now(),
		expiresAt: time.Now().Add(20 * time.Millisecond),
	}
	s.mu.Lock()
	s.appCards["exp-card"] = card
	s.mu.Unlock()
	done := make(chan struct{})
	card.timer = time.AfterFunc(20*time.Millisecond, func() {
		s.mu.Lock()
		if cur, ok := s.appCards["exp-card"]; ok && cur == card {
			delete(s.appCards, "exp-card")
		}
		s.mu.Unlock()
		close(done)
	})
	<-done
	s.mu.Lock()
	_, stillThere := s.appCards["exp-card"]
	s.mu.Unlock()
	if stillThere {
		t.Fatal("到期的 appCard 应已从 map 删除")
	}
}
