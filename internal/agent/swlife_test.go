package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestLogrotate(t *testing.T) {
	old := logrotateDir
	logrotateDir = t.TempDir()
	defer func() { logrotateDir = old }()
	a := &Agent{Sys: newFake(t), Out: io.Discard, req: &proto.Request{}}
	f := a.swLogrotate("redis", []string{"/opt/ferry-software/redis/logs/redis.log"})
	b, _ := os.ReadFile(f)
	for _, want := range []string{"# " + ManagedMarker, "/opt/ferry-software/redis/logs/redis.log {", "daily", "rotate 14", "copytruncate"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("没有 %q：\n%s", want, b)
		}
	}
	// 别人的配置不删
	other := filepath.Join(logrotateDir, "ferry-other")
	os.WriteFile(other, []byte("/var/log/x {}\n"), 0o644)
	removeLogrotate(other)
	removeLogrotate(f)
	if _, err := os.Stat(other); err != nil {
		t.Error("不是 Ferry 写的不能删")
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("Ferry 写的应当删掉")
	}
	if a.swLogrotate("x", nil) != "" {
		t.Error("没有日志时不写")
	}
}

func TestValidKeep(t *testing.T) {
	for _, p := range []string{"nacos/data", "sentinel-dashboard.jar", "mysql/lib/private/libaio.so.1"} {
		if !validKeep(p) {
			t.Errorf("%s 应当可以", p)
		}
	}
	for _, p := range []string{"", ".", "..", "../x", "/etc", "a/../../b", "a//b"} {
		if validKeep(p) {
			t.Errorf("%s 应当拒绝", p)
		}
	}
}

func TestDumpCommandBin(t *testing.T) {
	my := proto.BackupItem{Kind: "mysql", Database: "*", User: "app", Port: 3307, Bin: "/opt/ferry-software/mysql/mysql/bin"}
	got := dumpCommand(my, nil, false)
	if !strings.Contains(got, "'/opt/ferry-software/mysql/mysql/bin/mysqldump'") || !strings.HasSuffix(got, "--all-databases") || strings.Contains(got, "'*'") {
		t.Errorf("mysql 全部库：%s", got)
	}
	if got := dumpCommand(my, nil, true); !strings.Contains(got, "/mysql/bin/mysql'") || strings.Contains(got, "'*'") {
		t.Errorf("mysql 恢复全部库：%s", got)
	}
	pg := proto.BackupItem{Kind: "postgres", Database: "*", Bin: "/opt/pg/bin"}
	if got := dumpCommand(pg, nil, false); !strings.HasPrefix(got, "'/opt/pg/bin/pg_dumpall' --clean --if-exists") {
		t.Errorf("pg 全部库：%s", got)
	}
	if got := dumpCommand(pg, nil, true); !strings.Contains(got, "-d postgres") {
		t.Errorf("pg 恢复全部库：%s", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 10}
	for i := 0; i < 100; i++ {
		b.Write([]byte("0123456789"))
	}
	b.Write([]byte("END"))
	if s := b.String(); !strings.HasSuffix(s, "3456789END") || !strings.Contains(s, "993 字节") {
		t.Errorf("%q", s)
	}
	small := &cappedBuffer{max: 10}
	small.Write([]byte("abc"))
	if small.String() != "abc" {
		t.Errorf("%q", small.String())
	}
}
