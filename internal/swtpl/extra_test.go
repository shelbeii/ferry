package swtpl

import (
	"strings"
	"testing"
)

func builtin(t *testing.T, id string) *Template {
	t.Helper()
	for _, tp := range Builtin() {
		if tp.ID == id {
			return tp
		}
	}
	t.Fatalf("没有内置模板 %s", id)
	return nil
}

// 内置模板的每一种装法：建议的项都能填（没被锁住、键名对、值合格）。
func TestBuiltinExtraSuggestions(t *testing.T) {
	declared := 0
	for _, tp := range Builtin() {
		for i := range tp.Variants {
			v := &tp.Variants[i]
			for _, x := range tp.Extras {
				if !x.appliesTo(v) {
					t.Errorf("%s 的「%s」不能用附加配置 %s", tp.ID, v.Label, x.ID)
					continue
				}
				kv := map[string]string{}
				for _, s := range x.Suggest {
					val := s.Example
					if val == "" && x.Format == "keyvalue" {
						val = "1"
					}
					kv[s.Key] = val
				}
				sets, err := tp.ResolveExtras(v, map[string]map[string]string{x.ID: kv})
				if err != nil {
					t.Errorf("%s / %s：%v", tp.ID, v.Label, err)
					continue
				}
				if len(sets) != 1 || len(sets[0].Items) != len(kv) {
					t.Errorf("%s：附加配置 %s 只认出 %v", tp.ID, x.ID, sets)
				}
			}
		}
		if len(tp.Extras) > 0 {
			declared++
		}
	}
	if declared < 8 {
		t.Errorf("声明了附加配置的内置模板只有 %d 个", declared)
	}
}

func TestExtraLocked(t *testing.T) {
	cases := []struct {
		tpl, key, want string
	}{
		{"mysql", "port", "「端口」"},
		{"mysql", "bind_address", "「允许其他机器访问」"}, // - 和 _ 等价
		{"mysql", "Lower-Case-Table-Names", "「表名不区分大小写」"},
		{"mysql", "datadir", "安装目录"},
		{"mysql", "daemonize", "影响 Ferry"},
		{"redis", "PORT", "「端口」"},
		{"redis", "requirepass", "「密码」"},
		{"postgresql", "listen_addresses", "「允许其他机器访问」"},
		{"postgresql", "logging_collector", "影响 Ferry"},
		{"sentinel", "server.port", "「端口」"}, // -Dserver.port 和 --server.port 是同一项
		{"nacos", "nacos.server.main.port", "「主端口」"},
		{"minio", "MINIO_ROOT_PASSWORD", "「密码」"},
		{"zlmediakit", "HTTP.port", "「HTTP / API 端口」"},
		{"zlmediakit", "rtsp.sslport", "影响 Ferry"},
	}
	for _, c := range cases {
		tp := builtin(t, c.tpl)
		_, err := tp.ResolveExtras(&tp.Variants[0], map[string]map[string]string{"": {c.key: "1"}})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 的 %s：%v（要提到 %s）", c.tpl, c.key, err, c.want)
		}
	}
	// 没引用变量的项可以改（模板里写的默认值）
	tp := builtin(t, "mysql")
	if _, err := tp.ResolveExtras(&tp.Variants[0], map[string]map[string]string{"mysqld": {"max_connections": "2000", "character-set-server": "utf8mb4"}}); err != nil {
		t.Error(err)
	}
}

