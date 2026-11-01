package swtpl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinTemplates(t *testing.T) {
	list := Builtin()
	ids := map[string]bool{}
	for _, tp := range list {
		ids[tp.ID] = true
	}
	for _, id := range []string{"nacos", "sentinel", "mysql", "redis", "mariadb", "postgresql", "minio", "nginx", "zlmediakit"} {
		if !ids[id] {
			t.Errorf("缺少内置模板 %s", id)
		}
	}
}

func TestValidateProtoSets(t *testing.T) {
	base := "id: x\nname: X\nparams:\n  - {key: port, type: port, default: \"1\"%s}\n  - {key: password, type: password}\n" +
		"variants:\n  - user: x\n    system: {dnf: [x]}\n    steps:\n      - {run: \"true\"%s}\n    service: {exec: /x, ready: {port: \"${port}\"}}\n"
	cases := map[string][2]string{
		"udp 端口":       {", proto: udp", ""},
		"tcp 和 udp":    {", proto: both", ""},
		"sets 写参数名":    {"", ", sets: password"},
		"proto 不认识":    {", proto: sctp", ""},
		"sets 写了没有的参数": {"", ", sets: secret"},
	}
	for what, c := range cases {
		_, err := Parse([]byte(fmt.Sprintf(base, c[0], c[1])))
		bad := strings.Contains(what, "不认识") || strings.Contains(what, "没有")
		if (err != nil) != bad {
			t.Errorf("%s：%v", what, err)
		}
	}
	p := Param{Proto: "both"}
	if !p.TCP() || !p.UDP() || (&Param{}).UDP() || (&Param{Proto: "udp"}).TCP() {
		t.Error("TCP / UDP")
	}
}

func TestExpand(t *testing.T) {
	vs := Vars{"port": "8848", "public": "true", "private": "false", "password": "pw", "dir": "/opt/x"}
	cases := map[string]string{
		"${dir}/a:${port}":                    "/opt/x/a:8848",
		"${port+1000}":                        "9848",
		"${public?0.0.0.0|127.0.0.1}":         "0.0.0.0",
		"${private?0.0.0.0|127.0.0.1}":        "127.0.0.1",
		"${public?BY '${password}';|}":        "BY 'pw';",
		"${private?BY '${password}';|}":       "",
		"-Dx=$${SENTINEL_PASSWORD} ${port}":   "-Dx=${SENTINEL_PASSWORD} 8848",
		"for d in $d $(ls); do echo $p; done": "for d in $d $(ls); do echo $p; done",
	}
	for in, want := range cases {
		got, err := vs.Expand(in)
		if err != nil || got != want {
			t.Errorf("%q → %q（%v），期望 %q", in, got, err, want)
		}
	}
	if _, err := vs.Expand("${nope}"); err == nil {
		t.Error("没有的变量应当报错")
	}
	if r, _ := vs.Expand("${random:12}"); len(r) != 12 {
		t.Errorf("random：%q", r)
	}
	if !vs.Cond("public") || vs.Cond("!public") || !vs.Cond("") || vs.Cond("private") {
		t.Error("Cond 不对")
	}
}

func TestPick(t *testing.T) {
	var redis *Template
	for _, tp := range Builtin() {
		if tp.ID == "redis" {
			redis = tp
		}
	}
	rocky := OS{ID: "rocky", Like: []string{"rhel", "centos", "fedora"}, Pretty: "Rocky Linux 9"}
	ubuntu := OS{ID: "ubuntu", Like: []string{"debian"}, Pretty: "Ubuntu 24.04"}
	alpine := OS{ID: "alpine", Pretty: "Alpine Linux"}
	if i, err := redis.Pick(rocky, "amd64", -1); err != nil || !strings.HasPrefix(redis.Variants[i].System.DNF[0], "redis|") {
		t.Errorf("rocky：%d %v", i, err)
	}
	if i, err := redis.Pick(ubuntu, "arm64", -1); err != nil || redis.Variants[i].System.APT[0] != "redis-server" {
		t.Errorf("ubuntu：%d %v", i, err)
	}
	if _, err := redis.Pick(alpine, "amd64", -1); err == nil || !strings.Contains(err.Error(), "Alpine") {
		t.Errorf("alpine 应当没有装法：%v", err)
	}
	mysql, _ := Find(t.TempDir(), "mysql")
	if _, err := mysql.Pick(rocky, "arm64", -1); err == nil {
		t.Error("MySQL 不支持 arm64")
	}
}

