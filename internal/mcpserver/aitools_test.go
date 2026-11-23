package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectElicit 连上 ferry mcp 的编程软件支持在自己的界面里弹表单，answer 是用户的回答。
func connectElicit(t *testing.T, dir string, answer func(msg string) *mcp.ElicitResult) *mcp.ClientSession {
	t.Helper()
	srv, closeSrv := New(Options{Version: "test", Dir: dir})
	t.Cleanup(closeSrv)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		ElicitationHandler: func(ctx context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return answer(r.Params.Message), nil
		}})
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestAITools(t *testing.T) {
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
	settings.Save(st)

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

	// 填密码：在 Ferry 里填，编程软件只拿到代号
	var asked []string
	cs := connectElicit(t, t.TempDir(), func(msg string) *mcp.ElicitResult {
		asked = append(asked, msg)
		return &mcp.ElicitResult{Action: "decline"}
	})
	fields := []ai.SecretField{{Name: "db_pw", Label: "数据库新密码", Confirm: true}}
	go func() {
		r := <-reqs
		if r.Action != "secret" || len(r.Fields) != 1 || r.Fields[0].Name != "db_pw" || r.ClientConfirmed {
			t.Errorf("填密码的请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true})
		out, _ := json.Marshal(map[string]string{"output": "用户在 Ferry 里填好了：代号 {{secret:db_pw}}"})
		hub.Done(r.ID, approve.Outcome{OK: true, Data: out})
	}()
	text, isErr := callText(t, cs, "ask_secret", map[string]any{"target": "test", "fields": fields, "purpose": "改数据库密码"})
	if isErr || !strings.Contains(text, "{{secret:db_pw}}") {
		t.Fatalf("ask_secret：%v %s", isErr, text)
	}
	if len(asked) != 0 {
		t.Fatalf("密码不能在编程软件里问：%v", asked)
	}
	if text, isErr := callText(t, cs, "ask_secret", map[string]any{"target": "test", "fields": []map[string]any{{"name": "Bad", "label": "x"}}, "purpose": "x"}); !isErr || !strings.Contains(text, "名字") {
		t.Fatalf("名字不对应该报错：%s", text)
	}

	// An older desktop setting still routes each request to Ferry.
	st.MCP.ConfirmIn = "client"
	settings.Save(st)
	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "改了配置"}
	go func() {
		r := <-reqs
		if r.ClientConfirmed || r.Action != "restart" || r.Reason != "改了配置" {
			t.Errorf("Ferry 审批请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Note: "你在 Ferry 里拒绝了"})
	}()
	if text, isErr := callText(t, cs, "app_control", restart); !isErr || !strings.Contains(text, "你在 Ferry 里拒绝了") || len(asked) != 0 {
		t.Fatalf("Ferry 里拒绝：%v %s，客户端弹窗：%v", isErr, text, asked)
	}

	r := approve.Request{Action: "restart", Config: shop, Target: "test", Apps: []string{"api"}, ClientConfirmed: true}
	if err := PrepareRequest(&r); err == nil || !strings.Contains(err.Error(), "在 Ferry 里确认") {
		t.Fatalf("没选在编程软件里确认：%v", err)
	}
	r = approve.Request{Action: "secret", Config: shop, Target: "test", Fields: fields, ClientConfirmed: true}
	if err := PrepareRequest(&r); err == nil {
		t.Fatal("填密码不能在编程软件里确认")
	}
	r = approve.Request{Action: "exec", Tool: "write_file", Config: shop, Target: "test", Input: json.RawMessage(`{"command":"ls"}`)}
	if err := PrepareRequest(&r); err == nil || !strings.Contains(err.Error(), "对不上") {
		t.Fatalf("操作和工具对不上：%v", err)
	}

	// 装软件时补充模板参数没有的设置：附加配置原样交给桌面版（确认框的内容要连上服务器才算得出，见 engine 的测试和 e2e）
	hub.Prepare = func(*approve.Request) error { return nil }
	t.Cleanup(func() { hub.Prepare = PrepareRequest })
	go func() {
		r := <-reqs
		var in map[string]any
		json.Unmarshal(r.Input, &in)
		extra, _ := in["extra"].(map[string]any)
		if r.Action != "install" || r.Tool != "software_install" || extra["innodb_buffer_pool_size"] != "1G" || extra["max_connections"] != float64(900) {
			t.Errorf("装软件的请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Note: "先不装"})
	}()
	install := map[string]any{"target": "test", "template": "mysql", "reason": "装数据库", "extra": map[string]any{"innodb_buffer_pool_size": "1G", "max_connections": 900}}
	if text, isErr := callText(t, cs, "software_install", install); !isErr || !strings.Contains(text, "先不装") {
		t.Fatalf("software_install：%v %s", isErr, text)
	}
}