func TestResolveExtrasErrors(t *testing.T) {
	my := builtin(t, "mysql")
	v := &my.Variants[0]
	two := &Template{ID: "two", Name: "Two", Extras: []Extra{{ID: "a", Format: "env"}, {ID: "b", Format: "env"}}}
	plain := &Template{ID: "plain", Name: "Plain"}
	long := strings.Repeat("x", maxExtraValue+1)
	many := map[string]string{}
	for i := 0; i <= maxExtraItems; i++ {
		many["k"+strings.Repeat("a", i)] = "1"
	}
	for _, c := range []struct {
		name string
		tp   *Template
		in   map[string]map[string]string
		want string
	}{
		{"没有这个 id", my, map[string]map[string]string{"nope": {"a": "1"}}, "没有附加配置 nope"},
		{"几处时要写 id", two, map[string]map[string]string{"": {"A": "1"}}, "写明是哪一处"},
		{"模板没有附加配置", plain, map[string]map[string]string{"": {"a": "1"}}, "没有可以补充的配置项"},
		{"键名", my, map[string]map[string]string{"": {"a b": "1"}}, "键名"},
		{"段名开头", my, map[string]map[string]string{"": {"[client]": "1"}}, "键名"},
		{"换行", my, map[string]map[string]string{"": {"sql_mode": "a\ninit-file=/tmp/x"}}, "不能换行"},
		{"太长", my, map[string]map[string]string{"": {"sql_mode": long}}, "太长"},
		{"太多", my, map[string]map[string]string{"": many}, "最多"},
		{"同一项", my, map[string]map[string]string{"": {"max-connections": "1", "max_connections": "2"}}, "同一项"},
		{"写了两次", my, map[string]map[string]string{"": {"max_connections": "1"}, "mysqld": {"max_connections": "2"}}, "写了两次"},
		{"redis 要写值", builtin(t, "redis"), map[string]map[string]string{"": {"maxmemory": " "}}, "要写值"},
		{"zlm 要写段", builtin(t, "zlmediakit"), map[string]map[string]string{"": {"enable": "1"}}, "段.键"},
	} {
		vv := v
		if len(c.tp.Variants) > 0 {
			vv = &c.tp.Variants[0]
		}
		_, err := c.tp.ResolveExtras(vv, c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：%v（要提到 %s）", c.name, err, c.want)
		}
	}
	if sets, err := my.ResolveExtras(v, nil); err != nil || sets != nil {
		t.Errorf("没填附加配置：%v %v", sets, err)
	}
	if sets, err := plain.ResolveExtras(v, map[string]map[string]string{"": {}}); err != nil || sets != nil {
		t.Errorf("空的附加配置不算填了：%v %v", sets, err)
	}
}

