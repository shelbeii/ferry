package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/dbx"
	sqlite "github.com/ncruces/go-sqlite3/driver"
)

func TestDBApprovedSQLiteExecution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	path := filepath.Join(home, "test.sqlite")
	db, err := sqlite.Open(filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE items(value INT); INSERT INTO items VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	c, err := dbx.Save(dbx.Conn{Kind: dbx.SQLite, File: path, Name: "本机文件", AI: true, AIWrite: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(approvedKey, stmt string, served bool) (string, int) {
		var stdout, stderr bytes.Buffer
		data, err := json.Marshal(dbReq{DBCheck: approvedKey, Stmts: []string{stmt}})
		if err != nil {
			t.Fatal(err)
		}
		getenv := func(key string) string {
			if key == "FERRY_DB_REQ" {
				return string(data)
			}
			return os.Getenv(key)
		}
		code := run([]string{"db", "ai-exec", c.ID, "main", "--json"}, &cliIO{
			ctx: t.Context(), stdout: &stdout, stderr: &stderr, getenv: getenv, served: served,
		})
		return stdout.String() + stderr.String(), code
	}
	const update = "UPDATE items SET value=2 WHERE value=1"
	if text, code := execute(c.ApprovalKey(), update, false); code == 0 || !strings.Contains(text, "只能由 Ferry 桌面版调用") {
		t.Fatalf("直接调用未被拦截：%s", text)
	}
	if text, code := execute(c.ApprovalKey(), update, true); code != 0 {
		t.Fatalf("确认后的 SQLite 写入：%s", text)
	}
	if text, code := execute("stale", "DELETE FROM items", true); code == 0 || !strings.Contains(text, "确认后发生变化") {
		t.Fatalf("旧审批未被拦截：%s", text)
	}
	var value int
	if err := db.QueryRow("SELECT value FROM items").Scan(&value); err != nil || value != 2 {
		t.Fatalf("数据库最终状态：%d, %v", value, err)
	}
}
