package engine

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

func TestSplitBatches(t *testing.T) {
	for _, c := range []struct {
		n, canary, batch int
		want             [][]int
	}{
		{3, 0, 1, [][]int{{0}, {1}, {2}}},
		{4, 0, 2, [][]int{{0, 1}, {2, 3}}},
		{5, 0, 2, [][]int{{0, 1}, {2, 3}, {4}}},
		{4, 1, 2, [][]int{{0}, {1, 2}, {3}}},
		{3, 1, 5, [][]int{{0}, {1, 2}}},
		{2, 2, 1, [][]int{{0}, {1}}}, // 先发的不少于全部：不单独一批
		{1, 0, 0, [][]int{{0}}},
	} {
		if got := splitBatches(c.n, c.canary, c.batch); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitBatches(%d,%d,%d) = %v，应为 %v", c.n, c.canary, c.batch, got, c.want)
		}
	}
}

func TestRolloutOf(t *testing.T) {
	obs := config.Duration(2 * time.Minute)
	e := &Engine{File: &config.File{Rollout: &config.Rollout{Batch: 3, Canary: 1, Observe: &obs, OnFailure: "rollback"}}}
	r := e.rolloutOf(RolloutOptions{})
	if r != (rolloutPlan{batch: 3, canary: 1, observe: 2 * time.Minute, rollback: true}) {
		t.Errorf("用配置：%+v", r)
	}
	zero := 0
	if r := e.rolloutOf(RolloutOptions{Batch: 1, Canary: &zero, OnFailure: "stop"}); r != (rolloutPlan{batch: 1}) {
		t.Errorf("命令行覆盖配置（不先发时也不观察）：%+v", r)
	}
	if d := r.describe(4); d != "先切换 1 台，观察 2 分钟，确认后再部署其余的；每次 3 台同时切换；有一台失败时把这次已经切换的回滚到部署前的版本" {
		t.Errorf("说明：%s", d)
	}
	if d := (&Engine{File: &config.File{}}).rolloutOf(RolloutOptions{}).describe(3); d != "" {
		t.Errorf("默认不用说明：%q", d)
	}
	bad := -1
	if CheckRollout(RolloutOptions{Canary: &bad}) == nil || CheckRollout(RolloutOptions{OnFailure: "retry"}) == nil || CheckRollout(RolloutOptions{Batch: 2}) != nil {
		t.Error("参数检查")
	}
}

// logRep 记下全部输出；answer 不为空时能中途问用户。
type logRep struct {
	mu     sync.Mutex
	lines  []string
	answer *bool
	asked  []string
}

func (r *logRep) add(kind, f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, kind+" "+fmt.Sprintf(f, a...))
}
func (r *logRep) Step(f string, a ...any)  { r.add("step", f, a...) }
func (r *logRep) Info(f string, a ...any)  { r.add("info", f, a...) }
func (r *logRep) Warn(f string, a ...any)  { r.add("warn", f, a...) }
func (r *logRep) Error(f string, a ...any) { r.add("error", f, a...) }
func (r *logRep) Event(_ string, ev proto.Event) {
	r.add("event", "%s %s", ev.App, ev.Msg)
}
func (r *logRep) Progress(string, int64, int64) {}
func (r *logRep) BuildOutput() io.Writer        { return io.Discard }
func (r *logRep) Confirm(string) bool           { return false }
func (r *logRep) Secret(string) (string, bool)  { return "", false }
func (r *logRep) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

type askRep struct{ *logRep }

func (r askRep) Ask(q string) (bool, bool) {
	r.mu.Lock()
	r.asked = append(r.asked, q)
	r.mu.Unlock()
	return *r.answer, true
}

// fakeFleet n 台服务器；fail 里的切换失败，unhealthy 里的观察时不健康。记下切换、回滚的顺序和同时在切换的最多几台。
type fakeFleet struct {
	mu                  sync.Mutex
	switched, reverted  []string
	running, maxRunning int
	fail, unhealthy     map[string]bool
	revertFail          map[string]bool
}

