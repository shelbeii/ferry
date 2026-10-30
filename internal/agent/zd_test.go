package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// zdApp 不停机发布的应用：两个槽位的健康检查都按应用名判断（假系统里单元在运行、且属于这个应用就算通过）。
func zdApp(f *fakeSys, name string) proto.AppSpec {
	s := javaApp(name)
	b := strings.TrimSuffix(s.Unit, ".service") + "-b.service"
	h := proto.Health{TCP: "127.0.0.1:" + name, TimeoutSec: 20}
	s.ZD = &proto.ZeroDowntime{DrainSec: 5, Slots: [2]proto.Slot{
		{Name: "a", Port: 18101, Unit: s.Unit, UnitText: ManagedMarker + "\nExecStart=slots/a\n", Health: h},
		{Name: "b", Port: 18102, Unit: b, UnitText: ManagedMarker + "\nExecStart=slots/b\n", Health: h},
	}}
	s.UnitText, s.Health = s.ZD.Slots[0].UnitText, h
	f.appOf[b] = name
	return s
}

func (f *fakeSys) slot(t *testing.T, app string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.root, ".ferry", "apps", app, "slot"))
	var st zdState
	json.Unmarshal(b, &st)
	return st.Slot
}

// idx 调用记录里第一次出现 call 的位置（没有为 -1）。
func (f *fakeSys) idx(call string) int { return slices.Index(f.calls, call) }

func TestZeroDowntimeDeploy(t *testing.T) {
	f := newFake(t)
	s := zdApp(f, "Api")
	a, b := s.ZD.Slots[0].Unit, s.ZD.Slots[1].Unit

	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"Api": []byte("v1")}))
	if r.code != 0 || f.current("Api") != id1 || f.slot(t, "Api") != "a" || f.units[a].active != "active" || !f.units[a].enabled {
		t.Fatalf("首次部署用槽位 a：\n%s", r.dump())
	}
	if f.links[filepath.Join(f.root, "apps", "Api", "slots", "a")] != filepath.Join("..", "versions", id1) ||
		f.links[filepath.Join(f.root, "apps", "Api", "running")] != filepath.Join("versions", id1) {
		t.Fatalf("链接：%v", f.links)
	}
	if !realDir(filepath.Join(f.root, "apps", "Api", "versions", id1, "logs")) {
		t.Fatal("新版本启动前要准备好它的日志目录")
	}

	f.calls = nil
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{"Api": []byte("v2")}))
	if r.code != 0 || f.current("Api") != id2 || f.slot(t, "Api") != "b" {
		t.Fatalf("第二次用槽位 b：\n%s", r.dump())
	}
	if f.units[b].active != "active" || f.units[a].active != "inactive" || f.units[a].enabled || !f.units[b].enabled {
		t.Fatalf("新的在运行、旧的停了且不再开机自启：a=%+v b=%+v", f.units[a], f.units[b])
	}
	start, stop := f.idx("restart "+b), f.idx("stop "+a)
	if start < 0 || stop < 0 || start > stop {
		t.Fatalf("要先启动新版本再停旧版本：%v", f.calls)
	}
	if !r.has(proto.EvWait, "等 5 秒再停旧版本") {
		t.Fatalf("停旧版本前等 drain：\n%s", r.dump())
	}

	// 新版本起不来：停掉新的，旧的一直在运行
	f.unitBad[a] = true
	f.calls = nil
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id3, map[string][]byte{"Api": []byte("v3")}))
	if r.code != proto.CodeFailed || f.current("Api") != id2 || f.slot(t, "Api") != "b" || f.units[b].active != "active" {
		t.Fatalf("失败时旧版本照常：\n%s", r.dump())
	}
	if f.idx("stop "+b) >= 0 || !r.has(proto.EvRollback, "旧版本一直在运行，没有中断") {
		t.Fatalf("旧版本不能停：%v\n%s", f.calls, r.dump())
	}
	var out proto.DeployOutcome
	json.Unmarshal(r.result.Data, &out)
	if out.Apps[0].Result != "ROLLED_BACK" {
		t.Fatalf("%+v", out)
	}

	// 回滚：上一个可用版本 id1（id3 失败了）在空着的槽位 a 上启动
	delete(f.unitBad, a)
	r = exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{s}})
	if r.code != 0 || f.current("Api") != id1 || f.slot(t, "Api") != "a" || f.units[a].active != "active" || f.units[b].active != "inactive" {
		t.Fatalf("回滚：\n%s", r.dump())
	}

	// 状态、启停按正在用的槽位
	st := exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{s}})
	var sts []proto.AppStatus
	json.Unmarshal(st.result.Data, &sts)
	if len(sts) != 1 || sts[0].Unit != a || sts[0].Current == nil || sts[0].Current.ID != id1 {
		t.Fatalf("状态：%s", st.result.Data)
	}
}

func TestZeroDowntimeFromNormalAndBack(t *testing.T) {
	f := newFake(t)
	plain := javaApp("Api")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{plain}, id1, map[string][]byte{"Api": []byte("v1")})); r.code != 0 {
		t.Fatalf("\n%s", r.dump())
	}
	run := filepath.Join(f.root, "apps", "Api", "running")
	if !realDir(run) {
		t.Fatal("普通部署的 running 是目录")
	}

	// 打开不停机发布：旧版本在槽位 a 的单元里（以前普通部署的），新版本用槽位 b
	s := zdApp(f, "Api")
	a, b := s.ZD.Slots[0].Unit, s.ZD.Slots[1].Unit
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{"Api": []byte("v2")}))
	if r.code != 0 || f.slot(t, "Api") != "b" || f.units[b].active != "active" || f.units[a].active != "inactive" {
		t.Fatalf("从普通部署换过来：\n%s", r.dump())
	}
	if realDir(run) || !realDir(filepath.Join(f.root, "apps", "Api", "versions", id1)) || f.links[run] != filepath.Join("versions", id2) {
		t.Fatalf("running 换成链接、旧版本放回 versions：%v", f.links)
	}
	if f.idx("restart "+b) > f.idx("stop "+a) {
		t.Fatalf("先启动新的再停旧的：%v", f.calls)
	}

	// 去掉不停机发布：停掉两个槽位，删掉 b 的单元，按普通方式部署
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{plain}, id3, map[string][]byte{"Api": []byte("v3")}))
	if r.code != 0 || f.current("Api") != id3 || !realDir(run) || f.units[a].active != "active" || f.units[b].active != "inactive" {
		t.Fatalf("换回普通部署：\n%s", r.dump())
	}
	if _, err := os.Stat(filepath.Join(f.unitDir, b)); err == nil {
		t.Fatal("槽位 b 的单元文件应删掉")
	}
	if _, err := os.Stat(filepath.Join(f.root, ".ferry", "apps", "Api", "slot")); err == nil {
		t.Fatal("slot 记录应删掉")
	}
}

func TestZeroDowntimePortTaken(t *testing.T) {
	f := newFake(t)
	s := zdApp(f, "Api")
	// 槽位 a 的端口被别的程序占着：fake 里让 Probe 对它返回成功
	other := "other.service"
	f.units[other] = &fakeUnit{active: "active"}
	f.appOf[other] = "18101"
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"Api": []byte("v1")}))
	if r.code != proto.CodeFailed || !r.has(proto.EvFail, "端口 18101 已经被别的程序占用") {
		t.Fatalf("\n%s", r.dump())
	}
}
