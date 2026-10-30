package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
)

func localFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	p := filepath.Join(dir, "ferry.yaml")
	os.WriteFile(p, []byte(`version: 1
project: demo
vars:
  repo: ./src
builds:
  mvn: {workdir: "${repo}", command: mvn package}
targets:
  a: {host: 10.0.0.1, runAs: app, identityFile: ~/.ssh/not-here-ferry-test}
  b: {host: 10.0.0.2, runAs: app, auth: password}
apps:
  - {name: api, type: java, build: mvn, artifact: {path: target/api.jar}}
`), 0o644)
	return p
}

// 没有本机设置：和 ferry.yaml 一样；私钥不在这台电脑上时查得出来
func TestLocalNone(t *testing.T) {
	p := localFixture(t)
	f, err := LoadWithLocal(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Targets["a"].Local != nil || len(f.LocalVars) != 0 {
		t.Fatal("没有本机设置")
	}
	ps := f.MachineProblems()
	if len(ps) != 1 || ps[0].Kind != "key-missing" || ps[0].Target != "a" {
		t.Fatalf("%+v", ps)
	}
	if _, err := os.Stat(filepath.Dir(LocalFile(f.Project, f.Path))); err == nil {
		t.Fatal("只读的时候不应创建项目目录")
	}
}

// 本机设置覆盖私钥、登录方式、变量；检查、保存、读回
func TestLocalApply(t *testing.T) {
	p := localFixture(t)
	f, _ := Load(p)
	key := filepath.Join(t.TempDir(), "id_here")
	os.WriteFile(key, []byte("x"), 0o600)
	other := t.TempDir()
	l := &Local{
		Vars:    map[string]string{"repo": other},
		Targets: map[string]*LocalTarget{"a": {Auth: "key", IdentityFile: key}, "b": {Auth: "key"}},
	}
	if err := CheckLocal(f, l); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocal(f, l); err != nil {
		t.Fatal(err)
	}
	g, err := LoadWithLocal(p)
	if err != nil {
		t.Fatal(err)
	}
	if a := g.Targets["a"]; a.IdentityFile != key || a.Auth != "key" || a.Local == nil {
		t.Fatalf("%+v", a)
	}
	if b := g.Targets["b"]; b.Auth != "key" || b.IdentityFile != "" {
		t.Fatalf("密码登录改成默认私钥：%+v", b)
	}
	if g.Vars["repo"] != other || strings.Join(g.LocalVars, ",") != "repo" {
		t.Fatal(g.Vars, g.LocalVars)
	}
	if ps := g.MachineProblems(); len(ps) != 0 {
		t.Fatalf("%+v", ps)
	}
	// 改成密码登录
	l.Targets["a"] = &LocalTarget{Auth: "password"}
	WriteLocal(f, l)
	g, _ = LoadWithLocal(p)
	if a := g.Targets["a"]; a.Auth != "password" || a.IdentityFile != "" {
		t.Fatalf("%+v", a)
	}
	// 清空后删掉文件
	if err := WriteLocal(f, &Local{Targets: map[string]*LocalTarget{"a": {}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LocalFile(f.Project, f.Path)); !os.IsNotExist(err) {
		t.Fatal("空的本机设置应删掉文件")
	}
}

// 源码目录不在：说明是哪个变量
func TestLocalWorkdirMissing(t *testing.T) {
	p := localFixture(t)
	f, _ := Load(p)
	f.ApplyLocal(&Local{Vars: map[string]string{"repo": filepath.Join(t.TempDir(), "gone")}})
	var got *MachineProblem
	for _, x := range f.MachineProblems() {
		if x.Kind == "workdir-missing" {
			got = &x
		}
	}
	if got == nil || got.Build != "mvn" || strings.Join(got.Vars, ",") != "repo" || !strings.Contains(got.Msg, "repo") {
		t.Fatalf("%+v", got)
	}
}

func TestCheckLocal(t *testing.T) {
	p := localFixture(t)
	f, _ := Load(p)
	err := CheckLocal(f, &Local{
		Vars:    map[string]string{"nope": "x"},
		Targets: map[string]*LocalTarget{"zz": {}, "a": {Auth: "password", IdentityFile: "k"}, "b": {Auth: "agent"}},
	})
	for _, want := range []string{"vars 里没有 nope", "没有目标 zz", "密码登录时不要设置私钥", "只能是 key 或 password"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("缺少「%s」：%v", want, err)
		}
	}
}

// 移动配置文件前：列出按配置文件所在目录写的本机路径；绝对路径、~ 开头的不算
func TestRelativePaths(t *testing.T) {
	p := localFixture(t)
	f, _ := Load(p)
	got := strings.Join(f.RelativePaths(), "|")
	if got != "builds.mvn.workdir：${repo}|api 的 artifact.path：target/api.jar" {
		t.Fatalf("%q", got)
	}
	abs := filepath.ToSlash(t.TempDir())
	f.ApplyLocal(&Local{Vars: map[string]string{"repo": abs}})
	f.Apps[0].Artifact.Path = "${repo}/target/api.jar"
	if rel := f.RelativePaths(); len(rel) != 0 {
		t.Fatalf("都是绝对路径：%q", rel)
	}
	f.Targets["a"].IdentityFile = "keys/id"
	f.Apps[0].Artifact.Path = "${configDir}/api.jar"
	if got := strings.Join(f.RelativePaths(), "|"); got != "targets.a.identityFile：keys/id|api 的 artifact.path：${configDir}/api.jar" {
		t.Fatalf("%q", got)
	}
}

// 本机设置按服务器分开：变量只对那台服务器生效，别的服务器还按 ferry.yaml；只改登录方式的也算
func TestLocalTargetVars(t *testing.T) {
	p := localFixture(t)
	os.WriteFile(p, []byte(`version: 1
project: demo
vars:
  repo: ./src
  jdk: /home/app/jdk-25
builds:
  mvn: {workdir: "${repo}", command: mvn package}
targets:
  a: {host: 10.0.0.1, runAs: app}
  b: {host: 10.0.0.2, runAs: app, vars: {port: "1"}}
apps:
  - {name: api, type: java, build: mvn, artifact: {path: "${repo}/target/api.jar"}}
`), 0o644)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	l := &Local{Targets: map[string]*LocalTarget{
		"a": {Vars: map[string]string{"jdk": "/opt/jdk"}},
		"b": {Auth: "password", Vars: map[string]string{"port": "2"}},
	}}
	if err := CheckLocal(f, l); err != nil {
		t.Fatal(err)
	}
	if err := CheckLocal(f, &Local{Targets: map[string]*LocalTarget{"a": {Vars: map[string]string{"port": "2"}}}}); err == nil {
		t.Fatal("a 没有 port 这个变量，应该报错")
	}
	if err := WriteLocal(f, l); err != nil {
		t.Fatal(err)
	}
	g, err := LoadWithLocal(p)
	if err != nil {
		t.Fatal(err)
	}
	a, b := g.Targets["a"], g.Targets["b"]
	if a.Vars["jdk"] != "/opt/jdk" || g.Vars["jdk"] != "/home/app/jdk-25" || strings.Join(a.LocalVars, ",") != "jdk" {
		t.Fatalf("a：%+v %v", a.Vars, a.LocalVars)
	}
	if a.Local != nil {
		t.Fatal("a 只改了变量，登录方式按 ferry.yaml")
	}
	if b.Vars["port"] != "2" || b.Vars["jdk"] != "" || b.Local == nil || b.Local.Auth != "password" || b.Local.Vars != nil {
		t.Fatalf("b：%+v %+v", b.Vars, b.Local)
	}
	if use := f.LocalUseVars(); !use["repo"] || use["jdk"] {
		t.Fatalf("用在这台电脑上的变量：%v", use)
	}
	if !(&LocalTarget{}).Empty() || (&LocalTarget{Vars: map[string]string{"x": "1"}}).Empty() {
		t.Fatal("Empty")
	}
}
