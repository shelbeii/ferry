package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/safe"
)

// 一次部署到多台服务器时怎么切换（ferry.yaml 的 rollout，命令行、部署对话框可以覆盖）：
// 分批（同一批同时切换）、先切换几台后暂停确认（可以先观察一会儿）、有一台失败时停下或把这次已经切换的回滚。
// 上传在这之前已经同时做完（uploadAll）；不写 rollout 时和以前一样：一台一台切换，一台失败就停。

// RolloutOptions 这次部署的切换方式；没给的项用 ferry.yaml 的 rollout。
type RolloutOptions struct {
	Batch     int            // 一次切换几台；0 = 用配置
	Canary    *int           // 先切换几台；nil = 用配置
	Observe   *time.Duration // 先切换的观察多久；nil = 用配置
	OnFailure string         // stop | rollback；空 = 用配置
}

// Asker 能在部署中途问用户、等回答的输出（桌面版：弹窗；回答经 ferry serve 的 input 交回）。
// asked 为 false：问不了（没有回答的通道）。
type Asker interface {
	Ask(prompt string) (yes, asked bool)
}

// rolloutPlan 合并后的切换方式。
type rolloutPlan struct {
	batch, canary int
	observe       time.Duration
	rollback      bool
}

// RolloutOf 这次的切换方式：选项优先，其次 ferry.yaml 的 rollout，都没有 = 一台一台、不暂停、失败就停。
func (e *Engine) rolloutOf(o RolloutOptions) rolloutPlan {
	r := rolloutPlan{batch: 1}
	if c := e.File.Rollout; c != nil {
		if c.Batch > 0 {
			r.batch = c.Batch
		}
		r.canary = c.Canary
		if c.Observe != nil {
			r.observe = c.Observe.Std()
		}
		r.rollback = c.OnFailure == config.OnFailureRollback
	}
	if o.Batch > 0 {
		r.batch = o.Batch
	}
	if o.Canary != nil {
		r.canary = *o.Canary
	}
	if o.Observe != nil {
		r.observe = *o.Observe
	}
	if o.OnFailure != "" {
		r.rollback = o.OnFailure == config.OnFailureRollback
	}
	if r.canary <= 0 {
		r.canary, r.observe = 0, 0
	}
	return r
}

// RolloutText 部署到 n 台时的切换方式（给人看；和默认一样时为空）。
func (e *Engine) RolloutText(o RolloutOptions, n int) string { return e.rolloutOf(o).describe(n) }

// RolloutCanary 部署到 n 台时会不会先发几台、暂停确认。
func (e *Engine) RolloutCanary(o RolloutOptions, n int) bool {
	r := e.rolloutOf(o)
	return r.canary > 0 && r.canary < n
}

// CheckRollout 命令行、部署对话框给的切换方式对不对。
func CheckRollout(o RolloutOptions) error {
	switch {
	case o.Batch < 0 || o.Batch > config.RolloutMaxBatch:
		return fmt.Errorf("一次切换几台：写 1～%d", config.RolloutMaxBatch)
	case o.Canary != nil && (*o.Canary < 0 || *o.Canary > config.RolloutMaxBatch):
		return fmt.Errorf("先切换几台：写 0～%d", config.RolloutMaxBatch)
	case o.Observe != nil && (*o.Observe < 0 || *o.Observe > config.RolloutMaxObserve):
		return errors.New("观察多久：写 0～1h，如 2m")
	case o.OnFailure != "" && o.OnFailure != config.OnFailureStop && o.OnFailure != config.OnFailureRollback:
		return fmt.Errorf("失败时：写 stop 或 rollback（当前 %q）", o.OnFailure)
	}
	return nil
}

