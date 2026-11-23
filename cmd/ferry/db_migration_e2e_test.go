package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/dbx"
	"github.com/go-sql-driver/mysql"
)

func TestMigrationE2ESidecar(t *testing.T) {
	if os.Getenv("FERRY_MIGRATION_SIDECAR") == "1" {
		os.Exit(serve(os.Stdin, os.Stdout))
	}
}

// Set FERRY_MIGRATION_E2E to postgres=host:port,mysql=host:port[,mariadb=host:port].
// Servers must be test instances. The suite creates and removes its own databases.
// FERRY_MIGRATION_MYSQL_FALLBACK can name a MySQL target with local_infile=OFF.
func TestDatabaseMigrationE2E(t *testing.T) {
	spec := os.Getenv("FERRY_MIGRATION_E2E")
	if spec == "" {
		t.Skip("没有设置 FERRY_MIGRATION_E2E")
	}
	for _, entry := range strings.Split(spec, ",") {
		kind, source, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("非法测试服务 %q", entry)
		}
		t.Run(kind, func(t *testing.T) { testDatabaseMigration(t, kind, source) })
	}
}

type migrationWireResult struct {
	Kind        string          `json:"kind"`
	Msg         string          `json:"msg"`
	OK          bool            `json:"ok"`
	Data        json.RawMessage `json:"data"`
	Error       string          `json:"error"`
	ErrorDetail json.RawMessage `json:"errorDetail"`
	Logs        []string        `json:"-"`
}

type migrationSidecar struct {
	t    *testing.T
	in   *json.Encoder
	out  *json.Decoder
	seq  int
	pass string
}

func startMigrationSidecar(t *testing.T, ctx context.Context, password string) *migrationSidecar {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrationE2ESidecar$")
	cmd.Env = append(os.Environ(), "FERRY_MIGRATION_SIDECAR=1", "FERRY_HOME="+t.TempDir())
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		if err := cmd.Wait(); err != nil {
			t.Logf("sidecar: %v %s", err, stderr.String())
		}
	})
	s := &migrationSidecar{t: t, in: json.NewEncoder(in), out: json.NewDecoder(out), pass: password}
	var ready struct{ Ready bool }
	if err := s.out.Decode(&ready); err != nil || !ready.Ready {
		t.Fatalf("sidecar 没有就绪：%+v %v", ready, err)
	}
	return s
}

func (s *migrationSidecar) do(args []string, env map[string]string, cancelAt string) migrationWireResult {
	s.t.Helper()
	s.seq++
	id := strconv.Itoa(s.seq)
	if err := s.in.Encode(map[string]any{"id": id, "op": "run", "args": append([]string{"--json"}, args...), "env": env}); err != nil {
		s.t.Fatal(err)
	}
	var result migrationWireResult
	var lines []string
	for {
		var wire struct {
			ID   string
			Outs []string
			Err  string
			Exit *int
		}
		if err := s.out.Decode(&wire); err != nil {
			s.t.Fatalf("%v：%v %v", args, err, lines)
		}
		if wire.ID != id {
			s.t.Fatalf("请求 ID = %s，需要 %s", wire.ID, id)
		}
		if wire.Err != "" {
			lines = append(lines, wire.Err)
		}
		for _, raw := range wire.Outs {
			lines = append(lines, raw)
			var message migrationWireResult
			if err := json.Unmarshal([]byte(raw), &message); err != nil {
				s.t.Fatal(err)
			}
			if message.Kind == "result" {
				result = message
			}
			cancelOnProgress := cancelAt == "progress" && message.Kind == "progress"
			cancelOnStep := cancelAt != "" && message.Kind == "step" && message.Msg == cancelAt
			if cancelOnProgress || cancelOnStep {
				cancelAt = ""
				if err := s.in.Encode(map[string]any{"id": id, "op": "cancel"}); err != nil {
					s.t.Fatal(err)
				}
			}
		}
		if wire.Exit != nil {
			if result.Kind == "" || result.OK != (*wire.Exit == 0) || cancelAt != "" {
				s.t.Fatalf("%v：exit %d，cancelAt %q，输出 %v", args, *wire.Exit, cancelAt, lines)
			}
			result.Logs = lines
			return result
		}
	}
}

