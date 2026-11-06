package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
)

// 助手里的 config_schema、config_get、config_edit：确认框里是差异，同意后写入；只读模式没有 config_edit。
func TestAIConfigTools(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	p := filepath.Join(t.TempDir(), "ferry.yaml")
	src := "version: 1\nproject: shop\n\ntargets:\n  test:\n    host: 10.0.0.5\n    runAs: shop\n\napps:\n  # 后端\n  - name: api\n    type: binary\n    artifact: {path: bin/api}\n"
	os.WriteFile(p, []byte(src), 0o644)
	f, err := config.LoadWithLocal(p)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{File: f}
	tg := f.Targets["test"]
	find := func(change bool, name string) (*struct {
		run     func(string) (string, error)
		approve func(string) (string, string, bool)
	}, bool) {
		for _, x := range e.aiConfigTools(tg, change) {
			if x.Name == name {
				x := x
				return &struct {
					run     func(string) (string, error)
					approve func(string) (string, string, bool)
				}{
					run: func(in string) (string, error) { return x.Run(context.Background(), json.RawMessage(in)) },
					approve: func(in string) (string, string, bool) {
						if x.Approve == nil {
							return "", "", false
						}
						ap := x.Approve(context.Background(), json.RawMessage(in))
						if ap == nil {
							return "", "", false
						}
						return ap.Detail, ap.Danger, true
					},
				}, true
			}
		}
		return nil, false
	}
	if _, ok := find(false, "config_edit"); ok {
		t.Fatal("只读模式不该有 config_edit")
	}
	sc, _ := find(false, "config_schema")
	if out, err := sc.run(`{"path":"apps.[].service.restart"}`); err != nil || !strings.Contains(out, "什么时候重启") {
		t.Fatalf("%s %v", out, err)
	}
	get, _ := find(false, "config_get")
	if out, err := get.run(`{"path":"apps.api"}`); err != nil || !strings.Contains(out, "artifact: {path: bin/api}") {
		t.Fatalf("%s %v", out, err)
	}

	ed, _ := find(true, "config_edit")
	in := `{"edits":[{"op":"set","path":"apps.api.service.args","value":["--port","9000"]},{"op":"set","path":"apps.api.hooks.afterSwitch","value":["curl -fs localhost:9000/warm"]}]}`
	detail, danger, ok := ed.approve(in)
	if !ok || !strings.Contains(detail, "+     service:") || !strings.Contains(detail, "改到的：") || !strings.Contains(danger, "部署时在服务器上执行的命令") {
		t.Fatalf("确认：%v %q %q", ok, detail, danger)
	}
	out, err := ed.run(in)
	if err != nil || !strings.Contains(out, `"written": true`) {
		t.Fatalf("%s %v", out, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "  # 后端\n  - name: api\n") || !strings.Contains(string(b), "      args: [--port, \"9000\"]\n") {
		t.Fatalf("写进去的：\n%s", b)
	}
	// 校验不通过：不用确认，执行时告诉模型原因
	bad := `{"edits":[{"op":"set","path":"apps.api.typo","value":1}]}`
	if _, _, ok := ed.approve(bad); ok {
		t.Fatal("校验不通过不该让用户确认")
	}
	if _, err := ed.run(bad); err == nil || !strings.Contains(err.Error(), "校验不通过") {
		t.Fatal(err)
	}
}
