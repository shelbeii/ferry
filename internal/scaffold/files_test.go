package scaffold

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

func TestGenerateExtraFiles(t *testing.T) {
	dir := t.TempDir()
	spec := Spec{ProjectDir: dir, Project: "shop",
		Targets: []TargetSpec{{Name: "prod", Host: "10.0.0.5", User: "root", Root: "/data/shop", RunAs: "shop"}},
		Apps: []AppSpec{{Name: "api", Type: "binary", Command: "go build -o build/api .", Artifact: "build/api", Health: "none",
			Files: []FileSpec{{From: "config.yaml"}, {From: "deploy/${target}/app.yml", To: "conf/app.yml"}, {From: "start.sh", Exec: true}, {From: " "}}}}}
	path := filepath.Join(dir, "ferry.yaml")
	text, err := Generate(spec, path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := config.LoadContent([]byte(text), path)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	var got []string
	for _, x := range f.App("api").Artifact.Files {
		got = append(got, x.From+"→"+x.To)
		if x.From == "start.sh" && !x.Exec {
			t.Errorf("start.sh 应可执行：\n%s", text)
		}
	}
	if strings.Join(got, "|") != "config.yaml→config.yaml|deploy/${target}/app.yml→conf/app.yml|start.sh→start.sh" {
		t.Fatalf("%s\n%s", strings.Join(got, "|"), text)
	}
}

func TestDetectConfigCandidates(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/api\n\ngo 1.22\n")
	write(t, dir, "main.go", "package main\nfunc main() {}\n")
	write(t, dir, "config.yaml", "port: 8080\n")
	write(t, dir, ".env", "A=1\n")
	write(t, dir, "conf/x.toml", "")
	write(t, dir, "docker-compose.yml", "")
	write(t, dir, ".golangci.yml", "")
	write(t, dir, "renovate.json", "{}")
	d, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	var api *Suggestion
	for i := range d.Apps {
		if d.Apps[i].Type == "binary" {
			api = &d.Apps[i]
		}
	}
	if api == nil {
		t.Fatalf("应识别出 Go 程序：%+v", d.Apps)
	}
	if got := strings.Join(api.ConfigFiles, ","); got != ".env,conf,config.yaml" {
		t.Fatalf("候选：%s", got)
	}
}

// 现成的产物不在项目目录里：向导选了完整路径（正斜杠），原样写进配置
func TestGenerateOutsideArtifact(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.ToSlash(filepath.Join(t.TempDir(), "release", "app.jar"))
	spec := Spec{ProjectDir: dir, Project: "shop",
		Targets: []TargetSpec{{Name: "prod", Host: "10.0.0.5", User: "root", Root: "/data/shop", RunAs: "shop"}},
		Apps:    []AppSpec{{Name: "api", Type: "java", Artifact: jar, Health: "none"}}}
	path := filepath.Join(dir, "ferry.yaml")
	text, err := Generate(spec, path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := config.LoadContent([]byte(text), path)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	a := f.App("api")
	if a.Artifact.Path != jar || a.Build != nil {
		t.Fatalf("产物 %q，构建 %v：\n%s", a.Artifact.Path, a.Build, text)
	}
	if got := f.LocalPath(a.Artifact.Path); got != filepath.Clean(filepath.FromSlash(jar)) {
		t.Fatalf("本机路径 %s", got)
	}
}
