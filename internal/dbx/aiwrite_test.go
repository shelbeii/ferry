package dbx

import (
	"strings"
	"testing"
)

func TestCheckAIWrite(t *testing.T) {
	ok := []struct{ kind, sql string }{
		{MySQL, "INSERT INTO users (name) VALUES ('张三')"},
		{MySQL, "UPDATE users SET name = 'x' WHERE id = 1; DELETE FROM users WHERE id = 2"},
		{MySQL, "CREATE TABLE t (id INT PRIMARY KEY, note TEXT) ENGINE=InnoDB"},
		{MySQL, "ALTER TABLE t ADD COLUMN age INT, ADD INDEX idx_age (age)"},
		{MySQL, "CREATE SPATIAL INDEX sp ON geo (g)"},
		{MySQL, "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (uid) REFERENCES users (id)"},
		{MySQL, "DROP TABLE IF EXISTS t; TRUNCATE TABLE logs; RENAME TABLE a TO b"},
		{MySQL, "REPLACE INTO kv VALUES ('a', 1)"},
		{MySQL, "INSERT INTO a SELECT * FROM b WHERE id > 10"},
		{MySQL, "SELECT * FROM users WHERE id = 1 FOR UPDATE"},
		{MySQL, "CREATE OR REPLACE VIEW v AS SELECT 1"},
		{Postgres, "INSERT INTO users (name) VALUES ('a') RETURNING id"},
		{Postgres, "CREATE TABLE public.t (id serial PRIMARY KEY)"},
		{Postgres, "INSERT INTO t (id) VALUES (nextval('t_id_seq'))"},
		{Postgres, "COMMENT ON TABLE t IS '订单'"},
		{Postgres, "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$"},
		{Postgres, "WITH d AS (DELETE FROM t WHERE id = 1 RETURNING *) SELECT count(*) FROM d"},
		{Postgres, "CREATE INDEX idx ON t (id)"},
	}
	for _, c := range ok {
		if _, err := CheckAIWrite(c.kind, "app", c.sql); err != nil {
			t.Errorf("%s 应该可以：%v", c.sql, err)
		}
	}
	bad := []struct{ kind, db, sql, want string }{
		{MySQL, "app", "", "没有"},
		{MySQL, "app", "GRANT ALL ON *.* TO 'x'@'%'", "不能执行 GRANT"},
		{MySQL, "app", "SET GLOBAL max_connections = 1", "不能执行 SET"},
		{MySQL, "app", "BEGIN; DELETE FROM t WHERE id = 1; COMMIT", "不能执行 BEGIN"},
		{MySQL, "app", "CALL p()", "不能执行 CALL"},
		{MySQL, "app", "LOAD DATA INFILE '/etc/passwd' INTO TABLE t", "不能执行 LOAD"},
		{MySQL, "app", "USE mysql", "不能执行 USE"},
		{MySQL, "app", "CREATE USER 'x'@'%' IDENTIFIED BY 'p'", "账号"},
		{MySQL, "app", "DROP USER root", "账号"},
		{MySQL, "app", "ALTER USER root IDENTIFIED BY 'x'", "账号"},
		{MySQL, "app", "CREATE EVENT e ON SCHEDULE EVERY 1 DAY DO DELETE FROM t", "账号"},
		{MySQL, "app", "CREATE DEFINER=`root`@`%` EVENT e ON SCHEDULE EVERY 1 DAY DO SELECT 1", "账号"},
		{MySQL, "app", "CREATE DATABASE x", "建库"},
		{MySQL, "app", "DROP SCHEMA app", "建库"},
		{MySQL, "app", "SELECT * FROM t INTO OUTFILE '/tmp/x'", "文件"},
		{MySQL, "app", "CREATE TABLE t (id INT) DATA DIRECTORY = '/tmp'", "文件"},
		{MySQL, "app", "CREATE FUNCTION sys_exec RETURNS INT SONAME 'lib.so'", "系统命令"},
		{MySQL, "app", "UPDATE t SET a = SLEEP(100)", "函数"},
		{MySQL, "app", "INSERT INTO t SELECT LOAD_FILE('/etc/passwd')", "函数"},
		{MySQL, "app", "UPDATE mysql.user SET authentication_string = '' WHERE user = 'root'", "系统表"},
		{MySQL, "mysql", "DELETE FROM user WHERE id = 1", "mysql 系统库"},
		{MySQL, "app", "INSERT INTO t VALUES (1) /*!, (SLEEP(9)) */", "/*!"},
		{MySQL, "app", strings.Repeat("INSERT INTO t VALUES (1);", AIMaxStmts+1), "最多"},
		{Postgres, "app", "ALTER SYSTEM SET work_mem = '1GB'", "账号"},
		{Postgres, "app", "CREATE EXTENSION dblink", "账号"},
		{Postgres, "app", "CREATE ROLE x LOGIN", "账号"},
		{Postgres, "app", "ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO x", "账号"},
		{Postgres, "app", "CREATE INDEX CONCURRENTLY i ON t (id)", "CONCURRENTLY"},
		{Postgres, "app", "COPY t FROM PROGRAM 'id'", "不能执行 COPY"},
		{Postgres, "app", "DO $$ BEGIN PERFORM 1; END $$", "不能执行 DO"},
		{Postgres, "app", "CREATE FUNCTION f() RETURNS int LANGUAGE plpython3u AS $$ import os $$", "系统命令"},
		{Postgres, "app", "CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_terminate_backend(1); END $$", "函数"},
		{Postgres, "app", "UPDATE pg_catalog.pg_class SET relname = 'x'", "系统表"},
		{Postgres, "app", "DELETE FROM t WHERE id = 1; SELECT pg_sleep(100)", "第 2 条"},
	}
	for _, c := range bad {
		_, err := CheckAIWrite(c.kind, c.db, c.sql)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 应该拒绝（%s）：%v", c.sql, c.want, err)
		}
	}
}

