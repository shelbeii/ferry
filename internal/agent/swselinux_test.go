package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestParsePkgChanges(t *testing.T) {
	dnf := `Last metadata expiration check: 0:01:02 ago.
Dependencies resolved.
================================================================================
 Package             Arch     Version              Repository   Size
================================================================================
Installing:
 redis               x86_64   6.2.18-1.el9         appstream    1.3 M
Upgrading:
 openssl-libs        x86_64   1:3.2.2-6.el9        baseos       2.1 M
 python3-very-long-package-name
                     noarch   1.0-1.el9            baseos       10 k
Removing dependent packages:
 old-thing           x86_64   1.0-1                @System      1 M
Installing dependencies:
 jemalloc            x86_64   5.2.1-2.el9          epel         236 k

Transaction Summary
================================================================================
Install  2 Packages
Upgrade  2 Packages

Operation aborted.
`
	got := strings.Join(parsePkgChanges(dnf), "、")
	if got != "openssl-libs（升级）、python3-very-long-package-name（升级）、old-thing（删除）" {
		t.Errorf("dnf：%s", got)
	}
	apt := "NOTE: This is only a simulation!\nInst libssl3 [3.0.2-0ubuntu1.10] (3.0.2-0ubuntu1.15 Ubuntu:22.04/jammy-updates [amd64])\n" +
		"Inst redis-server (5:6.0.16-1ubuntu1 Ubuntu:22.04/jammy [amd64])\nRemv old-redis [1.0]\nConf redis-server (5:6.0.16-1ubuntu1)\n"
	if got := strings.Join(parsePkgChanges(apt), "、"); got != "libssl3（升级）、old-redis（删除）" {
		t.Errorf("apt：%s", got)
	}
	if got := parsePkgChanges("Dependencies resolved.\nInstalling:\n redis x86_64 6.2 appstream 1 M\n\nTransaction Summary\n"); len(got) != 0 {
		t.Errorf("只新装时不算改动：%v", got)
	}
	for out, want := range map[string]string{
		"No match for argument: redis\nError: Unable to find a match: redis": "redis",
		"E: Unable to locate package redis":                                  "redis",
		"No package redis available.":                                        "redis",
		"Installing:\n redis":                                                "",
	} {
		if got := strings.Join(missingPkgs(out, []string{"redis"}), " "); got != want {
			t.Errorf("%q：%q", out, got)
		}
	}
}

// 演练出会升级已装的包时停下来（返回 CodeConfirm 和要改动的包），同意后再装
func TestSystemInstallChanges(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	f.shellFail["rpm -q --quiet 'redis'"] = true
	f.shellOut = map[string]string{"dnf install --assumeno redis 2>&1": "Upgrading:\n openssl-libs x86_64 3.2 baseos 2 M\n\nTransaction Summary\n"}
	_, err := a.systemInstall("redis", &proto.PlanSystem{DNF: []string{"redis"}}, "", false)
	pc, ok := err.(*pkgChangesError)
	if !ok || strings.Join(pc.changes, " ") != "openssl-libs（升级）" {
		t.Fatalf("应当停下来问：%v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "sh dnf install -y") {
			t.Fatal("没同意时不能装")
		}
	}
	if got, err := a.systemInstall("redis", &proto.PlanSystem{DNF: []string{"redis"}, AllowChanges: true}, "", false); err != nil || len(got) != 1 {
		t.Fatalf("同意后：%v %v", got, err)
	}
	// 软件源里没有：说清楚
	f.shellOut["dnf install --assumeno redis 2>&1"] = "No match for argument: redis\n"
	if _, err := a.systemInstall("redis", &proto.PlanSystem{DNF: []string{"redis"}}, "", false); err == nil || !strings.Contains(err.Error(), "软件源里没有 redis") {
		t.Errorf("没有包：%v", err)
	}
}

