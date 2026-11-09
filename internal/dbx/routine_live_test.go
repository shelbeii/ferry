package dbx

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 函数、存储过程：列表、定义、保存（改坏了要恢复原来的）、删除；还有备份再还原。
//
//	FERRY_DBX_LIVE="mysql=127.0.0.1:33061,postgres=127.0.0.1:54321" FERRY_DBX_PASSWORD=… go test ./internal/dbx -run LiveRoutines
func TestLiveRoutines(t *testing.T) {
	spec := os.Getenv("FERRY_DBX_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_DBX_LIVE")
	}
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		t.Run(name, func(t *testing.T) { liveRoutines(t, name, addr) })
	}
}

func liveRoutines(t *testing.T, name, addr string) {
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	kind, user, dbName := MySQL, "root", "ferry_rt"
	if name == "postgres" {
		kind, user, dbName = Postgres, "postgres", "postgres"
	}
	m := &Manager{clients: map[string]*Client{}}
	cl, release, err := m.Open(ctx, &Conn{ID: "rt-" + name, Name: name, Kind: kind, Host: host, Port: pn, User: user}, os.Getenv("FERRY_DBX_PASSWORD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	exec := func(db string, stmts ...string) {
		t.Helper()
		rs, err := cl.Exec(ctx, db, stmts)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.Error != "" {
				t.Fatalf("%s: %s", r.SQL, r.Error)
			}
		}
	}
	schema := ""
	var fn, proc string
	if kind == MySQL {
		cl.Exec(ctx, "", []string{"DROP DATABASE IF EXISTS ferry_rt"})
		exec("", "CREATE DATABASE ferry_rt")
		exec(dbName, "CREATE TABLE t (id INT PRIMARY KEY, v VARCHAR(20))", "INSERT INTO t VALUES (1, 'a'), (2, 'b')")
		fn = "CREATE FUNCTION add1(x INT) RETURNS INT DETERMINISTIC COMMENT '加一' RETURN x + 1"
		proc = "CREATE PROCEDURE cnt(IN lim INT, OUT n INT)\nBEGIN\n  SELECT COUNT(*) INTO n FROM t WHERE id <= lim;\nEND;"
	} else {
		schema = "ferry_rt"
		cl.Exec(ctx, dbName, []string{"DROP SCHEMA IF EXISTS ferry_rt CASCADE"})
		exec(dbName, "CREATE SCHEMA ferry_rt", "CREATE TABLE ferry_rt.t (id INT PRIMARY KEY, v TEXT)", "INSERT INTO ferry_rt.t VALUES (1, 'a'), (2, 'b')")
		fn = "CREATE FUNCTION ferry_rt.add1(x integer) RETURNS integer LANGUAGE sql AS $$ SELECT x + 1 $$;"
		proc = "CREATE PROCEDURE ferry_rt.cnt(lim integer) LANGUAGE plpgsql AS $$\nBEGIN\n  PERFORM count(*) FROM ferry_rt.t WHERE id <= lim;\nEND;\n$$;"
	}
	for _, s := range []string{fn, proc} {
		if err := cl.SaveRoutine(ctx, dbName, nil, s); err != nil {
			t.Fatalf("新建：%v\n%s", err, s)
		}
	}
	list := func() map[string]Routine {
		rs, err := cl.Routines(ctx, dbName)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Routine{}
		for _, r := range rs {
			if schema == "" || r.Schema == schema {
				out[r.Name] = r
			}
		}
		return out
	}
	rs := list()
	f, p := rs["add1"], rs["cnt"]
	if f.Type != "function" || p.Type != "procedure" || !strings.Contains(strings.ToLower(f.Args), "x int") || !strings.Contains(strings.ToLower(f.Returns), "int") {
		t.Fatalf("列表：%+v", rs)
	}
	if kind == MySQL && (f.Comment != "加一" || !strings.Contains(p.Args, "OUT n int")) {
		t.Fatalf("MySQL 注释、参数：%+v %+v", f, p)
	}
	ref := func(r Routine) RoutineRef {
		return RoutineRef{Schema: r.Schema, Name: r.Name, Type: r.Type, Args: r.Args, OID: r.OID}
	}
	def, err := cl.RoutineDef(ctx, dbName, ref(p))
	if err != nil || !strings.Contains(def, "cnt") || strings.Contains(def, "DEFINER") {
		t.Fatalf("定义：%v\n%s", err, def)
	}
	// 改：MySQL 删了重建，PostgreSQL CREATE OR REPLACE
	changed := strings.Replace(def, "id <= lim", "id < lim", 1)
	if err := cl.SaveRoutine(ctx, dbName, ptr(ref(p)), changed); err != nil {
		t.Fatalf("保存改动：%v", err)
	}
	def2, _ := cl.RoutineDef(ctx, dbName, ref(list()["cnt"]))
	if !strings.Contains(def2, "id < lim") {
		t.Fatalf("没改上：%s", def2)
	}
	// 改坏了：报错，原来的还在
	bad := strings.Replace(def2, "BEGIN", "BEGIN oops;", 1)
	if err := cl.SaveRoutine(ctx, dbName, ptr(ref(list()["cnt"])), bad); err == nil {
		t.Fatal("写错了应该报错")
	} else if kind == MySQL && !strings.Contains(err.Error(), "原来的 cnt 还在") {
		t.Fatalf("报错里要说原来的还在：%v", err)
	}
	if _, ok := list()["cnt"]; !ok {
		t.Fatal("改坏了以后原来的不见了")
	}
	// 只读连接不能改
	ro := cl.Conn
	ro.ReadOnly = true
	rocl := &Client{Conn: ro}
	if err := rocl.SaveRoutine(ctx, dbName, nil, fn); err != ErrReadOnly {
		t.Fatalf("只读：%v", err)
	}

	// 备份再还原：改了数据、删了函数，还原后回来
	savedHome(t)
	bp, err := NewBackupPath("rt", dbName, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Dump(ctx, dbName, DumpOptions{Out: bp, Data: true, Schema: schema}, nil); err != nil {
		t.Fatal(err)
	}
	tbl := TableName(kind, dbName, schema, "t")
	exec(dbName, "DELETE FROM "+tbl+" WHERE id = 2")
	drop, _ := OpSQL(kind, dbName, schema, "drop-function", []string{"add1", f.Args})
	if kind == MySQL {
		drop, _ = OpSQL(kind, dbName, schema, "drop-function", []string{"add1"})
	}
	exec(dbName, drop...)
	if _, ok := list()["add1"]; ok {
		t.Fatal("没删掉")
	}
	restore := func(what string) {
		t.Helper()
		r, err := cl.ImportSQL(ctx, dbName, ImportSQLOptions{File: bp}, nil)
		if err != nil || r.Failed > 0 || r.Stopped {
			t.Fatalf("%s：%v %+v", what, err, r)
		}
	}
	restore("还原")
	if _, ok := list()["add1"]; !ok {
		t.Fatal("还原后函数没回来")
	}
	rs2, err := cl.Query(ctx, dbName, "SELECT COUNT(*) FROM "+tbl, QueryOptions{})
	if err != nil || rs2[0].Error != "" || len(rs2[0].Rows) != 1 || fmt.Sprint(rs2[0].Rows[0][0]) != "2" {
		t.Fatalf("还原后的行数：%+v %v", rs2, err)
	}
	// 再还原一次（库里已经有这些对象）也要能成功
	restore("第二次还原")

	if kind == MySQL {
		cl.Exec(ctx, "", []string{"DROP DATABASE IF EXISTS ferry_rt"})
	} else {
		cl.Exec(ctx, dbName, []string{"DROP SCHEMA IF EXISTS ferry_rt CASCADE"})
	}
}

func ptr[T any](v T) *T { return &v }
