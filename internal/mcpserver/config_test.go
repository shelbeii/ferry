package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/settings"
)

func TestConfigTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	file := filepath.Join(t.TempDir(), "approver.json")
	oldFile := approverFile
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile = oldFile })

	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), "# 商城\n"+shopYAML)
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}}
	settings.Save(st)
	cs := connect(t, t.TempDir())
	call := func(tool string, args map[string]any, want ...string) string {
		t.Helper()
		text, isErr := callText(t, cs, tool, args)
		if isErr {
			t.Fatalf("%s 出错：%s", tool, text)
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Fatalf("%s 的结果里要有「%s」：\n%s", tool, w, text)
			}
		}
		return text
	}
	expectErr := func(tool string, args map[string]any, want string) {
		t.Helper()
		if text, isErr := callText(t, cs, tool, args); !isErr || !strings.Contains(text, want) {
			t.Fatalf("%s 应该报「%s」：%v %s", tool, want, isErr, text)
		}
	}

	// 查说明
	call("config_schema", nil, "howTo", `"name": "apps"`, "builtinVars", "本机设置")
	call("config_schema", map[string]any{"path": "apps.[].service.health"}, `"label": "健康检查"`, `"name": "timeout"`, `"default": "180s"`)
	call("config_schema", map[string]any{"path": "targets.test"}, `"name": "host"`, `"required": "必填"`)
	call("config_schema", map[string]any{"query": "jvm"}, "apps.[].service.jvmArgs")
	expectErr("config_schema", map[string]any{"path": "apps.[].nope"}, "没有字段 nope")

	// 看现在的
	call("config_get", map[string]any{"project": "商城"}, "# 商城", "production: true")
	call("config_get", map[string]any{"path": "apps.api.artifact"}, "artifact: {path: target/api.jar}", `"path": "target/api.jar"`)
	call("config_get", map[string]any{"path": "apps.api.service.health"}, "没有写这一项")
	call("config_get", map[string]any{"scope": "local"}, `"canSet"`, "local.json")

	// 校验、试改
	call("config_check", nil, `"ok": true`)
	call("config_check", map[string]any{"content": "version: 1\nproject: shop\n"}, `"ok": false`)
	call("config_check", map[string]any{"edits": []map[string]any{{"op": "set", "path": "apps.api.service.jvmArgs", "value": []string{"-Xmx512m"}}}},
		`"ok": true`, `+       jvmArgs: [-Xmx512m]`)
	call("config_check", map[string]any{"edits": []map[string]any{{"op": "set", "path": "apps.api.typo", "value": 1}}}, `"ok": false`, "typo")

	jvm := map[string]any{"project": "商城", "edits": []map[string]any{{"op": "set", "path": "apps.api.service.jvmArgs", "value": []string{"-Xmx512m"}}}, "reason": "内存不够"}
	// 没有打开开关；校验不通过；生产服务器
	expectErr("config_edit", jvm, "允许编程软件")
	st.MCP.AllowChanges = true
	settings.Save(st)
	expectErr("config_edit", map[string]any{"edits": []map[string]any{{"op": "set", "path": "apps.api.typo", "value": 1}}, "reason": "x"}, "校验不通过")
	expectErr("config_edit", map[string]any{"edits": []map[string]any{{"op": "set", "path": "targets.prod.host", "value": "1.2.3.4"}}, "reason": "x"}, "生产服务器")
	expectErr("config_edit", map[string]any{"edits": []map[string]any{{"op": "delete", "path": "targets.test.production"}}, "reason": "x"}, "没有 targets.test.production")
	// 桌面版没打开
	expectErr("config_edit", jvm, "Ferry 桌面版")

	// 桌面版打开着：确认框里是差异，同意后按 Input（带着原文摘要）写入
	hub := approve.NewHub(file)
	hub.Prepare = PrepareRequest
	reqs := make(chan approve.Request, 4)
	unsub, err := hub.Subscribe(func(kind string, v any) {
		if kind == "request" {
			reqs <- v.(approve.Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	go func() {
		r := <-reqs
		var in engine.ConfigEditInput
		json.Unmarshal(r.Input, &in)
		if r.Action != "config-edit" || r.Config != shop || r.Reason != "内存不够" || in.Base == "" || len(r.Detail) != 2 ||
			!strings.Contains(r.Detail[1], "+       jvmArgs: [-Xmx512m]") || !strings.Contains(r.Detail[0], "改到的：apps.api.service.jvmArgs") {
			t.Errorf("请求：%+v %+v", r, in)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true})
		// 桌面版执行（ferry config-edit 做的事）
		plan, err := engine.PlanConfigEdit(r.Config, in)
		if err == nil && plan.Base == in.Base {
			err = plan.Write()
		}
		out := approve.Outcome{OK: err == nil}
		if err != nil {
			out.Error = err.Error()
		}
		hub.Done(r.ID, out)
	}()
	call("config_edit", jvm, `"state": "完成"`)
	b, _ := os.ReadFile(shop)
	if !strings.HasPrefix(string(b), "# 商城\n") || !strings.Contains(string(b), "    service:\n      jvmArgs: [-Xmx512m]\n") {
		t.Fatalf("写进去的：\n%s", b)
	}

	// 改本机设置
	go func() {
		r := <-reqs
		if !strings.Contains(r.Detail[0], "本机设置") || !strings.Contains(r.Danger, "登录") {
			t.Errorf("请求：%+v", r)
		}
		hub.Decide(r.ID, approve.Decision{Approve: true})
		var in engine.ConfigEditInput
		json.Unmarshal(r.Input, &in)
		plan, err := engine.PlanConfigEdit(r.Config, in)
		if err == nil {
			err = plan.Write()
		}
		hub.Done(r.ID, approve.Outcome{OK: err == nil})
	}()
	call("config_edit", map[string]any{"scope": "local", "edits": []map[string]any{{"op": "set", "path": "targets.test.auth", "value": "password"}}, "reason": "这台电脑没有私钥"},
		`"state": "完成"`)
	call("config_get", map[string]any{"scope": "local"}, `"auth": "password"`)
}
