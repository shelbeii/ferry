package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

const jobsYAML = `version: 1
project: shop
targets:
  test:
    host: 127.0.0.1
    root: /opt/shop
    runAs: shop
apps:
  - name: api
    type: java
    artifact: {path: target/api.jar}
  - name: report
    type: java
    artifact: {path: target/report.jar}
    job: {schedule: ["*-*-* 02:30:00", hourly], timeout: 1h}
backups:
  - name: db
    mysql: {database: shop}
    envFile: /etc/shop/backup.env
    upload:
      s3: {endpoint: 'https://s3.example.com', bucket: shop-bk}
  - name: files
    paths: [/opt/shop/data]
`

// 助手的现在运行定时任务、取回异地的备份：确认框的内容，参数不对时不打扰用户
func TestAIJobTools(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ferry.yaml")
	os.WriteFile(p, []byte(jobsYAML), 0o644)
	f, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{File: f}
	tg, _ := f.Target("test")
	tools := map[string]func(string) string{}
	for _, tool := range e.aiTools(nil, tg, AIConfirm, "c") {
		tool := tool
		tools[tool.Name] = func(in string) string {
			ap := tool.Approve(context.Background(), json.RawMessage(in))
			if ap == nil {
				return ""
			}
			return ap.Title + " | " + ap.Detail
		}
	}
	run, fetch := tools["run_job"], tools["backup_fetch"]
	if run == nil || fetch == nil {
		t.Fatal("允许改动时要有 run_job、backup_fetch")
	}
	if got := run(`{"app":"report"}`); !strings.Contains(got, "现在运行定时任务 report") || !strings.Contains(got, "*-*-* 02:30:00、hourly") || !strings.Contains(got, "1h0m0s") {
		t.Errorf("run_job：%s", got)
	}
	if run(`{"app":"api"}`) != "" || run(`{"app":"x"}`) != "" {
		t.Error("不是定时任务的不用确认（执行时把错误交回模型）")
	}
	if got := fetch(`{"name":"db","file":"db-20261112-023000.sql.gz.age"}`); !strings.Contains(got, "db-20261112-023000.sql.gz.age 取回服务器") {
		t.Errorf("backup_fetch：%s", got)
	}
	for _, in := range []string{`{"name":"files","file":"files-1.tar.gz"}`, `{"name":"db","file":"../db-1"}`, `{"name":"db","file":"files-1"}`, `{"file":"db-1"}`} {
		if fetch(in) != "" {
			t.Errorf("%s 不对，不用确认", in)
		}
	}
	for _, tool := range e.aiTools(nil, tg, AIReadOnly, "c") {
		if tool.Name == "run_job" || tool.Name == "backup_fetch" {
			t.Errorf("只读模式不应有 %s", tool.Name)
		}
	}
	if _, err := e.RemoteBackups("test", "files"); err == nil || !strings.Contains(err.Error(), "没有设置传到异地") {
		t.Errorf("files 没设 upload，不用连服务器：%v", err)
	}
}
