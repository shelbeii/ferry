package dbx

import (
	"context"
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
