package mcpserver

import (
	"path/filepath"
	"regexp"
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

const jobYAML = shopYAML + `  - name: report
    type: java
    artifact: {path: target/report.jar}
    job: {schedule: daily}
backups:
  - name: db
    mysql: {database: shop}
    envFile: /etc/shop/backup.env
    upload:
      s3: {endpoint: 'https://s3.example.com', bucket: shop-bk}
  - name: files
    paths: [/opt/shop/apps/api/data]
`

// 现在运行定时任务、取回异地的备份：参数检查、交给桌面版的请求、桌面版那边再检查一遍
func TestRunJobAndFetch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	withTestKey(t)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	file := filepath.Join(t.TempDir(), "approver.json")
	oldFile := approverFile
	approverFile = func() string { return file }
	t.Cleanup(func() { approverFile = oldFile })

	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), jobYAML)
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
	expectErr("run_job", map[string]any{"project": "商城", "target": "test", "app": "api", "reason": "x"}, "不是定时任务")
	expectErr("run_job", map[string]any{"project": "商城", "target": "test", "app": "nope", "reason": "x"}, "没有应用")
	expectErr("run_job", map[string]any{"project": "商城", "target": "prod", "app": "report", "reason": "x"}, "生产服务器")
	expectErr("backup_fetch", map[string]any{"project": "商城", "target": "test", "name": "files", "file": "files-1.tar.gz", "reason": "x"}, "没有设置传到异地")
	expectErr("backup_fetch", map[string]any{"project": "商城", "target": "test", "name": "db", "file": "../db-1.sql.gz", "reason": "x"}, "不是 db 的备份文件")
	expectErr("backup_fetch", map[string]any{"project": "商城", "target": "test", "name": "db", "file": "files-1.tar.gz", "reason": "x"}, "不是 db 的备份文件")
	// 参数没问题：交给桌面版（这里桌面版没打开）
	expectErr("run_job", map[string]any{"project": "商城", "target": "test", "app": "report", "reason": "x"}, "桌面版")
	expectErr("backup_fetch", map[string]any{"project": "商城", "target": "test", "name": "db", "file": "db-20261112-023000.sql.gz", "reason": "x"}, "桌面版")
	// 异地的列表：只有设了 upload 的；参数没问题时连服务器（这里连不上）
	expectErr("backup_status", map[string]any{"project": "商城", "target": "test", "remote": true}, "连接")

	text, _ := callText(t, cs, "list_projects", nil)
	if !regexp.MustCompile(`"schedule":\s*\[\s*"daily"`).MatchString(text) {
		t.Errorf("list_projects 要带上定时任务的时间：%s", text)
	}
	if got := opTitle(approve.Request{Action: "run-job", Target: "test", Input: []byte(`{"app":"report"}`)}); got != "现在运行定时任务 report（test）" {
		t.Errorf("标题：%s", got)
	}
	if got := opTitle(approve.Request{Action: "backup-fetch", Target: "test", Input: []byte(`{"name":"db","file":"db-1.sql.gz"}`)}); got != "取回异地的备份 db-1.sql.gz（test）" {
		t.Errorf("标题：%s", got)
	}
	// 桌面版收到的请求：工具要对得上，参数不对的拒绝（检查在连服务器之前）
	for _, c := range []struct {
		r    approve.Request
		want string
	}{
		{approve.Request{Action: "run-job", Tool: "run_command", Config: shop, Target: "test", Input: []byte(`{"app":"report"}`)}, "对不上"},
		{approve.Request{Action: "run-job", Tool: "run_job", Config: shop, Target: "prod", Input: []byte(`{"app":"report"}`)}, "生产服务器"},
	} {
		err := PrepareRequest(&c.r)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v：%v，应为 %q", c.r, err, c.want)
		}
	}
}
