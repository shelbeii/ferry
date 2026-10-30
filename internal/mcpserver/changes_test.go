package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/settings"
)

func TestChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	file := filepath.Join(t.TempDir(), "approver.json")
	oldFile, oldCap := approverFile, waitCap
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile, waitCap = oldFile, oldCap })

	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), shopYAML)
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}}
	settings.Save(st)
	cs := connect(t, t.TempDir())
	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "改了配置"}
	expectErr := func(tool string, args map[string]any, want string) {
		t.Helper()
		if text, isErr := callText(t, cs, tool, args); !isErr || !strings.Contains(text, want) {
			t.Fatalf("%s 应该报「%s」：%v %s", tool, want, isErr, text)
		}
	}

	// 没有打开开关；生产服务器；不在允许的目标里；参数不全
	expectErr("app_control", restart, "允许编程软件请求部署")
	st.MCP.AllowChanges = true
	st.MCP.Targets = []string{"shop/prod", "blog/test"}
	settings.Save(st)
	expectErr("deploy", map[string]any{"target": "prod", "apps": []string{"api"}, "reason": "上线"}, "生产服务器")
	expectErr("app_control", restart, "不在 Ferry 设置里允许")
	st.MCP.Targets = nil
	settings.Save(st)
	expectErr("deploy", map[string]any{"target": "test", "apps": []string{}, "reason": "x"}, "apps")
	expectErr("app_control", map[string]any{"target": "test", "action": "restart", "apps": []string{"web"}, "reason": "x"}, "没有应用 web")
	// 桌面版没打开
	expectErr("app_control", restart, "Ferry 桌面版")

	// 桌面版打开着：模拟桌面版（它也检查一遍）
	hub := approve.NewHub(file)
	hub.Prepare = PrepareRequest
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

	// 拒绝
	go func() { r := <-reqs; hub.Decide(r.ID, approve.Decision{Note: "你在 Ferry 里拒绝了"}) }()
	expectErr("app_control", restart, "没有执行重启 api（test）")

	// 同意，40 秒内做完：直接返回结果
	go func() {
		r := <-reqs
		if r.Action != "restart" || r.Target != "test" || r.Project != "shop" || r.Reason != "改了配置" || r.Client != "test-client 1.0" ||
			r.Session == "" || len(r.Detail) == 0 || !strings.Contains(r.Detail[0], "线上状态：读取失败") {
			t.Errorf("请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true})
		time.Sleep(50 * time.Millisecond)
		hub.Progress(r.ID, "==> 连接 test")
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`[{"app":"api","result":"OK"}]`), Log: []string{"已就绪 api"}})
	}()
	text, isErr := callText(t, cs, "app_control", restart)
	if isErr || !strings.Contains(text, `"state": "完成"`) || !strings.Contains(text, `"result": "OK"`) || !strings.Contains(text, "已就绪 api") {
		t.Fatalf("同意后：%v %s", isErr, text)
	}

	// 等得久：先返回 id，用 task_status 接着查；失败时带上过程
	waitCap = 300 * time.Millisecond
	deploy := map[string]any{"target": "test", "apps": []string{"api"}, "skipBuild": true, "reason": "修了 bug"}
	text, isErr = callText(t, cs, "deploy", deploy)
	var v struct{ ID, State string }
	if isErr || json.Unmarshal([]byte(text), &v) != nil || v.ID == "" || !strings.Contains(v.State, "等用户") {
		t.Fatalf("还没确认：%v %s", isErr, text)
	}
	r := <-reqs
	if r.ID != v.ID || !r.SkipBuild || !strings.Contains(strings.Join(r.Detail, "\n"), "跳过") {
		t.Fatalf("部署请求：%+v", r)
	}
	hub.Decide(r.ID, approve.Decision{Approve: true})
	time.Sleep(50 * time.Millisecond)
	hub.Progress(r.ID, "==> 构建")
	time.Sleep(50 * time.Millisecond)
	if text, _ := callText(t, cs, "task_status", map[string]any{"id": v.ID, "wait": 1}); !strings.Contains(text, "正在执行") || !strings.Contains(text, "==> 构建") {
		t.Fatalf("执行中：%s", text)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		hub.Done(r.ID, approve.Outcome{Error: "构建 build 失败：exit status 1", Log: []string{"[ERROR] Foo.java:[12,5] 找不到符号"}})
	}()
	expectErr("task_status", map[string]any{"id": v.ID, "wait": 5}, "找不到符号")
	expectErr("task_status", map[string]any{"id": "nope"}, "没有这个 id")

	// 结果也记下：同意 / 拒绝
	b, _ := os.ReadFile(LogFile())
	log := string(b)
	if !strings.Contains(log, `"tool":"restart（结果）"`) || !strings.Contains(log, `"decision":"拒绝"`) || !strings.Contains(log, `"decision":"同意"`) ||
		!strings.Contains(log, `"tool":"deploy（结果）"`) {
		t.Errorf("调用记录：\n%s", log)
	}
}