func (f *fakeFleet) install(t *testing.T) {
	s, h, r := switchOne, healthOf, revertOne
	t.Cleanup(func() { switchOne, healthOf, revertOne = s, h, r })
	switchOne = func(e *Engine, p *targetPlan, _ uploaded, id string, _ DeployGit, _ string, _ DeployOptions) (*DeployReport, error) {
		f.mu.Lock()
		f.running++
		f.maxRunning = max(f.maxRunning, f.running)
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		f.mu.Lock()
		f.running--
		f.switched = append(f.switched, p.t.Name)
		f.mu.Unlock()
		rep := &DeployReport{Target: p.t.Name, Release: id, Apps: []proto.AppOutcome{{App: "api", Result: "OK"}}}
		if f.fail[p.t.Name] {
			rep.Apps[0].Result = "ROLLED_BACK"
			return rep, errors.New("api 没有就绪")
		}
		return rep, nil
	}
	healthOf = func(e *Engine, p *targetPlan) string {
		if f.unhealthy[p.t.Name] {
			return "api 健康检查不通过"
		}
		return ""
	}
	revertOne = func(e *Engine, p *targetPlan, r *DeployReport, rep Reporter) []string {
		f.mu.Lock()
		f.reverted = append(f.reverted, p.t.Name)
		f.mu.Unlock()
		if f.revertFail[p.t.Name] {
			return []string{"api@" + p.t.Name}
		}
		r.Apps[0].Result, r.Reverted = "REVERTED", true
		return nil
	}
}

func fleet(names ...string) []*targetPlan {
	var out []*targetPlan
	for _, n := range names {
		out = append(out, &targetPlan{t: &config.Target{Name: n}, s: &session{}, before: map[string]string{"api": "old"}})
	}
	return out
}

func results(rs []*DeployReport) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Target+"="+r.Apps[0].Result)
	}
	return strings.Join(out, " ")
}

