package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const patchSrc = `# 商城的部署配置
version: 1
project: shop

vars:
  port: "8080"   # 后端端口

targets:
  test:
    host: 10.0.0.5
    user: deploy
    runAs: shop
  prod:
    host: 10.0.0.9
    production: true

apps:
  # 后端
  - name: api
    type: java
    build: mvn -q package
    artifact:
      path: target/api.jar
    service:
      jvmArgs: [-Xms256m, -Xmx512m]
      health: {tcp: "${port}", timeout: 60s}
      env:
        TZ: Asia/Shanghai

  - name: web
    type: static
    artifact: {path: dist}
    static:
      port: "80"
`

func edit(t *testing.T, src string, edits ...Edit) string {
	t.Helper()
	out, err := ApplyEdits([]byte(src), edits)
	if err != nil {
		t.Fatalf("%v: %v", edits, err)
	}
	return string(out)
}

func val(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// diffLines 两段文字不一样的行（- 原来的，+ 改后的），按行号对齐前后相同的部分。
func diffLines(a, b string) []string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	i := 0
	for i < len(x) && i < len(y) && x[i] == y[i] {
		i++
	}
	j, k := len(x), len(y)
	for j > i && k > i && x[j-1] == y[k-1] {
		j--
		k--
	}
	var out []string
	for _, l := range x[i:j] {
		out = append(out, "- "+l)
	}
	for _, l := range y[i:k] {
		out = append(out, "+ "+l)
	}
	return out
}

func TestApplyEdits(t *testing.T) {
	cases := []struct {
		name  string
		edits []Edit
		want  []string
	}{
		{"改一个值，保留行尾注释", []Edit{{Op: "set", Path: "vars.port", Value: val("9090")}},
			[]string{`-   port: "8080"   # 后端端口`, `+   port: "9090"   # 后端端口`}},
		{"列表里按名字找", []Edit{{Op: "set", Path: "apps.api.type", Value: val("binary")}},
			[]string{"-     type: java", "+     type: binary"}},
		{"按序号找", []Edit{{Op: "set", Path: "apps.1.artifact.path", Value: val("build")}},
			[]string{"-     artifact: {path: dist}", "+     artifact: {path: build}"}},
		{"新加一个键", []Edit{{Op: "set", Path: "targets.test.port", Value: val(2222)}},
			[]string{"+     port: 2222"}},
		{"新加好几级", []Edit{{Op: "set", Path: "apps.api.service.restart.when", Value: val("always")}},
			[]string{"+       restart:", "+         when: always"}},
		{"写成一行的对象里改", []Edit{{Op: "set", Path: "apps.api.service.health.timeout", Value: val("90s")}},
			[]string{`-       health: {tcp: "${port}", timeout: 60s}`, `+       health: {tcp: "${port}", timeout: 90s}`}},
		{"一行的列表里加", []Edit{{Op: "append", Path: "apps.api.service.jvmArgs", Value: val("-Dfile.encoding=UTF-8")}},
			[]string{"-       jvmArgs: [-Xms256m, -Xmx512m]", "+       jvmArgs: [-Xms256m, -Xmx512m, -Dfile.encoding=UTF-8]"}},
		{"删掉一个键", []Edit{{Op: "delete", Path: "targets.test.runAs"}},
			[]string{"-     runAs: shop"}},
		{"删掉对象里唯一的键", []Edit{{Op: "delete", Path: "apps.api.service.env.TZ"}},
			[]string{"-       env:", "-         TZ: Asia/Shanghai", "+       env: {}"}},
		{"键名里有点", []Edit{{Op: "set", Path: "apps.api.service.env[spring.profiles.active]", Value: val("test")}},
			[]string{"+         spring.profiles.active: test"}},
		{"整个对象换成新的", []Edit{{Op: "set", Path: "apps.web.static", Value: json.RawMessage(`{"port":"8081","spa":true,"proxy":{"/api/":"http://127.0.0.1:8080/"}}`)}},
			[]string{`-       port: "80"`, `+       port: "8081"`, "+       spa: true", "+       proxy:", "+         /api/: http://127.0.0.1:8080/"}},
		{"加一个应用", []Edit{{Op: "append", Path: "apps", Value: json.RawMessage(`{"name":"job","type":"binary","artifact":{"path":"bin/job"},"service":{"args":["--once"]}}`)}},
			[]string{"+   - name: job", "+     type: binary", "+     artifact:", "+       path: bin/job", "+     service:", "+       args: [--once]"}},
		{"删掉一个应用（连同上面的注释）", []Edit{{Op: "delete", Path: "apps.api"}},
			nil}, // 下面单独检查
		{"删掉列表项的第一个键", []Edit{{Op: "delete", Path: "apps.web.name"}},
			[]string{"-   - name: web", "-     type: static", "+   - type: static"}},
		{"顶层新加", []Edit{{Op: "set", Path: "git.requireClean", Value: val(true)}},
			[]string{"+ git:", "+   requireClean: true", "+ "}},
		{"多行文字", []Edit{{Op: "set", Path: "apps.api.hooks.beforeSwitch", Value: json.RawMessage(`[{"run":"echo a\necho b","timeout":"2m"}]`)}},
			[]string{"+     hooks:", "+       beforeSwitch:", "+         - run: |-", "+             echo a", "+             echo b", "+           timeout: 2m"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := edit(t, patchSrc, c.edits...)
			if c.want == nil {
				return
			}
			if got := diffLines(patchSrc, out); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("差异：\n%s\n要的：\n%s\n全文：\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"), out)
			}
		})
	}
}

