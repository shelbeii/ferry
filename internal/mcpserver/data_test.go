package mcpserver

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/settings"
)

const backupYAML = shopYAML + `backups:
  - name: db
    mysql: {database: shop}
    beforeDeploy: true
    targets: [test]
  - name: files
    paths: [/opt/shop/apps/api/data]
`

// 现在备份：参数检查、配置里没有的项、交给桌面版的请求、桌面版那边再检查一遍
func TestBackupRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	withTestKey(t)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	file := filepath.Join(t.TempDir(), "approver.json")
	oldFile := approverFile
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile = oldFile })

	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), backupYAML)
	plain := writeProject(t, filepath.Join(t.TempDir(), "blog"), strings.ReplaceAll(shopYAML, "project: shop", "project: blog"))
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}, {Path: plain, Name: "博客"}}
	st.MCP.AllowChanges = true
	settings.Save(st)
	cs := connect(t, t.TempDir())
	expectErr := func(tool string, args map[string]any, want string) {
		t.Helper()
		if text, isErr := callText(t, cs, tool, args); !isErr || !strings.Contains(text, want) {
			t.Fatalf("%s 应该报「%s」：%v %s", tool, want, isErr, text)
		}
	}
	expectErr("backup_run", map[string]any{"project": "商城", "target": "test", "name": "../x", "reason": "x"}, "名字不对")
	expectErr("backup_run", map[string]any{"project": "商城", "target": "prod", "reason": "x"}, "生产服务器")
	expectErr("backup_run", map[string]any{"project": "商城", "target": "test", "name": "logs", "reason": "x"}, "有：db、files")
	expectErr("backup_run", map[string]any{"project": "博客", "target": "test", "reason": "x"}, "没有在 test 上的 backups")
	expectErr("backup_status", map[string]any{"project": "商城", "target": "test", "files": true, "name": "A B"}, "名字不对")
	// 参数没问题：连服务器（这里连不上）
	expectErr("backup_status", map[string]any{"project": "商城", "target": "test"}, "连接")
	expectErr("monitor_status", map[string]any{"project": "商城", "target": "test", "check": true}, "连接")
	expectErr("deploy_changes", map[string]any{"project": "商城", "target": "test"}, "连接")

	if got := opTitle(approve.Request{Action: "backup-run", Target: "test"}); got != "现在备份 全部（test）" {
		t.Errorf("标题：%s", got)
	}
	if got := opTitle(approve.Request{Action: "backup-run", Target: "test", Backup: "db"}); got != "现在备份 db（test）" {
		t.Errorf("标题：%s", got)
	}
	for _, c := range []struct {
		r    approve.Request
		want string
	}{
		{approve.Request{Action: "backup-run", Config: shop, Target: "test", Backup: "x;y"}, "名字不对"},
		{approve.Request{Action: "backup-run", Config: shop, Target: "prod", Backup: "db"}, "生产服务器"},
		{approve.Request{Action: "backup-run", Config: plain, Target: "test"}, "没有在 test 上的 backups"},
		{approve.Request{Action: "backup-run", Config: shop, Target: "test", Backup: "db"}, ""},
		{approve.Request{Action: "backup-run", Config: shop, Target: "test"}, ""},
	} {
		err := PrepareRequest(&c.r)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v：%v，应为 %q", c.r, err, c.want)
		}
	}
}
