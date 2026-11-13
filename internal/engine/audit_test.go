package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/proto"
)

// 这台电脑上的那份：追加、新的在前、按类型 / 人 / 项目 / 目标筛
func TestLocalAudit(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	saveLocalAudit(proto.AuditEntry{Time: time.Now().Add(-time.Hour), Actor: "张三（zs@pc）", Project: "shop", Target: "prod", Kind: "terminal-open", Result: "ok"})
	saveLocalAudit(proto.AuditEntry{Time: time.Now(), Actor: "李四（ls@pc）", Project: "shop", Target: "test", Kind: "file-remove", Object: "/a", Result: "ok"})
	AuditLocal("王五（ww@pc）", "db-exec", "本机 MySQL / shop", "UPDATE t SET a=1", errors.New("拒绝"))

	all, err := LocalAuditLog(proto.AuditQuery{}, "", "")
	if err != nil || len(all) != 3 || all[0].Kind != "db-exec" || all[0].Result != "failed" || all[0].Error != "拒绝" || all[2].Kind != "terminal-open" {
		t.Fatalf("%+v %v", all, err)
	}
	if got, _ := LocalAuditLog(proto.AuditQuery{}, "shop", "prod"); len(got) != 1 || got[0].Kind != "terminal-open" {
		t.Fatalf("按目标：%+v", got)
	}
	if got, _ := LocalAuditLog(proto.AuditQuery{Kind: "file-"}, "", ""); len(got) != 1 {
		t.Fatalf("按类型：%+v", got)
	}
	if got, _ := LocalAuditLog(proto.AuditQuery{Actor: "李四"}, "", ""); len(got) != 1 || got[0].Object != "/a" {
		t.Fatalf("按人：%+v", got)
	}
	if got, _ := LocalAuditLog(proto.AuditQuery{Since: time.Now().Add(-time.Minute)}, "", ""); len(got) != 2 {
		t.Fatalf("按时间：%+v", got)
	}
}

// 改动服务器的 agent 命令：本机也按同样的规则记
func TestAuditOfShared(t *testing.T) {
	if k, _, _, ok := proto.AuditOf("deploy", &proto.Request{Apps: []proto.AppSpec{{Name: "api"}}}); !ok || k != "deploy" {
		t.Fatal(k, ok)
	}
	if _, _, _, ok := proto.AuditOf("status", &proto.Request{}); ok {
		t.Fatal("查看的不记")
	}
}
