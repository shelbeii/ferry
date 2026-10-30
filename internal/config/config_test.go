package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `
version: 1
project: demo
vars:
  profile: prod
  javaOpts: -Dspring.profiles.active=${profile}
builds:
  maven:
    command: mvn -pl ${each:module} -am package
x-svc: &svc
  java: /opt/jdk/bin/java
  jvmArgs: ["${javaOpts}", "-Xmx${heap}"]
  envFile: -/etc/demo.env
  health: {tcp: "${port}"}
targets:
  prod:
    host: 10.0.0.1
    port: 2222
    runAs: demo
    root: /home/demo/app
apps:
  - name: Gateway
    type: java
    order: 20
    build: maven
    vars: {module: gw, heap: 256m, port: "8080"}
    artifact: {path: gw/target/gw.jar}
    service:
      <<: *svc
      health: {tcp: "8080", timeout: 90s}
  - name: System
    type: java
    order: 10
    build: maven
    vars: {module: sys, heap: 512m, port: "9201"}
    artifact: {path: sys/target/sys.jar}
    service: *svc
  - name: web
    type: static
    order: 30
    build: {command: npm run build, workdir: ../web}
    artifact: {path: ../web/dist}
    static: {link: /usr/share/nginx/html/web, reload: "nginx -s reload"}
`

func load(t *testing.T, text string) (*File, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, DefaultFileName)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoadSample(t *testing.T) {
	f, err := load(t, strings.Replace(sample, "x-svc: &svc", "", 1))
	if err == nil {
		t.Fatal("顶层未知字段应报错")
	}
	_ = f
}

func TestLoadWithAnchorsViaVars(t *testing.T) {
	// 顶层不允许未知字段，锚点放在第一个应用的 service 上
	text := strings.Replace(sample, "x-svc: &svc\n", "", 1)
	text = strings.Replace(text, "  java: /opt/jdk/bin/java\n  jvmArgs: [\"${javaOpts}\", \"-Xmx${heap}\"]\n  envFile: -/etc/demo.env\n  health: {tcp: \"${port}\"}\n", "", 1)
	text = strings.Replace(text, "    service:\n      <<: *svc\n      health: {tcp: \"8080\", timeout: 90s}", `    service: &svc
      java: /opt/jdk/bin/java
      jvmArgs: ["${javaOpts}", "-Xmx${heap}"]
      envFile: -/etc/demo.env
      health: {tcp: "${port}", timeout: 90s}`, 1)
	f, err := load(t, text)
	if err != nil {
		t.Fatal(err)
	}
	apps := f.Ordered()
	if apps[0].Name != "System" || apps[1].Name != "Gateway" || apps[2].Name != "web" {
		t.Fatalf("顺序错误：%v", f.AppNames())
	}
	sys := f.App("system")
	if sys.Service.Unit != "demo-system.service" {
		t.Fatalf("默认单元名：%s", sys.Service.Unit)
	}
	if sys.Service.Health.TCP != "127.0.0.1:${port}" || sys.Service.Health.Timeout.Std() != 90*time.Second {
		t.Fatalf("health：%+v", sys.Service.Health)
	}
	if len(sys.Service.EnvFile) != 1 || sys.Service.EnvFile[0] != "-/etc/demo.env" {
		t.Fatalf("envFile：%v", sys.Service.EnvFile)
	}
	tg, _ := f.Target("prod")
	if tg.KeepReleases != 5 || tg.User != "root" || tg.Port != 2222 {
		t.Fatalf("target 默认值：%+v", tg)
	}
	sel, err := f.Select(tg, []string{"gateway,System"})
	if err != nil || len(sel) != 2 || sel[0].Name != "System" {
		t.Fatalf("Select：%v %v", sel, err)
	}
	web := f.App("web")
	if web.Artifact.Verify.Static.Index != "index.html" {
		t.Fatal("static 默认 index")
	}
	if f.ResolveBuild(web).Command != "npm run build" {
		t.Fatal("内联构建")
	}
}

func TestValidationErrors(t *testing.T) {
	_, err := load(t, `
version: 1
project: Demo
targets:
  t1: {host: "", root: /opt, arch: mips}
apps:
  - {name: a, type: java, artifact: {path: a.jar}, build: nope}
  - {name: A, type: wat, artifact: {path: b}}
`)
	if err == nil {
		t.Fatal("应报错")
	}
	for _, want := range []string{"project", "host", "太浅", "arch", "不在 builds", "重复", "type 只能", "运行用户"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("缺少错误 %q：\n%v", want, err)
		}
	}
}

