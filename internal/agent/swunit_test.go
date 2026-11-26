package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func TestSwunitService(t *testing.T) {
	f := newFake(t)
	os.WriteFile(filepath.Join(f.unitDir, "mariadb.service"), []byte("[Service]\n"), 0o644)
	run := func(action string) run {
		return exec1(t, f, "swunit", &proto.Request{SwUnit: "mariadb.service", Action: action})
	}
	if r := run("start"); r.code != 0 || !r.has(proto.EvReady, "运行中") {
		t.Fatalf("启动：\n%s", r.dump())
	}
	if r := run("stop"); r.code != 0 || !r.has(proto.EvStop, "已停止") {
		t.Fatalf("停止：\n%s", r.dump())
	}
	// 起来又退出的要报出来，并带 journald 的最后几行
	f.unitBad["mariadb.service"] = true
	if r := run("restart"); r.code == 0 || !r.has(proto.EvDiag, "Address already in use") {
		t.Fatalf("起不来：\n%s", r.dump())
	}
	if r := run("logs"); r.code != 0 || !strings.Contains(string(r.result.Data), "Starting") {
		t.Fatalf("日志：\n%s", r.dump())
	}
	// Ferry 部署的应用的单元不在这里管；没有的服务、无效的名字拒绝
	os.WriteFile(filepath.Join(f.unitDir, "demo-api.service"), []byte(ManagedMarker+"\n"), 0o644)
	if r := exec1(t, f, "swunit", &proto.Request{SwUnit: "demo-api.service", Action: "stop"}); r.code == 0 || !strings.Contains(r.result.Msg, "应用") {
		t.Fatalf("应用的单元：\n%s", r.dump())
	}
	if r := exec1(t, f, "swunit", &proto.Request{SwUnit: "nope.service", Action: "start"}); r.code == 0 || !strings.Contains(r.result.Msg, "没有系统服务") {
		t.Fatalf("没有的服务：\n%s", r.dump())
	}
	for _, bad := range []proto.Request{
		{SwUnit: "x; rm -rf /.service", Action: "stop"},
		{SwContainer: "a b", Action: "stop"},
		{SwUnit: "a.service", SwContainer: "b", Action: "stop"},
		{SwUnit: "a.service", Action: "kill"},
	} {
		req := bad
		if r := exec1(t, f, "swunit", &req); r.code != proto.CodeBadInput {
			t.Errorf("%+v 应拒绝：%d %s", bad, r.code, r.result.Msg)
		}
	}
}

type flakyStartSys struct {
	*fakeSys
	crashAfter time.Duration
	startedAt  time.Time
	changePID  bool
}

func (s *flakyStartSys) Systemctl(args ...string) error {
	if err := s.fakeSys.Systemctl(args...); err != nil {
		return err
	}
	if len(args) >= 2 && (args[0] == "start" || args[0] == "restart") {
		s.startedAt = s.fakeSys.Now()
	}
	return nil
}

func (s *flakyStartSys) UnitProps(unit string) UnitProps {
	p := s.fakeSys.UnitProps(unit)
	if !s.startedAt.IsZero() && s.fakeSys.Now().Sub(s.startedAt) >= s.crashAfter {
		if s.changePID {
			p.PID++
		} else {
			p.Active, p.Sub, p.PID = "failed", "failed", 0
		}
	}
	return p
}

// Type=simple 服务重启瞬间返回 active/running，几百毫秒后退出或主进程号变化，必须判为没有起来。
func TestSwunitServiceDetectsImmediateCrash(t *testing.T) {
	for _, tc := range []struct {
		name      string
		changePID bool
	}{
		{"crash-to-failed", false},
		{"crash-loop-pid-changed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			os.WriteFile(filepath.Join(f.unitDir, "fakesvc.service"), []byte("[Service]\nExecStart=/bin/false\n"), 0o644)
			sys := &flakyStartSys{fakeSys: f, crashAfter: 300 * time.Millisecond, changePID: tc.changePID}
			var out bytes.Buffer
			a := &Agent{Sys: sys, Out: &out}
			req := &proto.Request{
				Proto:    proto.Version,
				Root:     f.root,
				FerryDir: filepath.Join(filepath.Dir(f.root), "ferry-lib"),
				Actor:    "tester@pc",
				SwUnit:   "fakesvc.service",
				Action:   "restart",
			}
			code := a.Run("swunit", req)
			if code == 0 || !strings.Contains(out.String(), "没有起来") {
				t.Fatalf("重启后瞬间退出应判失败（code=%d）：\n%s", code, out.String())
			}
		})
	}
}
