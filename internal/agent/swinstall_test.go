package agent

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestValidInstallDir(t *testing.T) {
	good := []string{"/opt/ferry-software/nacos", "/data/mysql", "/home/apps/sentinel", "/usr/local/nacos"}
	bad := []string{"opt/x", "/opt", "/data", "/x", "/opt/../etc/x", "/etc/nacos", "/usr/lib/x", "/opt/my app", "/opt/x/", "/root/x", "/tmp/x"}
	for _, d := range good {
		if err := validInstallDir(d); err != nil {
			t.Errorf("%s：%v", d, err)
		}
	}
	for _, d := range bad {
		if err := validInstallDir(d); err == nil {
			t.Errorf("%s 应当拒绝", d)
		}
	}
}

func TestValidatePlan(t *testing.T) {
	ok := func() *proto.SoftwarePlan {
		return &proto.SoftwarePlan{Template: "nacos", Title: "Nacos", Name: "nacos", Version: "3.2.4", Dir: "/opt/ferry-software/nacos", User: "nacos",
			Package: &proto.PlanPackage{File: "nacos-server-3.2.4.tar.gz", SHA256: strings.Repeat("a", 64), Format: "tar.gz", Strip: 1, Into: "nacos"},
			Files:   []proto.PlanFile{{Path: "/opt/ferry-software/nacos/a.conf"}},
			Props:   []proto.PlanProps{{File: "/opt/ferry-software/nacos/nacos/conf/application.properties"}},
			Exec:    "/x/java -jar a.jar", ReadyPort: 8848, Ports: []int{8848, 9848, 8080}, Env: map[string]string{"PW": "x"}}
	}
	if err := validatePlan(ok()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*proto.SoftwarePlan){
		"名称":         func(p *proto.SoftwarePlan) { p.Name = "Nacos" },
		"root 用户":    func(p *proto.SoftwarePlan) { p.User = "root" },
		"端口重复":       func(p *proto.SoftwarePlan) { p.Ports = append(p.Ports, 8848) },
		"目录":         func(p *proto.SoftwarePlan) { p.Dir = "/etc/nacos" },
		"写到目录外":      func(p *proto.SoftwarePlan) { p.Files[0].Path = "/etc/passwd" },
		"改目录外的配置":    func(p *proto.SoftwarePlan) { p.Props[0].File = "/opt/ferry-software/other/x" },
		"放到目录外":      func(p *proto.SoftwarePlan) { p.Package.Into = "../x" },
		"格式":         func(p *proto.SoftwarePlan) { p.Package.Format = "rar" },
		"包名":         func(p *proto.SoftwarePlan) { p.System = &proto.PlanSystem{DNF: []string{"redis; rm -rf /"}} },
		"环境变量换行":     func(p *proto.SoftwarePlan) { p.Env["PW"] = "a\nb" },
		"启动命令换行":     func(p *proto.SoftwarePlan) { p.Exec = "a\nExecStartPre=/bin/x" },
		"既没安装包也没软件源": func(p *proto.SoftwarePlan) { p.Package = nil },
		"账号带引号":      func(p *proto.SoftwarePlan) { p.Account = "a'b" },
	}
	for what, change := range cases {
		p := ok()
		change(p)
		if err := validatePlan(p); err == nil {
			t.Errorf("%s：应当拒绝", what)
		}
	}
}

