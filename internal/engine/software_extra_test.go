package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/swpkg"
	"github.com/captain/ferry/internal/swtpl"
)

// builtinPlan 用默认参数渲染内置模板的一种装法，再合并附加配置。
func builtinPlan(t *testing.T, id string, variant int, extra map[string]map[string]string) (*proto.SoftwarePlan, error) {
	t.Helper()
	var tpl *swtpl.Template
	for _, x := range swtpl.Builtin() {
		if x.ID == id {
			tpl = x
		}
	}
	v := &tpl.Variants[variant]
	vars := swtpl.Vars{"name": id, "dir": "/opt/sw/" + id, "user": v.User, "host": "10.0.0.1", "version": "3.2.4",
		"java": "/j/bin/java", "package": "/x/pkg"}
	for _, p := range tpl.Params {
		vars[p.Key] = p.Default
		switch p.Type {
		case "password":
			vars[p.Key] = "Passw0rd-1"
		case "bool":
			vars[p.Key] = "false"
		}
	}
	sets, err := tpl.ResolveExtras(v, extra)
	if err != nil {
		return nil, err
	}
	if err := extrasWritten(v, sets, vars); err != nil {
		return nil, err
	}
	var pkg *swpkg.Meta
	if v.Package != nil {
		pkg = &swpkg.Meta{File: "pkg.tar.gz", SHA256: strings.Repeat("a", 64), Size: 1}
	}
	pl, err := renderPlan(tpl, v, vars, pkg)
	if err != nil {
		t.Fatal(err)
	}
	return pl, applyExtras(pl, sets, vars)
}

func planFiles(pl *proto.SoftwarePlan, path string) []string {
	var out []string
	for _, f := range pl.Files {
		if f.Path == path {
			out = append(out, f.Content)
		}
	}
	return out
}

