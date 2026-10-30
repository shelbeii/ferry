package config

import (
	"strings"
	"testing"
)

const filesBase = `
version: 1
project: demo
targets:
  prod: {host: 10.0.0.1, runAs: demo}
apps:
  - name: api
    type: binary
    artifact:
      path: build/api
      files:
`

func TestExtraFiles(t *testing.T) {
	f, err := load(t, filesBase+`        - config.yaml
        - deploy/certs/
        - {from: "deploy/${target}/app.yml", to: conf/app.yml/}
        - {from: start.sh, exec: true}
`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range f.App("api").Artifact.Files {
		s := x.From + "→" + x.To
		if x.Exec {
			s += "(x)"
		}
		got = append(got, s)
	}
	want := "config.yaml→config.yaml|deploy/certs/→certs|deploy/${target}/app.yml→conf/app.yml|start.sh→start.sh(x)"
	if strings.Join(got, "|") != want {
		t.Fatalf("附带文件：%s", strings.Join(got, "|"))
	}
}

func TestExtraFilesErrors(t *testing.T) {
	for _, c := range []struct{ files, want string }{
		{`        - {from: a.yml, to: logs/a.yml}`, "运行日志的目录"},
		{`        - {from: a.yml, to: api}`, "和程序文件同名"},
		{`        - {from: a.yml, to: ../a.yml}`, "无效"},
		{`        - {from: a.yml, to: /etc/a.yml}`, "相对路径"},
		{`        - {from: a.yml, to: 'c\a.yml'}`, "不能用的字符"},
		{`        - "conf-${target}.yml"`, "请写 to"},
		{`        - {from: a.yml, to: "${x}.yml"}`, "不能用变量"},
		{"        - conf\n        - {from: b.yml, to: conf/b.yml}", "重复"},
		{`        - {from: a.yml, mode: 1}`, "未知字段"},
		{`        - {to: a.yml}`, "不能为空"},
	} {
		_, err := load(t, filesBase+c.files+"\n")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应报「%s」，实际 %v", c.files, c.want, err)
		}
	}
	_, err := load(t, `
version: 1
project: demo
targets:
  prod: {host: 10.0.0.1}
apps:
  - name: web
    type: static
    artifact: {path: dist, files: [a.yml]}
`)
	if err == nil || !strings.Contains(err.Error(), "只用于 java、binary") {
		t.Fatalf("静态站点不能有附带文件：%v", err)
	}
}
