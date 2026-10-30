package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// permFixture 和 TestServiceAccessProblems 一样的单元：包装脚本 + 临时目录。
func permFixture(t *testing.T) (f *fakeSys, s proto.AppSpec, script, scripts, tmp string) {
	f = newFake(t)
	base := t.TempDir()
	t.Chdir(base)
	mk := func(rel, data string) string {
		p := filepath.Join(base, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if data == "" {
			os.MkdirAll(p, 0o755)
		} else {
			os.WriteFile(p, []byte(data), 0o755)
		}
		return filepath.ToSlash(strings.TrimPrefix(p, filepath.VolumeName(p)))
	}
	script = mk("app/scripts/with-env.sh", "#!/bin/sh\nexec \"$@\"\n")
	tmp = mk("app/data/tmp", "")
	bash := mk("usr/bin/bash", "ELF")
	scripts = filepath.ToSlash(filepath.Dir(filepath.FromSlash(script)))
	s = javaApp("Job")
	s.UnitText = ManagedMarker + "\n[Service]\nUser=svc\nExecStart=" + bash + " " + script + " -Djava.io.tmpdir=" + tmp + "\n"
	return
}

func runAccess(f *fakeSys, s *proto.AppSpec, mode string) *proto.Preflight {
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root, FixPerms: mode}}
	p := &proto.Preflight{}
	a.checkServiceAccess(s, p, map[string]error{})
	return p
}

func TestFixPermsPlanThenApply(t *testing.T) {
	f, s, _, scripts, tmp := permFixture(t)
	dir := os.ModeDir
	// 172 上遇到的两种情况：脚本目录 root 700（其他用户进不去），临时目录 root:app 755（组只能读）
	f.denied = map[string]bool{scripts: true, tmp + "|w": true}
	f.owners = map[string]Ownership{
		scripts: {UID: 0, GID: 0, User: "root", Group: "root", Perm: dir | 0o700},
		tmp:     {UID: 0, GID: 1001, User: "root", Group: "app", Perm: dir | 0o755},
	}

	// 不自动修正：两个问题
	if p := runAccess(f, &s, ""); len(p.Problems) != 2 || len(p.Fixable) != 0 {
		t.Fatalf("problems：%v", p.Problems)
	}
	// plan：只列出，不改；没有问题
	p := runAccess(f, &s, FixPlan)
	if len(p.Problems) != 0 || len(p.Fixable) != 2 || len(f.calls) != 0 {
		t.Fatalf("plan：problems=%v fixable=%v calls=%v", p.Problems, p.Fixable, f.calls)
	}
	joined := strings.Join(p.Fixable, "\n")
	if !strings.Contains(joined, "chown svc: "+tmp) || !strings.Contains(joined, scripts) {
		t.Fatalf("fixable：\n%s", joined)
	}
	// apply：真的改，改完重新检查都通过
	p = runAccess(f, &s, FixApply)
	if len(p.Problems) != 0 || len(p.Fixed) != 2 {
		t.Fatalf("apply：problems=%v fixed=%v", p.Problems, p.Fixed)
	}
	calls := strings.Join(f.calls, "\n")
	if !strings.Contains(calls, "chown 1000:1000 "+tmp) || !strings.Contains(calls, "chmod 701 "+scripts) {
		t.Fatalf("应 chown 临时目录、给脚本目录加 o+x：\n%s", calls)
	}
}

func TestFixPermsPrefersACL(t *testing.T) {
	f, s, _, scripts, _ := permFixture(t)
	f.acl = true
	f.denied = map[string]bool{scripts: true}
	f.owners = map[string]Ownership{scripts: {UID: 0, GID: 0, User: "root", Group: "root", Perm: os.ModeDir | 0o700}}
	p := runAccess(f, &s, FixApply)
	if len(p.Problems) != 0 || !strings.Contains(strings.Join(f.calls, "\n"), "setfacl u:svc:x "+scripts) {
		t.Fatalf("应只给运行用户开 ACL：problems=%v calls=%v", p.Problems, f.calls)
	}
}