func TestMatchFile(t *testing.T) {
	p := &Package{File: "mysql-${version}-linux-glibc2.17-x86_64-minimal.tar.xz"}
	if v, ok := p.MatchFile("mysql-8.4.10-linux-glibc2.17-x86_64-minimal.tar.xz"); !ok || v != "8.4.10" {
		t.Errorf("%q %v", v, ok)
	}
	if _, ok := p.MatchFile("mysql-8.4.10-linux-glibc2.28-aarch64.tar.xz"); ok {
		t.Error("arm 包不应匹配")
	}
	if FormatOf("", "a.TGZ") != "tar.gz" || FormatOf("", "minio") != "binary" || FormatOf("zip", "x") != "zip" {
		t.Error("FormatOf 不对")
	}
	zlm := &Package{File: "zlmediakit-centos7-x86_64-${version}.tar.gz"}
	for _, version := range []string{"fcb2889", "master+fcb2889", "1.0.0"} {
		if got, ok := zlm.MatchFile("zlmediakit-centos7-x86_64-" + version + ".tar.gz"); !ok || got != version {
			t.Errorf("源码版本 %q 未被识别：%q %v", version, got, ok)
		}
	}
	for _, name := range []string{"zlmediakit-centos7-x86_64-.tar.gz", "zlmediakit-centos7-x86_64-../../other.tar.gz"} {
		if _, ok := zlm.MatchFile(name); ok {
			t.Errorf("不应匹配无效安装包名 %q", name)
		}
	}
}

func TestUserTemplates(t *testing.T) {
	dir := t.TempDir()
	src := []byte(`id: myapp
name: 我的服务
category: 自定义
params:
  - {key: port, label: 端口, type: port, default: "9000"}
variants:
  - label: 导入的程序
    user: myapp
    package: {format: binary, into: myapp}
    service:
      exec: ${dir}/myapp --port ${port}
      ready: {port: "${port}"}
`)
	if _, err := Save(dir, src); err != nil {
		t.Fatal(err)
	}
	// 覆盖内置的 redis
	b, _ := BuiltinSource("redis")
	os.WriteFile(filepath.Join(dir, "redis.yaml"), b, 0o644)
	// 坏的模板：变量没定义
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("id: bad\nname: x\nvariants:\n  - label: x\n    user: u\n    package: {format: binary}\n    service: {exec: \"${nope}\", ready: {port: \"1\"}}\n"), 0o644)
	list, errs := All(dir)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "nope") {
		t.Errorf("errs：%v", errs)
	}
	found := map[string]*Template{}
	for _, tp := range list {
		found[tp.ID] = tp
	}
	if found["myapp"] == nil || found["myapp"].Builtin || !found["redis"].Overrides || found["nacos"] == nil || !found["nacos"].Builtin {
		t.Errorf("列表不对：%v", found)
	}
	if err := Delete(dir, "nacos"); err == nil {
		t.Error("内置模板不能删")
	}
	if err := Delete(dir, "myapp"); err != nil {
		t.Error(err)
	}
	// 未知字段
	if _, err := Parse([]byte("id: x\nname: x\nfoo: 1\n")); err == nil {
		t.Error("未知字段应当报错")
	}
}

// 随机串太长：模板通不过检查，展开时也报错（不会一下子用掉大量内存）
func TestRandomLimit(t *testing.T) {
	tpl := "id: x\nname: X\nvariants:\n  - user: x\n    system: {dnf: [x]}\n    files: [{path: '${dir}/a', content: '%s'}]\n    service: {exec: /x, ready: {port: '1'}}\n"
	if _, err := Parse([]byte(fmt.Sprintf(tpl, "${random:24}"))); err != nil {
		t.Errorf("正常长度：%v", err)
	}
	for _, bad := range []string{"${random:99999999999}", "${base64random:0}", "${random:x}"} {
		if _, err := Parse([]byte(fmt.Sprintf(tpl, bad))); err == nil || !strings.Contains(err.Error(), "长度") {
			t.Errorf("%s 应当拒绝：%v", bad, err)
		}
		if _, err := (Vars{}).Expand(bad); err == nil {
			t.Errorf("%s 展开时应当报错", bad)
		}
	}
}

// 参数里层层重复引用变量：展开到一定长度就报错，不会把内存用完
func TestExpandBomb(t *testing.T) {
	vs := Vars{"a": strings.Repeat("${a}", 100)}
	if _, err := vs.Expand("${a}"); err == nil || !strings.Contains(err.Error(), "太长") {
		t.Errorf("%v", err)
	}
}
