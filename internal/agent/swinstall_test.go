package agent

import (
	"archive/tar"
	"compress/gzip"
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
