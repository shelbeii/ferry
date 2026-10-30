package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const editSample = `# 部署配置
version: 1
project: demo
targets:
  a:
    host: 10.0.0.1   # 测试机
    user: deployer
    auth: password
    sudo: true
    runAs: svc
  b:
    host: 10.0.0.2
    identityFile: ~/.ssh/old
    runAs: svc
  c: {host: 10.0.0.3, runAs: svc}
apps:
  - {name: api, type: binary, artifact: {path: api}}
`

func TestSetTargetIdentity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ferry.yaml")
	os.WriteFile(p, []byte(strings.ReplaceAll(editSample, "\n", "\r\n")), 0o644)

	if err := SetTargetIdentity(p, "a", `C:\Users\me\.ferry\keys\demo-a_ed25519`); err != nil {
		t.Fatal(err)
	}
	if err := SetTargetIdentity(p, "b", "~/.ferry/keys/demo-b_ed25519"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	if !strings.Contains(s, "\r\n") || strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") {
		t.Fatal("应保留 CRLF 换行")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	for _, want := range []string{
		"# 部署配置",
		"  a:\n    identityFile: 'C:\\Users\\me\\.ferry\\keys\\demo-a_ed25519'\n    host: 10.0.0.1   # 测试机\n    user: deployer\n    sudo: true",
		"  b:\n    host: 10.0.0.2\n    identityFile: '~/.ferry/keys/demo-b_ed25519'\n",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少：\n%s\n实际：\n%s", want, s)
		}
	}
	if strings.Contains(s, "auth:") {
		t.Fatal("auth 行应删除")
	}
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Targets["a"].Auth != "key" || !strings.HasSuffix(f.Targets["a"].IdentityFile, "demo-a_ed25519") {
		t.Fatalf("%+v", f.Targets["a"])
	}
	if err := SetTargetIdentity(p, "c", "x"); err == nil || !strings.Contains(err.Error(), "块格式") {
		t.Fatalf("流式写法应拒绝：%v", err)
	}
	if err := SetTargetIdentity(p, "nope", "x"); err == nil {
		t.Fatal("没有的目标应报错")
	}
}

func TestAuthValidation(t *testing.T) {
	_, err := load(t, `
version: 1
project: demo
targets:
  a: {host: h, runAs: u, auth: password, identityFile: k}
  b: {host: h, runAs: u, auth: token}
apps:
  - {name: api, type: binary, artifact: {path: api}}
`)
	if err == nil || !strings.Contains(err.Error(), "不要同时设置") || !strings.Contains(err.Error(), "auth 只能是") {
		t.Fatalf("%v", err)
	}
}

func TestSetAppRuntime(t *testing.T) {
	text := `version: 1
project: demo
targets:
  a: {host: h, runAs: demo}
apps:
  - name: Api
    type: java
    artifact: {path: api.jar}
    service:
      java: /usr/bin/java   # 服务器上的 java
      jvmArgs: [-Xmx512m]
`
	f, err := load(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAppRuntime(f.Path, "api", 17); err != nil {
		t.Fatal(err)
	}
	g, err := Load(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	a := g.App("api")
	if a.Service.Runtime == nil || a.Service.Runtime.Java != 17 || a.Service.Java != "" || len(a.Service.JvmArgs) != 1 {
		t.Fatalf("%+v", a.Service)
	}
	b, _ := os.ReadFile(f.Path)
	if strings.Contains(string(b), "/usr/bin/java") || !strings.Contains(string(b), "    runtime: {java: 17}") {
		t.Fatalf("应替换 java 行：\n%s", b)
	}
	// 再改成 21：替换 runtime 行
	if err := SetAppRuntime(f.Path, "Api", 21); err != nil {
		t.Fatal(err)
	}
	if g, _ := Load(f.Path); g.App("api").Service.Runtime.Java != 21 {
		t.Fatal("应改成 21")
	}
	// 用了锚点的不自动改
	anchored := strings.Replace(text, "    service:\n", "    service: &svc\n", 1)
	f2, _ := load(t, anchored)
	if err := SetAppRuntime(f2.Path, "api", 17); err == nil || !strings.Contains(err.Error(), "手工") {
		t.Fatalf("锚点：%v", err)
	}
}
