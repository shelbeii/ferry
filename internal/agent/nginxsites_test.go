package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func hostedFixture(t *testing.T) (*fakeSys, *Agent, proto.SoftwareInstance, proto.AppSpec) {
	t.Helper()
	f := newFake(t)
	dir := t.TempDir()
	fd := filepath.Join(dir, "ferry")
	ng := filepath.Join(dir, "nginx")
	in := proto.SoftwareInstance{Name: "edge", Kind: "nginx", Title: "Nginx", Dir: ng, Unit: "ferry-edge.service", User: "svc", Hosting: &proto.NginxHosting{Binary: filepath.Join(dir, "nginx-bin"), Config: filepath.Join(ng, "nginx.conf"), Prefix: ng, Sites: filepath.Join(ng, "sites")}}
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root, FerryDir: fd, Project: "demo"}}
	a.instanceLookup = func(name string) (*proto.SoftwareInstance, error) {
		if name != in.Name {
			return nil, os.ErrNotExist
		}
		return &in, nil
	}
	os.MkdirAll(a.swDir(), 0o755)
	b, _ := json.Marshal(in)
	os.WriteFile(a.instFile(in.Name), b, 0o644)
	os.WriteFile(filepath.Join(f.unitDir, in.Unit), []byte(ManagedMarker), 0o644)
	f.unit(in.Unit).active = "active"
	s := proto.AppSpec{Name: "web", Type: proto.TypeStatic, NginxInstance: in.Name, SitePort: "45123", NginxConf: ManagedMarker + "\nserver { listen 45123; root /site; }\n", Health: proto.Health{HTTP: "http://127.0.0.1:45123/"}, KeepReleases: 3}
	return f, a, in, s
}

func TestHostedNginxDeployRollbackAndSiteControl(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	deploy := func(id, content string) run {
		req := f.stage(t, []proto.AppSpec{s}, id, map[string][]byte{"web": []byte(content)})
		req.FerryDir = a.req.FerryDir
		req.Project = a.req.Project
		return execConfigured(t, a, "deploy", req)
	}
	if r := deploy(id1, "v1"); r.code != 0 {
		t.Fatalf("首次发布：%s", r.dump())
	}
	file := a.nginxSitePath(&s, &in)
	if !exists(file) {
		t.Fatal("没有安装共享 nginx 站点配置")
	}
	if r := deploy(id2, "v2"); r.code != 0 {
		t.Fatalf("二次发布：%s", r.dump())
	}
	r := execConfigured(t, a, "rollback", &proto.Request{FerryDir: a.req.FerryDir, Project: a.req.Project, Apps: []proto.AppSpec{s}, To: id1})
	if r.code != 0 || f.current(s.Name) != id1 {
		t.Fatalf("站点回滚：%s", r.dump())
	}
	r = execConfigured(t, a, "control", &proto.Request{FerryDir: a.req.FerryDir, Project: a.req.Project, Apps: []proto.AppSpec{s}, Action: "stop"})
	if r.code != 0 || exists(file) {
		t.Fatalf("停用站点：%s", r.dump())
	}
	if f.unit(in.Unit).active != "active" {
		t.Fatal("停用站点不能停止共用 nginx")
	}
	r = execConfigured(t, a, "control", &proto.Request{FerryDir: a.req.FerryDir, Project: a.req.Project, Apps: []proto.AppSpec{s}, Action: "start"})
	if r.code != 0 || !exists(file) {
		t.Fatalf("启用站点：%s", r.dump())
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "stop "+in.Unit) || strings.HasPrefix(call, "restart "+in.Unit) {
			t.Fatal("部署站点不得重启共用实例")
		}
	}
}

func TestHostedNginxValidationFailureKeepsSite(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	os.MkdirAll(a.verMetaDir(s.Name, id1), 0o755)
	os.WriteFile(a.verFile(s.Name, id1, "nginx.conf"), []byte(s.NginxConf), 0o644)
	if err := a.publishNginxSite(&s, id1); err != nil {
		t.Fatal(err)
	}
	file := a.nginxSitePath(&s, &in)
	before, _ := os.ReadFile(file)
	os.MkdirAll(a.verMetaDir(s.Name, id2), 0o755)
	os.WriteFile(a.verFile(s.Name, id2, "nginx.conf"), []byte(ManagedMarker+"\nbroken;"), 0o644)
	f.shellFail["runuser -u 'svc' -- "+nginxCheckCommand(in.Hosting)+" 2>&1"] = true
	if err := a.publishNginxSite(&s, id2); err == nil {
		t.Fatal("校验失败不能发布")
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("校验失败必须保留原站点配置")
	}
}