// describe 给人看的切换方式（n 台）；和默认（一台一台、失败就停）一样时为空。
func (r rolloutPlan) describe(n int) string {
	var parts []string
	if r.canary > 0 && r.canary < n {
		s := fmt.Sprintf("先切换 %d 台", r.canary)
		if r.observe > 0 {
			s += "，观察 " + humanDuration(r.observe)
		}
		parts = append(parts, s+"，确认后再部署其余的")
	}
	if r.batch > 1 {
		parts = append(parts, fmt.Sprintf("每次 %d 台同时切换", r.batch))
	}
	if r.rollback {
		parts = append(parts, "有一台失败时把这次已经切换的回滚到部署前的版本")
	}
	return strings.Join(parts, "；")
}

// splitBatches n 台分批：先 canary 台（>0 且少于 n 时单独一批），其余每批 batch 台。
func splitBatches(n, canary, batch int) [][]int {
	if batch < 1 {
		batch = 1
	}
	var out [][]int
	i := 0
	if canary > 0 && canary < n {
		b := make([]int, canary)
		for j := range b {
			b[j] = j
		}
		out = append(out, b)
		i = canary
	}
	for i < n {
		end := min(i+batch, n)
		b := make([]int, 0, end-i)
		for j := i; j < end; j++ {
			b = append(b, j)
		}
		out = append(out, b)
		i = end
	}
	return out
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	case d >= time.Minute:
		return fmt.Sprintf("%d 分 %d 秒", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%d 秒", int(d/time.Second))
}

// errCanaryStopped 先发的上线后用户选了不继续。
var errCanaryStopped = errors.New("先发的已上线，没有继续部署其余的")

// switchAll 按切换方式把已上传的切换上线。reports 按目标的顺序（没切换的不在里面）。
func (e *Engine) switchAll(plans []*targetPlan, ups []uploaded, id string, git DeployGit, tmp string, o DeployOptions) ([]*DeployReport, error) {
	rp := e.rolloutOf(o.Rollout)
	n := len(plans)
	batches := [][]int{{0}}
	if n > 1 {
		batches = splitBatches(n, rp.canary, rp.batch)
		if d := rp.describe(n); d != "" {
			e.Rep.Info("切换方式：%s", d)
		}
	}
	reports := make([]*DeployReport, n)
	errs := make([]error, n)
	collect := func() []*DeployReport {
		var out []*DeployReport
		for _, r := range reports {
			if r != nil {
				out = append(out, r)
			}
		}
		return out
	}
	var tried []int // 已经切换过（成功或失败）的
	for bi, b := range batches {
		e.switchBatch(plans, ups, b, id, git, tmp, o, reports, errs)
		tried = append(tried, b...)
		var failed []string
		var first error
		for _, i := range b {
			if errs[i] != nil {
				failed = append(failed, plans[i].t.Name)
				if first == nil {
					first = fmt.Errorf("%s：%w", plans[i].t.Name, errs[i])
				}
				if reports[i] != nil {
					reports[i].Error = errs[i].Error()
				}
			}
		}
		var stop error
		head := strings.Join(failed, "、") + " 失败"
		switch {
		case first != nil:
			stop = first
			if len(failed) > 1 {
				stop = fmt.Errorf("%s（%v）", head, first)
			}
		case bi == 0 && rp.canary > 0 && len(batches) > 1:
			if err := e.canaryGate(plans, b, rest(plans, batches[1:]), rp); err != nil {
				stop, head = err, "先发的之后停下了"
			}
		}
		if stop == nil {
			continue
		}
		if left := rest(plans, batches[bi+1:]); len(left) > 0 {
			e.Rep.Error("%s，未部署：%s", head, strings.Join(left, "、"))
			for _, nb := range batches[bi+1:] {
				for _, i := range nb {
					e.dropStages(plans[i:i+1], ups[i:i+1])
				}
			}
		}
		if rp.rollback && n > 1 {
			e.revertSwitched(plans, tried, reports)
		}
		return collect(), stop
	}
	return collect(), nil
}

// rest 这几批的服务器名。
func rest(plans []*targetPlan, batches [][]int) []string {
	var out []string
	for _, b := range batches {
		for _, i := range b {
			out = append(out, plans[i].t.Name)
		}
	}
	return out
}

// switchBatch 同一批同时切换（只有一台时照常输出；多台时各台的输出前面带上服务器名）。
func (e *Engine) switchBatch(plans []*targetPlan, ups []uploaded, b []int, id string, git DeployGit, tmp string, o DeployOptions, reports []*DeployReport, errs []error) {
	if len(b) == 1 {
		i := b[0]
		reports[i], errs[i] = switchOne(e, plans[i], ups[i], id, git, filepath.Join(tmp, plans[i].t.Name), o)
		return
	}
	names := make([]string, len(b))
	for k, i := range b {
		names[k] = plans[i].t.Name
	}
	e.Rep.Step("同时切换 %s", strings.Join(names, "、"))
	prog := &mergedProgress{rep: e.Rep, done: make([]int64, len(b)), total: make([]int64, len(b))}
	var wg sync.WaitGroup
	for k, i := range b {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Recover("切换 "+plans[i].t.Name, &errs[i])
			ec := *e
			ec.Rep = &targetReporter{Reporter: e.Rep, target: plans[i].t.Name, prog: prog, i: k}
			defer plans[i].s.useEngine(&ec)()
			reports[i], errs[i] = switchOne(&ec, plans[i], ups[i], id, git, filepath.Join(tmp, plans[i].t.Name), o)
		}()
	}
	wg.Wait()
	prog.finish()
}

