package dbnative

import (
	"errors"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
	"github.com/go-sql-driver/mysql"
)

func TestValidateTempDir(t *testing.T) {
	base := proto.DBTransfer{
		Engine:  "mysql",
		Scope:   "database",
		Threads: 4,
		Source:  proto.DBEndpoint{Host: "10.0.0.1", Port: 3306, User: "root", Database: "src"},
		Target:  proto.DBEndpoint{Host: "10.0.0.2", Port: 3306, User: "root", Database: "dst"},
	}
	if err := validate(base); err != nil {
		t.Fatalf("默认配置应有效：%v", err)
	}
	bad := base
	bad.TempDir = "relative/tmp"
	if err := validate(bad); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("相对路径 TempDir 应报错：%v", err)
	}
	bad.TempDir = "/data/tmp\nrm -rf /"
	if err := validate(bad); err == nil {
		t.Fatalf("含换行的 TempDir 应报错")
	}
}

func TestIsMissingDatabaseErr(t *testing.T) {
	if !isMissingDatabaseErr("mysql", &mysql.MySQLError{Number: 1049, Message: "Unknown database 'foo'"}) {
		t.Fatal("应识别 MySQL 1049 错误")
	}
	if !isMissingDatabaseErr("postgres", errors.New(`ERROR: database "foo" does not exist (SQLSTATE 3D000)`)) {
		t.Fatal("应识别 PostgreSQL 3D000 错误")
	}
	if isMissingDatabaseErr("mysql", errors.New("connection refused")) {
		t.Fatal("不应将普通连接错误误判为库不存在")
	}
}

func TestFormatBytes(t *testing.T) {
	if got := formatBytes(512); got != "512 B" {
		t.Fatalf("got %q", got)
	}
	if got := formatBytes(2 * 1024 * 1024); got != "2.0 MiB" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressParsing(t *testing.T) {
	if pct, ok := parsePercentProgress("63% (12.40 MB / 19.60 MB)"); !ok || pct != 63 {
		t.Fatalf("parsePercentProgress = (%d, %v)", pct, ok)
	}
	if _, ok := parsePercentProgress("no percent here"); ok {
		t.Fatal("不应误判无百分比行")
	}
	if !isTableDoneLine("pg_dump", "pg_dump: dumping contents of table \"public.users\"") {
		t.Fatal("应识别 pg_dump 进度行")
	}
	if !isTableDoneLine("mydumper", "Thread 1: Finished dumping table `db`.`users`") {
		t.Fatal("应识别 mydumper 完成行")
	}
}
