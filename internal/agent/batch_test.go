package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// systemctl show 多个单元的输出按空行分段，按 Id 对应；ExecStart 一起读出来。
func TestParsePropsBlocks(t *testing.T) {
	text := "Id=a.service\nLoadState=loaded\nActiveState=active\nMainPID=12\nExecStart={ path=/usr/bin/a ; argv[]=/usr/bin/a --flag ; }\n\n" +
		"Id=b.service\nLoadState=not-found\nActiveState=inactive\nMainPID=0\n"
	got := parsePropsBlocks(text)
	if len(got) != 2 {
		t.Fatalf("读到 %d 个单元：%+v", len(got), got)
	}
	a := got["a.service"]
	if a.Load != "loaded" || a.Active != "active" || a.PID != 12 {
		t.Errorf("a.service 属性不对：%+v", a)
	}
	if path := execStartPath(a.ExecStart); path != "/usr/bin/a" {
		t.Errorf("a.service 程序 %q", path)
	}
	if got["b.service"].Load != "not-found" {
		t.Errorf("b.service 属性不对：%+v", got["b.service"])
	}
}

// 应用状态一次读完所有单元属性，不再逐个 systemctl show。
func TestStatusBatchesUnitProps(t *testing.T) {
	f := newFake(t)
	apps := []proto.AppSpec{javaApp("System"), javaApp("Auth"), javaApp("Job")}
	r := exec1(t, f, "status", &proto.Request{Apps: apps})
	if r.code != 0 {
		t.Fatalf("status：\n%s", r.dump())
	}
	if f.propsBatchCalls == 0 {
		t.Fatal("status 没有用批量查询")
	}
	if f.propsCalls != 0 {
		t.Fatalf("status 还逐个查了 %d 次 systemctl show", f.propsCalls)
	}
}

// 常驻 agent 里应用状态的防火墙结果会缓存：两次查看只扫一次。
func TestFirewallStateCachesStatusScan(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var scans int
	run := fakeRun(map[string]string{
		"systemctl is-active --quiet firewalld": "running\n",
		"--list-all-zones":                      zonePublic,
		"--get-default-zone":                    "public\n",
	})
	sys := &fwShellSys{fakeSys: f, run: func(cmd string, d time.Duration) (string, error) {
		if strings.Contains(cmd, "is-active --quiet firewalld") {
			mu.Lock()
			scans++
			mu.Unlock()
		}
		return run(cmd, d)
	}}
	a := &Agent{Sys: sys, req: &proto.Request{Root: f.root, FerryDir: t.TempDir()}, fw: &fwCache{}}
	first := a.firewallState(true)
	if first == nil || !first.Active {
		t.Fatalf("没读到防火墙：%+v", first)
	}
	mu.Lock()
	before := scans
	mu.Unlock()
	if second := a.firewallState(true); second != first {
		t.Fatal("第二次没有用缓存的结果")
	}
	mu.Lock()
	defer mu.Unlock()
	if scans != before {
		t.Fatalf("第二次又扫了防火墙（%d → %d 次）", before, scans)
	}
}

// fwShellSys 用假的命令输出代替服务器上的 shell。
type fwShellSys struct {
	*fakeSys
	run func(string, time.Duration) (string, error)
}

func (f *fwShellSys) Shell(cmd string, timeout time.Duration) (string, error) {
	return f.run(cmd, timeout)
}