func TestApplyEditsDeleteApp(t *testing.T) {
	out := edit(t, patchSrc, Edit{Op: "delete", Path: "apps.api"})
	if strings.Contains(out, "  # 后端\n") || strings.Contains(out, "name: api") || strings.Contains(out, "TZ") {
		t.Fatalf("没删干净：\n%s", out)
	}
	if !strings.Contains(out, "apps:\n  - name: web\n") {
		t.Fatalf("留下的不对：\n%s", out)
	}
}

func TestApplyEditsKeepsCRLF(t *testing.T) {
	src := strings.ReplaceAll(patchSrc, "\n", "\r\n")
	out := edit(t, src, Edit{Op: "set", Path: "targets.test.user", Value: val("root")})
	if strings.Count(out, "\r\n") != strings.Count(src, "\r\n") || !strings.Contains(out, "    user: root\r\n") {
		t.Fatalf("换行变了：%q", out)
	}
}

func TestApplyEditsAlignedList(t *testing.T) {
	src := "version: 1\nproject: x\napps:\n- name: a\n  type: java\n- name: b\n  type: binary\ntargets:\n  t: {host: h}\n"
	out := edit(t, src, Edit{Op: "append", Path: "apps", Value: json.RawMessage(`{"name":"c","type":"static"}`)},
		Edit{Op: "set", Path: "apps.a.order", Value: val(2)})
	want := "version: 1\nproject: x\napps:\n- name: a\n  type: java\n  order: 2\n- name: b\n  type: binary\n- name: c\n  type: static\ntargets:\n  t: {host: h}\n"
	if out != want {
		t.Fatalf("得到：\n%s", out)
	}
	out = edit(t, src, Edit{Op: "set", Path: "targets.t.user", Value: val("root")})
	if !strings.Contains(out, "  t: {host: h, user: root}\n") {
		t.Fatalf("得到：\n%s", out)
	}
}

func TestApplyEditsEmptyFile(t *testing.T) {
	out := edit(t, "# 还没写\n", Edit{Op: "set", Path: "version", Value: val(1)}, Edit{Op: "set", Path: "project", Value: val("x")})
	if out != "# 还没写\n\nversion: 1\nproject: x\n" {
		t.Fatalf("得到：%q", out)
	}
}

func TestApplyEditsErrors(t *testing.T) {
	src := patchSrc + "\nbase: &b\n  user: deploy\n  sub: {x: 1}\nother:\n  <<: *b\n  host: x\nalias: *b\n"
	for _, c := range []struct {
		e    Edit
		want string
	}{
		{Edit{Op: "set", Path: "apps.nope.type", Value: val("java")}, "里没有 nope"},
		{Edit{Op: "delete", Path: "targets.dev"}, "没有 targets.dev"},
		{Edit{Op: "set", Path: "vars.port.x", Value: val("1")}, "是一个值"},
		{Edit{Op: "delete", Path: "base"}, "锚点"},
		{Edit{Op: "set", Path: "other.sub.x", Value: val(2)}, "合并"},
		{Edit{Op: "delete", Path: "other.host"}, "合并"},
		{Edit{Op: "set", Path: "alias.user", Value: val("root")}, "*b"},
		{Edit{Op: "append", Path: "targets", Value: val("x")}, "不是列表"},
		{Edit{Op: "set", Path: "vars.port", Value: json.RawMessage(`null`)}, "delete"},
		{Edit{Op: "move", Path: "vars"}, "op 只能是"},
		{Edit{Op: "set", Path: "a..b", Value: val(1)}, "空的一段"},
	} {
		_, err := ApplyEdits([]byte(src), []Edit{c.e})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v：%v，要有「%s」", c.e, err, c.want)
		}
	}
	// 锚点里面的、合并对象自己写的键：可以改
	out := edit(t, src, Edit{Op: "set", Path: "base.user", Value: val("root")}, Edit{Op: "set", Path: "other.host", Value: val("y")},
		Edit{Op: "set", Path: "other.user", Value: val("admin")})
	if !strings.Contains(out, "base: &b\n  user: root\n") || !strings.Contains(out, "  <<: *b\n  host: y\n  user: admin\n") {
		t.Fatalf("得到：\n%s", out)
	}
}

