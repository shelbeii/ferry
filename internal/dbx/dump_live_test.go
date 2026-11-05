package dbx

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 导出再导回一个新库，内容要一样；CSV 导入（本机起的容器，和 TestLive 一样的 FERRY_DBX_LIVE）。
func TestLiveDumpRestore(t *testing.T) {
	spec := os.Getenv("FERRY_DBX_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_DBX_LIVE")
	}
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		t.Run(name, func(t *testing.T) { liveDumpRestore(t, name, addr) })
	}
}

func liveDumpRestore(t *testing.T, name, addr string) {
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	kind, user, src, dst := MySQL, "root", "ferry_src", "ferry_dst"
	if name == "postgres" {
		kind, user = Postgres, "postgres"
	}
	c := &Conn{ID: "dump-" + name, Name: name, Kind: kind, Host: host, Port: pn, User: user}
	m := &Manager{clients: map[string]*Client{}, keep: true}
	cl, release, err := m.Open(ctx, c, os.Getenv("FERRY_DBX_PASSWORD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	maria := cl.isMariaDB()
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
	dir := t.TempDir()
	if kind == MySQL {
		must("", "DROP DATABASE IF EXISTS ferry_src", "DROP DATABASE IF EXISTS ferry_dst", "CREATE DATABASE ferry_src", "CREATE DATABASE ferry_dst")
		must(src,
			// 子表 a_child 排在父表前面导出：导入时靠关外键检查；视图 a_top 用到 z_base，要排在后面
			"CREATE TABLE b_parent (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(50) NOT NULL, price DECIMAL(10,2), qty INT, total DECIMAL(12,2) AS (price * qty) STORED, data BLOB, born DATETIME(3), flag BIT(1))",
			"CREATE TABLE a_child (id INT PRIMARY KEY AUTO_INCREMENT, parent_id INT NOT NULL, note TEXT, CONSTRAINT fk_p FOREIGN KEY (parent_id) REFERENCES b_parent (id))",
			"CREATE TABLE log (id INT PRIMARY KEY AUTO_INCREMENT, msg VARCHAR(100))",
			"CREATE VIEW z_base AS SELECT id, name, qty FROM b_parent",
			"CREATE VIEW a_top AS SELECT * FROM z_base WHERE qty > 1",
		)
		must(src,
			"INSERT INTO b_parent (name, price, qty, data, born, flag) VALUES ('它''s \\\\ \"q\"\n第二行', 1.50, 3, 0x00FF10, '2026-01-02 03:04:05.678', b'1'), ('空', NULL, NULL, NULL, NULL, b'0')",
			"INSERT INTO a_child (parent_id, note) VALUES (1, 'x'), (1, NULL), (2, '😀')",
			"CREATE TRIGGER trg AFTER INSERT ON a_child FOR EACH ROW INSERT INTO log (msg) VALUES (CONCAT('child ', NEW.id))",
			"CREATE PROCEDURE addp(IN n VARCHAR(50)) BEGIN INSERT INTO b_parent (name) VALUES (n); SELECT LAST_INSERT_ID(); END",
			"CREATE FUNCTION twice(x INT) RETURNS INT DETERMINISTIC RETURN x * 2",
		)
		// 多点数据：多行一条 INSERT 要分好几条
		gen := "WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 1200) SELECT 1, CONCAT('row ', n) FROM s"
		if maria {
			gen = "SELECT 1, CONCAT('row ', seq) FROM seq_1_to_1200"
		}
		fill := []string{"SET SESSION cte_max_recursion_depth = 10000", "INSERT INTO a_child (parent_id, note) " + gen}
		if maria {
			fill = fill[1:]
		}
		must(src, fill...)
	} else {
		must("postgres", "DROP DATABASE IF EXISTS ferry_src", "DROP DATABASE IF EXISTS ferry_dst", "CREATE DATABASE ferry_src", "CREATE DATABASE ferry_dst")
		must(src,
			"CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')",
			"CREATE SEQUENCE shared_seq START 100",
			"CREATE TABLE b_parent (id INT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name VARCHAR(50) NOT NULL, price NUMERIC(10,2), qty INT, total NUMERIC(12,2) GENERATED ALWAYS AS (price * qty) STORED, data BYTEA, born TIMESTAMPTZ, m mood, tags TEXT[], j JSONB, CHECK (qty >= 0))",
			"CREATE TABLE a_child (id SERIAL PRIMARY KEY, parent_id INT NOT NULL REFERENCES b_parent (id), note TEXT, code INT DEFAULT nextval('shared_seq'))",
			"CREATE TABLE log (id BIGSERIAL PRIMARY KEY, msg TEXT)",
			"CREATE VIEW z_base AS SELECT id, name, qty FROM b_parent",
			"CREATE VIEW a_top AS SELECT * FROM z_base WHERE qty > 1",
			"CREATE MATERIALIZED VIEW mv AS SELECT parent_id, count(*) AS n FROM a_child GROUP BY parent_id",
			"CREATE FUNCTION log_child() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO log (msg) VALUES ('child ' || NEW.id); RETURN NEW; END $$",
			"CREATE FUNCTION twice(x int) RETURNS int LANGUAGE sql AS 'SELECT x * 2'",
			"INSERT INTO b_parent (name, price, qty, data, born, m, tags, j) VALUES (E'它''s \\\\ \"q\"\\n第二行', 1.50, 3, '\\x00ff10', '2026-01-02 03:04:05.678+08', 'happy', ARRAY['a','b,c'], '{\"k\": [1, \"x\"]}'), ('空', NULL, NULL, NULL, NULL, NULL, NULL, NULL)",
			"INSERT INTO a_child (parent_id, note) VALUES (1, 'x'), (1, NULL), (2, '😀')",
			"INSERT INTO a_child (parent_id, note) SELECT 1, 'row ' || n FROM generate_series(1, 1200) n",
			"REFRESH MATERIALIZED VIEW mv",
			"CREATE TRIGGER trg AFTER INSERT ON a_child FOR EACH ROW EXECUTE FUNCTION log_child()",
		)
	}

	// 导出
	out := filepath.Join(dir, "dump.sql")
	r, err := cl.Dump(ctx, src, DumpOptions{Out: out, Data: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	text := string(b)
	t.Logf("导出 %d 张表、%d 行、%d 字节", r.Tables, r.Rows, r.Bytes)
	if strings.Count(text, "INSERT INTO") > 10 {
		t.Errorf("应该多行一条 INSERT：%d 条", strings.Count(text, "INSERT INTO"))
	}
	if strings.Contains(text, "total") && strings.Contains(text, "INSERT INTO "+Quote(kind, "b_parent")+" (") {
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, "INSERT INTO ") && strings.Contains(l, "b_parent") && strings.Contains(l, Quote(kind, "total")) {
				t.Fatal("计算列不应该导出：" + l)
			}
		}
	}

	// 导回一个新库
	res, err := cl.ImportSQL(ctx, dst, ImportSQLOptions{File: out}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed > 0 {
		t.Fatalf("导入出错：%+v\n%s", res.Errors, text)
	}
	t.Logf("导入 %d 条语句", res.Stmts)

	same := func(q string) {
		t.Helper()
		a, _ := cl.Query(ctx, src, q, QueryOptions{Max: 5000})
		z, _ := cl.Query(ctx, dst, q, QueryOptions{Max: 5000})
		if a[0].Error != "" || z[0].Error != "" {
			t.Fatalf("%s：%s / %s", q, a[0].Error, z[0].Error)
		}
		as, zs := strings.TrimSpace(strings.Join(flat(a[0].Rows), "|")), strings.TrimSpace(strings.Join(flat(z[0].Rows), "|"))
		if as != zs {
			t.Fatalf("%s 不一样：\n原来 %.300s\n导入 %.300s", q, as, zs)
		}
	}
	same("SELECT * FROM b_parent ORDER BY id")
	same("SELECT * FROM a_child ORDER BY id")
	same("SELECT * FROM log ORDER BY id")
	same("SELECT * FROM a_top ORDER BY id")
	same("SELECT twice(21)")
	if kind == Postgres {
		same("SELECT * FROM mv ORDER BY parent_id")
	}

	// 导入后：自增接着编号，触发器还在，外键还在
	must(dst, "INSERT INTO a_child (parent_id, note) VALUES (2, 'new')")
	if kind == Postgres {
		must(dst, "INSERT INTO b_parent (name) VALUES ('new')")
	} else {
		must(dst, "CALL addp('new')")
	}
	q, _ := cl.Query(ctx, dst, "SELECT COUNT(*) FROM log WHERE msg LIKE 'child %'", QueryOptions{})
	q2, _ := cl.Query(ctx, src, "SELECT COUNT(*) FROM log WHERE msg LIKE 'child %'", QueryOptions{})
	if n1, _ := strconv.Atoi(q[0].Rows[0][0].(string)); n1 != atoi(q2[0].Rows[0][0])+1 {
		t.Fatalf("触发器没导回来：%v %v", q[0].Rows, q2[0].Rows)
	}
	if rs, _ := cl.Exec(ctx, dst, []string{"INSERT INTO a_child (parent_id) VALUES (999)"}); rs[0].Error == "" {
		t.Fatal("外键没导回来")
	}

	// CSV：表头、GBK、空的当 NULL
	csvText := "名字,数量,价格\n苹果,3,1.5\n梨,,\n\"带,逗号\",2,0.25\n"
	gbk, _ := simplifiedchinese.GB18030.NewEncoder().String(csvText)
	file := filepath.Join(dir, "in.csv")
	os.WriteFile(file, []byte(gbk), 0o644)
	p, err := PreviewCSV(CSVOptions{File: file})
	if err != nil || p.Encoding != "gbk" || len(p.Rows) != 4 || p.Rows[3][0] != "带,逗号" {
		t.Fatalf("预览：%+v %v", p, err)
	}
	ir, err := cl.ImportCSV(ctx, dst, CSVImport{CSVOptions: CSVOptions{File: file, Header: true}, Table: "b_parent", Columns: []string{"name", "qty", "price"}, EmptyNull: true}, nil)
	if err != nil || ir.Rows != 3 {
		t.Fatalf("导入 CSV：%+v %v", ir, err)
	}
	if r, _ := cl.Query(ctx, dst, "SELECT qty, price FROM b_parent WHERE name IN ('苹果', '梨', '带,逗号')", QueryOptions{}); len(r[0].Rows) != 3 {
		t.Fatalf("CSV 导进去的不对：%v", r[0].Rows)
	}
	if r, _ := cl.Query(ctx, dst, "SELECT qty, price FROM b_parent WHERE name = '梨'", QueryOptions{}); r[0].Rows[0][0] != nil || r[0].Rows[0][1] != nil {
		t.Fatalf("空的应该是 NULL：%v", r[0].Rows)
	}
	// 有一行不对：全部撤回
	os.WriteFile(file, []byte("name,qty\nok,1\nbad,abc\n"), 0o644)
	if _, err := cl.ImportCSV(ctx, dst, CSVImport{CSVOptions: CSVOptions{File: file, Header: true}, Table: "b_parent", Columns: []string{"name", "qty"}}, nil); err == nil {
		t.Fatal("abc 导进数字列应该失败")
	}
	if r, _ := cl.Query(ctx, dst, "SELECT COUNT(*) FROM b_parent WHERE name = 'ok'", QueryOptions{}); r[0].Rows[0][0] != "0" {
		t.Fatal("失败时应该全部撤回")
	}
}

func flat(rows [][]any) []string {
	var out []string
	for _, r := range rows {
		var cs []string
		for _, v := range r {
			switch x := v.(type) {
			case nil:
				cs = append(cs, "NULL")
			case Binary:
				cs = append(cs, x.Hex)
			default:
				cs = append(cs, cellText(x))
			}
		}
		out = append(out, strings.Join(cs, ","))
	}
	return out
}

func cellText(v any) string {
	if c, ok := v.(Cut); ok {
		return c.Text
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func atoi(v any) int {
	n, _ := strconv.Atoi(v.(string))
	return n
}
