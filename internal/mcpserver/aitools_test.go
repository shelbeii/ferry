package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// 设置里选了在编程软件里确认：编程软件里拒绝 → 不交给桌面版
	st.MCP.ConfirmIn = "client"
	settings.Save(st)
	restart := map[string]any{"target": "test", "action": "restart", "apps": []string{"api"}, "reason": "改了配置"}
	text, isErr = callText(t, cs, "app_control", restart)
	if !isErr || !strings.Contains(text, "test-client 1.0 里拒绝了") || len(asked) != 1 || !strings.Contains(asked[0], "重启 api") || !strings.Contains(asked[0], "改了配置") {
		t.Fatalf("编程软件里拒绝：%v %s %v", isErr, text, asked)
	}
	select {
	case r := <-reqs:
		t.Fatalf("拒绝了还交给了桌面版：%+v", r)
	default:
	}

	// 编程软件里同意 → 桌面版不再弹窗（请求里标明已确认），执行结果交回来
	cs2 := connectElicit(t, t.TempDir(), func(msg string) *mcp.ElicitResult {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}
	})
	go func() {
		r := <-reqs
		if !r.ClientConfirmed || r.Action != "restart" {
			t.Errorf("已确认的请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true, Note: "已在编程软件里确认"})
		time.Sleep(20 * time.Millisecond)
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`[{"app":"api","result":"OK"}]`)})
	}()
	if text, isErr := callText(t, cs2, "app_control", restart); isErr || !strings.Contains(text, `"result": "OK"`) {
		t.Fatalf("编程软件里同意：%v %s", isErr, text)
	}

	// 桌面版这边：设置里没选在编程软件里确认时，不认「已确认」；密码、新建项目只能在 Ferry 里确认
	st.MCP.ConfirmIn = ""
	settings.Save(st)
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
}
