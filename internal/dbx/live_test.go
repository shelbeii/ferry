package dbx

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 对真实数据库跑一遍（本机起的容器）：
//
//	FERRY_DBX_LIVE="mysql=127.0.0.1:33061,maria=127.0.0.1:33062,postgres=127.0.0.1:54321" FERRY_DBX_PASSWORD=… go test ./internal/dbx -run Live
func TestLive(t *testing.T) {
	spec := os.Getenv("FERRY_DBX_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_DBX_LIVE")
	}
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		t.Run(name, func(t *testing.T) { live(t, name, addr) })
	}
}

// designOf 把读到的表结构原样变成设计（结构设计页打开时就是这样）。
func designOf(ti *TableInfo) Design {
	d := Design{Schema: ti.Schema, Name: ti.Name, Orig: ti.Name, PrimaryKey: ti.PrimaryKey, Comment: ti.Comment, Engine: ti.Engine, Collation: ti.Collation, Charset: ti.Charset}
	for _, c := range ti.Columns {
		d.Columns = append(d.Columns, ColumnDef{Name: c.Name, Orig: c.Name, Type: c.Type, Nullable: c.Nullable, Default: c.Default, Auto: c.Auto, Extra: c.Extra, Collation: c.Collation, Comment: c.Comment})
	}
	for _, ix := range ti.Indexes {
		if !ix.Primary {
			d.Indexes = append(d.Indexes, IndexDef{Name: ix.Name, Orig: ix.Name, Columns: ix.Columns, Unique: ix.Unique, Method: ix.Method, Def: ix.Def})
		}
	}
	return d
}

