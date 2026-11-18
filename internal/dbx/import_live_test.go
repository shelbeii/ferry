package dbx

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLiveSQLImportBatches(t *testing.T) {
	spec := os.Getenv("FERRY_DBX_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_DBX_LIVE")
	}
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		t.Run(name, func(t *testing.T) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := strconv.Atoi(port)
			kind, user := MySQL, "root"
			if name == "postgres" {
				kind, user = Postgres, "postgres"
			}
			cfg := &Conn{ID: "import-" + name, Name: name, Kind: kind, Host: host, Port: p, User: user}
			ctx := t.Context()
			cl, release, err := (&Manager{clients: map[string]*Client{}}).Open(ctx, cfg, os.Getenv("FERRY_DBX_PASSWORD"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			admin, err := cl.DB(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			dbName := fmt.Sprintf("ferry_import_%x", time.Now().UnixNano())
			if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
				t.Fatal(err)
			}
			defer admin.ExecContext(ctx, "DROP DATABASE "+dbName)
			db, err := cl.DB(ctx, dbName)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.ExecContext(ctx, "CREATE TABLE items (id INT PRIMARY KEY, note TEXT)"); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(t.TempDir(), "import.sql")
			write := func(script string) {
				t.Helper()
				if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			const rows = 4000
			var script strings.Builder
			for i := 1; i <= rows; i++ {
				fmt.Fprintf(&script, "INSERT INTO items (id, note) VALUES (%d, 'it''s; 数据');\n", i)
			}
			write(script.String())
			for _, fast := range []bool{false, true} {
				if _, err := db.ExecContext(ctx, "TRUNCATE TABLE items"); err != nil {
					t.Fatal(err)
				}
				r, err := cl.ImportSQL(ctx, dbName, ImportSQLOptions{File: file, BatchInserts: fast}, nil)
				if err != nil || r.Stopped || r.Failed != 0 || r.LastSuccessfulStatement != rows || r.LastSuccessfulOffset != int64(script.Len()-1) {
					t.Fatalf("快速导入 %t: %+v, %v", fast, r, err)
				}
				t.Logf("快速导入 %t，%d 行：%d ms", fast, rows, r.Ms)
				var count int
				if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM items WHERE note = 'it''s; 数据'").Scan(&count); err != nil || count != rows {
					t.Fatalf("快速导入 %t，得到 %d 行：%v", fast, count, err)
				}
			}
			out := filepath.Join(t.TempDir(), "export.sql")
			exported, err := cl.Export(ctx, dbName, ExportOptions{Table: "items", Format: "sql", Out: out}, nil)
			if err != nil || exported.Rows != rows {
				t.Fatalf("导出 %d 行：%+v, %v", rows, exported, err)
			}
			dump, err := os.ReadFile(out)
			if err != nil || len(Split(kind, string(dump))) != rows/insertRows {
				t.Fatalf("导出的 INSERT 没有按行数分批：%v", err)
			}

			const prefix = "INSERT INTO items (id, note) VALUES (10001, 'before');\nSELECT 1;"
			write(prefix + "\nINSERT INTO items (id, note) VALUES (10002, 'first');\n" +
				"INSERT INTO items (id, note) VALUES (1, 'duplicate');\n" +
				"INSERT INTO items (id, note) VALUES (10003, 'last');\n")
			r, err := cl.ImportSQL(ctx, dbName, ImportSQLOptions{File: file, BatchInserts: true}, nil)
			if err != nil || !r.Stopped || r.LastSuccessfulStatement != 2 || r.LastSuccessfulOffset != int64(len(prefix)) ||
				len(r.Errors) != 1 || r.Errors[0].N != 3 || r.Errors[0].End != 5 {
				t.Fatalf("合并执行遇错：%+v, %v", r, err)
			}
			var after int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM items WHERE id > 10000").Scan(&after); err != nil || after != 1 {
				t.Fatalf("遇错后新写入 %d 行：%v", after, err)
			}
			r, err = cl.ImportSQL(ctx, dbName, ImportSQLOptions{File: file, BatchInserts: true, Continue: true}, nil)
			if err != nil || r.Stopped || r.Failed != 2 || r.LastSuccessfulStatement != 5 || len(r.Errors) != 2 || r.Errors[1].N != 4 || r.Errors[1].End != 0 {
				t.Fatalf("遇错继续执行：%+v, %v", r, err)
			}
			if kind == MySQL {
				write("/*!40101 SET @ferry_import_note = 'mysqldump' */;\nINSERT INTO items VALUES (10004, @ferry_import_note);")
				r, err := cl.ImportSQL(ctx, dbName, ImportSQLOptions{File: file}, nil)
				if err != nil || r.Failed != 0 || r.LastSuccessfulStatement != 2 {
					t.Fatalf("执行 mysqldump 会话设置：%+v, %v", r, err)
				}
				var note string
				if err := db.QueryRowContext(ctx, "SELECT note FROM items WHERE id = 10004").Scan(&note); err != nil || note != "mysqldump" {
					t.Fatalf("会话设置没有传递给后面的语句：%q, %v", note, err)
				}
				var isolated bool
				if err := db.QueryRowContext(ctx, "SELECT @ferry_import_note IS NULL").Scan(&isolated); err != nil || !isolated {
					t.Fatalf("导入的会话设置影响了普通连接：%v", err)
				}
			}
		})
	}
}
