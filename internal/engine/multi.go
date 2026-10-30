package engine

import (
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/captain/ferry/internal/gitinfo"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/safe"
)

// uploaded 一个目标上传的结果：交给 agent 部署的请求、实际上传 / 全量字节、是否增量。
type uploaded struct {
	req       *proto.Request
	up, total int64
	delta     bool
	started   time.Time
	err       error
}

// uploadAll 同时上传到各目标（每台用自己的连接）：网络慢、服务器多时省下的时间最多。
// 只有一台时照常输出；多台时各台的输出前面带上服务器名，进度合成一个（各台加起来）。
func (e *Engine) uploadAll(plans []*targetPlan, id string, git gitinfo.Info, tmp string, delta bool) []uploaded {
	out := make([]uploaded, len(plans))
	started := time.Now()
	if len(plans) == 1 {
		p := plans[0]
		req, up, total, err := e.upload(p.s, p.items, id, git, filepath.Join(tmp, p.t.Name), delta)
		out[0] = uploaded{req: req, up: up, total: total, delta: delta, started: started, err: err}
		return out
	}
	e.Rep.Step("同时上传到 %d 台服务器", len(plans))
	prog := &mergedProgress{rep: e.Rep, done: make([]int64, len(plans)), total: make([]int64, len(plans))}
	var wg sync.WaitGroup
	for i, p := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Recover("上传到 "+p.t.Name, &out[i].err)
			ec := *e
			ec.Rep = &targetReporter{Reporter: e.Rep, target: p.t.Name, prog: prog, i: i}
			req, up, total, err := ec.upload(p.s, p.items, id, git, filepath.Join(tmp, p.t.Name), delta)
			out[i] = uploaded{req: req, up: up, total: total, delta: delta, started: started, err: err}
		}()
	}
	wg.Wait()
	prog.finish()
	return out
}

// dropStages 不切换的目标：删掉已经上传的文件（删不掉也没关系，服务器 12 小时后自己清理）。
func (e *Engine) dropStages(plans []*targetPlan, ups []uploaded) {
	for i, u := range ups {
		if u.req != nil && u.req.Stage != "" {
			plans[i].s.c.RemoveAll(u.req.Stage)
		}
	}
}

// targetReporter 同时上传时某一台的输出：前面带服务器名，进度交给 mergedProgress 合成一个。
type targetReporter struct {
	Reporter
	target string
	prog   *mergedProgress
	i      int
}

func (r *targetReporter) Step(format string, args ...any) {
	r.Reporter.Step("%s：%s", r.target, fmt.Sprintf(format, args...))
}
func (r *targetReporter) Info(format string, args ...any) {
	r.Reporter.Info("%s：%s", r.target, fmt.Sprintf(format, args...))
}
func (r *targetReporter) Warn(format string, args ...any) {
	r.Reporter.Warn("%s：%s", r.target, fmt.Sprintf(format, args...))
}
func (r *targetReporter) Error(format string, args ...any) {
	r.Reporter.Error("%s：%s", r.target, fmt.Sprintf(format, args...))
}
func (r *targetReporter) Progress(_ string, done, total int64) { r.prog.set(r.i, done, total) }
func (r *targetReporter) BuildOutput() io.Writer               { return r.Reporter.BuildOutput() }

// mergedProgress 各台的上传进度加起来，作为一个进度输出（最多每 100 毫秒一次）。
type mergedProgress struct {
	mu          sync.Mutex
	rep         Reporter
	done, total []int64
	last        time.Time
}

func (m *mergedProgress) set(i int, done, total int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.done[i], m.total[i] = done, total
	if time.Since(m.last) < 100*time.Millisecond {
		return
	}
	m.last = time.Now()
	var d, t int64
	for j := range m.done {
		d += m.done[j]
		t += m.total[j]
	}
	if t > 0 && d < t {
		m.rep.Progress(fmt.Sprintf("%d 台服务器", len(m.done)), d, t)
	}
}

func (m *mergedProgress) finish() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t int64
	for _, x := range m.total {
		t += x
	}
	m.rep.Progress("", t, t)
}