func (s *migrationSidecar) save(kind, addr, name, user string, readOnly bool) dbx.Conn {
	s.t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		s.t.Fatal(err)
	}
	p, _ := strconv.Atoi(port)
	c := dbx.Conn{Kind: kind, Name: name, Host: host, Port: p, User: user, ReadOnly: readOnly}
	r := s.do([]string{"db", "save"}, map[string]string{"FERRY_DB_CONN": migrationJSON(c)}, "")
	if !r.OK || json.Unmarshal(r.Data, &c) != nil || c.ID == "" {
		s.t.Fatalf("保存连接：%+v", r)
	}
	return c
}

func (s *migrationSidecar) migrate(sub string, source, dest dbx.Conn, db string, options dbx.MigrationOptions, cancelAt string) migrationWireResult {
	s.t.Helper()
	return s.do([]string{"db", sub, source.ID, db}, map[string]string{
		dbx.PasswordEnv(source.ID): s.pass,
		dbx.PasswordEnv(dest.ID):   s.pass,
		"FERRY_DB_REQ": migrationJSON(map[string]any{"migration": struct {
			TargetConn string `json:"targetConn"`
			dbx.MigrationOptions
		}{dest.ID, options}}),
	}, cancelAt)
}

func migrationJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func testDatabaseMigration(t *testing.T, name, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	password := os.Getenv("FERRY_MIGRATION_PASSWORD")
	kind, user, defaultDB := dbx.MySQL, "root", ""
	if name == "postgres" {
		kind, user, defaultDB = dbx.Postgres, "postgres", "postgres"
	}
	admin := migrationTestDB(t, ctx, kind, addr, user, password, defaultDB)
	defer admin.Close()
	otherAddr := os.Getenv("FERRY_MIGRATION_TARGET_" + strings.ToUpper(name))
	if otherAddr == "" {
		otherAddr = addr
	}
	otherAdmin := migrationTestDB(t, ctx, kind, otherAddr, user, password, defaultDB)
	defer otherAdmin.Close()
	s := startMigrationSidecar(t, ctx, password)
	source := s.save(kind, addr, "源连接", user, true)
	dest := s.save(kind, otherAddr, "目标连接", user, false)
	readOnly := s.save(kind, otherAddr, "只读目标", user, true)
	stamp := "fm_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	mkDB := func(suffix, endpoint string, root *sql.DB) (string, *sql.DB) {
		db := stamp + "_" + suffix
		migrationExec(t, root, "CREATE DATABASE "+db)
		conn := migrationTestDB(t, ctx, kind, endpoint, user, password, db)
		// The sidecar also keeps connections. Close them before PostgreSQL drops the database.
		t.Cleanup(func() {
			_ = conn.Close()
			cleanup := migrationTestDB(t, context.Background(), kind, endpoint, user, password, defaultDB)
			defer cleanup.Close()
			q := "DROP DATABASE IF EXISTS " + db
			if kind == dbx.Postgres {
				q += " WITH (FORCE)"
			}
			if _, err := cleanup.Exec(q); err != nil {
				t.Errorf("清理 %s：%v", db, err)
			}
		})
		return db, conn
	}
	srcDB, src := mkDB("src", addr, admin)
	wholeDB, whole := mkDB("whole", otherAddr, otherAdmin)
	partDB, part := mkDB("part", otherAddr, otherAdmin)
	dataDB, data := mkDB("data", otherAddr, otherAdmin)
	failedDB, failed := mkDB("fail", otherAddr, otherAdmin)
	lockedDB, locked := mkDB("lock", otherAddr, otherAdmin)
	seedMigrationSource(t, kind, srcDB, src)

	wantOK := func(r migrationWireResult) {
		t.Helper()
		if !r.OK {
			t.Fatalf("迁移失败：%s %s", r.Error, r.ErrorDetail)
		}
	}
	wantError := func(r migrationWireResult, want string) {
		t.Helper()
		if r.OK || !strings.Contains(r.Error, want) {
			t.Fatalf("需要错误 %q，得到 %+v", want, r)
		}
	}
	run := func(sub string, o dbx.MigrationOptions) migrationWireResult {
		return s.migrate(sub, source, dest, srcDB, o, "")
	}
	const planSub, runSub = "migrate-plan", "migrate-run"
	full := dbx.MigrationOptions{TargetDB: wholeDB, Whole: true}
	preview := run(planSub, full)
	wantOK(preview)
	var plan dbx.MigrationPlan
	wantMethod := "LOAD DATA LOCAL INFILE"
	if kind == dbx.Postgres {
		wantMethod = "COPY"
	}
	if err := json.Unmarshal(preview.Data, &plan); err != nil || len(plan.Tables) != 5 || plan.Method != wantMethod {
		t.Fatalf("全库预检：%s %v", preview.Data, err)
	}
	wholeRun := run(runSub, full)
	wantOK(wholeRun)
	var result dbx.MigrationResult
	if err := json.Unmarshal(wholeRun.Data, &result); err != nil || len(result.Completed) != 5 || result.Rows != 1006 {
		t.Fatalf("全库结果：%s %v", wholeRun.Data, err)
	}
	checkMigrationFixture(t, kind, whole)
	wantError(run(runSub, full), "目标是空库")
	wantError(s.migrate(planSub, source, readOnly, srcDB, full, ""), "只读")
	wantError(s.migrate(planSub, source, source, srcDB, dbx.MigrationOptions{TargetDB: srcDB, Whole: true}, ""), "相同")

	parent, child, simple := dbx.MigrationTable{Name: "parent"}, dbx.MigrationTable{Name: "a_child"}, dbx.MigrationTable{Name: "simple"}
	selected := dbx.MigrationOptions{TargetDB: partDB, Tables: []dbx.MigrationTable{child}}
	wantError(run(planSub, selected), "补选依赖表")
	selected.Tables = append(selected.Tables, parent)
	wantOK(run(runSub, selected))
	checkMigrationValue(t, part, "SELECT count(*) FROM a_child", "2")
	checkMigrationValue(t, part, "SELECT count(*) FROM parent", "2")
	if _, err := part.Exec("SELECT * FROM simple"); err == nil {
		t.Fatal("迁移了未选中的表")
	}
	if _, err := part.Exec("INSERT INTO a_child(id,parent_id,note) VALUES (99,999,'invalid')"); err == nil {
		t.Fatal("指定表外键没有恢复")
	}

	dataOnly := dbx.MigrationOptions{TargetDB: dataDB, DataOnly: true, Tables: []dbx.MigrationTable{simple}}
	wantError(run(planSub, dataOnly), "预先建表")
	migrationExec(t, data, "CREATE TABLE simple (id int PRIMARY KEY, n bigint NOT NULL)")
	wantError(run(planSub, dataOnly), "不兼容")
	migrationExec(t, data, "DROP TABLE simple", "CREATE TABLE simple (id int PRIMARY KEY, n int NOT NULL)")
	wantOK(run(planSub, dataOnly))
	migrationExec(t, data, "INSERT INTO simple VALUES (9, 99)")
	wantError(run(runSub, dataOnly), "已有数据")
	checkMigrationValue(t, data, "SELECT n FROM simple WHERE id=9", "99")
	migrationExec(t, data, "DELETE FROM simple")
	wantOK(run(runSub, dataOnly))
	checkMigrationValue(t, data, "SELECT sum(n) FROM simple", "17")

	migrationExec(t, failed, "CREATE TABLE simple (id int PRIMARY KEY, n int NOT NULL CHECK (n<8))")
	dataOnly.TargetDB = failedDB
	wantOK(run(planSub, dataOnly))
	wantError(run(runSub, dataOnly), "复制数据")
	checkMigrationValue(t, failed, "SELECT count(*) FROM simple", "0")

	migrationExec(t, locked, "CREATE TABLE simple (id int PRIMARY KEY, n int NOT NULL)")
	dataOnly.TargetDB = lockedDB
	lock, unlock, step := lockMigrationFixture(t, ctx, locked, kind)
	canceled := s.migrate(runSub, source, dest, srcDB, dataOnly, step)
	_, _ = lock.ExecContext(ctx, unlock)
	_ = lock.Close()
	if canceled.OK {
		t.Fatalf("取消后迁移仍成功：%s", canceled.Data)
	}
	checkMigrationValue(t, locked, "SELECT count(*) FROM simple", "0")
	wantOK(run(runSub, dataOnly))
	checkMigrationValue(t, locked, "SELECT sum(n) FROM simple", "17")

	// Slow the real target network until a progress message arrives during COPY/LOAD DATA.
	// This exercises cancellation with a live input stream, in addition to waiting for a table lock.
	streamDB, stream := mkDB("cancel_stream", otherAddr, otherAdmin)
	migrationExec(t, stream, "CREATE TABLE stream (id int PRIMARY KEY, payload text NOT NULL)")
	throughRelay := s.save(kind, throttleMigrationTarget(t, ctx, otherAddr), "限速目标", user, false)
	streamOnly := dbx.MigrationOptions{TargetDB: streamDB, DataOnly: true, Tables: []dbx.MigrationTable{{Name: "stream"}}}
	if r := s.migrate(runSub, source, throughRelay, srcDB, streamOnly, "progress"); r.OK {
		t.Fatalf("传输已开始后取消仍成功：%s", r.Data)
	}
	checkMigrationValue(t, stream, "SELECT count(*) FROM stream", "0")

	if fallbackAddr := os.Getenv("FERRY_MIGRATION_MYSQL_FALLBACK"); fallbackAddr != "" && name == "mysql" {
		fallbackAdmin := migrationTestDB(t, ctx, kind, fallbackAddr, user, password, "")
		defer fallbackAdmin.Close()
		fallback := s.save(kind, fallbackAddr, "INSERT 回退目标", user, false)
		fallbackDB, fallbackSQL := mkDB("insert", fallbackAddr, fallbackAdmin)
		o := dbx.MigrationOptions{TargetDB: fallbackDB, Tables: []dbx.MigrationTable{parent}}
		r := s.migrate(planSub, source, fallback, srcDB, o, "")
		wantOK(r)
		if err := json.Unmarshal(r.Data, &plan); err != nil || plan.Method != "INSERT" {
			t.Fatalf("没有选择 INSERT：%s %v", r.Data, err)
		}
		wantOK(s.migrate(runSub, source, fallback, srcDB, o, ""))
		checkMigrationValue(t, fallbackSQL, "SELECT HEX(raw) FROM parent WHERE id=1", "00FF5C0A")
		checkMigrationValue(t, fallbackSQL, "SELECT CAST(flags AS UNSIGNED) FROM parent WHERE id=1", "255")
	}
}