func TestHostedNginxUnitAllowsFutureWebPorts(t *testing.T) {
	u := swUnitText(&proto.SoftwarePlan{Name: "edge", User: "nginx", Ports: []int{8081}, Hosting: &proto.NginxHosting{}})
	if !strings.Contains(u, "ExecReload=/bin/kill -HUP $MAINPID") || !strings.Contains(u, "AmbientCapabilities=CAP_NET_BIND_SERVICE") {
		t.Fatal("共享实例应允许后续站点监听 80、443，并支持重新加载")
	}
}

type failNginxReloadOnce struct {
	*fakeSys
	failed bool
}

func (f *failNginxReloadOnce) Systemctl(args ...string) error {
	if len(args) > 0 && args[0] == "reload" && !f.failed {
		f.failed = true
		return os.ErrPermission
	}
	return f.fakeSys.Systemctl(args...)
}

func TestHostedNginxReloadFailureRestoresSite(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	for _, id := range []string{id1, id2} {
		os.MkdirAll(a.verMetaDir(s.Name, id), 0o755)
		os.WriteFile(a.verFile(s.Name, id, "nginx.conf"), []byte(s.NginxConf+"# "+id), 0o644)
	}
	if err := a.publishNginxSite(&s, id1); err != nil {
		t.Fatal(err)
	}
	file := a.nginxSitePath(&s, &in)
	before, _ := os.ReadFile(file)
	a.Sys = &failNginxReloadOnce{fakeSys: f}
	if err := a.publishNginxSite(&s, id2); err == nil {
		t.Fatal("重新加载失败应报告失败")
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("重新加载失败应还原原站点文件")
	}
	if f.calls[len(f.calls)-1] != "reload "+in.Unit {
		t.Fatal("还原后应重新加载原站点配置")
	}
}

func TestSystemNginxSiteDeployAndStopWithoutInstallRecord(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("原生系统 nginx 路径需要 Linux")
	}
	f, a, in, s := hostedFixture(t)
	os.Remove(a.instFile(in.Name))
	a.instanceLookup = func(string) (*proto.SoftwareInstance, error) { return nil, os.ErrNotExist }
	in.Name, in.Unit, in.User = proto.SystemNginxInstance, "nginx.service", "root"
	in.Hosting.Binary = filepath.Join(in.Dir, "nginx")
	in.Hosting.System = true
	s.NginxInstance = in.Name
	f.unit(in.Unit).active = "active"
	f.shellOut = map[string]string{
		"systemctl show nginx.service -p LoadState -p ExecStart -p User --no-pager":                                           fmt.Sprintf("LoadState=loaded\nUser=\nExecStart={ path=%s ; argv[]=%s -p %s/ -c %s ; }\n", in.Hosting.Binary, in.Hosting.Binary, in.Dir, in.Hosting.Config),
		shellQuote(in.Hosting.Binary) + " -V 2>&1":                                                                            "nginx version: nginx/1.20.1\nconfigure arguments: --user=nginx",
		shellQuote(in.Hosting.Binary) + " -T -p " + shellQuote(in.Dir+"/") + " -c " + shellQuote(in.Hosting.Config) + " 2>&1": "# configuration file " + in.Hosting.Config + ":\nuser root; http { include " + in.Hosting.Sites + "/*.conf; }\n",
	}
	req := f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"web": []byte("native site")})
	req.FerryDir, req.Project = a.req.FerryDir, a.req.Project
	if r := execConfigured(t, a, "deploy", req); r.code != 0 {
		t.Fatalf("原生 nginx 发布：%s", r.dump())
	}
	file := a.nginxSitePath(&s, &in)
	if !exists(file) {
		t.Fatal("应发布独立站点片段")
	}
	if r := execConfigured(t, a, "control", &proto.Request{FerryDir: a.req.FerryDir, Project: a.req.Project, Apps: []proto.AppSpec{s}, Action: "stop"}); r.code != 0 {
		t.Fatalf("原生 nginx 停用站点：%s", r.dump())
	}
	if exists(file) || f.unit(in.Unit).active != "active" {
		t.Fatal("只能移除本站点配置，nginx 主服务应继续运行")
	}
	for _, c := range f.calls {
		if c == "stop nginx.service" || c == "restart nginx.service" {
			t.Fatal("发布与停用站点不能停止或重启系统 nginx")
		}
	}
}
