package config

import (
	"os"
	"strings"
	"testing"
)

const keepBase = `
version: 1
project: demo
targets:
  prod: {host: 10.0.0.1, runAs: demo}
apps:
  - name: web
    type: static
    artifact:
      path: dist   # 前端构建输出
      keep:
`

func TestKeep(t *testing.T) {
	f, err := load(t, keepBase+`        - ./config.js
        - {path: uploads/, shared: true}
        - "conf/*.json"
`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range f.App("web").Artifact.Keep {
		s := k.Path
		if k.Shared {
			s += "(共用)"
		}
		got = append(got, s)
	}
	if strings.Join(got, "|") != "config.js|uploads(共用)|conf/*.json" {
		t.Fatalf("保留：%s", strings.Join(got, "|"))
	}
}

func TestKeepErrors(t *testing.T) {
	for _, c := range []struct{ keep, want string }{
		{`        - ../x`, "无效"},
		{`        - /etc/nginx`, "相对路径"},
		{`        - {path: "up*", shared: true}`, "通配符"},
		{`        - .ferry/x`, "Ferry 自己的目录"},
		{"        - conf\n        - conf/a.json", "重复"},
		{`        - {path: a, mode: 1}`, "未知字段"},
		{`        - ""`, "不能为空"},
	} {
		_, err := load(t, keepBase+c.keep+"\n")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应报「%s」，实际 %v", c.keep, c.want, err)
		}
	}
	// java：不能保留程序文件、logs，不能和附带文件重叠
	java := `
version: 1
project: demo
targets:
  prod: {host: 10.0.0.1, runAs: demo}
apps:
  - name: api
    type: java
    artifact:
      path: build/api.jar
      files: [conf/app.yml]
      keep: [%s]
`
	for _, c := range []struct{ keep, want string }{
		{"api.jar", "程序文件"},
		{"logs", "运行日志"},
		{"app.yml", "附带文件"},
	} {
		_, err := load(t, strings.Replace(java, "%s", c.keep, 1))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应报「%s」，实际 %v", c.keep, c.want, err)
		}
	}
}