func TestSetProperties(t *testing.T) {
	f := filepath.Join(t.TempDir(), "application.properties")
	os.WriteFile(f, []byte("#nacos.server.main.port=1\nnacos.server.main.port=8848\nnacos.core.auth.plugin.nacos.token.secret.key=\nother=1\n"), 0o644)
	if err := setProperties(f, "", map[string]string{"nacos.server.main.port": "18848", "nacos.core.auth.plugin.nacos.token.secret.key": "abc=", "new.key": "v"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	want := "#nacos.server.main.port=1\nnacos.server.main.port=18848\nnacos.core.auth.plugin.nacos.token.secret.key=abc=\nother=1\n\nnew.key=v"
	if string(b) != want {
		t.Errorf("结果：\n%s", b)
	}
	// 空格分隔（redis.conf 之类）
	g := filepath.Join(t.TempDir(), "redis.conf")
	os.WriteFile(g, []byte("port 6379\n# requirepass x\nbind 127.0.0.1\n"), 0o644)
	setProperties(g, " ", map[string]string{"port": "6380", "requirepass": "pw"})
	if b, _ := os.ReadFile(g); string(b) != "port 6380\n# requirepass x\nbind 127.0.0.1\n\nrequirepass pw" {
		t.Errorf("空格分隔：\n%s", b)
	}
}

func TestSwUnitText(t *testing.T) {
	pl := &proto.SoftwarePlan{Title: "Sentinel 控制台", Name: "sentinel", Version: "1.8.10", Dir: "/opt/s", User: "sentinel",
		Exec: "/j/java -Dx=${SENTINEL_PASSWORD} -Dgc=%t -jar /opt/s/sentinel-dashboard.jar", Workdir: "/opt/s", Env: map[string]string{"SENTINEL_PASSWORD": "x"}}
	text := swUnitText(pl)
	for _, s := range []string{ManagedMarker, "User=sentinel\n", "EnvironmentFile=" + swEnvFile("/opt/s") + "\n", "ExecStart=/j/java -Dx=${SENTINEL_PASSWORD} -Dgc=%%t -jar /opt/s/sentinel-dashboard.jar\n", "WantedBy=multi-user.target"} {
		if !strings.Contains(text, s) {
			t.Errorf("单元文件里没有 %q：\n%s", s, text)
		}
	}
	if strings.Contains(text, "SENTINEL_PASSWORD=x") {
		t.Error("密码不能写进单元文件")
	}
}

func TestListenPortsIn(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "tcp"), []byte("  sl  local_address rem_address   st\n   0: 00000000:0CEA 00000000:0000 0A 0\n   1: 0100007F:1F90 0100007F:9C40 01 0\n"), 0o644)
	os.WriteFile(filepath.Join(d, "tcp6"), []byte("  sl  local_address\n   0: 00000000000000000000000000000000:2290 00000000000000000000000000000000:0000 0A 0\n"), 0o644)
	got := listenPortsIn(d)
	if !got[3306] || !got[8848] || got[8080] || len(got) != 2 {
		t.Errorf("%v", got)
	}
}

func TestUDPPortsIn(t *testing.T) {
	d := t.TempDir()
	// 9000 绑定着；40000 连着对方（往外发的临时端口）不算
	os.WriteFile(filepath.Join(d, "udp"), []byte("  sl  local_address rem_address   st\n   0: 00000000:2328 00000000:0000 07 0\n   1: 0100007F:9C40 08080808:0035 01 0\n"), 0o644)
	os.WriteFile(filepath.Join(d, "udp6"), []byte("  sl  local_address\n   0: 00000000000000000000000000000000:4650 00000000000000000000000000000000:0000 07 0\n"), 0o644)
	got := udpPortsIn(d)
	if !got[9000] || !got[18000] || got[40000] || len(got) != 2 {
		t.Errorf("%v", got)
	}
}

