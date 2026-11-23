package agent

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		"启动命令是目录":    func(p *proto.SoftwarePlan) { p.Exec = "/opt/ferry-software/nacos/bin/ --flag" },
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

func TestCheckResponseField(t *testing.T) {
	for body, want := range map[string]bool{
		`{"accessToken":"signed"}`:            true,
		`{"accessToken":null}`:                false,
		`{"accessToken":""}`:                  false,
		`{"message":"authentication failed"}`: false,
		`<html>gateway error</html>`:          false,
	} {
		if _, err := readResponseField(strings.NewReader(body), "accessToken"); (err == nil) != want {
			t.Errorf("响应 %s 验证错误 = %v", body, err)
		}
	}
}

func TestStepHTTPWithLogin(t *testing.T) {
	var created atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			if r.FormValue("username") != "nacos" || r.FormValue("password") != "Admin-123!" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			io.WriteString(w, `{"accessToken":"test-token"}`)
		case "/user":
			if r.Header.Get("accessToken") != "test-token" || r.FormValue("username") != "team_user" || r.FormValue("password") != "Member+123!" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			created.Store(true)
			io.WriteString(w, `{"data":"create user ok!"}`)
		case "/verify":
			if !created.Load() || r.FormValue("username") != "team_user" || r.FormValue("password") != "Member+123!" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			io.WriteString(w, `{"accessToken":"member-token"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("令牌请求发到了另一个服务")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer other.Close()
	a := &Agent{Sys: &fakeSys{}}
	login := &proto.PlanHTTP{Post: server.URL + "/login", Form: map[string]string{"username": "nacos", "password": "Admin-123!"}, ResponseField: "accessToken"}
	form := map[string]string{"username": "team_user", "password": "Member+123!"}
	if err := a.stepHTTP(&proto.PlanHTTP{Post: other.URL + "/user", Form: form, Login: login}); err == nil {
		t.Fatal("应拒绝跨源发送令牌")
	}
	if err := a.stepHTTP(&proto.PlanHTTP{Post: server.URL + "/user", Form: form, Login: login, ResponseField: "data"}); err != nil {
		t.Fatal(err)
	}
	if err := a.stepHTTP(&proto.PlanHTTP{Post: server.URL + "/verify", Form: form, ResponseField: "accessToken"}); err != nil {
		t.Fatal(err)
	}
}

func TestSoftwareELFDetectedArchitecture(t *testing.T) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("需要 Linux 的 ELF 测试可执行文件")
	}
	file, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkELFExecutable(file, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	other := "arm64"
	if runtime.GOARCH == other {
		other = "amd64"
	}
	if err := checkELFExecutable(file, other); err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("应拒绝另一种架构：%v", err)
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
	if c := ran(f, "dnf install -y"); len(c) != 1 || c[0] != "dnf install -y libaio 2>&1" {
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
	if c := ran(f, "DEBIAN_FRONTEND=noninteractive apt-get install"); len(c) != 1 || c[0] != "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-upgrade libnuma1 libaio1 2>&1" {
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

func TestSystemPackagePreviewOnlySimulates(t *testing.T) {
	f := newFake(t)
	f.shellFail["rpm -q --quiet 'nginx'"] = true
	const dryRun = "LC_ALL=C dnf install --assumeno nginx 2>&1"
	f.shellOut = map[string]string{dryRun: "Upgrading:\n openssl-libs x86_64 3.0.1\n\nTransaction Summary\n"}
	req := &proto.Request{SwResolve: &proto.SwResolveSpec{Packages: []string{"nginx"}}}
	result := exec1(t, f, "swresolve", req)
	var plan proto.SwResolution
	if result.code != proto.CodeOK || json.Unmarshal(result.result.Data, &plan) != nil ||
		strings.Join(plan.Packages, ",") != "nginx" || strings.Join(plan.Changes, ",") != "openssl-libs（升级）" {
		t.Fatal(result.dump())
	}
	if !slicesHas(f.calls, "sh "+dryRun) || slicesHas(f.calls, "sh dnf install -y nginx 2>&1") {
		t.Fatalf("预检只能模拟安装：%v", f.calls)
	}
	req.SwResolve.Packages = []string{"nginx; id"}
	if result := exec1(t, f, "swresolve", req); result.code != proto.CodeBadInput {
		t.Fatal(result.dump())
	}
}

func TestDependencyResolutionReportsRepositoryFailures(t *testing.T) {
	for _, tc := range []struct {
		manager, output, want string
	}{
		{"yum", "Cannot find a valid baseurl for repo: base/7/x86_64\nCould not retrieve mirrorlist http://mirrorlist.centos.org, HTTP 502\n", "旧镜像列表已停用"},
		{"dnf", "Failed to download metadata for repo 'custom': Status code: 404\nNo match for argument: nginx\n", "Status code: 404"},
		{"dnf", "No match for argument: nginx\n", "软件源里没有 nginx"},
		{"dnf", "However, you do not have any GPG public keys installed.\nProblem repository: [custom]\n", "缺少签名公钥"},
		{"apt-get", "E: Unable to locate package nginx\n", "APT 未能完整刷新软件包索引"},
	} {
		cmd := func(_ string, shell string, _ time.Duration) (string, error) {
			if strings.Contains(shell, "apt-get update") {
				return "W: Failed to fetch http://archive.example/debian/dists/x/InRelease\n", nil
			}
			return tc.output, errors.New("exit status 1")
		}
		_, err := pkgChanges(cmd, tc.manager, "", []string{"nginx"}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s：想要 %q，收到 %v", tc.manager, tc.want, err)
		}
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
