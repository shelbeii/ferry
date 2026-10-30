package engine

import (
	"io"
	"time"

	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/safe"
)

// TermIO 交互式终端的输入输出。
type TermIO struct {
	Cols, Rows int
	Stdin      io.Reader
	Stdout     io.Writer
	Resize     <-chan [2]int // 列、行
	// OnOpen shell 已启动：label 为 user@host:port
	OnOpen func(label string)
	// OnLatency 到服务器的往返时间（毫秒）：打开后马上量一次，之后每 LatencyEvery 量一次；slow 为超过 LatencyTimeout 还没回来
	OnLatency func(ms float64, slow bool)
}

// LatencyEvery 终端开着时多久量一次往返时间。
const LatencyEvery = 5 * time.Second

// Terminal 在目标上打开交互式终端：用配置里的登录方式（复用连接池里的连接），以登录用户启动 shell，
// 伪终端类型 xterm-256color。直到 shell 退出、连接断开或请求取消；返回 shell 的退出码。
func (e *Engine) Terminal(target string, tio TermIO) (int, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return -1, err
	}
	s, err := e.connect(t)
	if err != nil {
		return -1, err
	}
	defer s.close()
	stop := make(chan struct{})
	defer close(stop)
	code, err := s.c.Shell(e.ctx(), remote.ShellOptions{
		Term: "xterm-256color", Cols: tio.Cols, Rows: tio.Rows,
		Stdin: tio.Stdin, Stdout: tio.Stdout, Resize: tio.Resize,
		OnStart: func() {
			if tio.OnOpen != nil {
				tio.OnOpen(s.label)
			}
			if tio.OnLatency != nil {
				safe.Go("终端测延迟", func() { measureLatency(s.c, tio.OnLatency, stop) })
			}
		},
	})
	if err != nil && err != remote.ErrCanceled {
		s.broken.Store(true)
	}
	return code, err
}

// measureLatency 终端开着时定时量到服务器的往返时间，直到 stop 关闭。
func measureLatency(c *remote.Client, report func(ms float64, slow bool), stop <-chan struct{}) {
	t := time.NewTicker(LatencyEvery)
	defer t.Stop()
	for {
		d, err := c.Latency(LatencyTimeout, 0)
		select {
		case <-stop:
			return
		default:
		}
		switch {
		case err == remote.ErrSlow:
			report(0, true)
		case err == nil:
			report(latencyMs(d), false)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}