func throttleMigrationTarget(t *testing.T, ctx context.Context, destination string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(ctx)
	t.Cleanup(func() { stop(); _ = l.Close() })
	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				server, err := (&net.Dialer{}).DialContext(ctx, "tcp", destination)
				if err != nil {
					return
				}
				defer server.Close()
				stopRelay := context.AfterFunc(ctx, func() { _ = client.Close(); _ = server.Close() })
				defer stopRelay()
				go func() {
					_, _ = io.Copy(client, server)
					_ = client.Close()
				}()
				_, _ = io.Copy(migrationThrottle{server}, struct{ io.Reader }{client})
			}()
		}
	}()
	return l.Addr().String()
}

type migrationThrottle struct{ io.Writer }

func (w migrationThrottle) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(len(p)) * time.Second / (1 << 20))
	return w.Writer.Write(p)
}

func migrationTestDB(t *testing.T, ctx context.Context, kind, addr, user, password, name string) *sql.DB {
	t.Helper()
	var driver, dsn string
	if kind == dbx.Postgres {
		driver = "pgx"
		dsn = (&url.URL{Scheme: "postgres", Host: addr, User: url.UserPassword(user, password), Path: name, RawQuery: "sslmode=disable"}).String()
	} else {
		driver = "mysql"
		c := mysql.NewConfig()
		c.Net, c.Addr, c.User, c.Passwd, c.DBName = "tcp", addr, user, password, name
		dsn = c.FormatDSN()
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func migrationExec(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, q := range statements {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s：%v", q, err)
		}
	}
}

func checkMigrationValue(t *testing.T, db *sql.DB, q, value string) {
	t.Helper()
	var got string
	if err := db.QueryRow(q).Scan(&got); err != nil || got != value {
		t.Fatalf("%s = %q，应为 %q：%v", q, got, value, err)
	}
}

func lockMigrationFixture(t *testing.T, ctx context.Context, db *sql.DB, kind string) (*sql.Conn, string, string) {
	t.Helper()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	lock, unlock, step := "LOCK TABLES simple READ", "UNLOCK TABLES", "复制 simple"
	if kind == dbx.Postgres {
		if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
			t.Fatal(err)
		}
		lock, unlock, step = "LOCK TABLE simple IN SHARE MODE", "ROLLBACK", "复制 public.simple"
	}
	if _, err := c.ExecContext(ctx, lock); err != nil {
		t.Fatal(err)
	}
	return c, unlock, step
}

