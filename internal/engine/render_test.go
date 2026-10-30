package engine

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/artifact"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

func TestSdQuote(t *testing.T) {
	cases := map[string]string{
		"-Xmx512m":      "-Xmx512m",
		"a b":           `"a b"`,
		`say "hi"`:      `"say \"hi\""`,
		"100%":          "100%%",
		`C:\x`:          `"C:\\x"`,
		"--opt=$HOME/x": "--opt=$HOME/x",
	}
	for in, want := range cases {
		if got := sdQuote(in, false); got != want {
			t.Errorf("sdQuote(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestExampleShop(t *testing.T) {
	f, err := config.Load("../../examples/shop/ferry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("prod-a")
	gw := f.App("gateway")
	s, err := Spec(f, tg, gw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Unit != "shop-gateway.service" || s.User != "shop" || s.FileName != "ShopMall-gateway.jar" {
		t.Fatalf("%+v", s)
	}
	for _, want := range []string{
		"# managed by ferry",
		"Conflicts=shop-native-gateway.service",
		`Environment="SERVER_ADDRESS=0.0.0.0"`,
		"-jar /opt/shop/apps/Gateway/running/ShopMall-gateway.jar --spring.profiles.active=prod",
		"SyslogIdentifier=shop-Gateway",
		"ReadWritePaths=/opt/shop/apps /home/shop/app",
	} {
		if !strings.Contains(s.UnitText, want) {
			t.Errorf("单元文件缺少 %q：\n%s", want, s.UnitText)
		}
	}
	if strings.Contains(s.UnitText, "SPRING_CLOUD_NACOS_DISCOVERY_IP") || strings.Contains(s.UnitText, "${") {
		t.Errorf("Gateway 的 env 应覆盖，且不能残留变量：\n%s", s.UnitText)
	}
	sys, _ := Spec(f, tg, f.App("system"))
	if !strings.Contains(sys.UnitText, `Environment="SPRING_CLOUD_NACOS_DISCOVERY_IP=127.0.0.1"`) || sys.Health.TCP != "127.0.0.1:9201" {
		t.Errorf("System：%+v", sys)
	}
	if len(sys.SharedDirs) != 0 || len(sys.ArchiveLogs) != 0 || !strings.Contains(sys.UnitText, "-Dlogging.file.path=/opt/shop/apps/System/running/logs/ShopMall-system") {
		t.Errorf("日志应写在当前版本目录里：%v\n%s", sys.SharedDirs, sys.UnitText)
	}
	if sys.DiagLogs[0] != "/opt/shop/apps/System/running/logs/ShopMall-system/error.log" {
		t.Errorf("diagLogs：%v", sys.DiagLogs)
	}
}

func TestRelativeJavaRejected(t *testing.T) {
	f := &config.File{Project: "p", Dir: "."}
	tg := &config.Target{Name: "t", Root: "/opt/p", RunAs: "u", KeepReleases: 5}
	a := &config.App{Name: "a", Type: "java", Artifact: config.Artifact{Path: "a.jar"},
		Service: config.Service{Unit: "p-a.service", Java: "java"}}
	if _, err := Spec(f, tg, a); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("相对路径的 java 应报错：%v", err)
	}
}

func TestAppOwnRootInSpec(t *testing.T) {
	f := &config.File{Project: "p", Dir: "."}
	tg := &config.Target{Name: "t", Root: "/opt/p", RunAs: "u", KeepReleases: 5, Vars: map[string]string{"big": "/data/p"}}
	a := &config.App{Name: "Job", Type: "java", Root: "${big}", Artifact: config.Artifact{Path: "job.jar"},
		Service: config.Service{Unit: "p-job.service", Java: "/usr/bin/java", DiagLogs: []string{"logs/err.log"}}}
	s, err := Spec(f, tg, a)
	if err != nil {
		t.Fatal(err)
	}
	if s.Root != "/data/p" {
		t.Errorf("root：%q", s.Root)
	}
	if !strings.Contains(s.UnitText, "/data/p/apps/Job/running/job.jar") || !strings.Contains(s.UnitText, "WorkingDirectory=/data/p/apps/Job/running") {
		t.Errorf("单元文件应使用应用的部署目录：\n%s", s.UnitText)
	}
	if s.DiagLogs[0] != "/data/p/apps/Job/data/logs/err.log" {
		t.Errorf("diagLogs：%v", s.DiagLogs)
	}
	a.Root = ""
	if s, _ := Spec(f, tg, a); s.Root != "" || !strings.Contains(s.UnitText, "/opt/p/apps/Job/running/job.jar") {
		t.Errorf("不设置时用目标的 root：%q\n%s", s.Root, s.UnitText)
	}
}

func TestCheckJava(t *testing.T) {
	rep := &recRep{}
	e := &Engine{Rep: rep}
	it := &item{app: &config.App{Name: "Job"}, spec: proto.AppSpec{Name: "Job", Type: proto.TypeJava, Java: "/usr/bin/java"}, info: &artifact.Info{ClassMajor: 61}}
	p := &targetPlan{t: &config.Target{Name: "prod-a"}, pf: &proto.Preflight{
		Javas:     map[string]proto.Java{"/usr/bin/java": {Path: "/usr/bin/java", Version: "1.8.0_412", Major: 8}},
		JavaFound: []proto.Java{{Path: "/usr/lib/jvm/java-17/bin/java", Major: 17}, {Path: "/opt/jdk8/bin/java", Major: 8}},
	}}
	err := e.checkJava(p, it)
	if err == nil || !strings.Contains(err.Error(), "没有上传") {
		t.Fatalf("Java 8 跑不了 Java 17 的 jar：%v", err)
	}
	msg := strings.Join(rep.errors, "\n")
	if !strings.Contains(msg, "需要 Java 17 或更高") || !strings.Contains(msg, "是 Java 8（1.8.0_412）") ||
		!strings.Contains(msg, "修复：服务器上已有：/usr/lib/jvm/java-17/bin/java（Java 17）") || strings.Contains(msg, "/opt/jdk8") {
		t.Fatalf("说明：\n%s", msg)
	}
	p.pf.Javas["/usr/bin/java"] = proto.Java{Path: "/usr/bin/java", Major: 21, Version: "21.0.1"}
	if err := e.checkJava(p, it); err != nil {
		t.Fatalf("Java 21 可以：%v", err)
	}
}

// recRep 记录输出的 Reporter。
type recRep struct{ errors, warns []string }

func (r *recRep) Step(string, ...any)           {}
func (r *recRep) Info(string, ...any)           {}
func (r *recRep) Warn(f string, a ...any)       { r.warns = append(r.warns, fmt.Sprintf(f, a...)) }
func (r *recRep) Error(f string, a ...any)      { r.errors = append(r.errors, fmt.Sprintf(f, a...)) }
func (r *recRep) Event(string, proto.Event)     {}
func (r *recRep) Progress(string, int64, int64) {}
func (r *recRep) BuildOutput() io.Writer        { return io.Discard }
func (r *recRep) Confirm(string) bool           { return true }
func (r *recRep) Secret(string) (string, bool)  { return "", false }
