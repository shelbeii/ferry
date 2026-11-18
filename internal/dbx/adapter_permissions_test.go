package dbx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAdapterAIValidation(t *testing.T) {
	for _, tt := range []struct{ kind, db, src string }{
		{SQLServer, "app", "SELECT 1 EXEC sp_who"},
		{SQLServer, "app", "SELECT password_hash FROM [sys].[sql_logins]"},
		{ClickHouse, "app", "SELECT * FROM url('https://example.com/a')"},
		{ClickHouse, "app", "SELECT * FROM `system`.`users`"},
		{MongoDB, "app", `{"collection":"items","filter":{"$or":[{"$where":"return true"}]}}`},
		{MongoDB, "admin", `{"collection":"items"}`},
		{MongoDB, "app", `{"collection":"system.users"}`},
		{Dameng, "DM", "SELECT seq.NEXTVAL FROM DUAL"},
		{Kingbase, "test", "SELECT rolname FROM sys_catalog.sys_authid"},
		{Kingbase, "test", "SELECT sys_catalog.sys_sleep(1)"},
	} {
		if err := CheckAIQuery(tt.kind, tt.db, tt.src); err == nil {
			t.Errorf("%s 允许了 AI 查询 %q", tt.kind, tt.src)
		}
	}
	for _, tt := range []struct{ kind, db, src string }{
		{SQLServer, "app", "UPDATE [t] SET a=1 WHERE id=1 COMMIT"},
		{SQLServer, "app", "UPDATE [t] SET a=1 WHERE id=1 DELETE FROM [t]"},
		{SQLServer, "app", "CREATE LOGIN bad WITH PASSWORD='secret'"},
		{SQLServer, "app", "UPDATE [t] SET a=1 WHERE id=1 /* /* nested */ COMMIT */ EXEC sp_who"},
		{SQLite, "main", "ATTACH DATABASE 'another.db' AS another"},
		{ClickHouse, "app", "ALTER TABLE t UPDATE a=1 WHERE id=1; DELETE FROM t WHERE id=2"},
		{ClickHouse, "app", "CREATE TABLE t (n UInt64) ENGINE=URL('https://example.com/a', 'CSV')"},
		{MongoDB, "app", `{"operation":"deleteMany","collection":"items"}`},
		{MongoDB, "app", `{"operation":"dropDatabase","collection":"items"}`},
		{MongoDB, "app", `{"operation":"renameCollection","collection":"items","to":"system.users"}`},
		{Dameng, "DM", "CREATE TABLE t(id INT); INSERT INTO t VALUES(1)"},
		{Kingbase, "test", "CREATE USER somebody"},
		{Kingbase, "test", "DELETE FROM sys_catalog.sys_authid WHERE rolname = 'example'"},
	} {
		if _, err := CheckAIWrite(tt.kind, tt.db, tt.src); err == nil {
			t.Errorf("%s 允许了 AI 改动 %q", tt.kind, tt.src)
		}
	}
	for _, s := range []string{"WITH c AS (SELECT 1) DELETE FROM t", "SELECT @x:=1", "SELECT 1 /*T! SET SQL_MODE='ANSI' */"} {
		if err := checkTiDBRead("app", s); err == nil {
			t.Errorf("TiDB 允许了 %q", s)
		}
	}
}

