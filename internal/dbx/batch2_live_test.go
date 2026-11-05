package dbx

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// 估算影响行数、AI 用单独的账号、加密（本机起的容器，和 TestLive 一样的 FERRY_DBX_LIVE）。
func TestLiveBatch2(t *testing.T) {
	spec := os.Getenv("FERRY_DBX_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_DBX_LIVE")
	}
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		t.Run(name, func(t *testing.T) { liveBatch2(t, name, addr) })
	}
}

func liveBatch2(t *testing.T, name, addr string) {
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	kind, user, dbName := MySQL, "root", "ferry_b2"
	if name == "postgres" {
		kind, user, dbName = Postgres, "postgres", "postgres"
	}
	pw := os.Getenv("FERRY_DBX_PASSWORD")
	c := &Conn{ID: "b2-" + name, Name: name, Kind: kind, Host: host, Port: pn, User: user, AI: true, AIWrite: true}
	m := &Manager{clients: map[string]*Client{}, keep: true}
	cl, release, err := m.Open(ctx, c, pw, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	t.Logf("自动加密：%v（%s）", cl.Encrypted, cl.Version)
	must := func(db string, s ...string) {
		t.Helper()
		rs, err := cl.Exec(ctx, db, s)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.Error != "" {
				t.Fatal(r.SQL, r.Error)
			}
		}
	}
	if kind == MySQL {
		must("", "CREATE DATABASE IF NOT EXISTS ferry_b2")
		fill := []string{"SET SESSION cte_max_recursion_depth = 100000",
			"INSERT INTO t (id, v) WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 5000) SELECT n, n FROM s"}
		if cl.isMariaDB() {
			fill = []string{"INSERT INTO t (id, v) SELECT seq, seq FROM seq_1_to_5000"}
		}
		must(dbName, append(append([]string{"DROP TABLE IF EXISTS t", "CREATE TABLE t (id INT PRIMARY KEY, v INT)"}, fill...), "ANALYZE TABLE t")...)
		must("", "DROP USER IF EXISTS 'ferry_ro'@'%'", "CREATE USER 'ferry_ro'@'%' IDENTIFIED BY 'ropass1'", "GRANT SELECT ON ferry_b2.* TO 'ferry_ro'@'%'")
	} else {
		must(dbName, "DROP TABLE IF EXISTS t", "CREATE TABLE t (id INT PRIMARY KEY, v INT)",
			"INSERT INTO t SELECT n, n FROM generate_series(1, 5000) n", "ANALYZE t",
			"DROP ROLE IF EXISTS ferry_ro", "CREATE ROLE ferry_ro LOGIN PASSWORD 'ropass1'", "GRANT SELECT ON t TO ferry_ro")
	}

	// 估算：EXPLAIN，不执行
	stmts := []string{"INSERT INTO t VALUES (99999, 1)", "UPDATE t SET v = 0 WHERE id <= 100", "/*!80018 ANALYZE */ DELETE FROM t", "UPDATE no_such_table SET a = 1"}
	est, err := cl.EstimateRows(ctx, dbName, stmts)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("估算：%s", EstimateText(stmts, est))
	if len(est) != 2 || est[0].Index != 1 || est[0].Rows < 50 || est[0].Rows > 300 || est[1].Index != 2 || est[1].Rows < 2500 || est[1].Rows > 10000 {
		t.Fatalf("估算不对：%+v", est)
	}
	if r, _ := cl.Query(ctx, dbName, "SELECT COUNT(*) FROM t WHERE v = 0", QueryOptions{}); r[0].Rows[0][0] != "0" {
		t.Fatal("估算不应该执行语句")
	}

	// AI 用只读的账号：能查，改不了（数据库拦着）
	c.AIUser = "ferry_ro"
	ai, arel, err := m.Open(ctx, c.ForAI(), "ropass1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := ai.AIQuery(ctx, dbName, "SELECT COUNT(*) FROM t", 10); err != nil || r.Rows[0][0] != "5000" {
		t.Fatalf("AI 账号查：%v %v", r, err)
	}
	x, err := ai.AIExec(ctx, dbName, []string{"UPDATE t SET v = 1 WHERE id = 1"})
	if err != nil || x.OK || !strings.Contains(strings.ToLower(x.Results[0].Error), "denied") {
		t.Fatalf("只读账号应该改不了：%+v %v", x, err)
	}
	arel()
	if len(m.Opened()) != 2 {
		t.Fatal(m.Opened())
	}
	m.Close(c.ID) // 改了连接：AI 用的也关掉
	if len(m.Opened()) != 0 {
		t.Fatal(m.Opened())
	}

	// 加密
	tryTLS := func(mode string) (*Client, error) {
		cc := *c
		cc.ID, cc.TLS, cc.AIUser = "b2-tls-"+name+mode, mode, ""
		mm := &Manager{clients: map[string]*Client{}}
		x, rel, err := mm.Open(ctx, &cc, pw, nil)
		if err == nil {
			rel()
		}
		return x, err
	}
	x2, err := tryTLS(TLSRequire)
	switch {
	case kind == Postgres && cl.Encrypted: // 开了 SSL（TestTLSLive 那样设过）
		if err != nil || !x2.Encrypted {
			t.Fatalf("加密：%v", err)
		}
	case kind == Postgres: // 容器里的 PostgreSQL 默认没开 SSL
		if err == nil || !strings.Contains(err.Error(), "没开加密") {
			t.Fatalf("PostgreSQL 没开 SSL 时要加密应该说清楚：%v", err)
		}
		if cl.Encrypted {
			t.Fatal("PostgreSQL 没开 SSL，自动时应该不加密")
		}
	default: // MySQL、MariaDB 默认带自签的证书
		if err != nil || !x2.Encrypted {
			t.Fatalf("加密：%v", err)
		}
		if !cl.Encrypted {
			t.Fatal("自动时应该加密")
		}
		if _, err := tryTLS(TLSVerify); err == nil || !strings.Contains(err.Error(), "证书") {
			t.Fatalf("自签的证书验证应该不过：%v", err)
		}
	}
	if x3, err := tryTLS(TLSOff); err != nil || x3.Encrypted {
		t.Fatalf("不加密：%v", err)
	}
}