func TestParseEditPath(t *testing.T) {
	got, err := ParseEditPath("apps.api.service.env[a.b].x[0]")
	if err != nil || !reflect.DeepEqual(got, []string{"apps", "api", "service", "env", "a.b", "x", "0"}) {
		t.Fatalf("%v %v", got, err)
	}
}

// 各种写法改完都要和「对数据做同样的修改」一致（ApplyEdits 自己会核对，这里多跑几种组合）
func TestApplyEditsMany(t *testing.T) {
	paths := []string{"vars.port", "vars.new", "targets.test.host", "targets.test.vars.jdk", "targets.prod", "apps.api.service.jvmArgs",
		"apps.api.service.jvmArgs.0", "apps.api.service.health", "apps.api.service.health.tcp", "apps.web.artifact.path", "apps.web.static.port",
		"apps.api.service.env.TZ", "apps.api.artifact", "apps.0.build", "project", "backups"}
	values := []json.RawMessage{val("x"), val(3), val(true), val([]string{"a", "b"}), json.RawMessage(`{"k":"v","n":{"m":[1,2]}}`), json.RawMessage(`[{"name":"z","run":"x"}]`)}
	for _, p := range paths {
		for _, v := range values {
			for _, op := range []string{"set", "append"} {
				_, err := ApplyEdits([]byte(patchSrc), []Edit{{Op: op, Path: p, Value: v}})
				if err != nil && strings.Contains(err.Error(), "写法比较特别") {
					t.Errorf("%s %s %s：%v", op, p, v, err)
				}
			}
		}
		if _, err := ApplyEdits([]byte(patchSrc), []Edit{{Op: "delete", Path: p}}); err != nil && strings.Contains(err.Error(), "写法比较特别") {
			t.Errorf("delete %s：%v", p, err)
		}
	}
}

func TestPlanEdit(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	dir := t.TempDir()
	p := filepath.Join(dir, "ferry.yaml")
	src := "version: 1\nproject: shop\ntargets:\n  test: {host: 10.0.0.5, runAs: shop}\n  prod: {host: 10.0.0.9, runAs: shop, production: true}\napps:\n  - name: api\n    type: binary\n    artifact: {path: bin/api}\n"
	os.WriteFile(p, []byte(src), 0o644)

	plan, err := PlanEdit(p, []Edit{{Op: "set", Path: "apps.api.service.args", Value: val([]string{"--port", "9000"})}})
	if err != nil || len(plan.Problems) > 0 || plan.Danger != "" || !reflect.DeepEqual(plan.Changed, []string{"apps.api.service.args"}) {
		t.Fatalf("%+v %v", plan, err)
	}
	if err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "args: [--port, \"9000\"]") {
		t.Fatalf("写进去的：%s", b)
	}

	// 写之前文件被改过：不写
	plan, _ = PlanEdit(p, []Edit{{Op: "set", Path: "apps.api.order", Value: val(1)}})
	os.WriteFile(p, []byte(src), 0o644)
	if err := plan.Write(); err == nil || !strings.Contains(err.Error(), "被改过") {
		t.Fatalf("要拒绝：%v", err)
	}

	// 校验不通过：放在 Problems 里，不能写
	plan, err = PlanEdit(p, []Edit{{Op: "set", Path: "apps.api.typo", Value: val(1)}})
	if err != nil || len(plan.Problems) == 0 || plan.Write() == nil {
		t.Fatalf("%+v %v", plan, err)
	}

	// 会执行的命令：提醒
	plan, _ = PlanEdit(p, []Edit{{Op: "set", Path: "apps.api.hooks.beforeSwitch", Value: val([]string{"rm -rf /tmp/x"})}})
	if !strings.Contains(plan.Danger, "部署时在服务器上执行的命令") {
		t.Fatalf("要提醒：%+v", plan)
	}
	plan, _ = PlanEdit(p, []Edit{{Op: "append", Path: "apps", Value: json.RawMessage(`{"name":"job","type":"binary","artifact":{"path":"bin/job"},"build":"make job"}`)}})
	if !strings.Contains(plan.Danger, "构建命令") {
		t.Fatalf("新加的应用里的构建命令要提醒：%+v", plan)
	}
	plan, _ = PlanEdit(p, []Edit{{Op: "delete", Path: "targets.test"}})
	if !strings.Contains(plan.Danger, "删掉了服务器 test") {
		t.Fatalf("删掉服务器要提醒：%+v", plan)
	}

	// 生产服务器、production 这几项：不许改
	for _, e := range []Edit{{Op: "set", Path: "targets.prod.host", Value: val("1.2.3.4")}, {Op: "set", Path: "targets.test.production", Value: val(false)},
		{Op: "delete", Path: "targets.prod"}} {
		if _, err := PlanEdit(p, []Edit{e}); err == nil {
			t.Errorf("%v 要拒绝", e)
		}
	}
}