func TestSetAppKeep(t *testing.T) {
	text := `version: 1
project: demo
targets:
  a: {host: h, runAs: demo}
apps:
  - name: web
    type: static
    artifact:
      path: dist   # 前端构建输出
    static: {port: 8080}
  - name: api
    type: binary
    artifact: {path: api}
`
	f, err := load(t, text)
	if err != nil {
		t.Fatal(err)
	}
	keep := []*KeepItem{{Path: "config.js"}, {Path: "uploads", Shared: true}, {Path: "conf/*.json"}}
	if err := SetAppKeep(f.Path, "web", keep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Path)
	if !strings.Contains(string(b), "      path: dist   # 前端构建输出\n      keep: [config.js, {path: uploads, shared: true}, 'conf/*.json']") {
		t.Fatalf("应加在 artifact 里：\n%s", b)
	}
	g, err := Load(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if k := g.App("web").Artifact.Keep; len(k) != 3 || !k[1].Shared || k[2].Path != "conf/*.json" {
		t.Fatalf("%+v", k)
	}
	// 再改：替换那一行
	if err := SetAppKeep(f.Path, "web", []*KeepItem{{Path: ".env"}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := Load(f.Path); len(g.App("web").Artifact.Keep) != 1 || g.App("web").Artifact.Keep[0].Path != ".env" {
		t.Fatal("应替换成 .env")
	}
	// 写成多行的也能替换
	b, _ = os.ReadFile(f.Path)
	multi := strings.Replace(string(b), "      keep: [.env]   # 部署时保留服务器上的（不用包里的）\n", "      keep:\n        - .env   # 环境变量\n        - {path: data, shared: true}\n", 1)
	os.WriteFile(f.Path, []byte(multi), 0o644)
	if err := SetAppKeep(f.Path, "web", []*KeepItem{{Path: "a.js"}}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(f.Path)
	if strings.Contains(string(b), ".env") || strings.Contains(string(b), "shared") || !strings.Contains(string(b), "keep: [a.js]") || !strings.Contains(string(b), "static: {port: 8080}") {
		t.Fatalf("多行的应整个换掉：\n%s", b)
	}
	// 空 = 去掉
	if err := SetAppKeep(f.Path, "web", nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(f.Path)
	if strings.Contains(string(b), "keep") {
		t.Fatalf("应去掉 keep：\n%s", b)
	}
	// artifact 写成一行的：改成分行写再加 keep
	if err := SetAppKeep(f.Path, "api", []*KeepItem{{Path: "config.js"}}); err != nil {
		t.Fatalf("一行的 artifact：%v", err)
	}
	b, _ = os.ReadFile(f.Path)
	if !strings.Contains(string(b), "    artifact:\n      path: api\n      keep: [config.js]") {
		t.Fatalf("一行的 artifact 应改成分行写：\n%s", b)
	}
}

func TestSetAppKeepFlowArtifact(t *testing.T) {
	text := "version: 1\r\nproject: demo\r\ntargets:\r\n  a: {host: h, runAs: demo}\r\napps:\r\n" +
		"  - name: web\r\n    type: static\r\n    artifact: {path: D:/IDEA-WorkSpace/web/dist, exclude: [a.map, '*.log']}   # 构建输出\r\n    static:\r\n      port: 8000\r\n      spa: true\r\n" +
		"  - name: site\r\n    type: static\r\n    static: {port: 8001}\r\n    artifact: {path: 'dist 2', keep: [old.js]}\r\n" +
		"  - artifact: {path: x}\r\n    name: lead\r\n    type: static\r\n    static: {port: 8002}\r\n"
	f, err := load(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAppKeep(f.Path, "web", []*KeepItem{{Path: "config.js"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Path)
	want := "    artifact:   # 构建输出\r\n      path: D:/IDEA-WorkSpace/web/dist\r\n      exclude: [a.map, '*.log']\r\n      keep: [config.js]   # 部署时保留服务器上的（不用包里的）\r\n    static:\r\n"
	if !strings.Contains(string(b), want) {
		t.Fatalf("应改成分行写并保留 CRLF、注释：\n%s", b)
	}
	g, err := Load(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	w := g.App("web")
	if w.Artifact.Path == "" || len(w.Artifact.Exclude) != 2 || len(w.Artifact.Keep) != 1 || w.Static.Port != "8000" {
		t.Fatalf("%+v", w.Artifact)
	}
	// 一行里已有 keep：替换
	if err := SetAppKeep(f.Path, "site", []*KeepItem{{Path: "new.js"}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := Load(f.Path); len(g.App("site").Artifact.Keep) != 1 || g.App("site").Artifact.Keep[0].Path != "new.js" || g.App("site").Artifact.Path != "dist 2" {
		t.Fatalf("%+v", g.App("site").Artifact)
	}
	// artifact 在「- 」同一行
	if err := SetAppKeep(f.Path, "lead", []*KeepItem{{Path: "c.js"}}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(f.Path)
	if !strings.Contains(string(b), "  - artifact:\r\n      path: x\r\n      keep: [c.js]") {
		t.Fatalf("「- artifact」：\n%s", b)
	}
	// 一行写法、没有 keep 时去掉：不动文件
	os.WriteFile(f.Path, []byte(text), 0o644)
	if err := SetAppKeep(f.Path, "web", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.Path); string(b) != text {
		t.Fatalf("不应改动：\n%s", b)
	}
}

func TestSetAppKeepAnchor(t *testing.T) {
	text := `version: 1
project: demo
targets:
  a: {host: h, runAs: demo}
apps:
  - name: web
    type: static
    artifact: &art {path: dist}
    static: {port: 8000}
`
	f, err := load(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAppKeep(f.Path, "web", []*KeepItem{{Path: "config.js"}}); err == nil || !strings.Contains(err.Error(), "手工") {
		t.Fatalf("锚点：%v", err)
	}
}
