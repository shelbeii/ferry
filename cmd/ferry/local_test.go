package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
)

func TestInspectTargetRuntimeDirectory(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	const targets = "targets:\n  standard: {host: standard.example.test}\n  custom: {host: custom.example.test, ferryDir: /srv/ferry-components/}\n"
	for _, tc := range []struct{ name, yaml string }{
		{"project", "version: 1\nproject: demo\n" + targets + "apps:\n  - {name: api, type: java, artifact: {path: api.jar}, service: {user: app}}\n"},
		{"connection", "version: 1\nkind: connections\n" + targets},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "ferry.yaml")
			if err := os.WriteFile(configPath, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			out, code := runCLI(t, nil, "inspect", "-c", configPath, "--json")
			var result struct {
				Data struct {
					Targets []struct{ Name, RuntimeDir string }
				}
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil || code != 0 {
				t.Fatalf("inspect code %d, error %v, output %s", code, err, out)
			}
			want := map[string]string{"standard": "/usr/local/lib/ferry/runtimes", "custom": "/srv/ferry-components/runtimes"}
			for _, row := range result.Data.Targets {
				if row.RuntimeDir != want[row.Name] {
					t.Errorf("%s runtimeDir = %q, want %q", row.Name, row.RuntimeDir, want[row.Name])
				}
				delete(want, row.Name)
			}
			if len(want) != 0 {
				t.Fatalf("missing target directories: %v", want)
			}
		})
	}
}

// ferry.yaml 里没有项目变量时，本机设置给界面的列表是 []，不是 null（界面直接遍历）
func TestLocalShowEmptyLists(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	defer appdir.Reset()
	p := filepath.Join(t.TempDir(), "ferry.yaml")
	os.WriteFile(p, []byte("version: 1\nproject: demo\ntargets:\n  a: {host: 10.0.0.1, runAs: app, vars: {secrets: /x.env}}\napps:\n  - {name: api, type: java, artifact: {path: api.jar}}\n"), 0o644)
	f, err := config.LoadWithLocal(p)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := localShow(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rep)
	s := string(b)
	if strings.Contains(s, `"vars":null`) || strings.Contains(s, `"targets":null`) || strings.Contains(s, `"problems":null`) ||
		!strings.Contains(s, `"vars":[]`) || !strings.Contains(s, `"name":"secrets"`) {
		t.Fatalf("%s", s)
	}
}

func TestTargetDisplayNameKeepsLocalSettings(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	dir := t.TempDir()
	path := filepath.Join(dir, "ferry.yaml")
	key := filepath.Join(dir, "id_here")
	yaml := `version: 1
project: demo
targets:
  staging:
    host: staging.example.test
    runAs: app
    vars: {jdk: /opt/jdk}
  other:
    host: other.example.test
    runAs: app
apps:
  - {name: api, type: java, artifact: {path: api.jar}}
`
	for file, content := range map[string]string{path: yaml, key: "test key"} {
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	l := &config.Local{Targets: map[string]*config.LocalTarget{
		"staging": {Auth: "key", IdentityFile: key, Vars: map[string]string{"jdk": "/srv/jdk"}},
		"other":   {Auth: "password"},
	}}
	if err := config.WriteLocal(f, l); err != nil {
		t.Fatal(err)
	}
	localPath := config.LocalFile(f.Project, f.Path)
	before, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}

	renamed := strings.Replace(yaml, "  staging:\n", "  staging:\n    title: 测试服务器\n", 1)
	if err := os.WriteFile(path, []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err = config.LoadWithLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	target := f.Targets["staging"]
	if target == nil || target.Title != "测试服务器" || target.IdentityFile != key || f.KeyMissing(target) != "" || target.Vars["jdk"] != "/srv/jdk" {
		t.Fatalf("改名后的服务器设置：%+v", target)
	}
	if other := f.Targets["other"]; other.Title != "" || other.Auth != "password" {
		t.Fatalf("另一台服务器的设置：%+v", other)
	}
	after, err := os.ReadFile(localPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("本机设置文件应保持原样：%v", err)
	}

	rep, err := localShow(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range []any{inspect(f)["targets"], rep.Targets} {
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct{ Name, Title, IdentityFile string }
		if err := json.Unmarshal(data, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[1].Name != "staging" || rows[1].Title != "测试服务器" || rows[1].IdentityFile != key {
			t.Fatalf("桌面端应读到新显示名和原私钥：%+v", rows)
		}
	}
}