func live(t *testing.T, name, addr string) {
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	kind, user, dbName, schema := MySQL, "root", "ferry_test", ""
	if name == "postgres" {
		kind, user, dbName, schema = Postgres, "postgres", "postgres", "ferry_test"
	}
	c := &Conn{ID: "live-" + name, Name: name, Kind: kind, Host: host, Port: pn, User: user}
	m := &Manager{clients: map[string]*Client{}}
	cl, release, err := m.Open(ctx, c, os.Getenv("FERRY_DBX_PASSWORD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	t.Log("version", cl.Version)
	exec := func(stmts ...string) {
		t.Helper()
		rs, err := cl.Exec(ctx, dbName, stmts)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.Error != "" {
				t.Fatalf("%s: %s", r.SQL, r.Error)
			}
		}
	}
	// 清场、建库
	if kind == MySQL {
		cl.Exec(ctx, "", []string{"DROP DATABASE IF EXISTS ferry_test"})
		st, _ := OpSQL(kind, "", "", "create-db", []string{"ferry_test"})
		if _, err := cl.Exec(ctx, "", st); err != nil {
			t.Fatal(err)
		}
	} else {
		cl.Exec(ctx, dbName, []string{"DROP SCHEMA IF EXISTS ferry_test CASCADE"})
		st, _ := OpSQL(kind, "", "", "create-schema", []string{"ferry_test"})
		exec(st...)
	}
	// 建表
	textType := "varchar(64)"
	d := Design{Schema: schema, Name: "user", Columns: []ColumnDef{
		{Name: "id", Type: map[string]string{MySQL: "bigint", Postgres: "bigint"}[kind], Auto: true},
		{Name: "name", Type: textType, Default: p("''"), Comment: "名字"},
		{Name: "note", Type: "text", Nullable: true},
		{Name: "data", Type: map[string]string{MySQL: "blob", Postgres: "bytea"}[kind], Nullable: true},
		{Name: "created", Type: "timestamp", Nullable: true, Default: p("CURRENT_TIMESTAMP")},
	}, PrimaryKey: []string{"id"}, Indexes: []IndexDef{{Name: "idx_user_name", Columns: []string{"name"}}}, Comment: "用户表"}
	st, err := DesignSQL(kind, dbName, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec(st...)
	ti, err := cl.TableInfo(ctx, dbName, schema, "user")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ti.Key, ",") != "id" || len(ti.Columns) != 5 || ti.Comment != "用户表" {
		t.Fatalf("表结构不对：%+v", ti)
	}
	t.Log("DDL:\n" + ti.DDL)
	// 读到的结构原样再生成：不应该有语句
	if st, err := DesignSQL(kind, dbName, designOf(ti), ti); err != nil || len(st) != 0 {
		t.Fatalf("没改结构却生成了语句：%q %v", st, err)
	}
	// 改数据
	changes := []Change{
		{Op: "insert", Values: map[string]*string{"name": p("张三"), "note": p("it's")}},
		{Op: "insert", Values: map[string]*string{"name": p("李四"), "note": nil}},
	}
	stmts, err := ChangeSQL(kind, dbName, ti, changes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Apply(ctx, dbName, stmts); err != nil {
		t.Fatal(err)
	}
	exec("UPDATE " + TableName(kind, dbName, schema, "user") + " SET data = " + map[string]string{MySQL: "0x00FF10", Postgres: "'\\x00ff10'"}[kind] + " WHERE name = '张三'")
	r, err := cl.Rows(ctx, dbName, schema, "user", BrowseOptions{Sorts: []Sort{{Col: "id"}}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 || !r.More || r.Rows[0][1] != "张三" || r.Rows[0][2] != "it's" {
		t.Fatalf("读数据不对：%+v", r)
	}
	if b, ok := r.Rows[0][3].(Binary); !ok || b.Size != 3 || b.Hex != "00ff10" {
		t.Fatalf("二进制值不对：%#v（类型 %s）", r.Rows[0][3], r.Columns[3].Type)
	}
	id := r.Rows[0][0].(string)
	created := r.Rows[0][4]
	if created == nil {
		t.Fatal("created 没有默认值")
	}
	// 用读到的值定位、修改（值原样写回也要能匹配到）
	stmts, _ = ChangeSQL(kind, dbName, ti, []Change{{Op: "update", Key: map[string]*string{"id": &id}, Values: map[string]*string{"note": nil, "created": p(created.(string))}}})
	if _, err := cl.Apply(ctx, dbName, stmts); err != nil {
		t.Fatal(err)
	}
	// 改的行不存在 → 整个撤销
	gone := "999999"
	stmts, _ = ChangeSQL(kind, dbName, ti, []Change{{Op: "update", Key: map[string]*string{"id": &id}, Values: map[string]*string{"name": p("王五")}}, {Op: "delete", Key: map[string]*string{"id": &gone}}})
	if _, err := cl.Apply(ctx, dbName, stmts); err == nil {
		t.Fatal("删不存在的行应该报错")
	}
	if n, err := cl.Count(ctx, dbName, schema, "user", BrowseOptions{Where: "name = '王五'"}); err != nil || n != 0 {
		t.Fatalf("应该已撤销：%d %v", n, err)
	}
	// 筛选、计数
	if n, err := cl.Count(ctx, dbName, schema, "user", BrowseOptions{Where: "note IS NULL"}); err != nil || n != 2 {
		t.Fatalf("count = %d, %v", n, err)
	}
	// 筛选、排序（值作为参数，带 % 和引号也照原样匹配）
	exec("INSERT INTO " + TableName(kind, dbName, schema, "user") + " (name, note) VALUES ('50%_off''s', 'x'), ('5000', 'y')")
	for _, c := range []struct {
		f    []Filter
		want int64
	}{
		{[]Filter{{Col: "name", Op: "contains", Value: "%_off'"}}, 1},
		{[]Filter{{Col: "name", Op: "starts", Value: "50"}}, 2},
		{[]Filter{{Col: "id", Op: ">", Value: "1"}, {Col: "name", Op: "=", Value: "张三", Or: true}}, 4},
		{[]Filter{{Col: "note", Op: "null"}}, 2},
		{[]Filter{{Col: "note", Op: "not-empty"}}, 2},
		{[]Filter{{Col: "name", Op: "in", Value: "张三, 5000"}}, 2},
		{[]Filter{{Col: "id", Op: "between", Value: "2", Value2: "3"}}, 2},
		{[]Filter{{Col: "id", Op: "contains", Value: "1"}}, 1}, // 数字列也能「包含」
		{[]Filter{{Col: "name", Op: "=", Value: "张三"}, {Col: "id", Op: ">", Value: "100", Off: true}}, 1},
	} {
		n, err := cl.Count(ctx, dbName, schema, "user", BrowseOptions{Filters: c.f})
		if err != nil || n != c.want {
			t.Fatalf("筛选 %+v：%d 行（应为 %d），%v", c.f, n, c.want, err)
		}
	}
	r, err = cl.Rows(ctx, dbName, schema, "user", BrowseOptions{Filters: []Filter{{Col: "note", Op: "not-null"}}, Sorts: []Sort{{Col: "note", Desc: true}, {Col: "id"}}, Limit: 10})
	if err != nil || len(r.Rows) != 2 || r.Rows[0][2] != "y" {
		t.Fatalf("筛选加排序：%+v %v", r, err)
	}
	t.Log("界面上显示的 SQL：", r.SQL)
	exec("DELETE FROM " + TableName(kind, dbName, schema, "user") + " WHERE note IN ('x', 'y')")
	// 改结构：name 改名 nick 变长，加列，删 note，索引跟着改名
	d2 := designOf(ti)
	d2.Columns[1].Name = "nick"
	d2.Columns[1].Type = "varchar(100)"
	d2.Columns = append(d2.Columns[:2], d2.Columns[3:]...)
	d2.Columns = append(d2.Columns, ColumnDef{Name: "age", Type: "int", Nullable: false, Default: p("0")})
	d2.Indexes[0].Columns = []string{"nick"}
	d2.Indexes = append(d2.Indexes, IndexDef{Name: "uk_age_nick", Columns: []string{"age", "nick"}, Unique: true})
	d2.Name = "member"
	d2.Comment = "会员"
	st, err = DesignSQL(kind, dbName, d2, ti)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ALTER:\n" + strings.Join(st, ";\n"))
	exec(st...)
	ti2, err := cl.TableInfo(ctx, dbName, schema, "member")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range ti2.Columns {
		names = append(names, c.Name+" "+c.Type)
	}
	if got := strings.Join(names, ", "); !strings.Contains(got, "nick varchar(100)") && !strings.Contains(got, "nick character varying(100)") || strings.Contains(got, "note") || !strings.Contains(got, ", age int") {
		t.Fatalf("改后的列：%s", got)
	}
	if ti2.Comment != "会员" || len(ti2.Indexes) != 3 {
		t.Fatalf("改后的表：%+v", ti2)
	}
	if st, _ := DesignSQL(kind, dbName, designOf(ti2), ti2); len(st) != 0 {
		t.Fatalf("改后再读，没改却生成了语句：%q", st)
	}
	// 查询窗口：多条语句、会话保留、出错不影响后面
	q := "SELECT 1 AS a; SELECT * FROM " + TableName(kind, dbName, schema, "nope") + "; SELECT COUNT(*) FROM " + TableName(kind, dbName, schema, "member")
	rs, err := cl.Query(ctx, dbName, q, QueryOptions{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[0].Rows[0][0] != "1" || rs[1].Error == "" || rs[2].Rows[0][0] != "2" {
		t.Fatalf("查询结果：%+v", rs)
	}
	t.Log("错误：", rs[1].Error, rs[1].Code)
	// 只读
	ro := *c
	ro.ReadOnly = true
	ro.ID += "-ro"
	clRO, relRO, err := m.Open(ctx, &ro, os.Getenv("FERRY_DBX_PASSWORD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relRO()
	if _, err := clRO.Query(ctx, dbName, "DELETE FROM "+TableName(kind, dbName, schema, "member"), QueryOptions{}); err == nil {
		t.Fatal("只读连接不应该能删")
	}
	rs, _ = clRO.Query(ctx, dbName, "WITH x AS (SELECT 1) SELECT * FROM x", QueryOptions{})
	if rs[0].Error != "" {
		t.Fatal(rs[0].Error)
	}
	// 导出
	dir := t.TempDir()
	for _, f := range []string{"csv", "sql"} {
		out := filepath.Join(dir, "a."+f)
		er, err := cl.Export(ctx, dbName, ExportOptions{Format: f, Out: out, Schema: schema, Table: "member", Browse: BrowseOptions{Sorts: []Sort{{Col: "id"}}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(out)
		if er.Rows != 2 || !strings.Contains(string(b), "李四") {
			t.Fatalf("导出 %s：%+v\n%s", f, er, b)
		}
		if f == "sql" {
			// 导出的 INSERT 能导回去
			exec("DELETE FROM " + TableName(kind, dbName, schema, "member"))
			src := string(b)
			if kind == Postgres {
				src = strings.ReplaceAll(src, `INSERT INTO "public"."member"`, `INSERT INTO "ferry_test"."member"`)
			}
			rs, err := cl.Query(ctx, dbName, src, QueryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rs {
				if r.Error != "" {
					t.Fatalf("导回去失败：%s\n%s", r.Error, r.SQL)
				}
			}
		}
	}
	// 表列表
	ts, err := cl.Tables(ctx, dbName)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range ts {
		if x.Name == "member" && x.Schema == schema {
			found = true
		}
	}
	if !found {
		t.Fatalf("表列表里没有 member：%+v", ts)
	}
	// 导出结构和数据，删掉表后再导回去
	if kind == MySQL {
		exec("CREATE VIEW " + TableName(kind, dbName, schema, "member_v") + " AS SELECT id, nick FROM " + TableName(kind, dbName, schema, "member"))
	}
	dump := filepath.Join(dir, "dump.sql")
	dr, err := cl.Dump(ctx, dbName, DumpOptions{Out: dump, Schema: schema, Data: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dump)
	if dr.Rows != 2 || !strings.Contains(string(b), "DROP TABLE IF EXISTS") || strings.Contains(string(b), "DEFINER=") {
		t.Fatalf("导出结构和数据：%+v\n%s", dr, b)
	}
	if kind == MySQL {
		exec("DROP VIEW " + TableName(kind, dbName, schema, "member_v"))
	}
	exec("DROP TABLE " + TableName(kind, dbName, schema, "member"))
	rs, err = cl.Query(ctx, dbName, string(b), QueryOptions{Session: "imp"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Error != "" {
			t.Fatalf("导回去失败：%s\n%s", r.Error, r.SQL)
		}
	}
	if n, err := cl.Count(ctx, dbName, schema, "member", BrowseOptions{}); err != nil || n != 2 {
		t.Fatalf("导回去后 %d 行，%v", n, err)
	}
	only := filepath.Join(dir, "struct.sql")
	if _, err := cl.Dump(ctx, dbName, DumpOptions{Out: only, Schema: schema, Tables: []string{"member"}}, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(only); strings.Contains(string(b), "INSERT INTO") || !strings.Contains(string(b), "CREATE TABLE") {
		t.Fatalf("只导出结构：\n%s", b)
	}
	// 重命名
	st, _ = OpSQL(kind, dbName, schema, "rename", []string{"member", "member2"})
	exec(st...)
	if _, err := cl.TableInfo(ctx, dbName, schema, "member2"); err != nil {
		t.Fatal(err)
	}
	st, _ = OpSQL(kind, dbName, schema, "drop-table", []string{"member2"})
	exec(st...)
}