// observeTick 观察期间多久查一次。
var observeTick = 10 * time.Second

// canaryGate 先切换的那几台上线后：观察一会儿（健康检查一直要通过），再问要不要继续。
func (e *Engine) canaryGate(plans []*targetPlan, first []int, left []string, rp rolloutPlan) error {
	var names []string
	for _, i := range first {
		names = append(names, plans[i].t.Name)
	}
	who := strings.Join(names, "、")
	note := ""
	if rp.observe > 0 {
		e.Rep.Step("观察 %s %s：健康检查一直要通过", who, humanDuration(rp.observe))
		end := time.Now().Add(rp.observe)
		for {
			for _, i := range first {
				if p := healthOf(e, plans[i]); p != "" {
					return fmt.Errorf("%s 观察期间出了问题：%s", plans[i].t.Name, p)
				}
			}
			left := time.Until(end)
			if left <= 0 {
				break
			}
			select {
			case <-time.After(min(left, observeTick)):
			case <-e.ctx().Done():
				return ErrCanceled
			}
		}
		note = "，观察了 " + humanDuration(rp.observe) + "，一直正常"
		e.Rep.Info("%s 观察结束%s", who, note)
	}
	q := fmt.Sprintf("先发的 %s 已上线%s。继续部署剩下的 %s？", who, note, strings.Join(left, "、"))
	if a, ok := e.Rep.(Asker); ok {
		if yes, asked := a.Ask(q); asked {
			if !yes {
				return errCanaryStopped
			}
			return nil
		}
	}
	if e.Yes {
		e.Rep.Info("先发的 %s 已上线%s，接着部署剩下的（--yes 不暂停）", who, note)
		return nil
	}
	if !e.Rep.Confirm(q) {
		return errCanaryStopped
	}
	return nil
}

// unhealthy 这台上这次部署的应用有没有问题（没在运行、健康检查不通过）；读不到状态也算。
func (e *Engine) unhealthy(p *targetPlan) string {
	specs := make([]proto.AppSpec, 0, len(p.items))
	for _, it := range p.items {
		specs = append(specs, it.spec)
	}
	var st []proto.AppStatus
	if err := p.s.callData("status", &proto.Request{Apps: specs}, &st); err != nil {
		return "读不到状态：" + err.Error()
	}
	for _, a := range st {
		var sp *proto.AppSpec
		for k := range specs {
			if specs[k].Name == a.Name {
				sp = &specs[k]
			}
		}
		if sp == nil || sp.Job != nil || sp.Unit == "" {
			continue // 定时任务、没有服务的静态站点：没有一直在运行的东西可看
		}
		if a.Active != "active" {
			return fmt.Sprintf("%s 没在运行（%s）", a.Name, orDefault(a.Active, "未知"))
		}
		if a.Healthy != nil && !*a.Healthy {
			return a.Name + " 健康检查不通过"
		}
	}
	return ""
}

