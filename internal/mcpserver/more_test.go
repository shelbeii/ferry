package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestMoreTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	withTestKey(t)
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
	settings.Save(st)
	os.WriteFile(filepath.Join(home, "connections.yaml"), []byte("version: 1\nkind: connections\ntargets:\n  box:\n    host: 127.0.0.1\n    port: 1\n"), 0o600)
	cs := connect(t, t.TempDir())
	expectErr := func(tool string, args map[string]any, want string) {
		t.Helper()
		if text, isErr := callText(t, cs, tool, args); !isErr || !strings.Contains(text, want) {
			t.Fatalf("%s 应该报「%s」：%v %s", tool, want, isErr, text)
		}
	}

	// 「新建连接」的服务器：列出来，只能执行只读命令
	text, _ := callText(t, cs, "list_projects", nil)
	if !strings.Contains(text, `"name": "新建连接"`) || !strings.Contains(text, "只能用 run_command") || !strings.Contains(text, `"box"`) {
		t.Fatalf("list_projects：%s", text)
	}
	expectErr("app_status", map[string]any{"project": "新建连接", "target": "box"}, "只能用 run_command")
	expectErr("run_command", map[string]any{"project": "新建连接", "target": "box", "command": "rm -rf /tmp/x"}, "没有执行")
	expectErr("run_command", map[string]any{"project": "新建连接", "target": "box", "command": "df -h"}, "127.0.0.1") // 只读的会去连接（端口 1 连不上）

	// 构建日志
	expectErr("build_log", map[string]any{"project": "shop", "file": "../../x.log"}, "日志名不对")
	f, _ := config.Load(shop)
	logs := filepath.Join((&engine.Engine{File: f, Rep: quiet{}}).ProjectDir(), "logs")
	os.MkdirAll(logs, 0o755)
	os.WriteFile(filepath.Join(logs, "build-20261020-100000-build.log"), []byte("old\n"), 0o644)
	os.WriteFile(filepath.Join(logs, "build-20261022-100000-build.log"), []byte("[INFO] start\n[ERROR] Foo.java:[12,5] 找不到符号\npassword=Secr3t\n"), 0o644)
	text, isErr := callText(t, cs, "build_log", map[string]any{"project": "shop"})
	if isErr || !strings.Contains(text, "build-20261022-100000-build.log") || !strings.Contains(text, "找不到符号") || strings.Contains(text, "Secr3t") {
		t.Fatalf("build_log：%s", text)
	}
	if text, _ := callText(t, cs, "build_log", map[string]any{"project": "shop", "file": "build-20261020-100000-build.log"}); !strings.Contains(text, `"content": "old"`) {
		t.Fatalf("指定日志：%s", text)
	}

	// 识别项目
	app := t.TempDir()
	os.WriteFile(filepath.Join(app, "go.mod"), []byte("module example.com/hello\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(app, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	if text, isErr := callText(t, cs, "detect_project", map[string]any{"dir": app}); isErr || !strings.Contains(text, `"type": "binary"`) {
		t.Fatalf("detect_project：%v %s", isErr, text)
	}

	// 桌面版：软件启停、新建项目
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
	expectErr("software_control", map[string]any{"target": "test", "action": "restart", "name": "../x", "reason": "x"}, "软件名称不对")
	go func() {
		r := <-reqs
		if r.Action != "sw-restart" || r.Software != "redis" || len(r.Detail) == 0 {
			t.Errorf("软件请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true})
		time.Sleep(50 * time.Millisecond)
		hub.Done(r.ID, approve.Outcome{OK: true})
	}()
	if text, isErr := callText(t, cs, "software_control", map[string]any{"target": "test", "action": "restart", "name": "redis", "reason": "改了配置"}); isErr || !strings.Contains(text, "重启软件 redis（test）") {
		t.Fatalf("software_control：%v %s", isErr, text)
	}

	spec := map[string]any{"projectDir": app, "project": "hello",
		"targets": []map[string]any{{"name": "test", "host": "10.0.0.5", "user": "root", "root": "/opt/hello", "runAs": "hello"}},
		"apps":    []map[string]any{{"name": "hello", "type": "binary", "command": "go build -o hello .", "artifact": "hello", "health": "none"}}}
	go func() {
		r := <-reqs
		// 桌面版这边按 spec 重新生成了要写入的 ferry.yaml
		if r.Action != "create" || !strings.Contains(r.Content, "project: hello") || !strings.HasSuffix(r.Config, "ferry.yaml") {
			t.Errorf("新建请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true})
		time.Sleep(50 * time.Millisecond)
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`{"path":"x"}`)})
	}()
	if text, isErr := callText(t, cs, "create_project", map[string]any{"spec": spec, "reason": "接入 Ferry"}); isErr || !strings.Contains(text, "新建项目 hello") {
		t.Fatalf("create_project：%v %s", isErr, text)
	}
	os.WriteFile(filepath.Join(app, "ferry.yaml"), []byte("x"), 0o644)
	expectErr("create_project", map[string]any{"spec": spec, "reason": "x"}, "不会覆盖")

	// 有人绕过 ferry mcp 直接发：桌面版自己生成内容，不用对方给的
	r := approve.Request{Action: "create", Config: filepath.Join(app, "sub", "ferry.yaml"), Content: "假的", Spec: json.RawMessage(`{"projectDir":"` + filepath.ToSlash(app) + `","project":"hello","targets":[{"name":"t","host":"h","root":"/opt/h","runAs":"h"}],"apps":[{"name":"hello","type":"binary","artifact":"hello","health":"none"}]}`)}
	if err := PrepareRequest(&r); err != nil || r.Content == "假的" || !strings.Contains(r.Content, "project: hello") {
		t.Fatalf("Prepare：%v %q", err, r.Content)
	}

	// 提示
	ps, err := cs.ListPrompts(context.Background(), nil)
	if err != nil || len(ps.Prompts) != 2 {
		t.Fatalf("prompts：%v %+v", err, ps)
	}
	g, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "diagnose", Arguments: map[string]string{"app": "api", "symptom": "接口 500"}})
	if err != nil || !strings.Contains(g.Messages[0].Content.(*mcp.TextContent).Text, "接口 500") {
		t.Fatalf("diagnose：%v %+v", err, g)
	}

	// 调用记录：新的在前
	calls := Calls(CallFilter{}, 0, 3).Items
	if len(calls) != 3 || !strings.Contains(string(calls[0]), "create_project") {
		t.Fatalf("记录：%s", calls)
	}
}
