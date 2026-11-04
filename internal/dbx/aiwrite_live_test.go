package dbx

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// AI 改数据对真实数据库跑一遍（和 TestLive 用同样的环境变量）。
func TestLiveAIWrite(t *testing.T) {
	spec := os.Getenv("FERRY_DBX_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_DBX_LIVE")
	}
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		t.Run(name, func(t *testing.T) { liveAIWrite(t, name, addr) })
	}
}

func liveAIWrite(t *testing.T, name, addr string) {
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	kind, user, dbName, table := MySQL, "root", "ferry_aiw", "acct"
	if name == "postgres" {
		kind, user, dbName, table = Postgres, "postgres", "postgres", "ferry_aiw_acct"
	}
	c := &Conn{ID: "aiw-" + name, Name: name, Kind: kind, Host: host, Port: pn, User: user, AI: true, AIWrite: true}
	m := &Manager{clients: map[string]*Client{}}
	cl, release, err := m.Open(ctx, c, os.Getenv("FERRY_DBX_PASSWORD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	setup := []string{"DROP TABLE IF EXISTS " + table, "CREATE TABLE " + table + " (id INT PRIMARY KEY, balance INT NOT NULL)",
		"INSERT INTO " + table + " VALUES (1, 100), (2, 100)"}
	if kind == MySQL {
		if rs, err := cl.Exec(ctx, "", []string{"CREATE DATABASE IF NOT EXISTS ferry_aiw"}); err != nil || rs[0].Error != "" {
			t.Fatal(err, rs)
		}
	}
	if rs, err := cl.Exec(ctx, dbName, setup); err != nil || rs[len(rs)-1].Error != "" {
		t.Fatal(err, rs)
	}
	count := func(q string) string {
		t.Helper()
		r, err := cl.AIQuery(ctx, dbName, q, 0)
		if err != nil {
			t.Fatal(err)
		}
		return r.Rows[0][0].(string)
	}
	run := func(src string) *AIExecResult {
		t.Helper()
		stmts, err := CheckAIWrite(kind, dbName, src)
		if err != nil {
			t.Fatal(err)
		}
		r, err := cl.AIExec(ctx, dbName, stmts)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// 成功：提交，每条的影响行数
	r := run("UPDATE " + table + " SET balance = balance - 30 WHERE id = 1; UPDATE " + table + " SET balance = balance + 30 WHERE id = 2; SELECT sum(balance) FROM " + table)
	if !r.OK || !r.Committed || *r.Results[0].Affected != 1 || r.Results[2].Rows[0][0] != "200" {
		t.Fatalf("成功：%+v", r)
	}
	if got := count("SELECT balance FROM " + table + " WHERE id = 1"); got != "70" {
		t.Fatalf("没提交：%s", got)
	}

	// 中间一条失败：全部撤回
	r = run("UPDATE " + table + " SET balance = 0 WHERE id = 1; INSERT INTO " + table + " VALUES (2, 1)")
	if r.OK || r.Committed || len(r.Results) != 2 || r.Results[1].Error == "" || !strings.Contains(r.Note, "撤回") {
		t.Fatalf("失败：%+v", r)
	}
	if got := count("SELECT balance FROM " + table + " WHERE id = 1"); got != "70" {
		t.Fatalf("没撤回：%s", got)
	}

	// 建表改表：PostgreSQL 能撤回，MySQL 撤不回
	r = run("ALTER TABLE " + table + " ADD COLUMN note TEXT; INSERT INTO " + table + " (id, balance) VALUES (1, 1)")
	if r.OK {
		t.Fatal("应该失败")
	}
	cols := count("SELECT count(*) FROM information_schema.columns WHERE table_name = '" + table + "' AND column_name = 'note'")
	if kind == Postgres && (cols != "0" || strings.Contains(r.Note, "撤不回")) || kind == MySQL && (cols != "1" || !strings.Contains(r.Note, "撤不回")) {
		t.Fatalf("建表改表的撤回：列 %s，%s", cols, r.Note)
	}

	// 等锁：别的连接锁着这一行，等 aiLockWait 后放弃
	old := aiLockWait
	aiLockWait = 2 * time.Second
	defer func() { aiLockWait = old }()
	db, _ := cl.DB(ctx, dbName)
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET balance = balance WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r = run("UPDATE " + table + " SET balance = 1 WHERE id = 1")
	tx.Rollback()
	holder.Close()
	if r.OK || !strings.Contains(r.Results[0].Error, "锁不到") || time.Since(start) > 8*time.Second {
		t.Fatalf("等锁：%v %+v", time.Since(start), r.Results)
	}
	t.Log("等锁：", time.Since(start).Round(time.Millisecond), r.Results[0].Error)

	// 等锁的设置用完改回去
	if kind == MySQL {
		db.SetMaxOpenConns(1)
		var v string
		if err := db.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&v); err != nil || v != "50" {
			t.Fatalf("innodb_lock_wait_timeout 没改回去：%v %s", err, v)
		}
	}

	// 连接没允许修改、只读：不执行
	cl.Conn.AIWrite = false
	if _, err := cl.AIExec(ctx, dbName, []string{"DELETE FROM " + table}); !errors.Is(err, ErrAIWriteNotAllowed) {
		t.Fatalf("没允许：%v", err)
	}
	cl.Conn.AIWrite, cl.Conn.ReadOnly = true, true
	if _, err := cl.AIExec(ctx, dbName, []string{"DELETE FROM " + table}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("只读：%v", err)
	}
	cl.Conn.ReadOnly = false
	if got := count("SELECT count(*) FROM " + table); got != "2" {
		t.Fatalf("数据被改了：%s", got)
	}
}
