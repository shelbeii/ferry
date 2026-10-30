// Package approve 编程软件（经 ferry mcp）请求改动服务器时，交给打开着的 Ferry 桌面版，由用户点「同意」或「拒绝」；
// 同意后由桌面版当作普通任务执行（任务面板里看得到、能停止），过程和结果再交回编程软件。
//
// 桌面版的 ferry serve 在 127.0.0.1 的随机端口上等请求，把端口和一个随机令牌写进数据目录的 mcp/approver.json；
// ferry mcp 读这个文件、连上去、发请求，等用户的决定和执行结果。桌面版没打开时连不上：改动一律不做。
// 决定只能在桌面版里做出：连上来的一方只能提请求，不能替用户回答；要执行的命令由桌面版按请求里的
// 操作、目标、应用自己拼，不用请求方给的命令。
//
// 一个连接上的消息（每行一个 JSON）：
//
//	→ {"token":"…","request":{…}}
//	← {"decision":{"approve":false,"note":"…"}}               拒绝（随后断开）
//	← {"decision":{"approve":true}} {"progress":"…"}… {"outcome":{…}}   同意、执行中、结束
package approve

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/safe"
)

// DefaultFile 桌面版写端口和令牌、ferry mcp 读的文件（数据目录的 mcp/approver.json）。
func DefaultFile() string { return filepath.Join(appdir.SubDir("mcp"), "approver.json") }

// Request 一个待确认的改动。桌面版只按 Action、Config、Target、Apps、SkipBuild、To、Software、Port、Pids、Allow、RestartDocker、Backup、Spec 执行；其余是给人看的。
type Request struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Session   string    `json:"session,omitempty"` // 发请求的 ferry mcp 进程（这次连接）的随机标识：「这次连接内不再询问」按它算
	Client    string    `json:"client,omitempty"`  // 编程软件（它自己报的名称和版本）
	Action    string    `json:"action"`            // deploy | start | stop | restart | rollback | sw-start | sw-stop | sw-restart | port-open | port-close | fw-enable | ssh-kick | backup-run | create
	Project   string    `json:"project"`
	Config    string    `json:"config"`
	Target    string    `json:"target"`
	Host      string    `json:"host"`
	Apps      []string  `json:"apps,omitempty"`
	SkipBuild bool      `json:"skipBuild,omitempty"` // deploy：跳过构建
	To        string    `json:"to,omitempty"`        // rollback：回滚到的版本（空 = 上一个）
	Software  string    `json:"software,omitempty"`  // sw-*：Ferry 装的软件的名称
	Port      string    `json:"port,omitempty"`      // port-*：防火墙放行 / 关闭的端口，如 8080/tcp
	// fw-enable：打开防火墙前先放行的端口（8080/tcp……，SSH 端口自动放行）；RestartDocker 打开后重启 Docker
	Allow         []string `json:"allow,omitempty"`
	RestartDocker bool     `json:"restartDocker,omitempty"`
	Pids          []int    `json:"pids,omitempty"`   // ssh-kick：要断开的 SSH 连接（ssh_sessions 里的 pid）
	Backup        string   `json:"backup,omitempty"` // backup-run：备份哪一项（配置里 backups 的名字；空 = 全部）
	// create：新建项目（Config 是要生成的 ferry.yaml）。Spec 是向导的内容（scaffold.Spec）；
	// Content 是按 Spec 生成的 ferry.yaml，由桌面版这边生成（Prepare），不用请求方给的
	Spec    json.RawMessage `json:"spec,omitempty"`
	Content string          `json:"content,omitempty"`
	Detail  []string        `json:"detail,omitempty"` // 补充说明（代码提交、构建命令、线上版本……）
	Reason  string          `json:"reason,omitempty"` // 编程软件说的理由
}

// Decision 用户的决定。
type Decision struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note,omitempty"` // 拒绝时的原因（如「超时没有确认」）；同意时可以是「按授权自动同意」
}

// Outcome 桌面版执行的结果。
type Outcome struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"` // ferry 命令的结果（部署报告、启停结果……）
	Log   []string        `json:"log,omitempty"`  // 过程的最后几十行（失败时含构建输出）
}

// Timeout 等用户决定最多多久。
const Timeout = 5 * time.Minute

type info struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

type message struct {
	Decision *Decision `json:"decision,omitempty"`
	Progress string    `json:"progress,omitempty"`
	Outcome  *Outcome  `json:"outcome,omitempty"`
}

