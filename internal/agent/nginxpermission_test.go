package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 模拟服务由 systemd 授予低端口能力、普通 runuser 没有该能力的服务器。
type nginxPermissionSys struct {
	*fakeSys
	checks    []string
	owners    map[string][2]int
	badHealth bool
}

func (f *nginxPermissionSys) Shell(cmd string, timeout time.Duration) (string, error) {
	if strings.Contains(cmd, " -t ") {
		if strings.HasPrefix(cmd, "runuser ") {
			return "nginx: [emerg] bind() to 0.0.0.0:81 failed (13: Permission denied)", os.ErrPermission
		}
		f.checks = append(f.checks, cmd)
	}
	return f.fakeSys.Shell(cmd, timeout)
}

func (f *nginxPermissionSys) LookupUser(name string) (int, int, error) {
	if name == "nextsvc" {
		return 2000, 2000, nil
	}
	return f.fakeSys.LookupUser(name)
}

func (f *nginxPermissionSys) Lchown(file string, uid, gid int) error {
	f.owners[file] = [2]int{uid, gid}
	return f.fakeSys.Lchown(file, uid, gid)
}

func (f *nginxPermissionSys) Probe(h proto.Health) error {
	if f.badHealth {
		return errors.New("new site unhealthy")
	}
	return f.fakeSys.Probe(h)
}

func TestSwitchToNginxWithPrivilegedWelcomePort(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	// 站点本身使用高端口，但实例已有的欢迎页监听 81，整个主配置检查也会测试它。
	os.MkdirAll(in.Dir, 0755)
	os.WriteFile(in.Hosting.Config, []byte("http { server { listen 81; } }"), 0644)
	sys := &nginxPermissionSys{fakeSys: f, owners: map[string][2]int{}}
	a.Sys = sys
	old := "demo-web.service"
	os.WriteFile(filepath.Join(f.unitDir, old), []byte(ManagedMarker+"\n# project=demo app=web target=t1\n[Unit]\n"), 0644)
	f.unit(old).active = "active"
	r := execConfigured(t, a, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{s.Name: []byte("site")}))
	if r.code != 0 || exists(a.unitPath(old)) || a.readSiteBinding(s.Name).Instance != in.Name {
		t.Fatalf("应能从自带 nginx 切换到低端口共享实例：%s", r.dump())
	}
	if len(sys.checks) != 1 || sys.checks[0] != nginxCheckCommand(in.Hosting)+" 2>&1" {
		t.Fatalf("必须以 agent 身份校验所选实例的完整主配置：%v", sys.checks)
	}
	for _, name := range []string{"access.log", "error.log"} {
		file := filepath.Join(a.sharedDir(s.Name), "logs", name)
		if !exists(file) || sys.owners[file] != [2]int{1000, 1000} {
			t.Fatalf("配置测试前应准备运行用户的日志：%s, %v", file, sys.owners)
		}
	}
	for _, call := range f.calls {
		if call == "stop "+in.Unit || call == "restart "+in.Unit {
			t.Fatal("不能启停共享 nginx：", call)
		}
	}
}

func TestNginxMoveRollbackRestoresLogOwner(t *testing.T) {
	f, a, old, next, s := movingNginxFixture(t)
	next.User = "nextsvc"
	a.instanceLookup = func(name string) (*proto.SoftwareInstance, error) {
		if name == old.Name {
			return &old, nil
		}
		if name == next.Name {
			return &next, nil
		}
		return nil, os.ErrNotExist
	}
	sys := &nginxPermissionSys{fakeSys: f, owners: map[string][2]int{}, badHealth: true}
	a.Sys = sys
	s.NginxInstance = next.Name
	s.Health.TimeoutSec = 1
	r := execConfigured(t, a, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{s.Name: []byte("v2")}))
	if r.code == 0 || !r.has(proto.EvRollback, "已恢复 Nginx "+old.Name) || a.readSiteBinding(s.Name).Instance != old.Name || f.current(s.Name) != id1 {
		t.Fatalf("健康检查失败应恢复原实例和版本：%s", r.dump())
	}
	if len(sys.checks) < 4 {
		t.Fatal("迁移和恢复时都应使用具有低端口权限的校验：", sys.checks)
	}
	for _, name := range []string{"", "access.log", "error.log"} {
		file := filepath.Join(a.sharedDir(s.Name), "logs", name)
		if sys.owners[file] != [2]int{1000, 1000} {
			t.Fatalf("回滚后日志应属于原实例用户：%s, %v", file, sys.owners)
		}
	}
}

func TestNginxSiteLogsPreserveContentAndRejectNonFiles(t *testing.T) {
	f, a, _, s := hostedFixture(t)
	a.Sys = &nginxPermissionSys{fakeSys: f, owners: map[string][2]int{}}
	logs := filepath.Join(a.sharedDir(s.Name), "logs")
	os.MkdirAll(logs, 0755)
	file := filepath.Join(logs, "access.log")
	os.WriteFile(file, []byte("existing requests\n"), 0640)
	if err := a.prepareNginxSiteLogs(&s, "svc"); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != "existing requests\n" {
		t.Fatal("准备日志不能截断已有内容")
	}
	os.Remove(file)
	os.Mkdir(file, 0755)
	if err := a.prepareNginxSiteLogs(&s, "svc"); err == nil {
		t.Fatal("特殊日志路径必须在 root 配置校验前拒绝：", err)
	}
}

func TestFailedNginxSwitchStatusUsesRestoredStandalone(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	old := s
	old.NginxInstance, old.Unit, old.User = "", "demo-web.service", "svc"
	old.RuntimeNginx = "nginx"
	old.UnitText = ManagedMarker + "\n# project=demo app=web target=t1\n[Service]\nExecStart=/opt/nginx -c /opt/nginx.conf\n"
	if r := execConfigured(t, a, "deploy", f.stage(t, []proto.AppSpec{old}, id1, map[string][]byte{s.Name: []byte("old site")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	f.shellFail[nginxCheckCommand(in.Hosting)+" 2>&1"] = true
	s.SitePort, s.Health.HTTP = "45124", "http://127.0.0.1:45124/"
	s.NginxConf = ManagedMarker + "\nserver { listen 45124; root /site; }\n"
	r := execConfigured(t, a, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{s.Name: []byte("new site")}))
	if r.code == 0 || !r.has(proto.EvRollback, "仍由原来自带的 nginx 运行") || f.current(s.Name) != id1 || f.unit(old.Unit).active != "active" {
		t.Fatalf("应恢复旧版本和原来的 nginx 服务：%s", r.dump())
	}
	st := a.appStatus(&s, f.NginxDump)
	if st.Unit != old.Unit || st.Active != "active" || st.Port != "45123" || st.Healthy != nil {
		t.Fatalf("应显示原服务及实际端口，不使用新选择的健康检查：%+v", st)
	}
	if s.NginxInstance != in.Name || s.Health.HTTP != "http://127.0.0.1:45124/" {
		t.Fatal("查询状态不能改动待部署的配置")
	}
}
