package mcpserver

import (
	"context"
	"slices"
	"testing"

	"github.com/captain/ferry/internal/appdir"
)

func TestTools(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	tools := Tools("test")
	// 和编程软件看到的工具一样
	listed, err := connect(t, t.TempDir()).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	for _, tl := range listed.Tools {
		want = append(want, tl.Name)
	}
	byName := map[string]ToolInfo{}
	for _, tl := range tools {
		got = append(got, tl.Name)
		byName[tl.Name] = tl
	}
	if tools[0].Name != "list_projects" {
		t.Errorf("第一个应该是 list_projects：%s", tools[0].Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("工具：%v，应该是 %v", got, want)
	}

	if d := byName["deploy"]; !d.Change || !d.Server || d.Title == "" || d.Description == "" {
		t.Errorf("deploy：%+v", d)
	}
	if l := byName["list_projects"]; l.Change || l.Server || len(l.Params) != 0 {
		t.Errorf("list_projects：%+v", l)
	}
	// 参数按字段顺序；必填的、数组、嵌套的对象
	var names []string
	for _, p := range byName["deploy"].Params {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"project", "target", "apps", "skipBuild", "reason"}) {
		t.Errorf("deploy 的参数：%v", names)
	}
	if p := byName["deploy"].Params[2]; p.Type != "string[]" || !p.Required || p.Description == "" {
		t.Errorf("apps：%+v", p)
	}
	found := map[string]ParamInfo{}
	for _, p := range byName["create_project"].Params {
		found[p.Name] = p
	}
	if p := found["spec.targets[].host"]; p.Depth != 2 || !p.Required || p.Type != "string" {
		t.Errorf("spec.targets[].host：%+v", p)
	}
	if p := found["spec.apps[].files[].from"]; p.Depth != 3 {
		t.Errorf("spec.apps[].files[].from：%+v", p)
	}
	if p := found["spec.apps[].env"]; p.Type != "map" {
		t.Errorf("spec.apps[].env：%+v", p)
	}
}
