package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// autostartDelay：开机自启交给定时器（开机后等一会儿再启动），服务本身不 enable；停止、启动、去掉延迟都跟着改。
func TestBootDelay(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	sys.BootDelaySec = 90
	timer := strings.TrimSuffix(sys.Unit, ".service") + ".timer"
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	b, err := os.ReadFile(filepath.Join(f.unitDir, timer))
	if err != nil || !strings.HasPrefix(string(b), ManagedMarker) || !strings.Contains(string(b), "OnBootSec=90\n") ||
		!strings.Contains(string(b), "Unit="+sys.Unit+"\n") || !strings.Contains(string(b), "WantedBy=timers.target\n") {
		t.Fatalf("定时器：%v\n%s", err, b)
	}
	if f.units[sys.Unit].enabled || !f.units[timer].enabled || f.units[sys.Unit].active != "active" || f.units[timer].active == "active" {
		t.Fatalf("服务在运行、不 enable，定时器 enable（不马上启动）：%+v %+v", f.units[sys.Unit], f.units[timer])
	}
	if st := appState(t, f, sys); st.Autostart == nil || !*st.Autostart || st.BootDelaySec != 90 {
		t.Fatalf("状态里是开机自启：%+v", st)
	}

	// 停止：定时器也关掉；启动后恢复
	exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "stop"})
	if f.units[timer].enabled || f.units[sys.Unit].enabled {
		t.Fatalf("停止后开机不自启：%+v", f.units[timer])
	}
	if st := appState(t, f, sys); st.Autostart == nil || *st.Autostart || !st.Stopped {
		t.Fatalf("停止后的状态：%+v", st)
	}
	exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "start"})
	if !f.units[timer].enabled || f.units[sys.Unit].enabled {
		t.Fatalf("启动后恢复：%+v", f.units[timer])
	}

	// 去掉延迟：定时器删掉，服务自己开机自启
	sys.BootDelaySec = 0
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	if _, err := os.Stat(filepath.Join(f.unitDir, timer)); err == nil || !f.units[sys.Unit].enabled || f.units[timer].enabled {
		t.Fatalf("去掉延迟：%v %+v", err, f.units[sys.Unit])
	}

	// 已有不是 Ferry 生成的同名定时器：不覆盖，改成开机马上启动
	os.WriteFile(filepath.Join(f.unitDir, timer), []byte("[Timer]\nOnCalendar=daily\n"), 0o644)
	sys.BootDelaySec = 30
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, "20261016-120000-ccc3", map[string][]byte{"System": []byte("v3")}))
	if r.code != 0 || !f.units[sys.Unit].enabled || !strings.Contains(r.dump(), "不是 Ferry 生成的") {
		t.Fatalf("不覆盖别人的定时器：%s", r.dump())
	}
	if b, _ := os.ReadFile(filepath.Join(f.unitDir, timer)); !strings.Contains(string(b), "OnCalendar=daily") {
		t.Fatalf("被覆盖了：%s", b)
	}
}

// 告警：开机自启交给定时器的照样检查；开机后还在等的那段时间里没起来不报。
func TestMonitorBootDelay(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root, FerryDir: t.TempDir()}}
	os.WriteFile(filepath.Join(f.unitDir, "shop-api.service"), []byte(ManagedMarker+" — 由 ferry 生成\n# project=shop app=api target=prod bootDelay=60\n[Service]\n"), 0o644)
	os.WriteFile(filepath.Join(f.unitDir, "shop-api.timer"), []byte(ManagedMarker+" — 由 ferry 生成\n[Timer]\nOnBootSec=60\n"), 0o644)
	f.units["shop-api.service"] = &fakeUnit{active: "inactive", sub: "dead"}
	f.units["shop-api.timer"] = &fakeUnit{enabled: true}
	f.shellOut = map[string]string{"systemctl is-enabled 'shop-api.service' 2>/dev/null": "disabled\n"}
	entries := []monitorEntry{{MonitorSpec: proto.MonitorSpec{Project: "shop", Target: "prod"}, Root: f.root}}
	check := func(up time.Duration) int {
		old := sinceBoot
		sinceBoot = func() (time.Duration, bool) { return up, true }
		defer func() { sinceBoot = old }()
		out, _ := a.monitorCheck(entries, &monitorState{UnitSince: map[string]string{}})
		n := 0
		for _, x := range out {
			if x.Key == "unit:shop-api.service" {
				n++
			}
		}
		return n
	}
	if check(90*time.Second) != 0 {
		t.Fatal("开机 90 秒、还在等（60 秒 + 2 分钟）：不报")
	}
	if check(10*time.Minute) != 1 {
		t.Fatal("开机 10 分钟还没起来：要报")
	}
	f.units["shop-api.timer"].enabled = false // 手动关掉了
	if check(10*time.Minute) != 0 {
		t.Fatal("定时器也关了（不开机自启）：不报")
	}
}