func TestPlanLocalEdit(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	dir := t.TempDir()
	p := filepath.Join(dir, "ferry.yaml")
	os.WriteFile(p, []byte("version: 1\nproject: shop\nvars: {src: /code}\ntargets:\n  test: {host: 10.0.0.5, runAs: shop, vars: {jdk: /opt/jdk}}\n  prod: {host: 10.0.0.9, runAs: shop, production: true}\napps:\n  - name: api\n    type: binary\n    artifact: {path: bin/api}\n"), 0o644)

	plan, err := PlanLocalEdit(p, []Edit{{Op: "set", Path: "vars.src", Value: val("D:/code")}, {Op: "set", Path: "targets.test.auth", Value: val("password")}})
	if err != nil || len(plan.Problems) > 0 || !strings.Contains(plan.Danger, "登录") {
		t.Fatalf("%+v %v", plan, err)
	}
	if err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	f, _ := Load(p)
	l, _ := ReadLocal(f.Project, f.Path)
	if l.Vars["src"] != "D:/code" || l.Targets["test"].Auth != "password" {
		t.Fatalf("%+v", l)
	}
	for _, e := range []Edit{{Op: "set", Path: "vars.nope", Value: val("x")}} {
		plan, err := PlanLocalEdit(p, []Edit{e})
		if err != nil || len(plan.Problems) == 0 {
			t.Errorf("%v 要有问题：%+v %v", e, plan, err)
		}
	}
	for _, e := range []Edit{{Op: "set", Path: "targets.test.host", Value: val("x")}, {Op: "set", Path: "targets.prod.auth", Value: val("key")},
		{Op: "set", Path: "vars.src", Value: val(1)}} {
		if _, err := PlanLocalEdit(p, []Edit{e}); err == nil {
			t.Errorf("%v 要拒绝", e)
		}
	}
}

func TestSchemaFields(t *testing.T) {
	want, err := os.ReadFile("testdata/fields.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(SchemaFieldPaths(), "\n") + "\n"
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Fatal("schema.json 和 fields.txt 不一致：改了 desktop/src/editor/schema.ts 后要重新生成 schema.json（见 schemaExport.test.ts）")
	}
	n, norm, err := SchemaLookup([]string{"apps", "api", "service", "health", "timeout"})
	if err != nil || n.Kind != "dur" || norm != "apps.[].service.health.timeout" {
		t.Fatalf("%+v %s %v", n, norm, err)
	}
	if _, _, err := SchemaLookup([]string{"apps", "api", "nope"}); err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatal(err)
	}
	if hits := SchemaSearch("jvm", 5); len(hits) == 0 || hits[0].Path != "apps.[].service.jvmArgs" {
		t.Fatalf("%+v", hits)
	}
}

// 对象的键按表单里的顺序写，不管给的时候是什么顺序
func TestApplyEditsSchemaOrder(t *testing.T) {
	out := edit(t, patchSrc, Edit{Op: "set", Path: "apps.web.service.restart", Value: json.RawMessage(`{"within":"10m","max":3,"when":"always"}`)},
		Edit{Op: "append", Path: "apps", Value: json.RawMessage(`{"artifact":{"path":"bin/x"},"type":"binary","name":"x"}`)})
	if !strings.Contains(out, "restart:\n        when: always\n        max: 3\n        within: 10m\n") || !strings.Contains(out, "  - name: x\n    type: binary\n    artifact:\n") {
		t.Fatalf("得到：\n%s", out)
	}
}