func TestSwitchAll(t *testing.T) {
	run := func(f *fakeFleet, cfg *config.Rollout, rep Reporter, yes bool, names ...string) ([]*DeployReport, error) {
		f.install(t)
		plans := fleet(names...)
		e := &Engine{File: &config.File{Rollout: cfg}, Rep: rep, Yes: yes}
		return e.switchAll(plans, make([]uploaded, len(plans)), "r1", DeployGit{}, t.TempDir(), DeployOptions{})
	}

	// 默认：一台一台，第二台失败就停，第三台不部署，第一台不回滚
	f := &fakeFleet{fail: map[string]bool{"b": true}}
	rep := &logRep{}
	rs, err := run(f, nil, rep, true, "a", "b", "c")
	if err == nil || !strings.HasPrefix(err.Error(), "b：") || results(rs) != "a=OK b=ROLLED_BACK" || len(f.reverted) != 0 || f.maxRunning != 1 {
		t.Fatalf("默认：%v %s %v", err, results(rs), f.reverted)
	}
	if !strings.Contains(rep.text(), "error b 失败，未部署：c") || rs[1].Error == "" {
		t.Errorf("输出：%s", rep.text())
	}

	// 失败时回滚：已切换成功的 a 回滚，c 不部署
	f = &fakeFleet{fail: map[string]bool{"b": true}}
	rs, err = run(f, &config.Rollout{OnFailure: "rollback"}, &logRep{}, true, "a", "b", "c")
	if err == nil || results(rs) != "a=REVERTED b=ROLLED_BACK" || !reflect.DeepEqual(f.reverted, []string{"a"}) || !rs[0].Reverted {
		t.Fatalf("回滚：%v %s %v", err, results(rs), f.reverted)
	}

	// 分批：每次 2 台同时切换，耗时约等于两批
	f = &fakeFleet{}
	start := time.Now()
	rs, err = run(f, &config.Rollout{Batch: 2}, &logRep{}, true, "a", "b", "c", "d")
	if err != nil || len(rs) != 4 || f.maxRunning != 2 || time.Since(start) > 70*time.Millisecond {
		t.Fatalf("分批：%v %d 台，最多同时 %d，用了 %s", err, len(rs), f.maxRunning, time.Since(start))
	}

	// 同一批两台都失败、回滚：同批成功的也回滚
	f = &fakeFleet{fail: map[string]bool{"c": true, "d": true}}
	rs, err = run(f, &config.Rollout{Batch: 2, OnFailure: "rollback"}, &logRep{}, true, "a", "b", "c", "d", "e")
	if err == nil || !strings.Contains(err.Error(), "c、d 失败") || len(f.reverted) != 2 || results(rs) != "a=REVERTED b=REVERTED c=ROLLED_BACK d=ROLLED_BACK" {
		t.Fatalf("同批失败：%v %s %v", err, results(rs), f.reverted)
	}

	// 回滚又失败一台：写清楚要人工处理
	f = &fakeFleet{fail: map[string]bool{"c": true}, revertFail: map[string]bool{"a": true}}
	rep = &logRep{}
	run(f, &config.Rollout{OnFailure: "rollback"}, rep, true, "a", "b", "c")
	if !strings.Contains(rep.text(), "要人工处理：api@a") {
		t.Errorf("回滚失败：%s", rep.text())
	}

	// 先发一台：问用户，不继续 → 其余不部署；配了回滚时先发的也回滚
	no, yes := false, true
	f = &fakeFleet{}
	ar := askRep{&logRep{answer: &no}}
	rs, err = run(f, &config.Rollout{Canary: 1, OnFailure: "rollback"}, ar, true, "a", "b", "c")
	if !errors.Is(err, errCanaryStopped) || len(f.switched) != 1 || !reflect.DeepEqual(f.reverted, []string{"a"}) || len(ar.asked) != 1 ||
		!strings.Contains(ar.asked[0], "先发的 a 已上线。继续部署剩下的 b、c？") {
		t.Fatalf("先发后不继续：%v %v %v %v", err, f.switched, f.reverted, ar.asked)
	}
	// 继续 → 其余的分批部署
	f = &fakeFleet{}
	ar = askRep{&logRep{answer: &yes}}
	if rs, err = run(f, &config.Rollout{Canary: 1, Batch: 2}, ar, true, "a", "b", "c"); err != nil || len(rs) != 3 || f.maxRunning != 2 {
		t.Fatalf("先发后继续：%v %d %d", err, len(rs), f.maxRunning)
	}
	// 问不了、--yes：观察完自动继续；问不了、没有 --yes：Confirm（这里回答不）
	f = &fakeFleet{}
	if rs, err = run(f, &config.Rollout{Canary: 1}, &logRep{}, true, "a", "b"); err != nil || len(rs) != 2 {
		t.Fatalf("--yes 不暂停：%v", err)
	}
	f = &fakeFleet{}
	if _, err = run(f, &config.Rollout{Canary: 1}, &logRep{}, false, "a", "b"); !errors.Is(err, errCanaryStopped) {
		t.Fatalf("没确认：%v", err)
	}

	// 观察期间不健康：当作失败
	old := observeTick
	observeTick = 5 * time.Millisecond
	t.Cleanup(func() { observeTick = old })
	obs := config.Duration(30 * time.Millisecond)
	f = &fakeFleet{unhealthy: map[string]bool{"a": true}}
	rs, err = run(f, &config.Rollout{Canary: 1, Observe: &obs, OnFailure: "rollback"}, &logRep{}, true, "a", "b")
	if err == nil || !strings.Contains(err.Error(), "a 观察期间出了问题：api 健康检查不通过") || len(f.switched) != 1 || len(f.reverted) != 1 {
		t.Fatalf("观察不健康：%v %v %v", err, f.switched, f.reverted)
	}
	f = &fakeFleet{}
	rep = &logRep{}
	if rs, err = run(f, &config.Rollout{Canary: 1, Observe: &obs}, rep, true, "a", "b"); err != nil || !strings.Contains(rep.text(), "a 观察结束，观察了 0 秒，一直正常") {
		t.Fatalf("观察正常：%v\n%s", err, rep.text())
	}

	// 单台：和以前一样
	f = &fakeFleet{fail: map[string]bool{"a": true}}
	rs, err = run(f, &config.Rollout{OnFailure: "rollback", Canary: 1}, &logRep{}, true, "a")
	if err == nil || err.Error() != "a：api 没有就绪" || len(f.reverted) != 0 {
		t.Fatalf("单台：%v %v", err, f.reverted)
	}
}

// 同时切换时 agent 的事件带上服务器名
func TestTargetReporterEvent(t *testing.T) {
	rep := &logRep{}
	tr := &targetReporter{Reporter: rep, target: "b"}
	tr.Event("b", proto.Event{App: "api", Msg: "就绪"})
	tr.Event("b", proto.Event{Msg: "加锁"})
	if got := rep.text(); got != "event api@b 就绪\nevent  b：加锁" {
		t.Errorf("%q", got)
	}
}