func TestAddOSFamily(t *testing.T) {
	cases := []struct {
		id, mgr string
		like    []string
		want    string
	}{
		{"openEuler", "dnf", nil, "rhel"},
		{"kylin", "apt-get", nil, "debian"},
		{"kylin", "", nil, "rhel"},
		{"rocky", "dnf", []string{"rhel", "centos", "fedora"}, "rhel centos fedora"},
		{"alpine", "", nil, ""},
	}
	for _, c := range cases {
		o := &proto.OSInfo{ID: c.id, Like: c.like}
		addOSFamily(o, c.mgr)
		if got := strings.Join(o.Like, " "); got != c.want {
			t.Errorf("%s/%s：%q，期望 %q", c.id, c.mgr, got, c.want)
		}
	}
}

func TestMemInfo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meminfo")
	os.WriteFile(p, []byte("MemTotal:        3880000 kB\nMemFree:          200000 kB\nMemAvailable:    2048000 kB\n"), 0o644)
	if total, avail := memInfo(p); total != 3789 || avail != 2000 {
		t.Errorf("%d %d", total, avail)
	}
}

func TestSwSELinux(t *testing.T) {
	list := "SELinux Port Type              Proto    Port Number\n\n" +
		"redis_port_t                   tcp      6379, 16379, 26379\n" +
		"unreserved_port_t              tcp      61000-65535, 1024-32767\n" +
		"http_cache_port_t              tcp      8080, 8118, 8123, 10001-10010\n"
	if ty, exact := portTypes(list, "tcp", 6380); strings.Join(ty, " ") != "unreserved_port_t" || exact {
		t.Errorf("6380：%v %v", ty, exact)
	}
	if ty, exact := portTypes(list, "tcp", 8080); strings.Join(ty, " ") != "unreserved_port_t http_cache_port_t" || !exact {
		t.Errorf("8080：%v %v", ty, exact)
	}

	pl := &proto.SoftwarePlan{Name: "redis", Dir: "/opt/ferry-software/redis", Ports: []int{6379, 6380, 8080},
		SELinux: &proto.PlanSELinux{Files: "redis_var_lib_t", Port: "redis_port_t"}}
	// 没开 SELinux：什么都不做
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	if se := a.swSELinux(pl); se != nil {
		t.Errorf("没开时不用设置：%+v", se)
	}
	// 开着：目录加标签；6379 本来就是，6380 加上，8080 改过来
	f = newFake(t)
	a = &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	f.shellOut = map[string]string{"getenforce 2>/dev/null": "Enforcing\n", "semanage port -l 2>/dev/null": list}
	se := a.swSELinux(pl)
	if se == nil || se.Files != pl.Dir || len(se.Ports) != 2 || se.Ports[0] != 6380 || se.Ports[1] != 8080 {
		t.Fatalf("%+v", se)
	}
	var cmds []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "sh semanage ") || strings.HasPrefix(c, "sh restorecon") {
			cmds = append(cmds, strings.TrimPrefix(c, "sh "))
		}
	}
	want := []string{
		"semanage fcontext -a -t redis_var_lib_t '/opt/ferry-software/redis(/.*)?' 2>&1 || semanage fcontext -m -t redis_var_lib_t '/opt/ferry-software/redis(/.*)?' 2>&1",
		"restorecon -R '/opt/ferry-software/redis' 2>&1",
		"semanage port -l 2>/dev/null",
		"semanage port -a -t redis_port_t -p tcp 6380 2>&1",
		"semanage port -m -t redis_port_t -p tcp 8080 2>&1",
	}
	if strings.Join(cmds, "\n") != strings.Join(want, "\n") {
		t.Errorf("命令：\n%s", strings.Join(cmds, "\n"))
	}
	// 卸载时去掉
	f.calls = nil
	a.undoSELinux("redis", se)
	if got := strings.Join(f.calls, "\n"); !strings.Contains(got, "semanage fcontext -d '/opt/ferry-software/redis(/.*)?'") || !strings.Contains(got, "semanage port -d -p tcp 8080") {
		t.Errorf("卸载：%s", got)
	}
}
