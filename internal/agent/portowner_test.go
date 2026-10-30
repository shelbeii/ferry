package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// fakePorts 替换「谁在监听」：端口 → 进程号、单元、容器。
func fakePorts(t *testing.T, owners map[int][3]string) {
	t.Helper()
	oldL, oldO := listeningPorts, portOwner
	t.Cleanup(func() { listeningPorts, portOwner = oldL, oldO })
	listeningPorts = func() map[int]bool {
		m := map[int]bool{}
		for p := range owners {
			m[p] = true
		}
		return m
	}
	portOwner = func(port int) (int, string, string) {
		o, ok := owners[port]
		if !ok {
			return 0, "", ""
		}
		return 4321, o[1], o[2]
	}
}

func TestPortBusy(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root}}
	os.WriteFile(filepath.Join(f.unitDir, "shop-old.service"), []byte(ManagedMarker+"\n# project=shop app=Web target=t1\n[Unit]\n"), 0o644)
	fakePorts(t, map[int][3]string{
		8000: {"", "shop-old.service", ""},
		3306: {"", "mysqld.service", ""},
		6379: {"", "docker.service", ""},
		9000: {"", "", strings.Repeat("ab", 32)},
	})
	cases := map[int]string{
		8000: "Ferry 部署的应用 Web（项目 shop，服务 shop-old.service）占用",
		3306: "已被服务 mysqld.service 占用",
		6379: "Docker 容器映射占用",
		9000: "Docker 容器 abababababab 占用",
		8888: "",
	}
	for p, want := range cases {
		got := a.portBusy(p)
		if want == "" && got != "" || !strings.Contains(got, want) {
			t.Errorf("端口 %d：%q，期望含 %q", p, got, want)
		}
	}
	if got := a.portBusy(8000, "shop-old.service"); got != "" {
		t.Errorf("自己的单元占着不算：%q", got)
	}
	if got := a.inUseHints(&proto.AppSpec{Type: proto.TypeStatic}, []string{"[emerg] bind() to 0.0.0.0:8000 failed (98: Address already in use)"}, "x.service"); len(got) != 1 || !strings.Contains(got[0], "应用 Web") {
		t.Errorf("启动失败时指出占用者：%v", got)
	}
}

// 自带 nginx 的站点改用 nginx 实例：先停掉原来的服务，成功后删掉它
func TestStaticSwitchToInstanceRetiresUnit(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	old := "demo-web.service"
	os.WriteFile(filepath.Join(f.unitDir, old), []byte(ManagedMarker+"\n# project=demo app=web target=t1\n[Unit]\n"), 0o644)
	f.unit(old).active, f.unit(old).enabled = "active", true
	req := f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"web": []byte("v1")})
	req.FerryDir, req.Project = a.req.FerryDir, a.req.Project
	r := execConfigured(t, a, "deploy", req)
	if r.code != 0 {
		t.Fatalf("改用实例：%s", r.dump())
	}
	if exists(filepath.Join(f.unitDir, old)) || f.unit(old).active != "inactive" || f.unit(old).enabled {
		t.Fatalf("原来的服务应停用并删除：%v", f.calls)
	}
	if !exists(a.nginxSitePath(&s, &in)) {
		t.Fatal("站点应发布到实例")
	}
}

func TestStaticSwitchToInstanceFailureRestoresUnit(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	old := "demo-web.service"
	os.WriteFile(filepath.Join(f.unitDir, old), []byte(ManagedMarker+"\n# project=demo app=web target=t1\n[Unit]\n"), 0o644)
	f.unit(old).active = "active"
	f.shellFail["runuser -u 'svc' -- "+nginxCheckCommand(in.Hosting)+" 2>&1"] = true
	req := f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"web": []byte("v1")})
	req.FerryDir, req.Project = a.req.FerryDir, a.req.Project
	execConfigured(t, a, "deploy", req)
	if !exists(filepath.Join(f.unitDir, old)) || f.unit(old).active != "active" {
		t.Fatalf("失败时应恢复原来的服务：%v", f.calls)
	}
	if exists(a.nginxSitePath(&s, &in)) {
		t.Fatal("失败时实例里不应留下站点")
	}
}

// running/ 有程序却没有版本记录（目录从别的应用挪过来）：放回 versions/ 时起个名字，而不是报错
func TestActivateUnrecordedRunning(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root}}
	s := &proto.AppSpec{Name: "web", Type: proto.TypeStatic}
	os.MkdirAll(a.runningDir("web"), 0o755)
	os.WriteFile(filepath.Join(a.runningDir("web"), "index.html"), []byte("old"), 0o644)
	os.MkdirAll(filepath.Join(a.versionsDir("web"), id1), 0o755)
	if err := a.activate(s, id1); err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(a.versionsDir("web"), "20261015-100000-unknown", "index.html")
	if !exists(stored) || a.current("web") != id1 {
		t.Fatalf("旧的 running 应放到 %s，当前版本 %q", stored, a.current("web"))
	}
}