func seedMigrationSource(t *testing.T, kind, dbName string, db *sql.DB) {
	t.Helper()
	migrationExec(t, db, "CREATE TABLE simple (id int PRIMARY KEY, n int NOT NULL)", "INSERT INTO simple VALUES (1, 7),(2,10)",
		"CREATE TABLE event_log (id int NOT NULL)", "CREATE TABLE stream (id int PRIMARY KEY, payload text NOT NULL)")
	if kind == dbx.Postgres {
		migrationExec(t, db, "CREATE SCHEMA rare", "CREATE TYPE rare.status AS ENUM ('待处理','结束')",
			"CREATE TABLE parent (id int GENERATED ALWAYS AS IDENTITY (START WITH 10 INCREMENT BY 2) PRIMARY KEY, name text UNIQUE, raw bytea, state rare.status, calc int GENERATED ALWAYS AS (id*2) STORED)",
			"CREATE TABLE a_child (id int PRIMARY KEY, parent_id int REFERENCES parent(id), note text)",
			"CREATE INDEX child_note ON a_child ((lower(note)))",
			"INSERT INTO parent(name,raw,state) VALUES (E'中文\\t\\n\\\\N', decode('00ff5c0a','hex'), '待处理'), ('', NULL, '结束')",
			"INSERT INTO a_child VALUES (1,10,NULL),(2,12,'')",
			"INSERT INTO stream SELECT i, repeat('a', 8000) FROM generate_series(1,1000) i",
			"CREATE FUNCTION twice(n int) RETURNS int LANGUAGE SQL AS $$ SELECT n*2 $$",
			"CREATE PROCEDURE log_value(n int) LANGUAGE SQL AS $$ INSERT INTO event_log VALUES(n) $$",
			"CREATE FUNCTION on_child() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO event_log VALUES(NEW.id); RETURN NEW; END $$",
			"CREATE TRIGGER child_log AFTER INSERT ON a_child FOR EACH ROW EXECUTE FUNCTION on_child()",
			"CREATE VIEW rare.child_view AS SELECT id, note FROM public.a_child")
		return
	}
	migrationExec(t, db, "CREATE TABLE parent (id int AUTO_INCREMENT PRIMARY KEY, name varchar(80) UNIQUE, raw blob, flags bit(8), state enum('待处理','结束'), amount decimal(35,10), calc int GENERATED ALWAYS AS (length(name)) STORED)",
		"CREATE TABLE a_child (id int PRIMARY KEY, parent_id int, note varchar(80), CONSTRAINT parent_fk FOREIGN KEY (parent_id) REFERENCES parent(id))",
		"CREATE INDEX child_note ON a_child(note)",
		"INSERT INTO parent(name,raw,flags,state,amount) VALUES (CONCAT('中文', CHAR(9,10,92), 'N'), 0x00ff5c0a, b'11111111', '待处理', 123456789012345678901.0123456789), ('', NULL, NULL, '结束', NULL)",
		"ALTER TABLE parent AUTO_INCREMENT=20",
		"INSERT INTO a_child VALUES (1,1,NULL),(2,2,'')",
		"INSERT INTO stream SELECT a.n+10*b.n+100*c.n, REPEAT('a', 8000) FROM (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) a CROSS JOIN (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) b CROSS JOIN (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) c",
		"CREATE FUNCTION twice(n int) RETURNS int DETERMINISTIC NO SQL RETURN n*2",
		"CREATE PROCEDURE log_value(n int) INSERT INTO "+dbName+".event_log VALUES(n)",
		"CREATE TRIGGER child_log AFTER INSERT ON a_child FOR EACH ROW INSERT INTO event_log VALUES(NEW.id)",
		"CREATE VIEW child_view AS SELECT id, note FROM "+dbName+".a_child",
		"CREATE VIEW alias_view AS SELECT "+dbName+".id FROM a_child AS "+dbName)
}

