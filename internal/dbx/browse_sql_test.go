package dbx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	sqlitedriver "github.com/ncruces/go-sqlite3/driver"
)

func TestSQLiteFile(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	t.Setenv("FERRY_HOME", filepath.Join(dir, "ferry"))
	path := filepath.Join(dir, "客户 #100%.sqlite")
	writer, err := sqlitedriver.Open(sqliteURI(path, "rwc"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE [work;items] (id INTEGER PRIMARY KEY, title TEXT NOT NULL, payload BLOB, display TEXT AS (title || '!') STORED)",
		"INSERT INTO [work;items] (id, title, payload) VALUES (2, '第二条', x'0061'), (1, '第一条', NULL), (3, '第三条', NULL)",
		"CREATE VIEW work_view AS SELECT id, title FROM [work;items]",
	} {
		if _, err := writer.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	c, err := Save(Conn{Name: "本机", Kind: SQLite, File: path, SavePassword: true, AI: true}, nil, nil)
	if err != nil || !c.ReadOnly || c.SavePassword || c.AI {
		t.Fatalf("保存 SQLite：%+v, %v", c, err)
	}
	c, err = Get(c.ID)
	if err != nil || c.File != path {
		t.Fatalf("读取 SQLite 连接：%+v, %v", c, err)
	}
	m := Manager{}
	cl, release, err := m.Open(ctx, c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !strings.HasPrefix(cl.Version, "SQLite ") {
		t.Fatal(cl.Version)
	}
	dbs, err := cl.Databases(ctx)
	if err != nil || !slices.Equal(dbs, []Database{{Name: "main"}}) {
		t.Fatalf("库列表：%v, %v", dbs, err)
	}
	tables, err := cl.Tables(ctx, "main")
	if err != nil || len(tables) != 2 || tables[0].Name != "work;items" || tables[1].Type != "view" {
		t.Fatalf("表和视图：%+v, %v", tables, err)
	}
	info, err := cl.TableInfo(ctx, "main", "", "work;items")
	if err != nil || len(info.Columns) != 4 || !info.Columns[3].Generated || !slices.Equal(info.PrimaryKey, []string{"id"}) {
		t.Fatalf("表结构：%+v, %v", info, err)
	}
	r, err := cl.Rows(ctx, "", "", "work;items", BrowseOptions{Offset: 1, Limit: 1})
	if err != nil || r.Error != "" || len(r.Rows) != 1 || r.Rows[0][1] != "第二条" || !r.More {
		t.Fatalf("分页：%+v, %v", r, err)
	}
	rows, err := cl.Query(ctx, "", "SELECT 'a;b'; SELECT title FROM [work;items] WHERE id = 3; PRAGMA table_info([work;items])", QueryOptions{})
	if err != nil || len(rows) != 3 || rows[0].Rows[0][0] != "a;b" || rows[1].Rows[0][0] != "第三条" || len(rows[2].Rows) != 3 {
		t.Fatalf("查询与引号：%+v, %v", rows, err)
	}
	csv := filepath.Join(dir, "out.csv")
	got, err := cl.Export(ctx, "", ExportOptions{Table: "work;items", Out: csv}, nil)
	if err != nil || got.Rows != 3 {
		t.Fatalf("CSV：%+v, %v", got, err)
	}
	data, err := os.ReadFile(csv)
	if err != nil || !bytes.Contains(data, []byte("第二条")) {
		t.Fatalf("CSV 内容：%q, %v", data, err)
	}
	for _, out := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if _, err := cl.Export(ctx, "", ExportOptions{Query: "SELECT 1", Out: out}, nil); err == nil {
			t.Fatalf("允许覆盖 SQLite 文件 %q", out)
		}
	}
	other := filepath.Join(dir, "other.db")
	db, err := cl.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"ATTACH DATABASE " + Literal(SQLite, other) + " AS other",
		"CREATE TEMP TABLE t(id)",
		"PRAGMA query_only=OFF",
		"WITH t AS (SELECT 1) DELETE FROM [work;items]",
		"SELECT load_extension('test')",
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Errorf("只读连接执行了 %q", stmt)
		}
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ATTACH 创建了文件：%v", err)
	}
	if _, err := cl.Export(ctx, "", ExportOptions{Query: "PRAGMA user_version=2", Out: csv}, nil); err == nil {
		t.Fatal("导出允许修改 PRAGMA")
	}
	if after, err := os.ReadFile(csv); err != nil || !bytes.Equal(data, after) {
		t.Fatalf("失败的导出覆盖了原来的 CSV：%v", err)
	}
	if err := Remove(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("删除连接影响了数据库文件：%v", err)
	}
}

