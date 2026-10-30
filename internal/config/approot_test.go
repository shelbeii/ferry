package config

import (
	"strings"
	"testing"
)

const appRootSample = `
version: 1
project: demo
targets:
  a: {host: 10.0.0.1, runAs: demo, vars: {dataRoot: /data/demo}}
  b: {host: 10.0.0.2, runAs: demo, root: /srv/demo, vars: {dataRoot: /home2/demo}}
apps:
  - {name: Job, type: java, artifact: {path: job.jar}, root: "${dataRoot}"}
  - {name: Sys, type: java, artifact: {path: sys.jar}}
`

func TestAppRoot(t *testing.T) {
	f, err := load(t, appRootSample)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ target, app, want string }{
		{"a", "Job", "/data/demo"},
		{"b", "Job", "/home2/demo"},
		{"a", "Sys", "/opt/demo"},
		{"b", "Sys", "/srv/demo"},
	}
	for _, c := range cases {
		got, err := f.AppRoot(f.Targets[c.target], f.App(c.app))
		if err != nil || got != c.want {
			t.Errorf("%s/%s：%q %v，期望 %q", c.target, c.app, got, err, c.want)
		}
	}
}

func TestAppRootInvalid(t *testing.T) {
	for root, want := range map[string]string{
		"/data":             "太浅",
		"data/demo":         "绝对路径",
		"/etc/demo":         "系统目录",
		"/opt/demo/apps/x":  "apps",
		"/opt/demo/.ferry":  ".ferry",
		"/opt":              "太浅",
		"${nope}":           "未定义",
		"/opt/demo/../evil": "..",
	} {
		_, err := load(t, strings.Replace(appRootSample, `"${dataRoot}"`, `"`+root+`"`, 1))
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "apps.Job.root") {
			t.Errorf("root %q：期望含 %q 的错误，得到 %v", root, want, err)
		}
	}
	// 目标的部署目录在应用部署目录的 apps 里
	_, err := load(t, strings.Replace(strings.Replace(appRootSample, `"${dataRoot}"`, `/srv/x`, 1), "root: /srv/demo", "root: /srv/x/apps/y", 1))
	if err == nil || !strings.Contains(err.Error(), "目标的部署目录") {
		t.Errorf("嵌套：%v", err)
	}
}