func TestFixPermsGroupAndOwner(t *testing.T) {
	f, s, script, _, _ := permFixture(t)
	// 文件属于运行用户的组、组没有读权限 → chmod g+r
	f.denied = map[string]bool{script + "|r": true}
	f.owners = map[string]Ownership{script: {UID: 0, GID: 1001, User: "root", Group: "app", Perm: 0o701}}
	if p := runAccess(f, &s, FixApply); len(p.Problems) != 0 || !strings.Contains(strings.Join(f.calls, "\n"), "chmod 741 "+script) {
		t.Fatalf("g+r：problems=%v calls=%v", p.Problems, f.calls)
	}
}

func TestFixPermsNotFixable(t *testing.T) {
	f, s, script, _, tmp := permFixture(t)
	// 权限位允许却访问不了（ACL / 安全策略）：不自动改，照常报问题
	f.denied = map[string]bool{script + "|r": true}
	f.owners = map[string]Ownership{script: {UID: 0, GID: 0, User: "root", Group: "root", Perm: 0o755}}
	if p := runAccess(f, &s, FixApply); len(p.Problems) != 1 || len(p.Fixed) != 0 || !strings.Contains(p.Problems[0], "KYSEC") {
		t.Fatalf("ACL / 安全策略：problems=%v fixed=%v", p.Problems, p.Fixed)
	}
	// 要写入的目录正好是部署根目录：不交给运行用户
	f2, s2, _, _, tmp2 := permFixture(t)
	f2.denied = map[string]bool{tmp2 + "|w": true}
	f2.owners = map[string]Ownership{tmp2: {UID: 0, GID: 0, User: "root", Group: "root", Perm: os.ModeDir | 0o755}}
	a := &Agent{Sys: f2, req: &proto.Request{Root: tmp2, FixPerms: FixApply}}
	p := &proto.Preflight{}
	a.checkServiceAccess(&s2, p, map[string]error{})
	if len(p.Problems) != 1 || len(p.Fixed) != 0 {
		t.Fatalf("部署根目录不能交给运行用户：problems=%v fixed=%v", p.Problems, p.Fixed)
	}
	_ = tmp
}

func TestProtectedPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/": true, "/home": true, "/data": true, "/etc/nginx": true, "/usr/local/bin": true, "/root/x": true,
		"/home/shop/app": false, "/data/shop/tmp": false, "/opt/app/scripts": false,
	} {
		if protectedPath(p) != want {
			t.Errorf("%s：%v", p, !want)
		}
	}
	if !otherHome("/home/alice", "svc") || otherHome("/home/svc", "svc") || otherHome("/home/alice/app", "svc") {
		t.Error("otherHome")
	}
}

func TestFixRootDir(t *testing.T) {
	f := newFake(t)
	root := "/data/shop"
	f.owners = map[string]Ownership{root: {UID: 1000, GID: 1000, User: "svc", Group: "svc", Perm: os.ModeDir | os.ModeSetgid | 0o777}}
	f.denied = map[string]bool{}
	a := &Agent{Sys: f, req: &proto.Request{Root: root}}
	desc, ok := a.fixRootDir(root, root, false)
	if !ok || !strings.Contains(desc, "chown root:root") || !strings.Contains(desc, "chmod go-w") || len(f.calls) != 0 {
		t.Fatalf("plan：%q %v %v", desc, ok, f.calls)
	}
	// 上级目录不自动改
	if _, ok := a.fixRootDir(root, "/data", false); ok {
		t.Fatal("上级目录不应自动修改")
	}
}

func TestJavaVersions(t *testing.T) {
	for v, want := range map[string]int{"1.8.0_412": 8, "17.0.9": 17, "21": 21, "11.0.2+9": 11, "": 0} {
		if got := parseJavaMajor(v); got != want {
			t.Errorf("%q → %d，期望 %d", v, got, want)
		}
	}
	f := newFake(t)
	f.javas = map[string]string{"/usr/bin/java": "1.8.0_412"}
	s := javaApp("Job")
	s.Java = "/usr/bin/java"
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root, Apps: []proto.AppSpec{s}}}
	p := &proto.Preflight{}
	a.checkJavas(p)
	if j := p.Javas["/usr/bin/java"]; j.Major != 8 || j.Version != "1.8.0_412" {
		t.Fatalf("%+v", p.Javas)
	}
	s.Java = "/opt/missing/bin/java"
	a.req.Apps = []proto.AppSpec{s}
	p = &proto.Preflight{}
	a.checkJavas(p)
	if j := p.Javas[s.Java]; j.Major != 0 || j.Error == "" {
		t.Fatalf("不存在的 java：%+v", j)
	}
}
