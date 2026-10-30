package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/notify"
	"github.com/captain/ferry/internal/proto"
)

func TestMonitorAlertsOnChange(t *testing.T) {
	f := newFake(t)
	ferryDir := t.TempDir()
	unit := "shop-api.service"
	os.WriteFile(filepath.Join(f.unitDir, unit), []byte(ManagedMarker+" — 由 ferry 生成\n# project=shop app=api target=prod\n[Service]\n"), 0o644)
	os.WriteFile(filepath.Join(f.unitDir, "other.service"), []byte("[Service]\n"), 0o644) // 不是 Ferry 的：不管
	f.shellOut = map[string]string{"systemctl is-enabled 'shop-api.service' 2>/dev/null": "enabled\n"}
	f.units[unit] = &fakeUnit{active: "active", sub: "running", pid: 10}
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root, FerryDir: ferryDir}}

	// 打开监控
	a.req.Monitor = &proto.MonitorSpec{Action: "on", Project: "shop", Target: "prod", Channels: []proto.MonitorChannel{{Name: "群", Kind: "webhook", URL: "http://x"}}}
	if rc := a.monitor(); rc != 0 {
		t.Fatal("打开监控失败")
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "enable --now ferry-monitor.timer") {
		t.Fatalf("要装上定时器：%v", f.calls)
	}

	var sent []notify.Event
	send := func(_ []proto.MonitorChannel, ev notify.Event) map[string]string {
		sent = append(sent, ev)
		return map[string]string{"群": ""}
	}
	a.monitorRun(send)
	if len(sent) != 0 {
		t.Fatalf("一切正常时不发：%+v", sent)
	}

	// 应用停了：发一次告警；再检查还是停着：不再发
	f.units[unit].active, f.units[unit].sub = "failed", "failed"
	a.monitorRun(send)
	a.monitorRun(send)
	if len(sent) != 1 || sent[0].Action != "alert" || !strings.Contains(sent[0].Lines[0], "应用 api 停了") {
		t.Fatalf("%+v", sent)
	}
	// 恢复：发一次恢复
	f.units[unit].active, f.units[unit].sub = "active", "running"
	a.monitorRun(send)
	if len(sent) != 2 || sent[1].Action != "recovered" || !strings.Contains(sent[1].Lines[0], "已恢复") {
		t.Fatalf("%+v", sent)
	}

	// 不是部署引起的重启：一次性的提醒
	_, st := a.loadMonitor()
	st.UnitSince[unit] = "Sun 2026-10-25 10:00:00 CST"
	a.saveMonitor(nil, &st)
	a.monitorRun(send) // fake 的 Since 是空的：不算
	if len(sent) != 2 {
		t.Fatalf("没有启动时间时不判断重启：%+v", sent)
	}
	// 启动时间只精确到秒：同一秒里重启过，按主进程号看出来
	f.units[unit].since, f.units[unit].pid = "Sun 2026-10-25 10:00:00 CST", 100
	_, st = a.loadMonitor()
	st.UnitSince[unit] = "Sun 2026-10-25 10:00:00 CST" // 老版本记的只有启动时间：时间没变，不算
	a.saveMonitor(nil, &st)
	a.monitorRun(send)
	if len(sent) != 2 {
		t.Fatalf("老版本的记录：%+v", sent)
	}
	f.units[unit].pid = 101
	a.monitorRun(send)
	if len(sent) != 3 || !strings.Contains(sent[2].Lines[0], "应用 api 重启过") {
		t.Fatalf("同一秒里重启：%+v", sent)
	}
	a.monitorRun(send)
	if len(sent) != 3 {
		t.Fatalf("重启只提醒一次：%+v", sent)
	}

	// 状态：开着、发到哪
	a.req.Monitor = &proto.MonitorSpec{Action: "status", Project: "shop", Target: "prod"}
	cfg, st := a.loadMonitor()
	s := a.monitorStatus(cfg, st, "shop/prod")
	if !s.On || s.IntervalMin != 5 || s.Channels[0] != "群" || s.LastRun.IsZero() {
		t.Fatalf("%+v", s)
	}

	// 关掉：定时器也停掉（没有别的项目开着）
	a.req.Monitor = &proto.MonitorSpec{Action: "off", Project: "shop", Target: "prod"}
	a.monitor()
	if !strings.Contains(strings.Join(f.calls, "\n"), "disable --now ferry-monitor.timer") {
		t.Fatalf("%v", f.calls)
	}
}

func TestManagedUnitsHeader(t *testing.T) {
	f := newFake(t)
	os.WriteFile(filepath.Join(f.unitDir, "e2e-zd-b.service"), []byte(ManagedMarker+" — x\n# project=e2e app=zd target=e2e-a\n"), 0o644)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root}}
	if u := a.managedUnits()["e2e-zd-b.service"]; u != [3]string{"e2e", "zd", "e2e-a"} {
		t.Fatal(u)
	}
}

func TestExposed(t *testing.T) {
	for _, c := range []struct {
		p    proto.PortInfo
		want bool
	}{
		{proto.PortInfo{Listen: []string{"*"}}, true},
		{proto.PortInfo{Listen: []string{"127.0.0.1"}}, false},
		{proto.PortInfo{Listen: []string{"*"}, Firewall: "blocked"}, false},
		{proto.PortInfo{Listen: []string{"10.0.0.5"}, Internal: true}, false},
		{proto.PortInfo{Listen: []string{"*"}, Docker: true, Guard: true}, false},
		{proto.PortInfo{}, false},
	} {
		if exposed(c.p) != c.want {
			t.Errorf("%+v", c.p)
		}
	}
}
