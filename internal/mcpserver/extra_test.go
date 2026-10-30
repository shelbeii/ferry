package mcpserver

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/settings"
)

// 打开防火墙、断开 SSH 连接：参数检查、交给桌面版的请求、桌面版那边再检查一遍
func TestExtraChangeTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	withTestKey(t)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	file := filepath.Join(t.TempDir(), "approver.json")
	oldFile := approverFile
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile = oldFile })

	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), shopYAML)
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}}
	st.MCP.AllowChanges = true
	settings.Save(st)
	cs := connect(t, t.TempDir())
	expectErr := func(tool string, args map[string]any, want string) {
		t.Helper()
		if text, isErr := callText(t, cs, tool, args); !isErr || !strings.Contains(text, want) {
			t.Fatalf("%s 应该报「%s」：%v %s", tool, want, isErr, text)
		}
	}
	expectErr("firewall_enable", map[string]any{"target": "test", "allow": []string{"70000"}, "reason": "x"}, "端口不对")
	expectErr("firewall_enable", map[string]any{"target": "prod", "reason": "x"}, "生产服务器")
	expectErr("ssh_disconnect", map[string]any{"target": "test", "pids": []int{}, "reason": "x"}, "pids")
	expectErr("software_logs", map[string]any{"target": "test", "name": "mysql", "unit": "x.service"}, "写一个")
	expectErr("software_logs", map[string]any{"target": "test", "unit": "x; rm -rf /"}, "只能是名称")
	expectErr("port_check", map[string]any{"target": "test", "ports": []int{70000}}, "端口不对")

	// 先看服务器的现状：连不上时不去问用户
	expectErr("firewall_enable", map[string]any{"target": "test", "allow": []string{"80"}, "reason": "x"}, "连接")
	expectErr("ssh_disconnect", map[string]any{"target": "test", "pids": []int{1234}, "reason": "x"}, "连接")
	if got := opTitle(approve.Request{Action: "ssh-kick", Target: "test", Pids: []int{12, 34}}); got != "断开 SSH 连接 PID 12、34（test）" {
		t.Errorf("标题：%s", got)
	}
	if got := opTitle(approve.Request{Action: "fw-enable", Target: "test"}); got != "打开防火墙（test）" {
		t.Errorf("标题：%s", got)
	}

	// 桌面版那边的检查（防止有人绕过 ferry mcp 直接发请求）
	for _, c := range []struct {
		r    approve.Request
		want string
	}{
		{approve.Request{Action: "ssh-kick", Config: shop, Target: "test"}, "没有写明"},
		{approve.Request{Action: "ssh-kick", Config: shop, Target: "test", Pids: []int{1}}, "PID 不对"},
		{approve.Request{Action: "fw-enable", Config: shop, Target: "test", Allow: []string{"80"}}, "端口不对"},
		{approve.Request{Action: "fw-enable", Config: shop, Target: "prod"}, "生产服务器"},
		{approve.Request{Action: "ssh-kick", Config: shop, Target: "test", Pids: []int{1234}}, ""},
	} {
		err := PrepareRequest(&c.r)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v：%v，应为 %q", c.r, err, c.want)
		}
	}
}