func checkMigrationFixture(t *testing.T, kind string, db *sql.DB) {
	t.Helper()
	checkMigrationValue(t, db, "SELECT count(*) FROM stream", "1000")
	checkMigrationValue(t, db, "SELECT sum(length(payload)) FROM stream", "8000000")
	checkMigrationValue(t, db, "SELECT twice(21)", "42")
	checkMigrationValue(t, db, "SELECT count(*) FROM event_log", "0")
	view, raw, newID := "child_view", "SELECT HEX(raw) FROM parent WHERE id=1", "20"
	parentID := 1
	generated := "10"
	if kind == dbx.Postgres {
		view, raw, newID = "rare.child_view", "SELECT upper(encode(raw,'hex')) FROM parent WHERE id=10", "14"
		parentID = 10
		generated = "20"
	} else {
		checkMigrationValue(t, db, "SELECT CAST(flags AS UNSIGNED) FROM parent WHERE id=1", "255")
		checkMigrationValue(t, db, "SELECT amount FROM parent WHERE id=1", "123456789012345678901.0123456789")
		checkMigrationValue(t, db, "SELECT sum(id) FROM alias_view", "3")
	}
	checkMigrationValue(t, db, raw, "00FF5C0A")
	checkMigrationValue(t, db, fmt.Sprintf("SELECT name FROM parent WHERE id=%d", parentID), "中文\t\n\\N")
	checkMigrationValue(t, db, fmt.Sprintf("SELECT calc FROM parent WHERE id=%d", parentID), generated)
	checkMigrationValue(t, db, "SELECT count(*) FROM "+view+" WHERE note IS NULL", "1")
	checkMigrationValue(t, db, "SELECT count(*) FROM "+view+" WHERE note=''", "1")
	if kind == dbx.Postgres {
		checkMigrationValue(t, db, "SELECT count(*) FROM pg_indexes WHERE indexname='child_note'", "1")
	} else {
		checkMigrationValue(t, db, "SELECT count(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=database() AND INDEX_NAME='child_note'", "1")
	}
	migrationExec(t, db, "INSERT INTO parent (name) VALUES ('new')", fmt.Sprintf("INSERT INTO a_child VALUES (9,%d,'new')", parentID), "CALL log_value(42)")
	checkMigrationValue(t, db, "SELECT id FROM parent WHERE name='new'", newID)
	checkMigrationValue(t, db, "SELECT sum(id) FROM event_log", "51")
	if _, err := db.Exec("INSERT INTO a_child VALUES (8,900,'invalid')"); err == nil || err == io.EOF {
		t.Fatalf("外键没有恢复：%v", err)
	}
}
