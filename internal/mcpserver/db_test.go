package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/dbx"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/settings"
	sqlite "github.com/ncruces/go-sqlite3/driver"
	"github.com/zalando/go-keyring"
)

func mcpDatabaseTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	return home
}

func TestSQLiteMCPApproval(t *testing.T) {
	home := mcpDatabaseTestHome(t)
	dbFile := filepath.Join(home, "probe.sqlite")
	db, err := sqlite.Open(filepath.ToSlash(dbFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE items(id INT, value INT); INSERT INTO items VALUES (1, 3)"); err != nil {
		t.Fatal(err)
	}
	cn, err := dbx.Save(dbx.Conn{Kind: dbx.SQLite, Name: "本机文件", File: dbFile, AI: true, AIWrite: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	verifyMCPDatabaseApproval(t, home, cn, db, "items", dbFile)
}

// FERRY_KINGBASE_LIVE 指向专用测试实例，账号需能创建临时表和登录角色。
func TestKingbaseMCPApproval(t *testing.T) {
	addr := os.Getenv("FERRY_KINGBASE_LIVE")
	if addr == "" {
		t.Skip("没有设置 FERRY_KINGBASE_LIVE")
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	home := mcpDatabaseTestHome(t)
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	user := os.Getenv("FERRY_KINGBASE_USER")
	if user == "" {
		user = "system"
	}
	c := dbx.Conn{Kind: dbx.Kingbase, Name: "金仓测试库", Host: host, Port: port, User: user, Database: os.Getenv("FERRY_KINGBASE_DATABASE")}
	ctx := t.Context()
	client, release, err := (&dbx.Manager{}).Open(ctx, &c, os.Getenv("FERRY_KINGBASE_PASSWORD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !strings.Contains(strings.ToLower(client.Version), "kingbase") {
		t.Fatalf("实例不是 KingbaseES：%s", client.Version)
	}
	db, err := client.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ferry_mcp_%d", time.Now().UnixNano())
	table := dbx.TableName(dbx.Kingbase, "", "public", name)
	aiUser := name + "_ai"
	role := dbx.Quote(dbx.Kingbase, aiUser)
	aiPassword := "Ferry@Mcp-18!fixture"
	if _, err := db.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD "+dbx.Literal(dbx.Kingbase, aiPassword)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.ExecContext(cleanup, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Error(err)
		}
		if _, err := db.ExecContext(cleanup, "REVOKE USAGE ON SCHEMA public FROM "+role); err != nil {
			t.Error(err)
		}
		if _, err := db.ExecContext(cleanup, "DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	}()
	for _, stmt := range []string{
		"CREATE TABLE " + table + " (id INT PRIMARY KEY, value INT)",
		"INSERT INTO " + table + " VALUES (1, 3)",
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT, UPDATE ON " + table + " TO " + role,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	c.AI, c.AIWrite, c.AIUser = true, true, aiUser
	cn, err := dbx.Save(c, nil, &aiPassword)
	if err != nil {
		t.Fatal(err)
	}
	verifyMCPDatabaseApproval(t, home, cn, db, table, addr)
}

func verifyMCPDatabaseApproval(t *testing.T, home string, cn *dbx.Conn, db *sql.DB, table, location string) {
	t.Helper()
	file := filepath.Join(home, "approver.json")
	oldFile := approverFile
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile = oldFile })
	st := settings.Defaults()
	st.MCP.AllowChanges = true
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, home)
	if cn.AIUser != "" {
		text, failed := callText(t, cs, "db_query", map[string]any{
			"connection": cn.ID, "sql": "SELECT current_user, value FROM " + table + " WHERE id=1",
		})
		if failed || !strings.Contains(text, fmt.Sprintf("%q", cn.AIUser)) {
			t.Fatalf("查询未使用独立 AI 账号：%s", text)
		}
	}
	hub := approve.NewHub(file)
	hub.Prepare = PrepareRequest
	requests := make(chan approve.Request, 2)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		if kind == "request" {
			requests <- v.(approve.Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	query := "UPDATE " + table + " SET value=4 WHERE id=1"
	if cn.AIUser != "" {
		query += " AND current_user = " + dbx.Literal(cn.Kind, cn.AIUser)
	}
	review := func(accept bool) {
		var r approve.Request
		select {
		case r = <-requests:
		case <-t.Context().Done():
			return
		}
		if r.DBKind != cn.Kind || r.DBCheck != cn.ApprovalKey() || len(r.Detail) != 2 ||
			!strings.Contains(r.Detail[0], location) || (cn.AIUser != "" && !strings.Contains(r.Detail[0], cn.AIUser)) ||
			r.Detail[1] != query+";" {
			t.Errorf("确认请求缺少完整 SQL、位置或 AI 账号：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: accept})
		if accept {
			e := &engine.Engine{Ctx: t.Context()}
			result, err := e.AIDBExec(t.Context(), cn, r.Database, r.Stmts)
			if err != nil || !result.OK {
				t.Errorf("同意后写入：%+v, %v", result, err)
				hub.Done(r.ID, approve.Outcome{OK: false})
				return
			}
			data, _ := json.Marshal(result)
			hub.Done(r.ID, approve.Outcome{OK: true, Data: data})
		}
	}
	for _, tc := range []struct {
		accept bool
		value  int
	}{{false, 3}, {true, 4}} {
		go review(tc.accept)
		text, failed := callText(t, cs, "db_execute", map[string]any{"connection": cn.ID, "sql": query, "reason": "验证数据库的 AI 权限"})
		if failed == tc.accept {
			t.Fatalf("确认结果：%s", text)
		}
		var n int
		if err := db.QueryRow("SELECT value FROM " + table + " WHERE id=1").Scan(&n); err != nil || n != tc.value {
			t.Fatalf("数据库写入结果：%d, %v", n, err)
		}
	}
	cn.ReadOnly = true
	if _, err := dbx.Save(*cn, nil, nil); err != nil {
		t.Fatal(err)
	}
	if text, failed := callText(t, cs, "db_execute", map[string]any{"connection": cn.ID, "sql": query, "reason": "验证只读限制"}); !failed || !strings.Contains(text, "只读") {
		t.Fatalf("只读连接未拒绝修改：%s", text)
	}
	if text, failed := callText(t, cs, "db_query", map[string]any{"connection": cn.ID, "sql": "SELECT value FROM " + table}); failed || !strings.Contains(text, "\"4\"") {
		t.Fatalf("只读查询：%s", text)
	}
}

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
