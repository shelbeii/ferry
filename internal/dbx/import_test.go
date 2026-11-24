package dbx

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestSQLImportTracksExecutedOffsets(t *testing.T) {
	const first = "-- 导入任务\nINSERT INTO t VALUES ('a; b');"
	const second = "\nINSERT INTO t VALUES (2);"
	const final = "\n-- 已读到 EOF，但尚未执行\nINSERT INTO t VALUES (3);"
	for _, chunk := range []int{1, 11, 1024} {
		var read int64 = 3 // 跳过了 UTF-8 BOM
		var positions []int64
		err := eachStatement(t.Context(), MySQL, strings.NewReader(first+second+final), chunk, &read, func(stmts []string, offsets []int64) bool {
			for i := range stmts {
				positions = append(positions, offsets[i])
				if len(positions) == 2 {
					return false
				}
			}
			return true
		})
		if err != nil || len(positions) != 2 || positions[0] != int64(3+len(first)) || positions[1] != int64(3+len(first+second)) {
			t.Fatalf("chunk=%d，执行到 %v：%v", chunk, positions, err)
		}
	}
}

// 边读边拆：不管每次读多少，拆出来的和整段一起拆的一样。
func TestEachStatement(t *testing.T) {
	cases := map[string]string{
		MySQL: "SET NAMES utf8mb4;\n-- 注释; 里面有分号\nINSERT INTO t VALUES ('a;b', \"c\\\";\"), (1);\n" +
			"/* 多行\n注释; */ SELECT 1; SELECT 2;\n\nDELIMITER ;;\nCREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END;;\nDELIMITER ;\n" +
			"INSERT INTO t VALUES ('第二行\n还在字符串里;');\nSELECT `a;b` FROM t # 尾注释\n;\nSELECT 3",
		Postgres: "SET x = 1;\nCREATE FUNCTION f() RETURNS int AS $body$\nBEGIN\n  RETURN 1; -- ;\nEND\n$body$ LANGUAGE plpgsql;\n" +
			"DO $$ BEGIN CREATE TYPE m AS ENUM ('a'); EXCEPTION WHEN duplicate_object THEN NULL; END $$;\n" +
			"INSERT INTO t VALUES (E'a\\';b', 'x''y;');\nSELECT 1",
	}
	for kind, src := range cases {
		want := Split(kind, src)
		for chunk := 1; chunk <= len(src)+1; chunk++ {
			var got []string
			var read int64
			if err := eachStatement(t.Context(), kind, strings.NewReader(src), chunk, &read, func(s []string, _ []int64) bool { got = append(got, s...); return true }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s 每次读 %d 字节：\n%q\n要\n%q", kind, chunk, got, want)
			}
			if read != int64(len(src)) {
				t.Fatalf("读了 %d 字节，要 %d", read, len(src))
			}
		}
		if kind == MySQL && len(want) != 8 {
			t.Fatalf("MySQL 应该拆成 8 条：%q", want)
		}
	}
}

