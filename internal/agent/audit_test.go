package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func auditEntries(t *testing.T, f *fakeSys, q *proto.AuditQuery) []proto.AuditEntry {
	t.Helper()
	r := exec1(t, f, "audit", &proto.Request{AuditQuery: q})
	if r.code != 0 {
		t.Fatal(r.dump())
	}
	var out []proto.AuditEntry
	json.Unmarshal(r.result.Data, &out)
	return out
}

// 本机发来的（终端、文件、数据库）：补上时间、操作人、项目、目标，写文件和 syslog；读的时候新的在前、能按类型和人筛
func TestAuditAppendAndRead(t *testing.T) {
	f := newFake(t)
	add := func(kind, object string) run {
		return exec1(t, f, "audit", &proto.Request{Project: "demo", Target: "prod", Audit: &proto.AuditEntry{Kind: kind, Object: object, Detail: strings.Repeat("x", 3000)}})
	}
	if r := add("terminal-open", ""); r.code != 0 {
		t.Fatal(r.dump())
	}
	f.now = f.now.Add(time.Minute)
	add("file-remove", "/etc/nginx/a.conf")
	f.now = f.now.Add(time.Minute)
	add("file-save", "/etc/nginx/b.conf")
	if r := add("Bad Kind", ""); r.code != proto.CodeBadInput {
		t.Fatalf("类型无效应拒绝：%s", r.dump())
	}

	p := filepath.Join(filepath.Dir(f.root), "ferry-lib", "audit.jsonl")
	if st, err := os.Stat(p); err != nil || (st.Mode().Perm() != 0o600 && os.PathSeparator == '/') {
		t.Fatalf("audit.jsonl：%v %v", st, err)
	}
	if !strings.Contains(calls(f), "logger -t ferry-audit -- 'tester@pc 删除文件 /etc/nginx/a.conf（") {
		t.Fatalf("syslog：\n%s", calls(f))
	}

	all := auditEntries(t, f, nil)
	if len(all) != 3 || all[0].Kind != "file-save" || all[2].Kind != "terminal-open" {
		t.Fatalf("%+v", all)
	}
	e := all[1]
	if e.Actor != "tester@pc" || e.Project != "demo" || e.Target != "prod" || e.Result != "ok" || !e.Time.Equal(f.now.Add(-time.Minute)) ||
		len(e.Detail) > proto.AuditMaxDetail+len("…") || !strings.HasSuffix(e.Detail, "…") {
		t.Fatalf("%+v", e)
	}
	if got := auditEntries(t, f, &proto.AuditQuery{Kind: "file-"}); len(got) != 2 {
		t.Fatalf("按前缀：%+v", got)
	}
	if got := auditEntries(t, f, &proto.AuditQuery{Kind: "file-remove"}); len(got) != 1 || got[0].Object != "/etc/nginx/a.conf" {
		t.Fatalf("按类型：%+v", got)
	}
	if got := auditEntries(t, f, &proto.AuditQuery{Actor: "TESTER"}); len(got) != 3 {
		t.Fatalf("按人：%+v", got)
	}
	if got := auditEntries(t, f, &proto.AuditQuery{Actor: "someone"}); len(got) != 0 {
		t.Fatalf("按人：%+v", got)
	}
	if got := auditEntries(t, f, &proto.AuditQuery{Since: f.now.Add(-30 * time.Second)}); len(got) != 1 {
		t.Fatalf("按时间：%+v", got)
	}
	if got := auditEntries(t, f, &proto.AuditQuery{Limit: 2}); len(got) != 2 || got[0].Kind != "file-save" {
		t.Fatalf("条数：%+v", got)
	}
}

// 改动服务器的命令自动记（成功、失败都记），查看的不记
func TestAuditChangingCommands(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{sys}})
	exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "restart"})
	exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{sys}, To: "20200101-000000-zzzz"}) // 没有这个版本：失败
	exec1(t, f, "monitor", &proto.Request{Monitor: &proto.MonitorSpec{Action: "status", Project: "demo", Target: "prod"}})

	got := auditEntries(t, f, nil)
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.Kind+":"+e.Result)
	}
	if strings.Join(kinds, " ") != "rollback:failed control:ok deploy:ok" {
		t.Fatalf("%v", kinds)
	}
	if got[0].Error == "" || got[1].Detail != "restart" || got[2].Object != "System" || !strings.Contains(got[2].Detail, id1) {
		t.Fatalf("%+v", got)
	}

	// 没有操作人的（定时器自己运行的）不记
	var out bytes.Buffer
	a := &Agent{Sys: f, Out: &out}
	a.Run("control", &proto.Request{Proto: proto.Version, Root: f.root, FerryDir: filepath.Join(filepath.Dir(f.root), "ferry-lib"), Apps: []proto.AppSpec{sys}, Action: "restart"})
	if n := len(auditEntries(t, f, nil)); n != 3 {
		t.Fatalf("没有操作人也记了：%d 条", n)
	}
}

