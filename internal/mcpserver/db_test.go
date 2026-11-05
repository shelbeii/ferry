package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/settings"
)

func TestDBExecute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	file := filepath.Join(t.TempDir(), "approver.json")
	oldFile := approverFile
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile = oldFile })

	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), shopYAML)
	cfg, _ := json.Marshal(shop)
	conns := `{"connections":[
 {"id":"local","name":"本机库","kind":"mysql","host":"127.0.0.1","port":3306,"user":"root","database":"app","ai":true,"aiWrite":true},
 {"id":"look","name":"只能查","kind":"mysql","host":"127.0.0.1","port":3306,"user":"root","ai":true},
 {"id":"prod","name":"线上库","kind":"mysql","host":"127.0.0.1","port":3306,"user":"root","ai":true,"aiWrite":true,"via":{"config":` + string(cfg) + `,"target":"prod"}},
 {"id":"test","name":"测试库","kind":"postgres","host":"127.0.0.1","port":5432,"user":"postgres","ai":true,"aiWrite":true,"via":{"config":` + string(cfg) + `,"target":"test"}}
]}`
	os.MkdirAll(filepath.Join(home, "db"), 0o700)
	if err := os.WriteFile(filepath.Join(home, "db", "connections.json"), []byte(conns), 0o600); err != nil {
		t.Fatal(err)
	}
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}}
	settings.Save(st)
	cs := connect(t, t.TempDir())
	expectErr := func(args map[string]any, want string) {
		t.Helper()
		if text, isErr := callText(t, cs, "db_execute", args); !isErr || !strings.Contains(text, want) {
			t.Fatalf("应该报「%s」：%v %s", want, isErr, text)
		}
	}
	upd := map[string]any{"connection": "本机库", "sql": "UPDATE users SET vip = 1", "reason": "给全部用户开会员"}

	// 连接列表里标出能不能改
	text, _ := callText(t, cs, "db_connections", nil)
	if !strings.Contains(text, `"write": true`) || strings.Count(text, `"write": true`) != 3 {
		t.Fatalf("连接：%s", text)
	}

	// 设置里没允许改动；连接没允许修改；生产服务器；语句不行；桌面版没开
	expectErr(upd, "允许编程软件请求")
	st.MCP.AllowChanges = true
	settings.Save(st)
	expectErr(map[string]any{"connection": "只能查", "sql": "DELETE FROM t WHERE id = 1", "reason": "x"}, "没有允许 AI 修改")
	expectErr(map[string]any{"connection": "线上库", "sql": "DELETE FROM t WHERE id = 1", "reason": "x"}, "生产服务器")
	expectErr(map[string]any{"connection": "本机库", "sql": "GRANT ALL ON *.* TO x", "reason": "x"}, "不能执行 GRANT")
	expectErr(upd, "Ferry 桌面版")

	// 桌面版打开着：模拟桌面版（它也检查一遍，确认框的内容按连接重新生成）
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

	go func() {
		r := <-reqs
		if r.Action != "db-exec" || r.DB != "local" || r.DBName != "本机库" || r.Database != "app" || len(r.Stmts) != 1 ||
			!strings.Contains(r.Danger, "没写 WHERE") || len(r.Detail) != 2 || !strings.Contains(r.Detail[1], "UPDATE users SET vip = 1;") ||
			!strings.Contains(r.Detail[0], "直接连") || r.Target != "" || r.Reason != "给全部用户开会员" {
			t.Errorf("请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true})
		hub.Done(r.ID, approve.Outcome{OK: true, Data: json.RawMessage(`{"ok":true,"committed":true,"results":[{"sql":"UPDATE users SET vip = 1","affected":42}]}`)})
	}()
	text, isErr := callText(t, cs, "db_execute", upd)
	if isErr || !strings.Contains(text, `"affected": 42`) || !strings.Contains(text, "完成") {
		t.Fatalf("同意后：%v %s", isErr, text)
	}

	// 经服务器连的：带上服务器；拒绝
	go func() {
		r := <-reqs
		if r.Target != "test" || r.Project != "shop" || !strings.Contains(r.Detail[0], "经服务器 test") || !strings.Contains(r.Detail[0], "也能撤回") {
			t.Errorf("请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Note: "你在 Ferry 里拒绝了"})
	}()
	if text, isErr := callText(t, cs, "db_execute", map[string]any{"connection": "测试库", "sql": "ALTER TABLE t ADD c int", "reason": "加列"}); !isErr || !strings.Contains(text, "没有执行改数据库 测试库") {
		t.Fatalf("拒绝：%v %s", isErr, text)
	}

	// 桌面版这边再查一遍：连接改成不允许、绕过 ferry mcp 直接发的不合规语句
	r := approve.Request{Action: "db-exec", DB: "look", Stmts: []string{"DELETE FROM t WHERE id = 1"}}
	if err := PrepareRequest(&r); err == nil || !strings.Contains(err.Error(), "没有允许 AI 修改") {
		t.Fatalf("没允许的连接：%v", err)
	}
	r = approve.Request{Action: "db-exec", DB: "local", Stmts: []string{"DROP USER root"}}
	if err := PrepareRequest(&r); err == nil || !strings.Contains(err.Error(), "账号") {
		t.Fatalf("不合规的语句：%v", err)
	}
	r = approve.Request{Action: "db-exec", DB: "prod", Stmts: []string{"DELETE FROM t WHERE id = 1"}}
	if err := PrepareRequest(&r); err == nil || !strings.Contains(err.Error(), "生产服务器") {
		t.Fatalf("生产服务器：%v", err)
	}
	r = approve.Request{Action: "db-exec", DB: "local", Stmts: []string{"DELETE FROM t WHERE id = 1; DROP TABLE x"}}
	if err := PrepareRequest(&r); err == nil {
		t.Fatal("一条里塞两条语句应该拒绝")
	}
}

func TestRedisExecute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	file := filepath.Join(t.TempDir(), "approver.json")
	oldFile := approverFile
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile = oldFile })
	conns := `{"connections":[
 {"id":"cache","name":"缓存","kind":"redis","host":"127.0.0.1","port":6379,"database":"2","ai":true,"aiWrite":true},
 {"id":"sql","name":"本机库","kind":"mysql","host":"127.0.0.1","port":3306,"user":"root","ai":true,"aiWrite":true}
]}`
	os.MkdirAll(filepath.Join(home, "db"), 0o700)
	if err := os.WriteFile(filepath.Join(home, "db", "connections.json"), []byte(conns), 0o600); err != nil {
		t.Fatal(err)
	}
	st := settings.Defaults()
	st.MCP.AllowChanges = true
	settings.Save(st)
	cs := connect(t, t.TempDir())
	expectErr := func(tool string, args map[string]any, want string) {
		t.Helper()
		if text, isErr := callText(t, cs, tool, args); !isErr || !strings.Contains(text, want) {
			t.Fatalf("%s 应该报「%s」：%v %s", tool, want, isErr, text)
		}
	}
	// 不用连 Redis 就能拒绝的
	expectErr("redis_query", map[string]any{"connection": "缓存", "command": "KEYS *"}, "SCAN")
	expectErr("redis_query", map[string]any{"connection": "缓存", "command": "CONFIG GET requirepass"}, "不能用 CONFIG")
	expectErr("redis_query", map[string]any{"connection": "本机库", "command": "GET a"}, "不是 Redis")
	expectErr("db_query", map[string]any{"connection": "缓存", "sql": "SELECT 1"}, "redis_query")
	expectErr("db_execute", map[string]any{"connection": "缓存", "sql": "SET a 1", "reason": "x"}, "redis_execute")
	expectErr("redis_execute", map[string]any{"connection": "缓存", "commands": "FLUSHDB", "reason": "x"}, "FLUSHDB")

	// 桌面版收到的请求：命令一行一条，标出删除
	hub := approve.NewHub(file)
	hub.Prepare = PrepareRequest
	reqs := make(chan approve.Request, 2)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		if kind == "request" {
			reqs <- v.(approve.Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	go func() {
		r := <-reqs
		if r.DBKind != "redis" || r.Database != "2" || len(r.Stmts) != 2 || r.Detail[1] != "SET a 1\nDEL b" || !strings.Contains(r.Detail[0], "Redis") ||
			!strings.Contains(r.Detail[0], "db2") || !strings.Contains(r.Danger, "DEL") {
			t.Errorf("请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Note: "你在 Ferry 里拒绝了"})
	}()
	expectErr("redis_execute", map[string]any{"connection": "缓存", "commands": "SET a 1\nDEL b", "reason": "清理"}, "没有执行改数据库 缓存")
}