// revertSwitched 有一台失败（或先发的之后不继续）：把这次已经切换上线的应用回滚到部署前的版本，让各台保持同一个版本。
// 各台同时回滚；回滚不了的（第一次部署、回滚失败）写清楚，要人工处理。
func (e *Engine) revertSwitched(plans []*targetPlan, tried []int, reports []*DeployReport) {
	var todo []int
	for _, i := range tried {
		if r := reports[i]; r != nil {
			for _, a := range r.Apps {
				if a.Result == "OK" {
					todo = append(todo, i)
					break
				}
			}
		}
	}
	if len(todo) == 0 {
		return
	}
	e.Rep.Step("把这次已经切换的回滚到部署前的版本")
	var mu sync.Mutex
	var manual []string
	var wg sync.WaitGroup
	for _, i := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			defer safe.Recover("回滚 "+plans[i].t.Name, &err)
			rep := e.Rep
			if len(todo) > 1 {
				ec := *e
				ec.Rep = &targetReporter{Reporter: e.Rep, target: plans[i].t.Name, prog: &mergedProgress{rep: e.Rep, done: []int64{0}, total: []int64{0}}}
				rep = ec.Rep
				defer plans[i].s.useEngine(&ec)()
			}
			bad := revertOne(e, plans[i], reports[i], rep)
			mu.Lock()
			manual = append(manual, bad...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(manual) > 0 {
		e.Rep.Error("这几个没能回滚，要人工处理：%s", strings.Join(manual, "；"))
	}
}

// revertTarget 一台上：这次切换成功的应用逐个回滚到部署前在运行的版本。返回没能回滚的。
func (e *Engine) revertTarget(p *targetPlan, r *DeployReport, rep Reporter) []string {
	var bad []string
	for k := range r.Apps {
		a := &r.Apps[k]
		if a.Result != "OK" {
			continue
		}
		where := a.App + "@" + p.t.Name
		to := p.before[a.App]
		if to == "" {
			rep.Warn("%s：这次是第一次部署（之前没有版本），留着新版本", a.App)
			bad = append(bad, where+" 没有部署前的版本")
			continue
		}
		var sp proto.AppSpec
		for _, it := range p.items {
			if it.app.Name == a.App {
				sp = it.spec
			}
		}
		rep.Info("回滚 %s 到 %s", a.App, to)
		var out proto.AppOutcome
		if err := p.s.callData("rollback", &proto.Request{Apps: []proto.AppSpec{sp}, To: to}, &out); err != nil {
			rep.Error("回滚 %s 失败：%v", a.App, err)
			bad = append(bad, fmt.Sprintf("%s（手动执行 ferry rollback %s %s --to %s）", where, p.t.Name, a.App, to))
			continue
		}
		a.Result, a.Msg = "REVERTED", "部署成功了，因为别的服务器失败，已回滚到部署前的版本 "+to
		r.Reverted = true
	}
	return bad
}

// useEngine 这个连接上 agent 报的事件交给 ec（同时切换几台时，事件前面带上服务器名）；返回的函数换回原来的。
func (s *session) useEngine(ec *Engine) func() {
	old := s.e
	s.e = ec
	return func() { s.e = old }
}

// 测试时换成假的：切换一台、看一台的状态、回滚一台。
var (
	switchOne = (*Engine).switchTarget
	healthOf  = (*Engine).unhealthy
	revertOne = (*Engine).revertTarget
)
