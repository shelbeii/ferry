package agent

import (
	"archive/zip"
	"bytes"
	"reflect"
	"testing"

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
