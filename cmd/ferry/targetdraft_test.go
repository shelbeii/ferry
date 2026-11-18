package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
)

func TestTargetDraftChecksApplicationRequirements(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	path := filepath.Join(t.TempDir(), "ferry.yaml")
	base := `version: 1
project: demo
vars:
  commonEnv: /etc/demo/shared.env
targets:
  existing:
    host: 127.0.0.1
    runAs: app
    vars:
      secrets: /etc/demo/existing.env
      dataRoot: /srv/demo
apps:
  - name: api
    type: java
    artifact: {path: target/api.jar}
    vars: {envPath: "${secrets}"}
    service:
      envFile: ["${envPath}", "${commonEnv}"]
      secrets: [API_TOKEN]
  - name: web
    type: static
    artifact: {path: dist/}
    root: "${dataRoot}"
  - name: worker
    type: binary
    artifact: {path: worker}
    service: {user: worker}
`
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(target string) (string, engine.TargetDraftCheck) {
		t.Helper()
		draft := strings.Replace(base, "\napps:\n", "\n"+target+"\napps:\n", 1)
		out, code := runCLI(t, map[string]string{"FERRY_CONFIG_CONTENT": draft}, "validate", "-c", path, "--target", "new", "--from-env", "--json")
		if code != 0 {
			t.Fatalf("validate: %s", out)
		}
		var result struct {
			OK   bool
			Data engine.TargetDraftCheck
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil || !result.OK {
			t.Fatalf("检查结果：%s（%v）", out, err)
		}
		return draft, result.Data
	}

	_, incomplete := check("  new: {host: 127.0.0.1}")
	if !slices.Equal(incomplete.RunAsApps, []string{"api"}) || !incomplete.HasAppSecrets ||
		len(incomplete.MissingVars) != 2 || len(incomplete.MissingVars["secrets"]) != 1 ||
		!strings.Contains(incomplete.MissingVars["secrets"][0], "api service.envFile") ||
		len(incomplete.MissingVars["dataRoot"]) != 1 || len(incomplete.Problems) != 0 {
		t.Fatalf("需要在弹窗中报告的配置：%+v", incomplete)
	}

	_, subset := check("  new: {host: 127.0.0.1, apps: [worker]}")
	if len(subset.RunAsApps) != 0 || subset.HasAppSecrets || len(subset.MissingVars) != 0 || len(subset.Problems) != 0 {
		t.Fatalf("只检查这台服务器所选的应用：%+v", subset)
	}

	draft, complete := check("  new: {host: 127.0.0.1, runAs: app, vars: {secrets: /etc/demo/new.env, dataRoot: /srv/demo-new}}")
	if len(complete.MissingVars) != 0 || len(complete.Problems) != 0 {
		t.Fatalf("补齐后的检查结果：%+v", complete)
	}
	f, err := config.LoadContent([]byte(draft), path)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := engine.Spec(f, f.Targets["new"], f.App("api"))
	if err != nil || !strings.Contains(spec.UnitText, "EnvironmentFile=/etc/demo/new.env") {
		t.Fatalf("部署应使用新增服务器的 envFile：%v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != base {
		t.Fatalf("检查中不应写入草稿：%v", err)
	}

	// 编辑器也可以先添加服务器，再添加项目的第一个应用。
	empty, _, _ := strings.Cut(draft, "\napps:\n")
	out, code := runCLI(t, map[string]string{"FERRY_CONFIG_CONTENT": empty}, "validate", "-c", path, "--target", "new", "--from-env", "--json")
	if code != 0 {
		t.Fatalf("没有应用时也应能添加服务器：%s", out)
	}
}