func verifySQLAIWrite(t *testing.T, cl *Client, db, table, column string) {
	t.Helper()
	ctx := t.Context()
	q := "SELECT " + column + " FROM " + table + " WHERE id=1"
	first, err := cl.AIQuery(ctx, db, q, 1)
	if err != nil || len(first.Rows) != 1 {
		t.Fatalf("AI 读：%+v, %v", first, err)
	}
	c := cl.Conn
	c.AIWrite = false
	unauthorized := "UPDATE " + table + " SET " + column + "='blocked' WHERE id=1"
	blocked, release, err := (&Manager{}).Open(ctx, &c, cl.password, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := blocked.AIExec(ctx, db, []string{unauthorized}); !errors.Is(err, ErrAIWriteNotAllowed) {
		t.Fatalf("未授权连接执行了 AI 写入：%v", err)
	}
	update := "UPDATE " + table + " SET " + column + "='changed' WHERE id=1"
	rolled, err := cl.AIExec(ctx, db, []string{update, "DELETE FROM ferry_nonexistent_probe_table WHERE id=1"})
	if err != nil || rolled.OK || rolled.Committed {
		t.Fatalf("AI 改动回滚：%+v, %v", rolled, err)
	}
	after, err := cl.AIQuery(ctx, db, q, 1)
	if err != nil || len(after.Rows) != 1 || after.Rows[0][0] != first.Rows[0][0] {
		t.Fatalf("回滚后：%+v, %v", after, err)
	}
	changed, err := cl.AIExec(ctx, db, []string{update})
	if err != nil || !changed.OK || !changed.Committed {
		t.Fatalf("AI 写入：%+v, %v", changed, err)
	}
	after, err = cl.AIQuery(ctx, db, q, 1)
	if err != nil || len(after.Rows) != 1 || after.Rows[0][0] != "changed" {
		t.Fatalf("写入后：%+v, %v", after, err)
	}
}

// 仅在可创建临时测试表的专用实例运行。FERRY_DM_LIVE、FERRY_DM_PASSWORD。
func TestDamengLive(t *testing.T) {
	c := liveAdapterConn(t, Dameng, "FERRY_DM_LIVE", "SYSDBA")
	verifyNativeSQLLive(t, c, os.Getenv("FERRY_DM_PASSWORD"), "", "dm database server")
}

// 使用 PG 协议的测试实例，允许在测试库的 public 模式创建临时表。
func TestKingbaseLive(t *testing.T) {
	user := os.Getenv("FERRY_KINGBASE_USER")
	if user == "" {
		user = "system"
	}
	c := liveAdapterConn(t, Kingbase, "FERRY_KINGBASE_LIVE", user)
	c.Database = os.Getenv("FERRY_KINGBASE_DATABASE")
	verifyNativeSQLLive(t, c, os.Getenv("FERRY_KINGBASE_PASSWORD"), "public", "kingbase")
}

// 单独验证复用的 PostgreSQL 协议路径，实库测试另用 TestKingbaseLive。
func TestKingbasePostgresProtocol(t *testing.T) {
	c := liveAdapterConn(t, Kingbase, "FERRY_KINGBASE_PG_LIVE", "system")
	verifyNativeSQLLive(t, c, os.Getenv("FERRY_KINGBASE_PG_PASSWORD"), "public", "postgresql")
}

func verifyNativeSQLLive(t *testing.T, c Conn, pw, schema, version string) {
	t.Helper()
	ctx := t.Context()
	c.AI, c.AIWrite = true, true
	cl := openLiveAdapter(t, ctx, c, pw, false)
	t.Log(cl.Version)
	if !strings.Contains(strings.ToLower(cl.Version), version) {
		t.Fatalf("实例版本与测试类型不符：%s", cl.Version)
	}
	db, err := cl.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	dbs, err := cl.Databases(ctx)
	if err != nil || !slices.ContainsFunc(dbs, func(d Database) bool { return d.Name == cl.databaseName("") }) {
		t.Fatalf("默认库未出现在数据库列表中：%+v, %v", dbs, err)
	}
	name := fmt.Sprintf("ferry_perm_%d", time.Now().UnixNano())
	if c.Kind == Dameng {
		name = strings.ToUpper(name)
	}
	table := TableName(c.Kind, "", schema, name)
	for _, stmt := range []string{
		"CREATE TABLE " + table + " (id INT PRIMARY KEY, label VARCHAR(40))",
		"INSERT INTO " + table + " VALUES (1, '中文'), (2, 'second')",
		"ALTER TABLE " + table + " ADD added INT",
	} {
		r, err := cl.AIExec(ctx, "", []string{stmt})
		if err != nil || !r.OK {
			t.Fatalf("AI 写入 %q：%+v, %v", stmt, r, err)
		}
		if strings.HasPrefix(stmt, "CREATE TABLE") {
			defer db.ExecContext(ctx, "DROP TABLE "+table)
		}
	}
	if c.Kind == Kingbase {
		labelIndex := Quote(Kingbase, name+"_label_idx")
		if _, err := db.ExecContext(ctx, "CREATE INDEX "+labelIndex+" ON "+table+" (label)"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := cl.Tables(ctx, cl.databaseName(""))
	if err != nil || !slices.ContainsFunc(list, func(x Table) bool { return x.Name == name }) {
		t.Fatalf("找不到临时表：%v", err)
	}
	info, err := cl.TableInfo(ctx, "", schema, name)
	if err != nil || len(info.Columns) != 3 || len(info.PrimaryKey) != 1 {
		t.Fatalf("表结构：%+v, %v", info, err)
	}
	if c.Kind == Kingbase {
		if len(info.Indexes) != 2 || slices.ContainsFunc(list, func(x Table) bool { return x.Schema == "sys_catalog" || x.Schema == "sys" }) {
			t.Fatalf("金仓索引或系统模式筛选不正确：%+v", info.Indexes)
		}
		ro := c
		ro.ReadOnly, ro.AIWrite = true, false
		reader := openLiveAdapter(t, ctx, ro, pw, false)
		rs, err := reader.Query(ctx, "", "SELECT label FROM "+table+" WHERE id=1", QueryOptions{})
		if err != nil || len(rs) != 1 || rs[0].Error != "" || len(rs[0].Rows) != 1 || rs[0].Rows[0][0] != "中文" {
			t.Fatalf("只读连接查询：%+v, %v", rs, err)
		}
		if _, err := reader.Query(ctx, "", "UPDATE "+table+" SET label='blocked' WHERE id=1", QueryOptions{}); err == nil {
			t.Fatal("只读连接允许了修改")
		}
	}
	// 达梦未加引号的列名由服务器转为大写。
	r, err := cl.Rows(ctx, "", schema, name, BrowseOptions{Offset: 1, Limit: 1})
	if err != nil || r.Error != "" || len(r.Rows) != 1 || r.Rows[0][1] != "second" {
		t.Fatalf("分页：%+v, %v", r, err)
	}
	if x, err := cl.Export(ctx, "", ExportOptions{Query: "SELECT label FROM " + table, Out: filepath.Join(t.TempDir(), "out.csv")}, nil); err != nil || x.Rows != 2 {
		t.Fatalf("CSV：%+v, %v", x, err)
	}
	verifySQLAIWrite(t, cl, cl.databaseName(""), table, "label")
	if c.Kind == Dameng {
		verifyDamengReader(t, c, cl, name)
	}
}

func verifyDamengReader(t *testing.T, c Conn, cl *Client, name string) {
	t.Helper()
	ctx := t.Context()
	db, err := cl.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	reader := fmt.Sprintf("FERRY_RO_%d", time.Now().UnixNano())
	const password = "Ferry#ReadOnly891"
	if _, err := db.ExecContext(ctx, "CREATE USER "+reader+" IDENTIFIED BY \""+password+"\""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, "DROP USER "+reader); err != nil {
			t.Error(err)
		}
	})
	table := TableName(Dameng, "", "SYSDBA", name)
	for _, s := range []string{"GRANT CREATE SESSION TO " + reader, "GRANT SELECT ON " + table + " TO " + reader} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	c.AIUser, c.ReadOnly = reader, true
	via := openLiveAdapter(t, ctx, *c.ForAI(), password, true)
	if r, err := via.AIQuery(ctx, "", "SELECT label FROM "+table+" WHERE id=1", 1); err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "changed" {
		t.Fatalf("达梦 AI 独立账号：%+v, %v", r, err)
	}
}