func TestAuditOf(t *testing.T) {
	cases := []struct {
		cmd  string
		req  proto.Request
		kind string
	}{
		{"swinstall", proto.Request{Install: &proto.SoftwarePlan{Name: "mysql", Title: "MySQL", Version: "8.0"}}, "software-install"},
		{"swremove", proto.Request{Instance: "mysql", Purge: true}, "software-remove"},
		{"swconfig", proto.Request{SoftwareConfig: &proto.SoftwareConfigRequest{Operation: "read"}}, ""},
		{"swconfig", proto.Request{SoftwareConfig: &proto.SoftwareConfigRequest{Operation: "save", Instance: "nginx", FilePath: "/x.conf"}}, "software-config"},
		{"swunit", proto.Request{SwUnit: "redis.service", Action: "logs"}, ""},
		{"swunit", proto.Request{SwUnit: "redis.service", Action: "stop"}, "software-control"},
		{"portset", proto.Request{Port: 8080, Action: "open"}, "port"},
		{"fwenable", proto.Request{Allow: []string{"80/tcp"}}, "firewall-on"},
		{"monitor", proto.Request{Monitor: &proto.MonitorSpec{Action: "off"}}, "monitor-off"},
		{"monitor", proto.Request{Monitor: &proto.MonitorSpec{Action: "check"}}, ""},
		{"backup", proto.Request{Backup: &proto.BackupSpec{Action: "restore", Name: "db", File: "db-1.sql.gz"}}, "backup-restore"},
		{"backup", proto.Request{Backup: &proto.BackupSpec{Action: "list"}}, ""},
		{"backup", proto.Request{Backup: &proto.BackupSpec{Action: "before"}}, ""},
		{"logread", proto.Request{}, ""},
		{"software", proto.Request{}, ""},
	}
	for _, c := range cases {
		kind, _, _, ok := proto.AuditOf(c.cmd, &c.req)
		if kind != c.kind || ok != (c.kind != "") {
			t.Errorf("%s %+v：%q %v，应为 %q", c.cmd, c.req, kind, ok, c.kind)
		}
		if ok {
			if _, known := proto.AuditKinds[kind]; !known {
				t.Errorf("%s 没有中文名", kind)
			}
		}
	}
}

// 两个 agent 进程同时写：每行完整、不交错
func TestAuditConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				a := &Agent{Sys: noShell{}, req: &proto.Request{FerryDir: dir, Actor: fmt.Sprintf("w%d", w)}}
				if err := a.appendAudit(proto.AuditEntry{Kind: "file-save", Object: strings.Repeat("p", 3000)}); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(dir, auditFile))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 400 {
		t.Fatalf("%d 行", len(lines))
	}
	for _, l := range lines {
		var e proto.AuditEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("有交错的行：%v", err)
		}
	}
}

// noShell 真的文件系统，不执行 logger；锁总能拿到（Windows 上没有 flock）；now 不为零时用它当现在
type noShell struct {
	realSystem
	now time.Time
}

func (noShell) Shell(string, time.Duration) (string, error) { return "", nil }
func (noShell) Lock(string, time.Duration) (func(), error)  { return func() {}, nil }
func (s noShell) Now() time.Time {
	if s.now.IsZero() {
		return time.Now()
	}
	return s.now
}

// 超过 20 MB 轮转成 gz，留 12 个；读的时候不够会往前读轮转出去的
func TestAuditRotate(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.Local)
	for i := 0; i < 14; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("audit-202601%02d-000000.jsonl.gz", i+1)), nil, 0o600)
	}
	old, _ := json.Marshal(proto.AuditEntry{Time: now.Add(-time.Hour), Kind: "deploy", Actor: "old"})
	big := bytes.Repeat(append(old, '\n'), auditRotateAt/len(old)+1)
	os.WriteFile(filepath.Join(dir, auditFile), big, 0o600)

	a := &Agent{Sys: noShell{now: now}, req: &proto.Request{FerryDir: dir, Actor: "new"}}
	if err := a.appendAudit(proto.AuditEntry{Time: now, Kind: "file-save"}); err != nil {
		t.Fatal(err)
	}
	rot := rotatedAudit(dir)
	if len(rot) != auditKeep || filepath.Base(rot[0]) != "audit-20261113-100000.jsonl.gz" {
		t.Fatalf("%v", rot)
	}
	if st, _ := os.Stat(filepath.Join(dir, auditFile)); st.Size() > 1000 {
		t.Fatalf("没轮转：%d", st.Size())
	}
	got, err := readAudit(dir, proto.AuditQuery{Limit: 3})
	if err != nil || len(got) != 3 || got[0].Actor != "new" || got[1].Actor != "old" {
		t.Fatalf("%+v %v", got, err)
	}
	// Since 之前的就不再往前读
	got, _ = readAudit(dir, proto.AuditQuery{Since: now.Add(-time.Minute)})
	if len(got) != 1 {
		t.Fatalf("%d", len(got))
	}
}
