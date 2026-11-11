package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/safe"
)

// 常驻的 agent（协议见 proto/serve.go）：每条连接上开一个 ferry-agent serve，只读的查看都交给它。
// 单独运行一次 agent 要新开会话、等服务器答应执行，才把请求发过去，网络往返 3～4 次（经 sudo 时还要再走一次 sudo）；
// 交给常驻的只要发一行、收结果，1 次往返。启动不了或中途退出时，照原来的办法单独运行，结果一样。

// serveRetryAfter 常驻的 agent 启动失败后，这么久以内不再试（一直单独运行）。
const serveRetryAfter = 5 * time.Minute

// agentServer 一条连接上常驻的 agent。
type agentServer struct {
	pipe   *remote.Pipe
	wmu    sync.Mutex // 写请求
	seq    atomic.Int64
	served atomic.Int64 // 做完的请求数（启动后一个没做完就退出了，当作启动失败）

	mu    sync.Mutex
	calls map[string]*serveCall
	err   error         // 已经退出的原因
	done  chan struct{} // 退出后关闭
}

// serveCall 一个进行中的请求。
type serveCall struct {
	mu     sync.Mutex
	gone   bool // 调用方已经不等了（取消）：之后的输出丢掉
	onLine func(string)
	end    chan int
}

// errServeDown 常驻的 agent 用不了（没启动起来、已经退出）：改为单独运行。
var errServeDown = errors.New("常驻的 agent 用不了")

// serveStartTimeout 开常驻 agent 的会话最多等多久：等的时候拿着 srvMu，别的查看都在等它。
const serveStartTimeout = 15 * time.Second

func startAgentServer(c *remote.Client, cmd string) (*agentServer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), serveStartTimeout)
	defer cancel()
	p, err := c.StartPipeContext(ctx, cmd)
	if err != nil {
		return nil, err
	}
	a := &agentServer{pipe: p, calls: map[string]*serveCall{}, done: make(chan struct{})}
	safe.Go("常驻 agent 的输出", a.read)
	return a, nil
}

// read 把输出按请求号分给各个请求，直到常驻的 agent 退出（连接断了、会话关了）。
func (a *agentServer) read() {
	defer close(a.done)
	sc := bufio.NewScanner(a.pipe.Out)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		id, rest, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		a.mu.Lock()
		c := a.calls[id]
		code, end := strings.CutPrefix(rest, proto.ServeEnd)
		if end {
			delete(a.calls, id)
		}
		a.mu.Unlock()
		if c == nil {
			continue
		}
		if end {
			n, err := strconv.Atoi(code)
			if err != nil {
				n = proto.CodeFailed
			}
			a.served.Add(1)
			c.end <- n
			continue
		}
		c.mu.Lock()
		if !c.gone {
			c.onLine(rest)
		}
		c.mu.Unlock()
	}
	a.pipe.Close()
	werr := a.pipe.Wait()
	msg := strings.TrimSpace(a.pipe.Stderr())
	if msg == "" && werr != nil {
		msg = werr.Error()
	}
	a.mu.Lock()
	a.err = fmt.Errorf("常驻的 agent 已退出：%s", msg)
	a.calls = map[string]*serveCall{}
	a.mu.Unlock()
}

func (a *agentServer) alive() bool {
	select {
	case <-a.done:
		return false
	default:
		return true
	}
}

func (a *agentServer) close() { a.pipe.Close() }

// call 发一个请求，等它做完；onLine 收到这个请求的每一行输出（和单独运行时一样）。
// 返回 errServeDown 时请求可能没做、也可能做了一半，调用方改为单独运行（只读的查看，重做没有影响）。
func (a *agentServer) call(ctx context.Context, cmd string, req *proto.Request, onLine func(string)) (int, error) {
	id := strconv.FormatInt(a.seq.Add(1), 10)
	c := &serveCall{onLine: onLine, end: make(chan int, 1)}
	b, err := json.Marshal(proto.ServeRequest{ID: id, Cmd: cmd, Req: req})
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	if !a.alive() {
		a.mu.Unlock()
		return 0, errServeDown
	}
	a.calls[id] = c
	a.mu.Unlock()
	forget := func() {
		a.mu.Lock()
		delete(a.calls, id)
		a.mu.Unlock()
		c.mu.Lock()
		c.gone = true
		c.mu.Unlock()
	}
	a.wmu.Lock()
	_, err = a.pipe.In.Write(append(b, '\n'))
	a.wmu.Unlock()
	if err != nil {
		forget()
		return 0, errServeDown
	}
	select {
	case code := <-c.end:
		return code, nil
	case <-a.done:
		select { // 退出前刚好做完的
		case code := <-c.end:
			return code, nil
		default:
		}
		forget()
		return 0, errServeDown
	case <-ctx.Done():
		forget() // 输出丢掉；让服务器上也停下（还在排队的不做了，正在运行的命令杀掉）
		a.cancel(id)
		return 0, remote.ErrCanceled
	}
}

// cancel 告诉常驻的 agent 不用做请求 id 了。不等它写完（连接卡住时写也会卡住）。
func (a *agentServer) cancel(id string) {
	b, _ := json.Marshal(proto.ServeRequest{ID: id, Cancel: true})
	safe.Go("取消常驻 agent 的请求", func() {
		a.wmu.Lock()
		defer a.wmu.Unlock()
		a.pipe.In.Write(append(b, '\n'))
	})
}

// server 这条连接上常驻的 agent：还没有、已经退出的启动一个；最近启动失败过的返回 nil（单独运行）。
func (s *conn) server() *agentServer {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.srv != nil && s.srv.alive() {
		return s.srv
	}
	if s.srv != nil && s.srv.served.Load() == 0 { // 上一个一个请求都没做完就退出了（sudo 要密码等）：一段时间内不再试
		s.srv, s.srvFailedAt = nil, time.Now()
	}
	if !s.srvFailedAt.IsZero() && time.Since(s.srvFailedAt) < serveRetryAfter {
		return nil
	}
	srv, err := startAgentServer(s.c, s.sudo+s.agent+" serve")
	if err != nil {
		s.srvFailedAt = time.Now()
		return nil
	}
	s.srv, s.srvFailedAt = srv, time.Time{}
	return srv
}

// callServe 交给常驻的 agent 做；常驻的用不了时 ok 为 false（调用方改为单独运行）。
func (s *session) callServe(cmd string, req *proto.Request, onLine func(string)) (code int, ok bool, err error) {
	srv := s.server()
	if srv == nil {
		return 0, false, nil
	}
	code, err = srv.call(s.e.ctx(), cmd, req, onLine)
	if errors.Is(err, errServeDown) {
		return 0, false, nil
	}
	return code, true, err
}