func TestApplyExtrasBuiltin(t *testing.T) {
	// MySQL：安装时、就绪后改写的两份 my.cnf 都带上，只改 [mysqld]
	for _, id := range []string{"mysql", "mariadb"} {
		for vi := 0; vi < 2 && (id == "mariadb" || vi == 0); vi++ {
			pl, err := builtinPlan(t, id, vi, map[string]map[string]string{"": {"innodb_buffer_pool_size": "1G", "max_connections": "1500", "default-time-zone": "+08:00"}})
			if err != nil {
				t.Fatal(err)
			}
			files := planFiles(pl, "/opt/sw/"+id+"/my.cnf")
			if len(files) != 2 {
				t.Fatalf("%s：my.cnf 有 %d 份", id, len(files))
			}
			for _, c := range files {
				mysqld, client, _ := strings.Cut(c, "[client]")
				if !strings.Contains(mysqld, "innodb_buffer_pool_size=1G\n") || !strings.Contains(mysqld, "default-time-zone=+08:00\n") ||
					strings.Contains(client, "innodb") {
					t.Errorf("%s 的 my.cnf：\n%s", id, c)
				}
				if id == "mysql" && (!strings.Contains(c, "max_connections=1500\n") || strings.Contains(c, "max_connections=500")) {
					t.Errorf("max_connections 没替换：\n%s", c)
				}
			}
		}
	}

	// Redis：两种装法（dnf / apt）
	for vi := range 2 {
		pl, err := builtinPlan(t, "redis", vi, map[string]map[string]string{"conf": {"maxmemory": "2gb", "appendonly": "no"}})
		if err != nil {
			t.Fatal(err)
		}
		c := planFiles(pl, "/opt/sw/redis/redis.conf")[0]
		if !strings.Contains(c, "\nmaxmemory 2gb") || !strings.Contains(c, "\nappendonly no\n") || strings.Contains(c, "appendonly yes") {
			t.Errorf("redis.conf：\n%s", c)
		}
	}

	// Nacos：安装包里的文件，排在模板的 props 后面交给 agent 改
	pl, err := builtinPlan(t, "nacos", 0, map[string]map[string]string{"": {"db.num": "1", "spring.sql.init.platform": "mysql"}})
	if err != nil {
		t.Fatal(err)
	}
	last := pl.Props[len(pl.Props)-1]
	if last.File != "/opt/sw/nacos/nacos/conf/application.properties" || last.Sep != "=" || last.Set["db.num"] != "1" || len(last.Set) != 2 {
		t.Errorf("nacos 的 props：%+v", pl.Props)
	}

	// PostgreSQL：启动参数 -c，值有空格时加引号
	for vi := range 2 {
		pl, err := builtinPlan(t, "postgresql", vi, map[string]map[string]string{"": {"shared_buffers": "1GB", "timezone": "Asia/Shanghai", "log_line_prefix": "%m [%p] "}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(pl.Exec, ` -c "log_line_prefix=%m [%p]" -c shared_buffers=1GB -c timezone=Asia/Shanghai`) {
			t.Errorf("postgres 的启动命令：%s", pl.Exec)
		}
	}

	// Sentinel：Spring Boot 的 --名称=值，在 -jar 之后
	pl, err = builtinPlan(t, "sentinel", 0, map[string]map[string]string{"": {"server.servlet.session.timeout": "7200s"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(pl.Exec, "sentinel-dashboard.jar --server.servlet.session.timeout=7200s") {
		t.Errorf("sentinel 的启动命令：%s", pl.Exec)
	}

	// MinIO：环境变量
	pl, err = builtinPlan(t, "minio", 0, map[string]map[string]string{"": {"MINIO_SERVER_URL": "https://s3.example.com", "MINIO_SITE_REGION": "cn east"}})
	if err != nil {
		t.Fatal(err)
	}
	if pl.Env["MINIO_SERVER_URL"] != "https://s3.example.com" || pl.Env["MINIO_SITE_REGION"] != "cn east" || pl.Env["MINIO_ROOT_PASSWORD"] != "Passw0rd-1" {
		t.Errorf("minio 的环境变量：%v", pl.Env)
	}

	// ZLMediaKit：段.键，没有的段加上
	pl, err = builtinPlan(t, "zlmediakit", 0, map[string]map[string]string{"": {"hook.enable": "1", "hls.segDur": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	c := planFiles(pl, "/opt/sw/zlmediakit/config.ini")[0]
	if !strings.Contains(c, "[hook]\nenable=1\n") || !strings.HasSuffix(c, "\n[hls]\nsegDur=2\n") {
		t.Errorf("config.ini：\n%s", c)
	}
}

// 不填附加配置时，安装计划和以前一模一样。
func TestApplyExtrasNoop(t *testing.T) {
	for _, tpl := range swtpl.Builtin() {
		before, err := builtinPlan(t, tpl.ID, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := builtinPlan(t, tpl.ID, 0, map[string]map[string]string{})
		if before.Exec != after.Exec || len(before.Files) != len(after.Files) || len(before.Props) != len(after.Props) {
			t.Errorf("%s：空的附加配置改了安装计划", tpl.ID)
		}
		for i := range before.Files {
			// 随机串每次不同，只比较不含随机串的文件
			if !strings.Contains(before.Files[i].Content, "ferry-") && before.Files[i].Content != after.Files[i].Content {
				t.Errorf("%s：%s 变了", tpl.ID, before.Files[i].Path)
			}
		}
	}
}

func TestExtrasWritten(t *testing.T) {
	tpl, err := swtpl.Parse([]byte(`id: demo
name: Demo
params:
  - {key: cfg, label: 写配置, type: bool}
variants:
  - label: 包
    user: demo
    package: {format: binary, into: demo}
    files:
      - {path: "${dir}/demo.ini", if: cfg, content: "[main]\nmode=a\n"}
    service: {exec: "${dir}/demo", ready: {port: "80"}}
extras:
  - {id: main, label: 主配置, format: ini, file: "${dir}/demo.ini", section: main}
`))
	if err != nil {
		t.Fatal(err)
	}
	v := &tpl.Variants[0]
	sets, err := tpl.ResolveExtras(v, map[string]map[string]string{"": {"mode": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := extrasWritten(v, sets, swtpl.Vars{"cfg": "false"}); err == nil || !strings.Contains(err.Error(), "用不上") {
		t.Errorf("条件不满足时：%v", err)
	}
	if err := extrasWritten(v, sets, swtpl.Vars{"cfg": "true"}); err != nil {
		t.Error(err)
	}
}

// 预检：给出合并后的配置文件、启动命令，参数里的密码遮住。
func TestExtrasRendered(t *testing.T) {
	find := func(id string) *swtpl.Template {
		for _, x := range swtpl.Builtin() {
			if x.ID == id {
				return x
			}
		}
		return nil
	}
	redis := find("redis")
	v := &redis.Variants[0]
	vars := swtpl.Vars{"name": "redis", "dir": "/opt/sw/redis", "user": "redis", "host": "h", "port": "6379", "public": "false", "password": "Secret-pw-123"}
	sets, err := redis.ResolveExtras(v, map[string]map[string]string{"": {"maxmemory": "2gb"}})
	if err != nil {
		t.Fatal(err)
	}
	out := extrasRendered(redis, v, vars, sets, "", 0, nil)
	if len(out) != 1 || !strings.Contains(out[0].Content, "maxmemory 2gb") || !strings.Contains(out[0].Content, "requirepass ******") || strings.Contains(out[0].Content, "Secret-pw-123") {
		t.Errorf("redis 预检：%+v", out)
	}
	if out[0].File != "/opt/sw/redis/redis.conf" {
		t.Errorf("文件：%s", out[0].File)
	}

	pg := find("postgresql")
	v = &pg.Variants[0]
	vars = swtpl.Vars{"name": "pg", "dir": "/opt/sw/pg", "user": "postgres", "host": "h", "port": "5432", "public": "false", "account": "postgres", "password": "Secret-pw-123"}
	sets, _ = pg.ResolveExtras(v, map[string]map[string]string{"": {"shared_buffers": "1GB"}})
	out = extrasRendered(pg, v, vars, sets, "", 0, nil)
	if len(out) != 1 || !strings.HasSuffix(out[0].Exec, "-c shared_buffers=1GB") || out[0].Content != "" {
		t.Errorf("postgres 预检：%+v", out)
	}
}

func TestParseExtras(t *testing.T) {
	got, err := ParseExtras([]byte(`{"max_connections": 1000, "skip-name-resolve": "", "mysqld": {"sql_mode": "A,B", "big": 1e3, "on": true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got[""]["max_connections"] != "1000" || got[""]["skip-name-resolve"] != "" || got["mysqld"]["sql_mode"] != "A,B" || got["mysqld"]["big"] != "1e3" || got["mysqld"]["on"] != "true" {
		t.Errorf("%v", got)
	}
	for _, s := range []string{"", "null", "{}"} {
		if got, err := ParseExtras([]byte(s)); err != nil || got != nil {
			t.Errorf("%q：%v %v", s, got, err)
		}
	}
	for in, want := range map[string]string{`[1]`: "JSON 对象", `{"a": [1]}`: "文字或数字", `{"a": {"b": null}}`: "文字或数字"} {
		if _, err := ParseExtras([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s：%v", in, err)
		}
	}
}

// AI 助手、编程软件装软件：确认框里列出附加配置（像密码的遮住），写错的不打扰用户（执行时把错误交回模型）。
func TestAIInstallExtrasApproval(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	e := &Engine{}
	var tool *ai.Tool
	for _, x := range e.aiTools(nil, &config.Target{Name: "a", User: "root"}, AIConfirm, "c") {
		if x.Name == "software_install" {
			tool = &x
		}
	}
	if tool == nil {
		t.Fatal("没有 software_install")
	}
	if _, ok := tool.Schema["properties"].(map[string]any)["extra"]; !ok {
		t.Error("参数里没有 extra")
	}
	ap := tool.Approve(context.Background(), json.RawMessage(`{"template":"mysql","extra":{"innodb_buffer_pool_size":"1G","max_connections":900}}`))
	if ap == nil || !strings.Contains(ap.Detail, "附加配置「MySQL 配置（my.cnf 的 [mysqld]）」") ||
		!strings.Contains(ap.Detail, "[mysqld] innodb_buffer_pool_size=1G") || !strings.Contains(ap.Detail, "[mysqld] max_connections=900") {
		t.Errorf("确认内容：%+v", ap)
	}
	ap = tool.Approve(context.Background(), json.RawMessage(`{"template":"nacos","extra":{"db.password.0":"plain-pw","db.user.0":"{{secret:dbuser}}"}}`))
	if ap == nil || strings.Contains(ap.Detail, "plain-pw") || !strings.Contains(ap.Detail, "db.password.0=******") || !strings.Contains(ap.Detail, "db.user.0={{secret:dbuser}}") {
		t.Errorf("密码要遮住：%+v", ap)
	}
	for _, bad := range []string{`{"template":"mysql","extra":{"port":"1"}}`, `{"template":"mysql","extra":[1]}`, `{"template":"nginx","extra":{"a":"1"}}`} {
		if ap := tool.Approve(context.Background(), json.RawMessage(bad)); ap != nil {
			t.Errorf("%s 应该交回模型，不弹确认：%+v", bad, ap)
		}
	}
}

// 模板库交给模型的内容不能超过结果的长度上限（超过时中间被省略，模型就看不到部分模板），给自己的模板留出余量。
func TestAITemplatesFitResultLimit(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	e := &Engine{}
	for _, x := range e.aiTools(nil, &config.Target{Name: "a", User: "root"}, AIReadOnly, "c") {
		if x.Name != "software_templates" {
			continue
		}
		out, err := x.Run(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > ai.ResultLimit*3/4 {
			t.Errorf("模板库 %d 字节，超过上限 %d 的四分之三", len(out), ai.ResultLimit)
		}
		for _, tp := range swtpl.Builtin() {
			if !strings.Contains(out, `"id":"`+tp.ID+`"`) {
				t.Errorf("模板库里没有 %s", tp.ID)
			}
		}
		if !strings.Contains(out, `"suggest":["innodb_buffer_pool_size=1G"`) || !strings.Contains(out, `"where":"my.cnf [mysqld]"`) {
			t.Errorf("附加配置：%s", out)
		}
		return
	}
	t.Fatal("没有 software_templates")
}
