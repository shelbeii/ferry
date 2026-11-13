package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJobConfig(t *testing.T) {
	head := "version: 1\nproject: p\ntargets:\n  a: {host: 1.2.3.4, runAs: demo}\napps:\n"
	load := func(app string) (*File, error) {
		p := filepath.Join(t.TempDir(), "ferry.yaml")
		os.WriteFile(p, []byte(head+app), 0o644)
		return Load(p)
	}
	f, err := load("  - name: report\n    type: java\n    artifact: {path: report.jar}\n    job:\n      schedule: [daily, \"Mon..Fri 09:00\"]\n      timeout: 30m\n")
	if err != nil {
		t.Fatal(err)
	}
	j := f.App("report").Job
	if j == nil || len(j.Schedule) != 2 || j.TimeoutOr() != 30*time.Minute || !j.CatchUpOn() {
		t.Fatalf("%+v", j)
	}
	// 一个时间也可以直接写字符串
	if f, err := load("  - name: report\n    type: binary\n    artifact: {path: report}\n    job: {schedule: hourly}\n"); err != nil || f.App("report").Job.Schedule[0] != "hourly" {
		t.Fatal(err)
	}
	for what, c := range map[string]string{
		"没写 schedule":    "    job: {timeout: 1h}\n",
		"schedule 有怪字符":  "    job: {schedule: \"daily; rm -rf /\"}\n",
		"timeout 太长":     "    job: {schedule: daily, timeout: 200h}\n",
		"和 health 一起":    "    job: {schedule: daily}\n    service: {health: {tcp: \"8080\"}}\n",
		"和 restart 一起":   "    job: {schedule: daily}\n    service: {restart: {when: always}}\n",
		"和 autostart 一起": "    job: {schedule: daily}\n    service: {autostart: false}\n",
		"和不停机发布一起":       "    job: {schedule: daily}\n    service: {zeroDowntime: {ports: [8081, 8082]}}\n",
	} {
		_, err := load("  - name: report\n    type: java\n    artifact: {path: report.jar}\n" + c)
		if err == nil || !strings.Contains(err.Error(), "job") && !strings.Contains(err.Error(), "定时任务") {
			t.Errorf("%s：应报错：%v", what, err)
		}
	}
	if _, err := load("  - name: web\n    type: static\n    artifact: {path: dist}\n    job: {schedule: daily}\n"); err == nil || !strings.Contains(err.Error(), "定时任务") {
		t.Errorf("静态站点不能是定时任务：%v", err)
	}
}
