package dbx

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// AI 查询对真实数据库跑一遍（和 TestLive 用同样的环境变量）。
func TestLiveAI(t *testing.T) {
	spec := os.Getenv("FERRY_DBX_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_DBX_LIVE")
	}
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		t.Run(name, func(t *testing.T) { liveAI(t, name, addr) })
	}
}

func liveAI(t *testing.T, name, addr string) {
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	kind, user, dbName, table := MySQL, "root", "ferry_ai", "nums"
	if name == "postgres" {
		kind, user, dbName, table = Postgres, "postgres", "postgres", "ferry_ai_nums"
	}
	c := &Conn{ID: "ai-" + name, Name: name, Kind: kind, Host: host, Port: pn, User: user, AI: true}
	m := &Manager{clients: map[string]*Client{}}
	cl, release, err := m.Open(ctx, c, os.Getenv("FERRY_DBX_PASSWORD"), nil)
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
				t.Fatalf("%s：%s", r.SQL, r.Error)
			}
		}
	}
	if kind == MySQL {
		exec("", "DROP DATABASE IF EXISTS ferry_ai", "CREATE DATABASE ferry_ai")
		exec(dbName, "CREATE TABLE nums (id INT PRIMARY KEY, note TEXT)",
			"INSERT INTO nums WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM s WHERE n < 700) SELECT n, REPEAT('x', 3000) FROM s")
	} else {
		exec(dbName, "DROP TABLE IF EXISTS ferry_ai_nums", "CREATE TABLE ferry_ai_nums (id INT PRIMARY KEY, note TEXT)",
			"INSERT INTO ferry_ai_nums SELECT n, repeat('x', 3000) FROM generate_series(1, 700) n")
	}

	// 默认 100 行，长文本截断，结果太长时少给几行
	r, err := cl.AIQuery(ctx, dbName, "SELECT id FROM "+table+" ORDER BY id", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != AIDefaultRows || !r.More || r.Rows[99][0] != "100" {
		t.Fatalf("默认行数：%d %v %v", len(r.Rows), r.More, r.Rows[len(r.Rows)-1])
	}
	r, err = cl.AIQuery(ctx, dbName, "SELECT id FROM "+table, 9999)
	if err != nil || len(r.Rows) != AIMaxRows || !r.More {
		t.Fatalf("最多行数：%v %d", err, len(r.Rows))
	}
	r, err = cl.AIQuery(ctx, dbName, "SELECT id, note FROM "+table+" ORDER BY id", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) >= 200 || !r.More || !strings.Contains(r.Note, "太长") || !strings.Contains(r.Rows[0][1].(string), "已截断") {
		t.Fatalf("长结果：%d 行 %q", len(r.Rows), r.Note)
	}

	// 只读事务：WITH … DELETE 过了开头的检查，数据库拒绝
	del := "WITH x AS (SELECT 1 AS a) DELETE FROM " + table + " WHERE id = 1"
	if kind == Postgres {
		del = "WITH d AS (DELETE FROM " + table + " WHERE id = 1 RETURNING id) SELECT * FROM d"
	}
	if _, err := cl.AIQuery(ctx, dbName, del, 0); err == nil {
		t.Fatal("只读事务里应该删不了")
	} else {
		t.Log("删除被拒绝：", err)
	}
	r, err = cl.AIQuery(ctx, dbName, "SELECT count(*) FROM "+table, 0)
	if err != nil || r.Rows[0][0] != "700" {
		t.Fatalf("数据被改了：%v %v", err, r)
	}

	// 超时：数据库那边停掉
	old := AITimeout
	AITimeout = 2 * time.Second
	defer func() { AITimeout = old }()
	start := time.Now()
	_, err = cl.AIQuery(ctx, dbName, "SELECT count(*) FROM "+table+" a, "+table+" b, "+table+" c WHERE a.note <> b.note OR b.id + c.id > 0", 0)
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("应该超时：%v", err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("超时用了 %v", d)
	}
	t.Log("超时：", time.Since(start).Round(time.Millisecond), err)

	// 超时设置用完改回去：同一个连接池里普通查询不受影响
	if kind == MySQL {
		v := "@@SESSION.max_execution_time"
		if strings.Contains(strings.ToLower(cl.Version), "mariadb") {
			v = "@@SESSION.max_statement_time"
		}
		db, _ := cl.DB(ctx, dbName)
		db.SetMaxOpenConns(1)
		var got string
		if err := db.QueryRowContext(ctx, "SELECT "+v).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != "0" && got != "0.000000" {
			t.Fatalf("%s 没改回去：%s", v, got)
		}
	}
}
