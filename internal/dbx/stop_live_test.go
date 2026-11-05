package dbx

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 停止语句、读够行数就停、同时打开连接（本机起的容器，和 TestLive 一样的 FERRY_DBX_LIVE）。
func TestLiveStop(t *testing.T) {
	spec := os.Getenv("FERRY_DBX_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_DBX_LIVE")
	}
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		t.Run(name, func(t *testing.T) { liveStop(t, name, addr) })
	}
}

func liveStop(t *testing.T, name, addr string) {
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	kind, user, dbName := MySQL, "root", "ferry_stop"
	if name == "postgres" {
		kind, user, dbName = Postgres, "postgres", "postgres"
	}
	c := &Conn{ID: "stop-" + name, Name: name, Kind: kind, Host: host, Port: pn, User: user, AI: true}
	m := &Manager{clients: map[string]*Client{}, keep: true}
	pw := os.Getenv("FERRY_DBX_PASSWORD")

	// 同时第一次打开：后来的等前一个打开完再复用，不会把它关掉
	var wg sync.WaitGroup
	errs := make([]error, 4)
	got := make([]*Client, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cl, rel, err := m.Open(ctx, c, pw, nil)
			if err != nil {
				errs[i] = err
				return
			}
			defer rel()
			got[i] = cl
			time.Sleep(200 * time.Millisecond)
			_, errs[i] = cl.Query(ctx, "", "SELECT 1", QueryOptions{})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("同时打开第 %d 个：%v", i, err)
		}
		if got[i] != got[0] {
			t.Fatal("同时打开的应该是同一个连接")
		}
	}

	cl, release, err := m.Open(ctx, c, pw, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
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
	const n = 300000
	if cl.isMariaDB() {
		must("", "CREATE DATABASE IF NOT EXISTS ferry_stop")
		must(dbName, "DROP TABLE IF EXISTS big", "CREATE TABLE big (id INT PRIMARY KEY AUTO_INCREMENT, v INT, pad VARCHAR(100))",
			"INSERT INTO big (v, pad) SELECT seq, REPEAT('x', 80) FROM seq_1_to_"+strconv.Itoa(n))
	} else if kind == MySQL {
		must("", "CREATE DATABASE IF NOT EXISTS ferry_stop")
		must(dbName, "DROP TABLE IF EXISTS big", "CREATE TABLE big (id INT PRIMARY KEY AUTO_INCREMENT, v INT, pad VARCHAR(100))",
			"SET SESSION cte_max_recursion_depth = 1000000",
			"INSERT INTO big (v, pad) WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < "+strconv.Itoa(n)+") SELECT n, REPEAT('x', 80) FROM s")
	} else {
		must(dbName, "DROP TABLE IF EXISTS big", "CREATE TABLE big (id SERIAL PRIMARY KEY, v INT, pad VARCHAR(100))",
			"INSERT INTO big (v, pad) SELECT n, repeat('x', 80) FROM generate_series(1, "+strconv.Itoa(n)+") n")
	}
	one := func(sid, q string) Result {
		t.Helper()
		rs, err := cl.Query(ctx, dbName, q, QueryOptions{Session: sid, Max: 1000})
		if err != nil {
			t.Fatal(err)
		}
		return rs[0]
	}

	// 读够 1000 行就停：会话里设的变量还在，后面的语句照常执行
	set, get := "SET @ferry_x = 7", "SELECT @ferry_x"
	if kind == Postgres {
		set, get = "SET ferry.x = '7'", "SELECT current_setting('ferry.x')"
	}
	one("s1", set)
	st := time.Now()
	r := one("s1", "SELECT * FROM big")
	if r.Error != "" || len(r.Rows) != 1000 || !r.More {
		t.Fatalf("SELECT *：%d 行 more=%v %s", len(r.Rows), r.More, r.Error)
	}
	t.Logf("SELECT * 不带 LIMIT：%v", time.Since(st))
	if r := one("s1", get); r.Error != "" || r.Rows[0][0] != "7" {
		t.Fatalf("停下后会话没了：%v %s", r.Rows, r.Error)
	}

	// 事务里的查询停下后，事务还能接着用（PostgreSQL 用了保存点）
	one("s2", "BEGIN")
	one("s2", "UPDATE big SET v = -5 WHERE id = 1")
	if r := one("s2", "SELECT * FROM big"); r.Error != "" || len(r.Rows) != 1000 {
		t.Fatalf("事务里的 SELECT *：%d 行 %s", len(r.Rows), r.Error)
	}
	if r := one("s2", "SELECT v FROM big WHERE id = 1"); r.Error != "" || r.Rows[0][0] != "-5" {
		t.Fatalf("停下后事务坏了：%v %s", r.Rows, r.Error)
	}
	one("s2", "ROLLBACK")

	// AI 查询同样不读完
	if ar, err := cl.AIQuery(ctx, dbName, "SELECT * FROM big", 100); err != nil || len(ar.Rows) != 100 || !ar.More {
		t.Fatalf("AI 查询：%v %v", ar, err)
	}

	// 点了停止：数据库里的 UPDATE 也停下，不会执行完生效
	slow := "UPDATE big SET v = -1 WHERE SLEEP(0.0005) = 0"
	running := "SELECT COUNT(*) FROM information_schema.processlist WHERE info LIKE 'UPDATE big%'"
	if kind == Postgres {
		slow = "UPDATE big SET v = -1 WHERE pg_sleep(0.0005) IS NOT NULL"
		running = "SELECT COUNT(*) FROM pg_stat_activity WHERE query LIKE 'UPDATE big%' AND state = 'active'"
	}
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	cl.Query(cctx, dbName, slow, QueryOptions{Session: "s3"})
	cancel()
	for i := 0; i < 50; i++ {
		if one("", running).Rows[0][0] == "0" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if r := one("", running); r.Rows[0][0] != "0" {
		t.Fatal("停止后 UPDATE 还在执行")
	}
	if r := one("", "SELECT COUNT(*) FROM big WHERE v = -1"); r.Rows[0][0] != "0" {
		t.Fatalf("停止的 UPDATE 改了 %v 行", r.Rows[0][0])
	}
}
