package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
)

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