func TestSQLImportExecutableCommentsAndLongLine(t *testing.T) {
	stmts := []string{
		"/*!40101 SET @saved = @@SQL_MODE */",
		"/*M!100100 SET sql_mode = '' */",
		"INSERT INTO t VALUES ('" + strings.Repeat("数据; ", 10000) + "')",
	}
	src := strings.Join(stmts, ";\r\n") + ";\r\n-- only a comment\n/* another comment */;"
	var got []string
	var read int64
	err := eachStatement(t.Context(), MySQL, strings.NewReader(src), 29, &read, func(s []string, _ []int64) bool {
		got = append(got, s...)
		return true
	})
	if err != nil || len(got) != len(stmts) || !reflect.DeepEqual(got, stmts) || read != int64(len(src)) {
		t.Fatalf("得到 %d 条语句，读取 %d 字节：%v", len(got), read, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	read = 0
	if err := eachStatement(ctx, MySQL, strings.NewReader(src), 29, &read, nil); err != context.Canceled || read != 0 {
		t.Fatalf("取消后读了 %d 字节：%v", read, err)
	}
}

func TestBatchSQLInserts(t *testing.T) {
	for _, kind := range []string{MySQL, Postgres} {
		head := "INSERT INTO " + Quote(kind, "target") + "." + Quote(kind, "t.table") + " (id, note) VALUES "
		first, second := head+"(1, 'it''s; fine')", head+"(-2, NULL)"
		s := []string{first, second, head + "(3, NOW())", second}
		got, n := batchSQLInserts(kind, s, maxImportBatchBytes)
		if n != 2 || got != first+",\n(-2, NULL)" {
			t.Fatalf("%s: %d, %s", kind, n, got)
		}
		for _, unsafe := range []string{
			head + "(1, 'a'), (2, 'b')",
			head + "(3, 'a') RETURNING id",
			head + "(3, 'a') ON DUPLICATE KEY UPDATE note = 'b'",
			head + "(3, DEFAULT)",
			head + "(3, 'a\\b')",
			head + "(3, 'a') /* hint */",
			"INSERT INTO other (id, note) VALUES (3, 'a')",
		} {
			got, n := batchSQLInserts(kind, []string{first, unsafe, second}, maxImportBatchBytes)
			if n != 1 || got != first {
				t.Fatalf("%s: 不应该合并 %s", kind, unsafe)
			}
		}
		if got, n := batchSQLInserts(kind, s, len(first)+1); n != 1 || got != first {
			t.Fatalf("%s: 合并后超过字节限制", kind)
		}
	}
}

func TestPreviewCSV(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, b, 0o644)
		return p
	}
	// UTF-8 带 BOM、分号分隔
	p, err := PreviewCSV(CSVOptions{File: write("a.csv", []byte("\xEF\xBB\xBFid;名字\n1;\"a;b\"\n"))})
	if err != nil || p.Delimiter != ";" || p.Encoding != "utf-8" || p.Rows[0][1] != "名字" || p.Rows[1][1] != "a;b" {
		t.Fatalf("%+v %v", p, err)
	}
	// GBK、制表符
	gbk, _ := simplifiedchinese.GB18030.NewEncoder().String("编号\t名称\n1\t中文名字\n")
	p, err = PreviewCSV(CSVOptions{File: write("b.tsv", []byte(gbk))})
	if err != nil || p.Delimiter != "\t" || p.Encoding != "gbk" || p.Rows[1][1] != "中文名字" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestImportSQLCopyFromStdin(t *testing.T) {
	src := "CREATE TABLE public.users (id int, name text);\n" +
		"COPY public.users (id, name) FROM stdin;\n" +
		"1\tO'Brien\n" +
		"2\t中文;分号\n" +
		"\\.\n" +
		"SELECT pg_catalog.setval('public.users_id_seq', 2, true);\n" +
		"COPY public.empty FROM STDIN WITH (FORMAT csv);\n" +
		"\\.\n" +
		"SELECT 1;\n"

	for _, chunk := range []int{7, 31, 128, 4096} {
		var read int64
		var stmts []string
		var copies []string
		var payloads []string
		err := eachStatementWithCopy(t.Context(), Postgres, strings.NewReader(src), chunk, &read,
			func(s []string, _ []int64) bool {
				stmts = append(stmts, s...)
				return true
			},
			func(stmt string, data io.Reader, endOffset func() int64) bool {
				b, rerr := io.ReadAll(data)
				if rerr != nil {
					t.Fatalf("chunk=%d 读取 COPY 数据失败：%v", chunk, rerr)
				}
				copies = append(copies, stmt)
				payloads = append(payloads, string(b))
				if endOffset() <= 0 {
					t.Fatalf("chunk=%d endOffset=%d", chunk, endOffset())
				}
				return true
			},
		)
		if err != nil {
			t.Fatalf("chunk=%d 失败：%v", chunk, err)
		}
		wantStmts := []string{
			"CREATE TABLE public.users (id int, name text)",
			"SELECT pg_catalog.setval('public.users_id_seq', 2, true)",
			"SELECT 1",
		}
		if !reflect.DeepEqual(stmts, wantStmts) {
			t.Fatalf("chunk=%d 普通语句不对：%q", chunk, stmts)
		}
		if len(copies) != 2 || payloads[0] != "1\tO'Brien\n2\t中文;分号\n" || payloads[1] != "" {
			t.Fatalf("chunk=%d COPY 结果不对：copies=%q payloads=%q", chunk, copies, payloads)
		}
	}
}

func TestOpenSQLReaderGBK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gbk.sql")
	gbk, err := simplifiedchinese.GB18030.NewEncoder().String("INSERT INTO t VALUES (1, '中文测试');\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(gbk), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd, closeReader, read, err := openSQLReader(f, 0)
	if err != nil || read != 0 {
		t.Fatalf("openSQLReader: %v read=%d", err, read)
	}
	if closeReader != nil {
		defer closeReader()
	}
	b, err := io.ReadAll(rd)
	if err != nil || !strings.Contains(string(b), "中文测试") {
		t.Fatalf("GBK 解码失败：%q %v", string(b), err)
	}
}

func TestOpenSQLReaderGzipAndResumeOffset(t *testing.T) {
	dir := t.TempDir()
	// 测试 createOut 自动 gzip 压缩 + openSQLReader 透明解压
	gzPath := filepath.Join(dir, "dump.sql.gz")
	of, err := createOut(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	const sqlText = "CREATE TABLE t (id int);\nINSERT INTO t VALUES (1);\n"
	if _, err := io.WriteString(of.w, sqlText); err != nil {
		t.Fatal(err)
	}
	if _, err := of.finish(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd, closeReader, _, err := openSQLReader(f, 0)
	if err != nil {
		t.Fatalf("打开 .sql.gz 失败：%v", err)
	}
	if closeReader != nil {
		defer closeReader()
	}
	got, err := io.ReadAll(rd)
	if err != nil || string(got) != sqlText {
		t.Fatalf("解压内容不对：%q %v", string(got), err)
	}
	if _, _, _, err := openSQLReader(f, 10); err == nil {
		t.Fatal(".sql.gz 不应允许按字节偏移续传")
	}

	// 测试未压缩 SQL 的 StartOffset 续传
	rawPath := filepath.Join(dir, "resume.sql")
	part1 := "INSERT INTO t VALUES (1);\n"
	part2 := "INSERT INTO t VALUES (2);\n"
	if err := os.WriteFile(rawPath, []byte(part1+part2), 0o644); err != nil {
		t.Fatal(err)
	}
	rf, err := os.Open(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	rd2, close2, off, err := openSQLReader(rf, int64(len(part1)))
	if err != nil || off != int64(len(part1)) {
		t.Fatalf("StartOffset 续传失败：%v off=%d", err, off)
	}
	if close2 != nil {
		defer close2()
	}
	rem, err := io.ReadAll(rd2)
	if err != nil || string(rem) != part2 {
		t.Fatalf("续传剩余内容不对：%q %v", string(rem), err)
	}
}

func TestExportTextNonUTF8AndNul(t *testing.T) {
	raw := []byte{0xff, 0x00, 0x41}
	if got := exportText(raw, "VARCHAR", true, MySQL); got != "0xff0041" {
		t.Fatalf("MySQL 非 UTF-8 字节导出应为 0xff0041，实际 %q", got)
	}
	if got := exportText(raw, "TEXT", true, Postgres); got != `'\xff0041'::bytea` {
		t.Fatalf("PG 非 UTF-8 字节导出应为 bytea hex，实际 %q", got)
	}
	nul := []byte("a\x00b")
	if got := exportText(nul, "VARCHAR", true, MySQL); got != "0x610062" {
		t.Fatalf("MySQL 含 NUL 字节导出应为 0x610062，实际 %q", got)
	}
}
