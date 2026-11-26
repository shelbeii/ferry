package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortInstallFlagParsing(t *testing.T) {
	c, err := parseArgs([]string{"port", "install", "prod", "firewalld", "--enable", "--allow", "80,443/tcp", "--restart-docker", "--network", "local", "--allow-changes", "-y"})
	if err != nil {
		t.Fatalf("parseArgs 失败：%v", err)
	}
	if len(c.pos) != 4 || c.pos[0] != "port" || c.pos[1] != "install" || c.pos[2] != "prod" || c.pos[3] != "firewalld" {
		t.Fatalf("位置参数解析异常：%v", c.pos)
	}
	if !c.has("enable") || !c.has("restart-docker") || !c.has("allow-changes") || !c.has("yes") {
		t.Fatalf("布尔参数解析异常：%v", c.flags)
	}
	if c.flags["allow"] != "80,443/tcp" || c.flags["network"] != "local" {
		t.Fatalf("值参数解析异常：%v", c.flags)
	}
	c2, err := parseArgs([]string{"port", "uninstall-auth", "prod", "firewalld", "--method", "ssh-password", "--challenge-id", "abc123", "--json"})
	if err != nil {
		t.Fatalf("uninstall-auth parseArgs 失败：%v", err)
	}
	if c2.flags["method"] != "ssh-password" || c2.flags["challenge-id"] != "abc123" || !c2.has("json") {
		t.Fatalf("uninstall-auth 参数解析异常：%v", c2.flags)
	}
}

func TestPortUninstallJSONRequiresAuthToken(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "ferry.yaml")
	err := os.WriteFile(cfg, []byte(`version: 1
project: demo
targets:
  prod:
    host: 127.0.0.1
    user: root
    root: /opt/demo
apps:
  - name: web
    type: static
    artifact:
      path: dist
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, nil, "port", "uninstall", "prod", "firewalld", "-c", cfg, "--json")
	if code == 0 {
		t.Fatalf("无令牌卸载不应成功：%s", out)
	}
	var last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil && obj["kind"] == "result" {
			last = obj
		}
	}
	if last == nil {
		t.Fatalf("未返回 result JSON：%s", out)
	}
	detail, _ := last["errorDetail"].(map[string]any)
	if detail["kind"] != "fw-auth-failed" {
		t.Fatalf("应返回 fw-auth-failed 错误详情：%v", last)
	}
}
