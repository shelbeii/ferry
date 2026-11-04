package dbx

import (
	"strings"
	"testing"
)

func TestCheckAIQuery(t *testing.T) {
	ok := []struct{ kind, db, sql string }{
		{MySQL, "shop", "SELECT * FROM orders WHERE id = 1"},
		{MySQL, "shop", "  select id, name from users limit 10;  "},
		{MySQL, "shop", "WITH t AS (SELECT 1 AS x) SELECT * FROM t"},
		{MySQL, "shop", "SHOW CREATE TABLE orders"},
		{MySQL, "shop", "EXPLAIN SELECT * FROM orders"},
		{MySQL, "shop", "DESC orders"},
		{MySQL, "information_schema", "SELECT * FROM PROCESSLIST"},
		{MySQL, "shop", "SELECT 'a -- b', `name` FROM users -- 注释"},
		{Postgres, "app", "SELECT * FROM public.users ORDER BY id"},
		{Postgres, "app", "EXPLAIN ANALYZE SELECT count(*) FROM users"},
		{Postgres, "app", "SELECT format('%s', name) FROM users"},
		{Postgres, "app", "TABLE users"},
	}
	for _, c := range ok {
		if err := CheckAIQuery(c.kind, c.db, c.sql); err != nil {
			t.Errorf("%s 应该可以：%v", c.sql, err)
		}
	}
	bad := []struct{ kind, db, sql, want string }{
		{MySQL, "shop", "", "没有"},
		{MySQL, "shop", "SELECT 1; SELECT 2", "一条"},
		{MySQL, "shop", "DELETE FROM orders", "只能执行查询"},
		{MySQL, "shop", "UPDATE orders SET x = 1", "只能执行查询"},
		{MySQL, "shop", "CALL p()", "只能执行查询"},
		{MySQL, "shop", "USE mysql", "只能执行查询"},
		{MySQL, "shop", "OPTIMIZE TABLE orders", "只能执行查询"},
		{MySQL, "shop", "SELECT * FROM orders INTO OUTFILE '/tmp/x'", "INTO"},
		{MySQL, "shop", "SELECT id INTO @x FROM orders", "INTO"},
		{MySQL, "shop", "SELECT 1 /*!50000 INTO OUTFILE '/tmp/x' */", "/*!"},
		{MySQL, "shop", "SELECT * FROM orders FOR UPDATE", "加锁"},
		{MySQL, "shop", "SELECT * FROM orders LOCK IN SHARE MODE", "加锁"},
		{MySQL, "shop", "SELECT SLEEP(100)", "函数"},
		{MySQL, "shop", "SELECT `sleep`(100)", "函数"},
		{MySQL, "shop", "SELECT SLEEP/**/(100)", "函数"},
		{MySQL, "shop", "SELECT BENCHMARK(100000000, MD5('a'))", "函数"},
		{MySQL, "shop", "SELECT LOAD_FILE('/etc/passwd')", "函数"},
		{MySQL, "shop", "SELECT GET_LOCK('x', 10)", "函数"},
		{MySQL, "shop", "SELECT user, authentication_string FROM mysql.user", "账号密码"},
		{MySQL, "shop", "SELECT * FROM `mysql`.`user`", "账号密码"},
		{MySQL, "shop", "SHOW CREATE USER root", "账号密码"},
		{MySQL, "mysql", "SELECT * FROM user", "mysql 系统库"},
		{Postgres, "app", "SELECT pg_sleep(100)", "函数"},
		{Postgres, "app", `SELECT "pg_sleep"(100)`, "函数"},
		{Postgres, "app", "SELECT pg_catalog.pg_sleep_for('1 hour')", "函数"},
		{Postgres, "app", "SELECT pg_terminate_backend(123)", "函数"},
		{Postgres, "app", "SELECT pg_cancel_backend(pid) FROM pg_stat_activity", "函数"},
		{Postgres, "app", "SELECT pg_read_file('/etc/passwd')", "函数"},
		{Postgres, "app", "SELECT lo_export(1, '/tmp/x')", "函数"},
		{Postgres, "app", "SELECT * FROM dblink('host=x', 'select 1') AS t(a int)", "函数"},
		{Postgres, "app", "SELECT query_to_xml('select pg_terminate_backend(1)', true, true, '')", "函数"},
		{Postgres, "app", "SELECT set_config('statement_timeout', '0', false)", "函数"},
		{Postgres, "app", "SELECT pg_advisory_lock(1)", "函数"},
		{Postgres, "app", "SELECT nextval('users_id_seq')", "函数"},
		{Postgres, "app", "SELECT * FROM users FOR NO KEY UPDATE", "加锁"},
		{Postgres, "app", "SELECT * INTO newtable FROM users", "INTO"},
		{Postgres, "app", "SELECT rolname, rolpassword FROM pg_authid", "账号密码"},
		{Postgres, "app", `SELECT U&"pg\005fsleep"(1)`, "U&"},
		{Postgres, "app", "COPY users TO '/tmp/x'", "只能执行查询"},
		{Postgres, "app", "DO $$ BEGIN PERFORM pg_sleep(1); END $$", "只能执行查询"},
	}
	for _, c := range bad {
		err := CheckAIQuery(c.kind, c.db, c.sql)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 应该拒绝（%s）：%v", c.sql, c.want, err)
		}
	}
}

func TestAICell(t *testing.T) {
	if aiCell(nil) != nil {
		t.Fatal("null")
	}
	if got := aiCell(Binary{Size: 10}); got != "（二进制 10 字节）" {
		t.Fatal(got)
	}
	long := strings.Repeat("中", aiMaxCell+5)
	got := aiCell(long).(string)
	if !strings.HasPrefix(got, strings.Repeat("中", aiMaxCell)+"…") || !strings.Contains(got, "已截断") {
		t.Fatal(got[:20])
	}
	if aiCell("abc") != "abc" {
		t.Fatal("short")
	}
}
