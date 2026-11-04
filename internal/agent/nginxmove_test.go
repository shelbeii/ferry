package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func movingNginxFixture(t *testing.T) (*fakeSys, *Agent, proto.SoftwareInstance, proto.SoftwareInstance, proto.AppSpec) {
	f, a, old, s := hostedFixture(t)
	next := old
	next.Name = "edge-next"
	next.Unit = "ferry-edge-next.service"
	next.Dir = filepath.Join(t.TempDir(), "next")
	h := *old.Hosting
	h.Config = filepath.Join(next.Dir, "nginx.conf")
	h.Sites = filepath.Join(next.Dir, "sites")
	h.Prefix = next.Dir
	next.Hosting = &h
	a.instanceLookup = func(name string) (*proto.SoftwareInstance, error) {
		if name == old.Name {
			return &old, nil
		}
		if name == next.Name {
			return &next, nil
		}
		return nil, os.ErrNotExist
	}
	f.unit(next.Unit).active = "active"
	os.WriteFile(filepath.Join(f.unitDir, next.Unit), []byte(ManagedMarker), 0644)
	req := f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{s.Name: []byte("v1")})
	result := execConfigured(t, a, "deploy", req)
	if result.code != 0 {
		t.Fatal(result.dump())
	}
	return f, a, old, next, s
}

func TestNginxMoveAndRuntimeRollback(t *testing.T) {
	f, a, old, next, s := movingNginxFixture(t)
	oldFile := a.nginxSitePath(&s, &old)
	oldData, _ := os.ReadFile(oldFile)
	s.NginxInstance = next.Name
	s.SitePort = "45124"
	s.Health.HTTP = "http://127.0.0.1:45124/"
	s.NginxConf = ManagedMarker + "\nserver { listen 45124; root /site; }\n"
	result := execConfigured(t, a, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{s.Name: []byte("v2")}))
	if result.code != 0 {
		t.Fatal(result.dump())
	}
	newFile := a.nginxSitePath(&s, &next)
	if exists(oldFile) || !exists(newFile) || a.readSiteBinding(s.Name).Instance != next.Name || f.current(s.Name) != id2 {
		t.Fatal("未完成迁移：", result.dump())
	}
	result = execConfigured(t, a, "rollback", &proto.Request{Apps: []proto.AppSpec{s}, To: id1})
	if result.code != 0 || exists(newFile) || a.readSiteBinding(s.Name).Instance != old.Name || f.current(s.Name) != id1 {
		t.Fatal("回滚应恢复原 Nginx：", result.dump())
	}
	if data, _ := os.ReadFile(oldFile); string(data) != string(oldData) {
		t.Fatal("原站点内容未恢复")
	}
	status := a.appStatus(&s, f.NginxDump)
	if status.Unit != old.Unit || status.Port != "45123" || status.Active != "active" {
		t.Fatal("应用列表应显示回滚后的原实例，而不是尚未部署的选择：", status)
	}
	info := decodeAppInfo(t, execConfigured(t, a, "appinfo", &proto.Request{Apps: []proto.AppSpec{s}}))
	if info.Status.Port != "45123" || infoValue(info, "Nginx 实例") != old.Name {
		t.Fatal("运行详情应显示回滚后的实际实例和端口：", info)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "stop "+old.Unit) || strings.HasPrefix(call, "restart "+old.Unit) || strings.HasPrefix(call, "stop "+next.Unit) || strings.HasPrefix(call, "restart "+next.Unit) {
			t.Fatal("不能启停共享实例：", call)
		}
	}
}

type failMoveOnce struct {
	*fakeSys
	cmd, unit string
	health    bool
	failed    bool
}

func (f *failMoveOnce) Shell(cmd string, d time.Duration) (string, error) {
	if cmd == f.cmd && !f.failed {
		f.failed = true
		return "bad config", errors.New("invalid nginx config")
	}
	return f.fakeSys.Shell(cmd, d)
}
func (f *failMoveOnce) Systemctl(args ...string) error {
	if len(args) == 2 && args[0] == "reload" && args[1] == f.unit && !f.failed {
		f.failed = true
		return errors.New("reload failed")
	}
	return f.fakeSys.Systemctl(args...)
}
func (f *failMoveOnce) Probe(h proto.Health) error {
	if f.health {
		f.failed = true
		return errors.New("new site unhealthy")
	}
	return f.fakeSys.Probe(h)
}

func TestNginxMoveFailuresRestoreOldInstance(t *testing.T) {
	for _, phase := range []string{"config", "reload", "health"} {
		t.Run(phase, func(t *testing.T) {
			f, a, old, next, s := movingNginxFixture(t)
			oldFile := a.nginxSitePath(&s, &old)
			before, _ := os.ReadFile(oldFile)
			broken := &failMoveOnce{fakeSys: f}
			switch phase {
			case "config":
				broken.cmd = nginxCheckCommand(next.Hosting) + " 2>&1"
			case "reload":
				broken.unit = next.Unit
			case "health":
				broken.health = true
			}
			a.Sys = broken
			s.NginxInstance = next.Name
			s.Health.TimeoutSec = 1
			result := execConfigured(t, a, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{s.Name: []byte("v2")}))
			if result.code == 0 || !broken.failed {
				t.Fatal("应触发迁移失败：", result.dump())
			}
			if f.current(s.Name) != id1 || a.readSiteBinding(s.Name).Instance != old.Name || exists(a.nginxSitePath(&s, &next)) {
				t.Fatal("失败后必须恢复原版本和原实例：", result.dump())
			}
			if data, _ := os.ReadFile(oldFile); string(data) != string(before) {
				t.Fatal("原站点配置未恢复")
			}
			var outcome struct {
				Apps []proto.AppOutcome `json:"apps"`
			}
			if json.Unmarshal(result.result.Data, &outcome) != nil || len(outcome.Apps) == 0 || outcome.Apps[0].Result != "ROLLED_BACK" {
				t.Fatal(result.dump())
			}
		})
	}
}
