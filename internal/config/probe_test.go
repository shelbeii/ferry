package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeDraftIgnoresIncompleteApps(t *testing.T) {
	path := localFixture(t)
	draft := []byte(`version: 1
project: demo
builds: {unfinished: {command: ""}}
targets:
  a: {host: 10.0.0.8, identityFile: ./keys/deploy, apps: [not-created-yet], appRuntimes: {not-created-yet: {java: /jdk/bin/java}}}
apps:
  - {name: unfinished, type: node}
`)
	if _, err := LoadContent(draft, path); err == nil {
		t.Fatal("草稿应尚不能部署")
	}
	f, err := LoadProbeContent(draft, path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Project != "demo" || f.Kind != KindConnections || !f.Targets["a"].Plain || f.Targets["a"].Host != "10.0.0.8" {
		t.Fatalf("没有使用草稿连接：%+v", f)
	}
	if f.Dir != filepath.Dir(path) || f.Path != path || len(f.Apps) != 0 || len(f.Builds) != 0 || len(f.Targets["a"].Apps) != 0 {
		t.Fatalf("应用没有隔离，或路径丢失：%+v", f)
	}
	saved, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(saved), "10.0.0.8") {
		t.Fatal("检测草稿不应保存配置", err)
	}
}

func TestProbeDraftKeepsLocalLogin(t *testing.T) {
	path := localFixture(t)
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteLocal(f, &Local{Targets: map[string]*LocalTarget{"a": {Auth: "password"}}}); err != nil {
		t.Fatal(err)
	}
	draft, _ := os.ReadFile(path)
	probed, err := LoadProbeContent(draft, path)
	if err != nil {
		t.Fatal(err)
	}
	if probed.Targets["a"].Auth != "password" || probed.Targets["a"].IdentityFile != "" || probed.Targets["a"].Local == nil {
		t.Fatalf("没有沿用本机登录设置：%+v", probed.Targets["a"])
	}
}

func TestProbeDraftStillValidatesConnections(t *testing.T) {
	path := localFixture(t)
	for _, draft := range []string{
		"version: 1\nproject: demo\ntargets: {prod: {host: ''}}",
		"version: 1\nproject: demo\ntargets: {prod: {host: example.test, auth: unsupported}}",
	} {
		if _, err := LoadProbeContent([]byte(draft), path); err == nil {
			t.Fatalf("错误连接应该被拦截：%s", draft)
		}
	}
}