func TestAIWriteDanger(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"UPDATE t SET a = 1 WHERE id = 2", ""},
		{"UPDATE t SET note = 'no where here'", "UPDATE 没写 WHERE"},
		{"DELETE FROM t", "DELETE 没写 WHERE"},
		{"DELETE FROM t WHERE id = 1", ""},
		{"DROP TABLE t", "删掉表"},
		{"TRUNCATE t", "清空表"},
		{"DROP VIEW v", "视图"},
		{"ALTER TABLE t DROP COLUMN a", "删掉列"},
		{"INSERT INTO t VALUES (1)", ""},
	}
	for _, c := range cases {
		got := AIWriteDanger(MySQL, Split(MySQL, c.sql))
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s：%q（要 %q）", c.sql, got, c.want)
		}
	}
	if n := AIWriteNotes(MySQL, []string{"ALTER TABLE t ADD c INT"}); !strings.Contains(n[0], "撤不回") {
		t.Fatal(n)
	}
	if n := AIWriteNotes(MySQL, []string{"UPDATE t SET a = 1 WHERE id = 1"}); strings.Contains(n[0], "撤不回") {
		t.Fatal(n)
	}
	if n := AIWriteNotes(Postgres, []string{"ALTER TABLE t ADD c INT"}); !strings.Contains(n[0], "也能撤回") {
		t.Fatal(n)
	}
}

// 连接上：没允许查询、只读时不能允许修改
func TestConnAIWriteNormalized(t *testing.T) {
	c := Conn{Name: "x", Kind: MySQL, Host: "h", Port: 3306, User: "u", AIWrite: true}
	if c.Check(); c.AIWrite {
		t.Fatal("没允许 AI 查询时不能允许修改")
	}
	c = Conn{Name: "x", Kind: MySQL, Host: "h", Port: 3306, User: "u", AI: true, AIWrite: true, ReadOnly: true}
	if c.Check(); c.AIWrite {
		t.Fatal("只读时不能允许修改")
	}
	c = Conn{Name: "x", Kind: MySQL, Host: "h", Port: 3306, User: "u", AI: true, AIWrite: true}
	if c.Check(); !c.AIWrite {
		t.Fatal("应该保留")
	}
}
