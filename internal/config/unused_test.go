package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写在用不到它的应用上的项：不报错，记成警告（整块用不到时只报那一块）。
func TestUnusedFieldWarnings(t *testing.T) {
	head := "version: 1\nproject: shop\ntargets:\n  t1: {host: 10.0.0.1, root: /data/shop, runAs: web}\n"
	f, err := load(t, head+`
apps:
  - name: base
    type: java
    artifact: {path: base.jar}
    service: &common {args: [--a]}
  - name: api
    type: java
    artifact: {path: api.jar}
    static: {port: "8081", proxy: {/api/: "http://127.0.0.1:8080/"}}
    service: {jvmArgs: [-Xmx1g]}
  - name: tool
    type: binary
    artifact: {path: tool, exclude: ["*.log"]}
    service: {jvmArgs: [-Xmx1g], args: [--b]}
  - name: py
    type: python
    artifact: {path: py}
    service:
      <<: *common
      exec: [python3, main.py]
  - name: web
    type: static
    artifact: {path: dist}
    static: {port: "8082"}
    service: {health: {status: [200]}}
`)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.WarningLines(), "\n")
	for _, want := range []string{
		"apps.api.static：java 应用用不到",
		"apps.tool.artifact.exclude：binary 应用用不到",
		"apps.tool.service.jvmArgs：binary 应用用不到这一项（只用于 java）",
		"apps.py.service.args：python 应用用不到", // <<: 合并进来的也算
		"apps.web.service.health：static 应用用不到",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("少了 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "static.port") || strings.Contains(got, "apps.api.service.jvmArgs") || strings.Contains(got, "apps.tool.service.args") {
		t.Errorf("整块用不到只报那一块；用得到的不报：\n%s", got)
	}
	if w := f.AppWarnings([]*App{f.App("tool")}); len(w) != 2 {
		t.Errorf("只看 tool：%v", w)
	}

	// 都用得到：没有警告
	f, err = load(t, head+"apps:\n  - name: api\n    type: java\n    artifact: {path: api.jar, exclude: []}\n    service: {jvmArgs: [-Xmx1g]}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Warnings) != 1 || f.Warnings[0].Path != "apps.api.artifact.exclude" {
		t.Errorf("写了空的也提示（写了不起作用）：%v", f.Warnings)
	}
	f, _ = load(t, head+"apps:\n  - name: api\n    type: java\n    artifact: {path: api.jar}\n    service: {jvmArgs: [-Xmx1g]}\n")
	if len(f.Warnings) != 0 {
		t.Errorf("没有用不到的：%v", f.Warnings)
	}
}

// 改配置：这次改出来的「用不到」放进 Warnings（能写），原来就有的不再报。
func TestEditPlanWarnings(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, DefaultFileName)
	os.WriteFile(p, []byte("version: 1\nproject: shop\ntargets:\n  t1: {host: 10.0.0.1, root: /data/shop, runAs: web}\napps:\n  - name: tool\n    type: binary\n    artifact: {path: tool}\n    service: {jvmArgs: [-Xmx1g]}\n"), 0o644)
	plan, err := PlanEdit(p, []Edit{{Op: "set", Path: "apps.tool.static.port", Value: []byte(`"8081"`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Problems) != 0 || len(plan.Warnings) != 1 || !strings.HasPrefix(plan.Warnings[0], "apps.tool.static：binary 应用用不到") {
		t.Fatalf("%v %v", plan.Problems, plan.Warnings)
	}
}