// ---------------------------------------------------------------- 桌面版这边

// Hub 桌面版（ferry serve）里收请求、等用户决定、把执行过程交回去。
type Hub struct {
	file string
	// Prepare 请求交给用户之前再检查一遍（设置里是否允许、是不是生产服务器……），补上要给人看的内容；不通过直接拒绝
	Prepare func(*Request) error

	mu      sync.Mutex
	ln      net.Listener
	token   string
	subs    map[int]func(kind string, v any) // 桌面版订阅（通常只有一个）
	nextSub int
	pending map[string]chan Decision // 等决定的
	running map[string]chan message  // 已同意、桌面版正在执行的
}

// NewHub file 是写端口和令牌的文件（数据目录的 mcp/approver.json）。
func NewHub(file string) *Hub {
	return &Hub{file: file, subs: map[int]func(string, any){}, pending: map[string]chan Decision{}, running: map[string]chan message{}}
}

// Subscribe 桌面版开始接收请求：emit("request", Request) 新请求；emit("gone", {"id","note"}) 请求不用再处理了（编程软件断开、超时）。
// 第一个订阅时开始监听；返回的函数取消订阅，没有订阅了就停止监听。
func (h *Hub) Subscribe(emit func(kind string, v any)) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ln == nil {
		if err := h.listenLocked(); err != nil {
			return nil, err
		}
	}
	id := h.nextSub
	h.nextSub++
	h.subs[id] = emit
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, id)
		if len(h.subs) == 0 && h.ln != nil {
			h.ln.Close()
			h.ln = nil
			h.removeFileLocked()
			for rid, ch := range h.pending { // 桌面版关了：还在等的都算拒绝
				ch <- Decision{Note: "Ferry 桌面版已关闭"}
				delete(h.pending, rid)
			}
		}
	}, nil
}

func (h *Hub) listenLocked() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	b := make([]byte, 24)
	rand.Read(b)
	h.token = hex.EncodeToString(b)
	data, _ := json.Marshal(info{Port: ln.Addr().(*net.TCPAddr).Port, Token: h.token, PID: os.Getpid()})
	os.MkdirAll(filepath.Dir(h.file), 0o700)
	tmp := h.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		ln.Close()
		return err
	}
	if err := os.Rename(tmp, h.file); err != nil {
		ln.Close()
		return err
	}
	h.ln = ln
	safe.Go("确认服务", func() { h.accept(ln) })
	return nil
}

// removeFileLocked 删掉自己写的端口文件（别的桌面版实例后来写的不删）。
func (h *Hub) removeFileLocked() {
	var cur info
	if b, err := os.ReadFile(h.file); err == nil && json.Unmarshal(b, &cur) == nil && cur.Token == h.token {
		os.Remove(h.file)
	}
}

func (h *Hub) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		safe.Go("确认请求", func() { h.handle(c) })
	}
}

type wireRequest struct {
	Token   string  `json:"token"`
	Request Request `json:"request"`
}

func (h *Hub) emitLocked(kind string, v any) {
	for _, emit := range h.subs {
		emit(kind, v)
	}
}

func (h *Hub) handle(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var w wireRequest
	if json.Unmarshal(line, &w) != nil {
		return
	}
	enc := json.NewEncoder(c)
	send := func(m message) error { return enc.Encode(m) }
	reject := func(note string) { send(message{Decision: &Decision{Note: note}}) }
	h.mu.Lock()
	ok := subtle.ConstantTimeCompare([]byte(w.Token), []byte(h.token)) == 1
	h.mu.Unlock()
	if !ok {
		reject("令牌不对")
		return
	}
	req := w.Request
	if req.ID == "" {
		b := make([]byte, 8)
		rand.Read(b)
		req.ID = hex.EncodeToString(b)
	}
	req.Time = time.Now()
	if h.Prepare != nil {
		if err := h.Prepare(&req); err != nil {
			reject(err.Error())
			return
		}
	}
	ch := make(chan Decision, 1)
	h.mu.Lock()
	if len(h.subs) == 0 {
		h.mu.Unlock()
		reject("Ferry 桌面版没有在等确认")
		return
	}
	if _, dup := h.pending[req.ID]; dup || h.running[req.ID] != nil {
		h.mu.Unlock()
		reject("请求 ID 重复")
		return
	}
	h.pending[req.ID] = ch
	h.emitLocked("request", req)
	h.mu.Unlock()

	// 对方断开（编程软件退出、取消了）：读到 EOF
	gone := make(chan struct{})
	safe.Go("确认请求", func() {
		c.SetReadDeadline(time.Time{})
		r.ReadByte()
		close(gone)
	})
	var d Decision
	select {
	case d = <-ch:
	case <-gone:
		d = Decision{Note: "编程软件取消了"}
	case <-time.After(Timeout):
		d = Decision{Note: fmt.Sprintf("%d 分钟内没有确认", int(Timeout/time.Minute))}
	}
	h.mu.Lock()
	delete(h.pending, req.ID)
	var msgs chan message
	if d.Approve {
		msgs = make(chan message, 64)
		h.running[req.ID] = msgs
	} else {
		h.emitLocked("gone", map[string]any{"id": req.ID, "note": d.Note})
	}
	h.mu.Unlock()
	if send(message{Decision: &d}) != nil || !d.Approve {
		h.finish(req.ID)
		return
	}
	// 执行中：把桌面版报来的进展交给对方，直到结果出来。对方断开了也照样执行完（结果没人收）。
	defer h.finish(req.ID)
	for m := range msgs {
		if send(m) != nil {
			return
		}
		if m.Outcome != nil {
			return
		}
	}
}

