package agent

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func fatJar(t *testing.T, libs ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, n := range append([]string{"META-INF/MANIFEST.MF"}, libs...) {
		w, _ := z.Create(n)
		w.Write([]byte("x"))
	}
	z.Close()
	return b.Bytes()
}

func TestClassifyPorts(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	jar := fatJar(t, "BOOT-INF/lib/sentinel-transport-simple-http-1.8.6.jar", "BOOT-INF/lib/xxl-job-core-2.4.0.jar")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": jar})); r.code != 0 {
		t.Fatal(r.dump())
	}
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root}}
	f.args = map[int][][]string{
		7: {{"/bin/sh", "/opt/run.sh"}, {"java", "-agentlib:jdwp=transport=dt_socket,server=y,suspend=n,address=*:5005",
			"-Dcom.sun.management.jmxremote.port=9999", "-jar", "System.jar", "--server.port=9201"}},
	}
	all := []string{"*:5005", "*:8721", "127.0.0.1:9201", "*:9999", "*:10000", "*:12345"}

	// 有健康检查端口：它是主端口，其余都是附加端口
	listen, extra := a.classifyPorts(&sys, "9201", all, 7)
	if !reflect.DeepEqual(listen, []string{"127.0.0.1:9201"}) {
		t.Fatalf("主端口 %q", listen)
	}
	use := map[string]string{}
	for _, e := range extra {
		use[e.Addr] = e.Use
		if (e.Addr == "*:5005" || e.Addr == "*:8721") != e.Risky && e.Addr != "*:9999" {
			t.Errorf("%s risky=%v", e.Addr, e.Risky)
		}
	}
	for addr, want := range map[string]string{"*:5005": "JDWP", "*:8721": "Sentinel", "*:9999": "JMX", "*:10000": "XXL-JOB", "*:12345": ""} {
		if got := use[addr]; (want == "" && got != "") || !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("%s 用途 %q，应含 %q", addr, got, want)
		}
	}

	// 没有健康检查：认出用途的算附加端口，--server.port 与认不出的算主端口
	listen, extra = a.classifyPorts(&sys, "", all, 7)
	if !reflect.DeepEqual(listen, []string{"127.0.0.1:9201", "*:12345"}) || len(extra) != 4 {
		t.Fatalf("%q %+v", listen, extra)
	}

	// 静态站点、二进制程序没有 jar：只看启动参数
	bin := proto.AppSpec{Name: "bin", Type: proto.TypeBinary, FileName: "bin"}
	if _, extra := a.classifyPorts(&bin, "9201", []string{"*:8721"}, 0); len(extra) != 1 || extra[0].Use != "" {
		t.Fatalf("%+v", extra)
	}
}

func TestJdwpAddress(t *testing.T) {
	for in, want := range map[string]string{
		"-agentlib:jdwp=transport=dt_socket,server=y,address=5005": "5005",
		"-Xrunjdwp:transport=dt_socket,address=127.0.0.1:8000":     "127.0.0.1:8000",
	} {
		if got, ok := jdwpAddress(in); !ok || got != want {
			t.Errorf("%s → %q", in, got)
		}
	}
	if _, ok := jdwpAddress("-Dfoo=1"); ok {
		t.Error("不是 jdwp")
	}
}

// 应用状态里不宜对外的端口：防火墙拦着的标上 blocked；只读运行时的规则，不读永久配置
func TestStatusFirewallMark(t *testing.T) {
	proc := t.TempDir()
	os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	os.WriteFile(filepath.Join(proc, "net", "route"), []byte("Iface\tDestination\tGateway\neth0\t00000000\t0101A8C0\n"), 0o644)
	var mu sync.Mutex
	var ran []string
	run := fakeRun(map[string]string{
		"systemctl is-active --quiet firewalld": "running\n",
		"--list-all-zones":                      zonePublic,
	})
	sc := &fwScan{proc: proc, root: t.TempDir(), runtimeOnly: true, run: func(cmd string, d time.Duration) (string, error) {
		mu.Lock()
		ran = append(ran, cmd)
		mu.Unlock()
		return run(cmd, d)
	}}
	fw := &statusFW{scan: sc.firewall}
	out := []proto.AppStatus{
		{Name: "System", Extra: []proto.ExtraPort{{Addr: "*:8721", Risky: true}, {Addr: "*:10000"}}},
		{Name: "Auth", Extra: []proto.ExtraPort{{Addr: "*:8080", Risky: true}, {Addr: "127.0.0.1:8720", Risky: true}, {Addr: "*:3306", Risky: true}}},
	}
	fw.start()
	if !fw.mark(out) {
		t.Fatal("有不宜对外的端口")
	}
	got := map[string]string{}
	for _, s := range out {
		for _, e := range s.Extra {
			got[e.Addr] = e.Firewall
		}
	}
	want := map[string]string{"*:8721": "blocked", "*:10000": "", "*:8080": "open", "127.0.0.1:8720": "", "*:3306": "partial"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
	for _, c := range ran {
		if strings.Contains(c, "--permanent") {
			t.Errorf("不该读永久配置：%s", c)
		}
	}

	// 没有不宜对外的端口：不查防火墙
	fw = &statusFW{scan: func() *fwState { t.Error("不该查防火墙"); return nil }}
	if fw.mark([]proto.AppStatus{{Extra: []proto.ExtraPort{{Addr: "127.0.0.1:8721", Risky: true}}}}) {
		t.Error("只监听本机的不算")
	}
}
