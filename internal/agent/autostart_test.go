package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func appState(t *testing.T, f *fakeSys, s proto.AppSpec) proto.AppStatus {
	t.Helper()
	r := exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{s}})
	var st []proto.AppStatus
	json.Unmarshal(r.result.Data, &st)
	return st[0]
}

// 手动停止：同时取消开机自启、记下是手动停的；启动、部署后恢复。
func TestStopDisablesAutostart(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	st := appState(t, f, sys)
	if !f.units[sys.Unit].enabled || st.Autostart == nil || !*st.Autostart || st.Stopped {
		t.Fatalf("部署后开机自启：%+v", st)
	}
	if r := exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "stop"}); r.code != 0 {
		t.Fatal(r.dump())
	}
	st = appState(t, f, sys)
	if f.units[sys.Unit].enabled || !st.Stopped || st.Autostart == nil || *st.Autostart {
		t.Fatalf("停止后不再开机自启：%+v", st)
	}
	if r := exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "start"}); r.code != 0 {
		t.Fatal(r.dump())
	}
	if st = appState(t, f, sys); !f.units[sys.Unit].enabled || st.Stopped {
		t.Fatalf("启动后恢复：%+v", st)
	}
	// 停着的时候部署：部署完在运行，开机自启、标记都恢复
	exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "stop"})
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	if st = appState(t, f, sys); !f.units[sys.Unit].enabled || st.Stopped || f.units[sys.Unit].active != "active" {
		t.Fatalf("部署后恢复：%+v", st)
	}
	// 停着的时候部署失败：回滚后还是停着、开机不自启
	exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "stop"})
	f.bad["20261016-120000-ccc3"] = true
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, "20261016-120000-ccc3", map[string][]byte{"System": []byte("v3")})); r.code == 0 {
		t.Fatal("应该失败")
	}
	if st = appState(t, f, sys); f.units[sys.Unit].enabled || !st.Stopped {
		t.Fatalf("回滚后保持手动停止：%+v enabled=%v", st, f.units[sys.Unit].enabled)
	}
}

// autostart: false：部署、启动后都不开机自启（以前开着的也关掉）。
func TestNoAutostart(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	sys.NoAutostart = true
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	if f.units[sys.Unit].enabled || f.units[sys.Unit].active != "active" {
		t.Fatalf("在运行、不开机自启：%+v", f.units[sys.Unit])
	}
	for _, action := range []string{"restart", "start"} {
		if r := exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: action}); r.code != 0 || f.units[sys.Unit].enabled {
			t.Fatalf("%s：%v\n%s", action, f.units[sys.Unit].enabled, r.dump())
		}
	}
}

// 告警：手动停的、不停机发布空着的槽位不报；autostart: false 的没开机自启也要报。
func TestMonitorStoppedAndNoAutostart(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root, FerryDir: t.TempDir()}}
	write := func(unit, header string) {
		os.WriteFile(filepath.Join(f.unitDir, unit), []byte(ManagedMarker+" — 由 ferry 生成\n"+header+"\n[Service]\n"), 0o644)
		f.units[unit] = &fakeUnit{active: "inactive", sub: "dead"}
	}
	write("shop-api.service", "# project=shop app=api target=prod")
	write("shop-job.service", "# project=shop app=job target=prod autostart=no")
	write("shop-web.service", "# project=shop app=web target=prod")
	write("shop-zd-a.service", "# project=shop app=zd target=prod")
	write("shop-zd-b.service", "# project=shop app=zd target=prod")
	f.shellOut = map[string]string{
		"systemctl is-enabled 'shop-api.service' 2>/dev/null":  "disabled\n",
		"systemctl is-enabled 'shop-job.service' 2>/dev/null":  "disabled\n",
		"systemctl is-enabled 'shop-web.service' 2>/dev/null":  "enabled\n",
		"systemctl is-enabled 'shop-zd-a.service' 2>/dev/null": "enabled\n",
		"systemctl is-enabled 'shop-zd-b.service' 2>/dev/null": "enabled\n",
	}
	a.markStopped("web", true)
	os.MkdirAll(filepath.Join(f.root, ".ferry", "apps", "zd"), 0o755)
	os.WriteFile(filepath.Join(f.root, ".ferry", "apps", "zd", "slot"), []byte(`{"slot":"b","units":["shop-zd-a.service","shop-zd-b.service"]}`), 0o644)
	f.units["shop-zd-b.service"].active, f.units["shop-zd-b.service"].sub = "active", "running"

	entries := []monitorEntry{{MonitorSpec: proto.MonitorSpec{Project: "shop", Target: "prod"}, Root: f.root}}
	out, _ := a.monitorCheck(entries, &monitorState{UnitSince: map[string]string{}})
	var got []string
	for _, x := range out {
		if strings.HasPrefix(x.Key, "unit:") {
			got = append(got, x.Key)
		}
	}
	if strings.Join(got, ",") != "unit:shop-job.service" {
		t.Fatalf("只报 job（autostart: false 停了）：%v", got)
	}
}