func (h *Hub) finish(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.running, id)
}

// Decide 用户在桌面版里做了决定。请求已经不在了（超时、取消）时返回错误。
func (h *Hub) Decide(id string, d Decision) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.pending[id]
	if !ok {
		return errors.New("这个请求已经结束了（超时或编程软件取消了）")
	}
	delete(h.pending, id)
	ch <- d
	return nil
}

// Progress 桌面版执行中的一行进展。对方已经断开时忽略。
func (h *Hub) Progress(id, line string) {
	h.mu.Lock() // 在锁里发：Done 删掉、关闭通道之后就不会再往里发
	defer h.mu.Unlock()
	if ch := h.running[id]; ch != nil {
		select {
		case ch <- message{Progress: line}:
		default: // 对方收得慢：进展丢几行没关系
		}
	}
}

// Done 桌面版执行完了。
func (h *Hub) Done(id string, o Outcome) {
	h.mu.Lock()
	ch := h.running[id]
	delete(h.running, id)
	h.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- message{Outcome: &o}:
	case <-time.After(10 * time.Second): // 对方早断开了，没人取
	}
	close(ch)
}

// ---------------------------------------------------------------- ferry mcp 这边

// ErrNoDesktop Ferry 桌面版没打开（或没在等确认）。
var ErrNoDesktop = errors.New("要先打开 Ferry 桌面版：这个操作要用户在 Ferry 里确认")

// Ask 把请求交给桌面版，等用户决定（最多 Timeout）；同意后等桌面版执行完，期间的进展交给 onProgress。
// 拒绝时 Outcome 为空。ctx 取消时放弃：还没决定的，桌面版上的确认框随之关闭；已经在执行的照样执行完。
func Ask(ctx context.Context, file string, req Request, onDecision func(Decision), onProgress func(string)) (Decision, *Outcome, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return Decision{}, nil, ErrNoDesktop
	}
	var in info
	if json.Unmarshal(b, &in) != nil || in.Port == 0 {
		return Decision{}, nil, ErrNoDesktop
	}
	var dialer net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	c, err := dialer.DialContext(dctx, "tcp", fmt.Sprintf("127.0.0.1:%d", in.Port))
	cancel()
	if err != nil {
		return Decision{}, nil, ErrNoDesktop
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if err := json.NewEncoder(c).Encode(wireRequest{Token: in.Token, Request: req}); err != nil {
		return Decision{}, nil, ErrNoDesktop
	}
	dec := json.NewDecoder(c)
	broken := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("和 Ferry 桌面版的连接断开了：%v", err)
	}
	c.SetReadDeadline(time.Now().Add(Timeout + 30*time.Second))
	var m message
	if err := dec.Decode(&m); err != nil || m.Decision == nil {
		return Decision{}, nil, broken(err)
	}
	d := *m.Decision
	if onDecision != nil {
		onDecision(d)
	}
	if !d.Approve {
		return d, nil, nil
	}
	c.SetReadDeadline(time.Time{}) // 执行多久都等（构建、上传可能很久）；桌面版关了连接就断
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			return d, nil, fmt.Errorf("执行结果未知：%w", broken(err))
		}
		if m.Outcome != nil {
			return d, m.Outcome, nil
		}
		if m.Progress != "" && onProgress != nil {
			onProgress(m.Progress)
		}
	}
}