func TestValidateExtras(t *testing.T) {
	base := `id: demo
name: Demo
params:
  - {key: port, label: 端口, type: port, default: "80"}
variants:
  - label: 包
    user: demo
    package: {format: binary, into: demo}
    files:
      - {path: "${dir}/demo.ini", content: "[main]\nport=${port}\nmode=a\n"}
    service: {exec: "${dir}/demo -c ${dir}/demo.ini", ready: {port: "${port}"}}
extras:
`
	ok := base + `  - {id: main, label: 主配置, format: ini, file: "${dir}/demo.ini", section: main, suggest: [{key: mode, example: b}]}
  - {id: env, label: 环境变量, format: env}
  - {id: args, label: 参数, format: args, prefix: "--"}
`
	tp, err := Parse([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tp.Extras[0].Locked, ","); got != "port" {
		t.Errorf("锁住的项：%s", got)
	}
	for name, add := range map[string]string{
		"格式":      `  - {id: a, label: A, format: toml, file: "${dir}/demo.ini"}`,
		"没写文件":    `  - {id: a, label: A, format: ini}`,
		"env 写文件": `  - {id: a, label: A, format: env, file: "${dir}/demo.ini"}`,
		"文件没人写":   `  - {id: a, label: A, format: ini, file: "${dir}/other.ini"}`,
		"文件变量":    `  - {id: a, label: A, format: ini, file: "${nope}/demo.ini"}`,
		"args 前缀": `  - {id: a, label: A, format: args}`,
		"前缀写法":    `  - {id: a, label: A, format: args, prefix: "; rm"}`,
		"建议被锁住":   `  - {id: a, label: A, format: ini, file: "${dir}/demo.ini", section: main, suggest: [{key: port}]}`,
		"id":      `  - {id: A, label: A, format: env}`,
		"重复 id":   "  - {id: a, label: A, format: env}\n  - {id: a, label: B, format: env}",
		"段只给 ini": `  - {id: a, label: A, format: env, section: main}`,
		"protect": `  - {id: a, label: A, format: env, protect: ["a b"]}`,
	} {
		if _, err := Parse([]byte(base + add + "\n")); err == nil {
			t.Errorf("%s：应该报错", name)
		}
	}
}

func TestMergeINI(t *testing.T) {
	x := &Extra{ID: "mysqld", Label: "MySQL", Format: "ini", Section: "mysqld"}
	content := "# 注释\n[mysqld]\nport=3306\nmax_connections=500\n# max-connections=9\nmax-connections=600\ninit-file=/x\n\n[client]\nmax_connections=1\nport=3306\n"
	set := ExtraSet{Extra: x, Items: []ExtraItem{
		{Section: "mysqld", Key: "max_connections", Value: "2000"},
		{Section: "mysqld", Key: "innodb_buffer_pool_size", Value: "1G"},
		{Section: "mysqld", Key: "skip-name-resolve"},
	}}
	got := set.Merge(content)
	want := "# 注释\n[mysqld]\nport=3306\nmax_connections=2000\n# max-connections=9\ninit-file=/x\ninnodb_buffer_pool_size=1G\nskip-name-resolve\n\n[client]\nmax_connections=1\nport=3306\n"
	if got != want {
		t.Errorf("合并结果：\n%s\n要：\n%s", got, want)
	}
	// 文件里没有这一段：加在末尾
	got = set.Merge("[client]\nport=1")
	if !strings.HasSuffix(got, "[client]\nport=1\n\n[mysqld]\nmax_connections=2000\ninnodb_buffer_pool_size=1G\nskip-name-resolve") {
		t.Errorf("新建段：%q", got)
	}

	// 不固定段：键写成「段.键」，段名不分大小写
	z := &Extra{ID: "ini", Label: "ZLM", Format: "ini"}
	set = ExtraSet{Extra: z, Items: []ExtraItem{
		{Section: "HOOK", Key: "enable", Value: "1"},
		{Section: "hls", Key: "segDur", Value: "2"},
		{Section: "api", Key: "apiDebug", Value: "0"},
	}}
	got = set.Merge("[api]\nsecret=s\n\n[hook]\nenable=0\n\n[shell]\nport=0\n")
	want = "[api]\nsecret=s\napiDebug=0\n\n[hook]\nenable=1\n\n[shell]\nport=0\n\n[hls]\nsegDur=2\n"
	if got != want {
		t.Errorf("合并结果：\n%s\n要：\n%s", got, want)
	}
}

func TestMergeLines(t *testing.T) {
	kv := ExtraSet{Extra: &Extra{Format: "keyvalue"}, Items: []ExtraItem{{Key: "appendonly", Value: "no"}, {Key: "maxmemory", Value: "2gb"}, {Key: "save", Value: "900 1"}}}
	got := kv.Merge("port 6379\nAPPENDONLY yes\nsave 3600 1\nsave 300 100\n\n# end\n")
	if want := "port 6379\nappendonly no\nsave 900 1\n\n# end\nmaxmemory 2gb\n"; got != want {
		t.Errorf("keyvalue：%q 要 %q", got, want)
	}
	pr := ExtraSet{Extra: &Extra{Format: "properties"}, Items: []ExtraItem{{Key: "db.num", Value: "1"}, {Key: "db.url.0", Value: "jdbc:mysql://h:3306/n?a=1&b=2"}}}
	got = pr.Merge("db.num: 0\nserver.port=8848")
	if want := "db.num=1\nserver.port=8848\ndb.url.0=jdbc:mysql://h:3306/n?a=1&b=2"; got != want {
		t.Errorf("properties：%q 要 %q", got, want)
	}
	if got := kv.Merge(""); got != "appendonly no\nmaxmemory 2gb\nsave 900 1\n" {
		t.Errorf("空文件：%q", got)
	}
}

func TestArgsAndEnvQuoting(t *testing.T) {
	pg := ExtraSet{Extra: &Extra{Format: "args", Prefix: "-c "}, Items: []ExtraItem{{Key: "shared_buffers", Value: "1GB"}, {Key: "log_line_prefix", Value: `%m [$p] "x"`}}}
	if got, want := pg.Args(), ` -c shared_buffers=1GB -c "log_line_prefix=%m [$$p] \"x\""`; got != want {
		t.Errorf("args：%s 要 %s", got, want)
	}
	sp := ExtraSet{Extra: &Extra{Format: "args", Prefix: "--"}, Items: []ExtraItem{{Key: "a.b", Value: "c d"}}}
	if got, want := sp.Args(), ` "--a.b=c d"`; got != want {
		t.Errorf("args：%s 要 %s", got, want)
	}
	for in, want := range map[string]string{"plain value": "plain value", `a"b`: `"a\"b"`, `c:\x`: `"c:\\x"`, " x": `" x"`} {
		if got := EnvValue(in); got != want {
			t.Errorf("EnvValue(%q) = %s 要 %s", in, got, want)
		}
	}
}

// 没写 extras 的模板（旧的用户模板）照样能用。
func TestTemplateWithoutExtras(t *testing.T) {
	tp, err := Parse([]byte(`id: old
name: Old
variants:
  - label: 包
    user: old
    package: {format: binary, into: old}
    service: {exec: "${dir}/old", ready: {port: "80"}}
`))
	if err != nil || len(tp.Extras) != 0 {
		t.Fatalf("%v %v", tp, err)
	}
	if sets, err := tp.ResolveExtras(&tp.Variants[0], nil); err != nil || sets != nil {
		t.Errorf("%v %v", sets, err)
	}
}