func TestUnknownField(t *testing.T) {
	_, err := load(t, `
version: 1
project: demo
targets: {t: {host: h, runAs: x}}
apps:
  - name: a
    type: java
    artifact: {path: a.jar}
    service: {jvmArg: [x]}
`)
	if err == nil || !strings.Contains(err.Error(), `第 9 行：不认识的字段 "jvmArg"`) || !strings.Contains(err.Error(), "升级到最新版") {
		t.Fatalf("未知字段应报错，并说明可能是新版 Ferry 写的：%v", err)
	}
}

func TestExpand(t *testing.T) {
	sc := &Scope{
		Builtins: map[string]string{"app": "System"},
		Layers:   []map[string]string{{"a": "x${b}"}, {"b": "${app}-y", "a": "ignored"}},
	}
	v, err := sc.Expand("$${literal} ${a}/${app}")
	if err != nil || v != "${literal} xSystem-y/System" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := sc.Expand("${nope}"); err == nil {
		t.Fatal("未定义变量应报错")
	}
	loop := &Scope{Layers: []map[string]string{{"a": "${b}", "b": "${a}"}}}
	if _, err := loop.Expand("${a}"); err == nil {
		t.Fatal("循环引用应报错")
	}
	if _, err := sc.Expand("${each:x}"); err == nil {
		t.Fatal("each 不在构建中应报错")
	}
}

func TestCheckRoot(t *testing.T) {
	for _, ok := range []string{"/opt/app", "/home/u/app", "/usr/local/app", "/data/x/y"} {
		if err := checkRoot(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"opt/app", "/opt", "/", "/etc/app", "/usr/app", "/opt/../etc", "/opt//x"} {
		if err := checkRoot(bad); err == nil {
			t.Errorf("%s 应被拒绝", bad)
		}
	}
}

// runtime 由 Ferry 提供 JDK：exec 里用 ${java} 可以；写了别的 java 路径要报出来
func TestRuntimeWithExec(t *testing.T) {
	head := "version: 1\nproject: p\ntargets: {a: {host: h, runAs: app}}\napps:\n  - name: api\n    type: java\n    artifact: {path: api.jar}\n    service:\n      runtime: {java: 25}\n"
	if _, err := load(t, head+"      exec: [\"${java}\", -Xmx512m, -jar, \"${artifact}\"]\n"); err != nil {
		t.Fatalf("exec 里用了 ${java}：%v", err)
	}
	_, err := load(t, head+"      exec: [/opt/jdk/bin/java, -jar, \"${artifact}\"]\n")
	if err == nil || !strings.Contains(err.Error(), "exec 里要用 ${java}") {
		t.Fatalf("应该报错：%v", err)
	}
}

// runtime.version：完整版本，且要和大版本一致
func TestRuntimeVersion(t *testing.T) {
	head := "version: 1\nproject: p\ntargets: {a: {host: h, runAs: app}}\napps:\n  - name: api\n    type: java\n    artifact: {path: api.jar}\n    service:\n"
	for _, ok := range []string{"25.0.4.1+1", "25.0.4.1+1-LTS", "21+35", "17.0.13+11"} {
		java := strings.Split(strings.Split(ok, "+")[0], ".")[0]
		if _, err := load(t, head+"      runtime: {java: "+java+", version: \""+ok+"\"}\n"); err != nil {
			t.Errorf("%s：%v", ok, err)
		}
	}
	for v, want := range map[string]string{"25": "要写完整的 JDK 版本", "25.0.4": "要写完整的 JDK 版本", "jdk-25.0.4+7": "要写完整的 JDK 版本", "21.0.5+11": "不是 Java 25 的版本"} {
		_, err := load(t, head+"      runtime: {java: 25, version: \""+v+"\"}\n")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s 应报「%s」：%v", v, want, err)
		}
	}
	f, err := load(t, head+"      runtime: {java: 25, version: 25.0.4.1+1}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Targets["a"].RuntimeJavaVersion("25.0.4.1+1"); got != "/usr/local/lib/ferry/runtimes/java-25.0.4.1_1/bin/java" {
		t.Fatalf("RuntimeJavaVersion：%s", got)
	}
}