func TestSwPortUsers(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{FerryDir: t.TempDir()}}
	os.MkdirAll(a.swDir(), 0o755)
	save := func(in proto.SoftwareInstance) {
		b, _ := json.Marshal(in)
		os.WriteFile(a.instFile(in.Name), b, 0o644)
	}
	save(proto.SoftwareInstance{Name: "nacos", Title: "Nacos", Unit: swUnit("nacos"), Dir: "/opt/ferry-software/nacos", Ports: map[string]int{"port": 8848}, AllPorts: []int{8848, 9848}})
	save(proto.SoftwareInstance{Name: "zlm", Title: "ZLMediaKit", Unit: swUnit("zlm"), Dir: "/opt/ferry-software/zlm", UDPPorts: []int{9000}})
	save(proto.SoftwareInstance{Name: "redis", Title: "Redis", Unit: swUnit("redis"), Dir: "/opt/ferry-software/redis", Ports: map[string]int{"port": 6379}}) // 老记录
	got := a.swPortUsers("")
	for _, k := range []string{"tcp/8848", "tcp/9848", "udp/9000", "tcp/6379"} {
		if got[k] == "" {
			t.Errorf("没有 %s：%v", k, got)
		}
	}
	if got["tcp/9000"] != "" || !strings.Contains(got["tcp/6379"], "Redis（redis）") {
		t.Errorf("%v", got)
	}
	if a.swPortUsers("redis")["tcp/6379"] != "" {
		t.Error("要跳过的实例不能算")
	}
}

func TestSystemInstall(t *testing.T) {
	ran := func(f *fakeSys, prefix string) []string {
		var out []string
		for _, c := range f.calls {
			if strings.HasPrefix(c, "sh "+prefix) {
				out = append(out, strings.TrimPrefix(c, "sh "))
			}
		}
		return out
	}
	// dnf：已经装着的不交给 dnf（不升级）
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	f.shellFail["rpm -q --quiet 'libaio'"] = true
	got, err := a.systemInstall("mysql", &proto.PlanSystem{DNF: []string{"libaio", "numactl-libs"}}, "", true)
	if err != nil || strings.Join(got, " ") != "libaio" {
		t.Fatalf("%v %v", got, err)
	}
	if c := ran(f, "dnf install"); len(c) != 1 || c[0] != "dnf install -y libaio 2>&1" {
		t.Errorf("dnf：%v", c)
	}
	// 全都装着：不执行 dnf，也不动系统服务
	f = newFake(t)
	a = &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	if got, err := a.systemInstall("redis", &proto.PlanSystem{DNF: []string{"redis"}, Disable: []string{"redis.service"}}, "", false); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if c := ran(f, "dnf install"); len(c) != 0 || slicesHas(f.calls, "disable --now redis.service") {
		t.Errorf("已装着时不能再装、不能动服务：%v", f.calls)
	}

	// apt：候选包名用软件源里有的；加 --no-upgrade
	f = newFake(t)
	a = &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	for _, c := range []string{"command -v dnf", "command -v yum",
		"dpkg-query -W -f='${Status}' 'libaio1t64' 2>/dev/null | grep -q 'install ok installed'",
		"dpkg-query -W -f='${Status}' 'libaio1' 2>/dev/null | grep -q 'install ok installed'",
		"dpkg-query -W -f='${Status}' 'libnuma1' 2>/dev/null | grep -q 'install ok installed'",
		"apt-cache show 'libaio1t64' 2>/dev/null | grep -q '^Package:'"} {
		f.shellFail[c] = true
	}
	got, err = a.systemInstall("mysql", &proto.PlanSystem{APT: []string{"libaio1t64|libaio1", "libnuma1"}}, "", true)
	if err != nil || strings.Join(got, " ") != "libnuma1 libaio1" {
		t.Fatalf("%v %v", got, err)
	}
	if c := ran(f, "DEBIAN_FRONTEND"); len(c) != 1 || c[0] != "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-upgrade libnuma1 libaio1 2>&1" {
		t.Errorf("apt：%v", c)
	}

	// 安装包需要的系统库：没有包管理器时跳过；只用软件源装的软件要报错
	f = newFake(t)
	a = &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	for _, m := range []string{"dnf", "yum", "apt-get"} {
		f.shellFail["command -v "+m] = true
	}
	if _, err := a.systemInstall("mysql", &proto.PlanSystem{DNF: []string{"libaio"}}, "", true); err != nil {
		t.Errorf("系统库：%v", err)
	}
	if _, err := a.systemInstall("redis", &proto.PlanSystem{DNF: []string{"redis"}}, "", false); err == nil {
		t.Error("没有包管理器时应当失败")
	}
}

func slicesHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// stopFailSys 停止服务失败（服务还在运行）
type stopFailSys struct{ *fakeSys }

func (f *stopFailSys) Systemctl(args ...string) error {
	if len(args) > 0 && args[0] == "disable" {
		return os.ErrPermission
	}
	return f.fakeSys.Systemctl(args...)
}

func TestSwRemoveStopFails(t *testing.T) {
	setup := func(sys System, f *fakeSys) (*Agent, string) {
		base := t.TempDir()
		dir := filepath.Join(base, "installed")
		os.MkdirAll(dir, 0o750)
		os.WriteFile(filepath.Join(dir, "data.db"), []byte("x"), 0o600)
		unit := swUnit("demo")
		u := f.unit(unit)
		u.active, u.sub, u.pid = "active", "running", 123
		os.WriteFile(filepath.Join(f.unitDir, unit), []byte(ManagedMarker+"\n"), 0o644)
		a := &Agent{Sys: sys, Out: io.Discard, req: &proto.Request{FerryDir: base, Instance: "demo", Purge: true}}
		a.instanceLookup = func(string) (*proto.SoftwareInstance, error) {
			return &proto.SoftwareInstance{Name: "demo", Unit: unit, Dir: dir}, nil
		}
		os.MkdirAll(a.swDir(), 0o755)
		os.WriteFile(a.instFile("demo"), []byte("{}"), 0o644)
		return a, dir
	}

	// 停不下来：什么都不删
	f := newFake(t)
	a, dir := setup(&stopFailSys{f}, f)
	if code := a.swremove(); code == proto.CodeOK {
		t.Fatal("停止失败时应当失败")
	}
	if _, err := os.Stat(filepath.Join(dir, "data.db")); err != nil {
		t.Error("停止失败时不能删数据")
	}
	if _, err := os.Stat(a.instFile("demo")); err != nil {
		t.Error("停止失败时不能删记录")
	}
	if _, err := os.Stat(filepath.Join(f.unitDir, swUnit("demo"))); err != nil {
		t.Error("停止失败时不能删单元")
	}

	// 正常卸载；单元文件已经不在（上次删到一半）时也能卸载
	for _, gone := range []bool{false, true} {
		f := newFake(t)
		a, dir := setup(f, f)
		if gone {
			os.Remove(filepath.Join(f.unitDir, swUnit("demo")))
			f.unit(swUnit("demo")).active, f.unit(swUnit("demo")).pid = "inactive", 0
		}
		if code := a.swremove(); code != proto.CodeOK {
			t.Fatalf("单元不在=%v：卸载失败 %d", gone, code)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("单元不在=%v：purge 应当删除目录", gone)
		}
		if _, err := os.Stat(a.instFile("demo")); err == nil {
			t.Errorf("单元不在=%v：应当删除记录", gone)
		}
	}
}

func TestExtractStrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar.gz")
	f, _ := os.Create(src)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, n := range []string{"pkg-1.0/", "pkg-1.0/bin/run", "pkg-1.0/conf/a.conf"} {
		if strings.HasSuffix(n, "/") {
			tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeDir, Mode: 0o755})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o755, Size: 1})
		tw.Write([]byte("x"))
	}
	tw.Close()
	gz.Close()
	f.Close()
	for strip, want := range map[int]string{0: "pkg-1.0/bin/run", 1: "bin/run", 2: "run"} {
		out := filepath.Join(dir, "out", string(rune('0'+strip)))
		os.MkdirAll(out, 0o755)
		if err := extractTarGzStrip(src, out, strip); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(want))); err != nil {
			t.Errorf("strip %d：没有 %s", strip, want)
		}
	}
	if _, err := archivePath("../x", 0); err == nil {
		t.Error("越界路径应当拒绝")
	}
}
