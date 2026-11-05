package dbx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

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
			if err := eachStatement(kind, strings.NewReader(src), chunk, &read, func(s []string) bool { got = append(got, s...); return true }); err != nil {
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