func TestSQLiteMissingAndInvalidFile(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.db")
	if err := os.WriteFile(invalid, bytes.Repeat([]byte("not a database"), 20), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.db")
	for _, path := range []string{invalid, missing, dir} {
		m := Manager{}
		c := Conn{Kind: SQLite, File: path}
		if _, release, err := m.Open(t.Context(), &c, "", nil); err == nil {
			release()
			t.Errorf("意外打开了 %s", path)
		}
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("测试连接创建了文件：%v", err)
	}
}

// 专用测试实例，账号需能创建临时数据库。FERRY_CLICKHOUSE_LIVE、FERRY_CLICKHOUSE_USER、FERRY_CLICKHOUSE_PASSWORD。
func TestClickHouseLive(t *testing.T) {
	c := liveAdapterConn(t, ClickHouse, "FERRY_CLICKHOUSE_LIVE", os.Getenv("FERRY_CLICKHOUSE_USER"))
	pw := os.Getenv("FERRY_CLICKHOUSE_PASSWORD")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m := Manager{}
	var pwErr *NeedPasswordError
	if _, release, err := m.Open(ctx, &c, "invalid-test-password", nil); !errors.As(err, &pwErr) {
		if release != nil {
			release()
		}
		t.Fatalf("错误密码：%v", err)
	}
	cl := openLiveAdapter(t, ctx, c, pw, false)
	if !strings.HasPrefix(cl.Version, "ClickHouse ") {
		t.Fatal(cl.Version)
	}
	setup := clickhouse.OpenDB(&clickhouse.Options{Addr: []string{c.Addr()}, Auth: clickhouse.Auth{Database: "default", Username: c.User, Password: pw}})
	defer setup.Close()
	database := fmt.Sprintf("ferry_test_%d", time.Now().UnixNano())
	qdb := Quote(ClickHouse, database)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+qdb); err != nil {
		t.Fatal(err)
	}
	defer setup.ExecContext(ctx, "DROP DATABASE "+qdb)
	const table = "test; `data\\rows"
	qtable := TableName(ClickHouse, database, "", table)
	for _, stmt := range []string{
		"CREATE TABLE " + qtable + " (id UInt64, label Nullable(String), n UInt64 DEFAULT 0) ENGINE = MergeTree ORDER BY id",
		"INSERT INTO " + qtable + " (id, label, n) VALUES (2, '中文', 18446744073709551615), (1, 'one', 0), (3, NULL, 0)",
		"CREATE VIEW " + qdb + ".read_view AS SELECT id, label FROM " + qtable,
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	c.Database = database
	via := openLiveAdapter(t, ctx, c, pw, true)
	dbs, err := via.Databases(ctx)
	if err != nil || !slices.Contains(dbs, Database{Name: database}) || !slices.Contains(dbs, Database{Name: "system", System: true}) {
		t.Fatalf("库列表：%v, %v", dbs, err)
	}
	tables, err := via.Tables(ctx, "")
	if err != nil || len(tables) != 2 || tables[0].Type != "view" || tables[1].Name != table {
		t.Fatalf("表和视图：%+v, %v", tables, err)
	}
	info, err := via.TableInfo(ctx, "", "", table)
	if err != nil || len(info.Columns) != 3 || !info.Columns[1].Nullable || info.Columns[2].Default == nil || !slices.Equal(info.PrimaryKey, []string{"id"}) {
		t.Fatalf("表结构：%+v, %v", info, err)
	}
	r, err := via.Rows(ctx, "", "", table, BrowseOptions{Offset: 1, Limit: 1})
	if err != nil || r.Error != "" || len(r.Rows) != 1 || !r.More || r.Rows[0][1] != "中文" || r.Rows[0][2] != "18446744073709551615" {
		t.Fatalf("分页与数值精度：%+v, %v", r, err)
	}
	got, err := via.Query(ctx, "", "SELECT getSetting('readonly') AS mode; SHOW TABLES; "+r.SQL+"; SELECT $query$a;b$query$", QueryOptions{})
	if err != nil || len(got) != 4 || len(got[0].Rows) != 1 || got[0].Rows[0][0] != "1" || len(got[1].Rows) != 2 || len(got[2].Rows) != 1 || len(got[3].Rows) != 1 || got[3].Rows[0][0] != "a;b" {
		t.Fatalf("只读查询：%+v, %v", got, err)
	}
	pool, err := via.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "CREATE TABLE "+qdb+".should_not_exist (id Int32) ENGINE=Memory"); err == nil {
		t.Fatal("数据库连接允许 CREATE TABLE")
	}
	if err := pool.QueryRowContext(ctx, "SELECT 1 SETTINGS readonly=0").Scan(new(int)); err == nil {
		t.Fatal("数据库连接允许关闭 readonly")
	}
	out := filepath.Join(t.TempDir(), "clickhouse.csv")
	f, err := via.Export(ctx, "", ExportOptions{Query: "SELECT n FROM " + qtable + " WHERE id=2", Out: out}, nil)
	if err != nil || f.Rows != 1 {
		t.Fatalf("导出：%+v, %v", f, err)
	}
	data, err := os.ReadFile(out)
	if err != nil || !bytes.Contains(data, []byte("18446744073709551615")) {
		t.Fatalf("CSV 内容：%q, %v", data, err)
	}
}
