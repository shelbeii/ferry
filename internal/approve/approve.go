// Package approve 编程软件（经 ferry mcp）请求改动服务器时，交给打开着的 Ferry 桌面版，由用户确认或由客户端确认后交桌面版执行；
// 同意后由桌面版当作普通任务执行（任务面板里看得到、能停止），过程和结果再交回编程软件。
//
// 桌面版的 ferry serve 在 127.0.0.1 的随机端口上等请求，把端口和一个随机令牌写进数据目录的 mcp/approver.json；
// ferry mcp 读这个文件、连上去、发请求。
//
// 支持两种交互：
//  1. 直接请求（op 为空）：桌面版弹窗确认并执行（始终忽略外部传入的 ClientConfirmed，防止本地进程伪造）。
//  2. 两阶段预检与提交（op=prepare / commit / cancel）：
//     - prepare：桌面版校验权限并计算权威预览、锁定 TOCTOU 快照（AIState/AIScope/Base/DBCheck），返回一次性 prepareToken。
//     - commit：消费一次性 prepareToken，按桌面端冻结的 Request 执行（客户确认时静默进任务面板，不抢焦点；回退时弹桌面确认框）。
//     - cancel：用户在编程软件拒绝或取消时销毁 prepareToken。
package approve

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/safe"
)

// DefaultFile 桌面版写端口和令牌、ferry mcp 读的文件（数据目录的 mcp/approver.json）。
func DefaultFile() string { return filepath.Join(appdir.SubDir("mcp"), "approver.json") }

// Request 一个待确认的改动。桌面版只按 Action、Config、Target、Targets、Apps、SkipBuild、Batch、Canary、ObserveSec、OnFailure、To、Software、Port、Pids、Allow、RestartDocker、Backup、Spec、Tool、Input、Fields 执行；其余是给人看的。
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
	// deploy：全量上传（不用增量）、失败时保留失败版本（不自动回滚）、不自动修正权限
	Full       bool `json:"full,omitempty"`
	NoRollback bool `json:"noRollback,omitempty"`
	KeepPerms  bool `json:"keepPerms,omitempty"`
	// Keep 这次保留服务器上的（应用 → 列表，JSON），代替 ferry.yaml 里的 artifact.keep
	Keep json.RawMessage `json:"keep,omitempty"`
	// deploy 到多台：Targets 是全部服务器（按切换的顺序，Target 是第一台）；Batch、Canary、ObserveSec、OnFailure 是切换方式
	// （0 / 空 = 用 ferry.yaml 的 rollout；Canary 为空 = 用配置）
	Targets    []string `json:"targets,omitempty"`
	Batch      int      `json:"batch,omitempty"`
	Canary     *int     `json:"canary,omitempty"`
	ObserveSec int      `json:"observeSec,omitempty"`
	OnFailure  string   `json:"onFailure,omitempty"`
	To         string   `json:"to,omitempty"`       // rollback：回滚到的版本（空 = 上一个）
	Software   string   `json:"software,omitempty"` // sw-*：Ferry 装的软件的名称
	Port       string   `json:"port,omitempty"`     // port-*：防火墙放行 / 关闭的端口，如 8080/tcp
	// fw-enable：打开防火墙前先放行的端口（8080/tcp……，SSH 端口自动放行）；RestartDocker 打开后重启 Docker
	Allow         []string `json:"allow,omitempty"`
	RestartDocker bool     `json:"restartDocker,omitempty"`
	Pids          []int    `json:"pids,omitempty"`   // ssh-kick：要断开的 SSH 连接（ssh_sessions 里的 pid）
	Backup        string   `json:"backup,omitempty"` // backup-run：备份哪一项（配置里 backups 的名字；空 = 全部）
	// create：新建项目（Config 是要生成的 ferry.yaml）。Spec 是向导的内容（scaffold.Spec）；
	// Content 是按 Spec 生成的 ferry.yaml，由桌面版这边生成（Prepare），不用请求方给的
	Spec    json.RawMessage `json:"spec,omitempty"`
	Content string          `json:"content,omitempty"`
	// exec | write-file | install | tpl-save：用服务器助手的工具 Tool 执行，Input 是它的参数（确认框里的内容由桌面版按它重新算）；
	// secret：让用户在 Ferry 里填 Fields，值只记在桌面版
	Tool    string           `json:"tool,omitempty"`
	Input   json.RawMessage  `json:"input,omitempty"`
	AIState string           `json:"aiState,omitempty"` // File snapshot captured by Ferry.
	AIScope string           `json:"aiScope,omitempty"` // Effective target and verified SSH identity.
	Fields  []ai.SecretField `json:"fields,omitempty"`
	Danger  string           `json:"danger,omitempty"` // 危险的原因（桌面版算的）
	// db-exec：在数据库连接 DB（ID；DBName 是它的名称）的库 Database 里执行 Stmts（经服务器连时 Config、Target 是那台服务器）
	DB            string   `json:"db,omitempty"`
	DBName        string   `json:"dbName,omitempty"`
	DBKind        string   `json:"dbKind,omitempty"`        // Redis 的 Stmts 为命令，MongoDB 为 Extended JSON，其余为 SQL
	DBCheck       string   `json:"dbCheck,omitempty"`       // 确认框展示的连接配置摘要
	DBEstimate    string   `json:"dbEstimate,omitempty"`    // EXPLAIN 估算的影响行数摘要（MySQL / PostgreSQL）
	DBEstimateMax int64    `json:"dbEstimateMax,omitempty"` // EXPLAIN 估算的最大影响行数（>= 1000 时确认框标红）
	Database      string   `json:"database,omitempty"`
	Stmts         []string `json:"stmts,omitempty"`
	// ClientConfirmed 用户已经在编程软件里确认了（只由桌面端 Hub 验完 prepareToken 后置位）：桌面版不再弹窗，直接静默执行
	ClientConfirmed bool     `json:"clientConfirmed,omitempty"`
	ConfirmChannel  string   `json:"confirmChannel,omitempty"` // desktop | mcp-app | elicitation | two-step
	ConfirmNote     string   `json:"confirmNote,omitempty"`    // 确认渠道说明（给桌面通知与审计记录用）
	Detail          []string `json:"detail,omitempty"`         // 补充说明（代码提交、构建命令、线上版本……）
	Reason          string   `json:"reason,omitempty"`         // 编程软件说的理由
}

// Prepared 桌面端预检后冻结的请求与一次性令牌。
type Prepared struct {
	Token              string    `json:"token"`
	Eligible           bool      `json:"eligible"`
	ForceDesktopReason string    `json:"forceDesktopReason,omitempty"`
	ExpiresAt          time.Time `json:"expiresAt"`
	Request            Request   `json:"request"`
}

// ConfirmEvidence 客户端确认的证据，Commit 时交由桌面端二次校验并记入审计。
type ConfirmEvidence struct {
	Channel           string `json:"channel,omitempty"` // mcp-app | elicitation | two-step
	Client            string `json:"client,omitempty"`
	Action            string `json:"action,omitempty"` // accept | decline | cancel
	TookMs            int64  `json:"tookMs,omitempty"`
	Note              string `json:"note,omitempty"`
	FallbackToDesktop bool   `json:"fallbackToDesktop,omitempty"`
	FallbackReason    string `json:"fallbackReason,omitempty"`
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

// maxWireLine 单条本地审批请求最大字节数（与 ferry serve 一致），防止异常大报文耗尽内存。
const maxWireLine = 32 << 20

// SessionInfo 已接入或曾接入桌面端审批 Hub 的 MCP 连接信息（用于设置页会话诊断与解除拉黑）。
type SessionInfo struct {
	Session         string    `json:"session"`
	PID             int       `json:"pid,omitempty"`
	Dir             string    `json:"dir,omitempty"`
	Client          string    `json:"client,omitempty"`
	ProtocolVersion string    `json:"protocolVersion,omitempty"`
	Channel         string    `json:"channel,omitempty"`
	Capabilities    []string  `json:"capabilities,omitempty"`
	LastSeen        time.Time `json:"lastSeen"`
	Blocked         bool      `json:"blocked"`
}

// processAlive 检查本地 ferry mcp 进程是否仍然存活（测试时可替换）。
var processAlive = osProcessAlive

type info struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

type message struct {
	Prepared *Prepared `json:"prepared,omitempty"`
	Error    string    `json:"error,omitempty"`
	Decision *Decision `json:"decision,omitempty"`
	Progress string    `json:"progress,omitempty"`
	Outcome  *Outcome  `json:"outcome,omitempty"`
}

// ---------------------------------------------------------------- 桌面版这边

type preparedEntry struct {
	req                Request
	eligible           bool
	forceDesktopReason string
	session            string
	createdAt          time.Time
	expiresAt          time.Time
}

// Hub 桌面版（ferry serve）里收请求、等用户决定、把执行过程交回去。
type Hub struct {
	file string
	// Prepare 请求交给用户之前再检查一遍（设置里是否允许、是不是生产服务器……），补上要给人看的内容；不通过直接拒绝
	Prepare func(*Request) error
	// PrepareClient 判断已预检通过的请求是否允许在编程软件里确认；未设置时一律不允许在客户端确认
	PrepareClient func(*Request) (eligible bool, forceDesktopReason string, err error)
	// VerifyCommit 桌面端在 Commit 客户端确认前复核（重读设置、核对渠道与耗时等）
	VerifyCommit func(*Request, ConfirmEvidence) error
	// OnSessionClose 当某个 ferry mcp 进程正常退出通知 bye 时回调（用于清理桌面端内存中的 mcp-<session> 密码）
	OnSessionClose func(session string)

	mu              sync.Mutex
	ln              net.Listener
	token           string
	subs            map[int]func(kind string, v any) // 桌面版订阅（通常只有一个）
	nextSub         int
	pending         map[string]chan Decision // 等决定的
	pendingSessions map[string]string        // pending ID → session（支持 --block 拉黑）
	running         map[string]chan message  // 已同意、桌面版正在执行的
	runningSessions map[string]string        // running ID → session（支持跨进程取消任务校验）
	outbox          map[string]chan message  // 每个等决定的请求收进展和结果的通道（同意时放进 running）
	prepared        map[string]*preparedEntry
	blocked         map[string]bool         // 被用户拉黑的 MCP session
	sessions        map[string]*SessionInfo // 近期活跃的 MCP 连接（会话诊断与解封用）
}

// NewHub file 是写端口和令牌的文件（数据目录的 mcp/approver.json）。
func NewHub(file string) *Hub {
	return &Hub{
		file:            file,
		subs:            map[int]func(string, any){},
		pending:         map[string]chan Decision{},
		pendingSessions: map[string]string{},
		running:         map[string]chan message{},
		runningSessions: map[string]string{},
		outbox:          map[string]chan message{},
		prepared:        map[string]*preparedEntry{},
		blocked:         map[string]bool{},
		sessions:        map[string]*SessionInfo{},
	}
}

// BlockSession 拉黑某个 MCP 连接会话：本次桌面版运行期间不再接受该会话的改动请求，并清理其未提交的预检令牌。
func (h *Hub) BlockSession(session string) {
	if session == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.blocked[session] = true
	if si, ok := h.sessions[session]; ok {
		si.Blocked = true
	}
	for tok, p := range h.prepared {
		if p.session == session {
			delete(h.prepared, tok)
		}
	}
}

// UnblockSession 解除对某个 MCP 连接会话的拉黑，并通知桌面端同步清理前端拦截表。
func (h *Hub) UnblockSession(session string) bool {
	if session == "" {
		return false
	}
	h.mu.Lock()
	was := h.blocked[session]
	delete(h.blocked, session)
	if si, ok := h.sessions[session]; ok {
		si.Blocked = false
	}
	if was {
		h.emitLocked("unblock", map[string]any{"session": session})
	}
	h.mu.Unlock()
	return was
}

// Sessions 返回当前存活或已拉黑的 MCP 连接会话列表（按拉黑优先、最近活跃时间倒序）。
// 自动探测并清理已被宿主强杀（未发送 bye）的失效进程，同时触发 OnSessionClose 释放其残留内存密码。
func (h *Hub) Sessions() []SessionInfo {
	now := time.Now()
	var dead []string
	var onClose func(string)
	h.mu.Lock()
	onClose = h.OnSessionClose
	out := make([]SessionInfo, 0, len(h.sessions)+len(h.blocked))
	seen := map[string]bool{}
	for id, si := range h.sessions {
		if si.PID > 0 && !processAlive(si.PID) {
			delete(h.sessions, id)
			delete(h.blocked, id)
			for tok, p := range h.prepared {
				if p.session == id {
					delete(h.prepared, tok)
				}
			}
			dead = append(dead, id)
			continue
		}
		if !h.blocked[id] && now.Sub(si.LastSeen) > 24*time.Hour {
			delete(h.sessions, id)
			continue
		}
		cp := *si
		cp.Blocked = h.blocked[id]
		if len(si.Capabilities) > 0 {
			cp.Capabilities = append([]string(nil), si.Capabilities...)
		}
		out = append(out, cp)
		seen[id] = true
	}
	for id := range h.blocked {
		if !seen[id] {
			out = append(out, SessionInfo{Session: id, Blocked: true})
		}
	}
	h.mu.Unlock()
	if onClose != nil {
		for _, id := range dead {
			onClose(id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Blocked != out[j].Blocked {
			return out[i].Blocked
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

func (h *Hub) recordSessionLocked(session string, pid int, dir, client, protoVer, channel string, caps []string) {
	if session == "" {
		return
	}
	si := h.sessions[session]
	if si == nil {
		si = &SessionInfo{Session: session}
		h.sessions[session] = si
	}
	if pid > 0 {
		si.PID = pid
	}
	if dir != "" {
		si.Dir = dir
	}
	if client != "" {
		si.Client = client
	}
	if protoVer != "" {
		si.ProtocolVersion = protoVer
	}
	if channel != "" {
		si.Channel = channel
	}
	if len(caps) > 0 {
		si.Capabilities = append([]string(nil), caps...)
	}
	si.LastSeen = time.Now()
	si.Blocked = h.blocked[session]
}

// CancelRunning 通知桌面端停止正在执行的 MCP 任务（仅允许发起该任务的会话停止）。
func (h *Hub) CancelRunning(id, session string) bool {
	if id == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running[id] == nil {
		return false
	}
	if owner := h.runningSessions[id]; owner != "" && session != "" && owner != session {
		return false
	}
	h.emitLocked("cancel", map[string]any{"id": id})
	return true
}

// IsSessionBlocked 该会话是否已被用户拉黑。
func (h *Hub) IsSessionBlocked(session string) bool {
	if session == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.blocked[session]
}

func (h *Hub) sweepPreparedLocked(now time.Time) {
	for tok, p := range h.prepared {
		if !now.Before(p.expiresAt) {
			delete(h.prepared, tok)
		}
	}
}

func cloneRequest(r Request) Request {
	b, err := json.Marshal(r)
	if err != nil {
		return r
	}
	var cp Request
	if json.Unmarshal(b, &cp) != nil {
		return r
	}
	return cp
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
			clear(h.pendingSessions)
			clear(h.prepared)
			for rid, ch := range h.running { // 避免已同意执行中的 ferry mcp 连接永久卡死
				select {
				case ch <- message{Outcome: &Outcome{OK: false, Error: "Ferry 桌面版已断开审批监听，执行结果未知"}}:
				default:
				}
				close(ch)
				delete(h.running, rid)
			}
			clear(h.runningSessions)
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
	if err := appdir.WriteFileAtomic(h.file, data, 0o600); err != nil {
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
	Token           string           `json:"token"`
	Op              string           `json:"op,omitempty"` // "" (直接桌面确认) | "prepare" | "commit" | "cancel" | "hello" | "bye" | "cancel-run"
	Request         Request          `json:"request,omitempty"`
	PrepareToken    string           `json:"prepareToken,omitempty"`
	Session         string           `json:"session,omitempty"`
	PID             int              `json:"pid,omitempty"`
	Dir             string           `json:"dir,omitempty"`
	OpID            string           `json:"opId,omitempty"`
	Client          string           `json:"client,omitempty"`
	ProtocolVersion string           `json:"protocolVersion,omitempty"`
	Channel         string           `json:"channel,omitempty"`
	Capabilities    []string         `json:"capabilities,omitempty"`
	Evidence        *ConfirmEvidence `json:"evidence,omitempty"`
}

func (h *Hub) emitLocked(kind string, v any) {
	for _, emit := range h.subs {
		emit(kind, v)
	}
}

func (h *Hub) handle(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(io.LimitReader(c, maxWireLine))
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
		if w.Op == "prepare" || w.Op == "cancel" || w.Op == "hello" || w.Op == "bye" || w.Op == "cancel-run" {
			send(message{Error: "令牌不对"})
		} else {
			reject("令牌不对")
		}
		return
	}
	switch w.Op {
	case "hello":
		h.mu.Lock()
		h.recordSessionLocked(w.Session, w.PID, w.Dir, w.Client, w.ProtocolVersion, w.Channel, w.Capabilities)
		h.mu.Unlock()
		send(message{})
	case "bye":
		h.handleBye(w, send)
	case "cancel-run":
		if !h.CancelRunning(w.OpID, w.Session) {
			send(message{Error: "任务不在运行中或不属于当前会话"})
			return
		}
		send(message{})
	case "prepare":
		h.handlePrepare(w, send)
	case "cancel":
		h.handleCancel(w, send)
	case "commit":
		h.handleCommit(c, r, w, send, reject)
	case "":
		h.handleAsk(c, r, w.Request, send, reject)
	default:
		reject("不支持的审批操作：" + w.Op)
	}
}

func (h *Hub) handleBye(w wireRequest, send func(message) error) {
	if w.Session != "" {
		h.mu.Lock()
		for tok, p := range h.prepared {
			if p.session == w.Session {
				delete(h.prepared, tok)
			}
		}
		if !h.blocked[w.Session] {
			delete(h.sessions, w.Session)
		}
		onClose := h.OnSessionClose
		h.mu.Unlock()
		if onClose != nil {
			onClose(w.Session)
		}
	}
	send(message{})
}

func (h *Hub) handlePrepare(w wireRequest, send func(message) error) {
	req := w.Request
	// 严禁外部伪造客户端已确认标记
	req.ClientConfirmed = false
	req.ConfirmChannel = ""
	req.ConfirmNote = ""
	if h.IsSessionBlocked(req.Session) {
		send(message{Error: "用户已拒绝这次连接的所有改动请求"})
		return
	}
	h.mu.Lock()
	hasSub := len(h.subs) > 0
	h.mu.Unlock()
	if !hasSub {
		send(message{Error: "Ferry 桌面版没有在等确认"})
		return
	}
	if req.ID == "" {
		b := make([]byte, 8)
		rand.Read(b)
		req.ID = hex.EncodeToString(b)
	}
	req.Time = time.Now()
	if h.Prepare != nil {
		if err := h.Prepare(&req); err != nil {
			send(message{Error: err.Error()})
			return
		}
	}
	eligible := false
	forceReason := "此操作需要在 Ferry 桌面端确认"
	if h.PrepareClient != nil {
		var err error
		eligible, forceReason, err = h.PrepareClient(&req)
		if err != nil {
			send(message{Error: err.Error()})
			return
		}
	}
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	now := time.Now()
	exp := now.Add(Timeout)
	frozen := cloneRequest(req)
	h.mu.Lock()
	h.sweepPreparedLocked(now)
	h.recordSessionLocked(req.Session, 0, "", req.Client, "", "", nil)
	h.prepared[tok] = &preparedEntry{
		req:                frozen,
		eligible:           eligible,
		forceDesktopReason: forceReason,
		session:            req.Session,
		createdAt:          now,
		expiresAt:          exp,
	}
	h.mu.Unlock()
	send(message{Prepared: &Prepared{
		Token:              tok,
		Eligible:           eligible,
		ForceDesktopReason: forceReason,
		ExpiresAt:          exp,
		Request:            cloneRequest(frozen),
	}})
}

func (h *Hub) handleCancel(w wireRequest, send func(message) error) {
	if w.PrepareToken != "" {
		h.mu.Lock()
		if p, ok := h.prepared[w.PrepareToken]; ok && (p.session == "" || p.session == w.Session) {
			delete(h.prepared, w.PrepareToken)
		}
		h.mu.Unlock()
	}
	send(message{})
}

func (h *Hub) handleCommit(c net.Conn, r *bufio.Reader, w wireRequest, send func(message) error, reject func(string)) {
	now := time.Now()
	h.mu.Lock()
	h.sweepPreparedLocked(now)
	entry, ok := h.prepared[w.PrepareToken]
	if ok {
		delete(h.prepared, w.PrepareToken)
	}
	h.mu.Unlock()
	if !ok || entry == nil {
		reject("这次确认已经失效或已使用（超过 5 分钟或重复提交），请重新发起")
		return
	}
	if !now.Before(entry.expiresAt) {
		reject("这次确认已超时（超过 5 分钟），请重新发起")
		return
	}
	if entry.session != "" && entry.session != w.Session {
		reject("确认令牌与当前连接不匹配")
		return
	}
	if h.IsSessionBlocked(entry.session) {
		reject("用户已拒绝这次连接的所有改动请求")
		return
	}
	// 始终执行服务端冻结的 Request，完全不接受 commit 阶段重新传入的参数
	req := cloneRequest(entry.req)
	req.Time = now
	var ev ConfirmEvidence
	if w.Evidence != nil {
		ev = *w.Evidence
	}
	if ev.FallbackToDesktop {
		req.ClientConfirmed = false
		req.ConfirmChannel = "desktop"
		req.ConfirmNote = ""
		if ev.FallbackReason != "" {
			req.Detail = append([]string{ev.FallbackReason}, req.Detail...)
		}
		h.runPending(c, r, req, send, reject)
		return
	}
	if !entry.eligible {
		reject(cmp.Or(entry.forceDesktopReason, "此操作需要在 Ferry 桌面端确认"))
		return
	}
	if h.VerifyCommit != nil {
		if err := h.VerifyCommit(&req, ev); err != nil {
			reject(err.Error())
			return
		}
	}
	req.ClientConfirmed = true
	req.ConfirmChannel = ev.Channel
	req.ConfirmNote = ev.Note
	h.runPending(c, r, req, send, reject)
}

func (h *Hub) handleAsk(c net.Conn, r *bufio.Reader, req Request, send func(message) error, reject func(string)) {
	// 直接 Ask 通道绝不允许绕过桌面端弹窗
	req.ClientConfirmed = false
	req.ConfirmChannel = ""
	req.ConfirmNote = ""
	if h.IsSessionBlocked(req.Session) {
		reject("用户已拒绝这次连接的所有改动请求")
		return
	}
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
	h.runPending(c, r, req, send, reject)
}

func (h *Hub) runPending(c net.Conn, r *bufio.Reader, req Request, send func(message) error, reject func(string)) {
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
	msgs := make(chan message, 64)
	h.pending[req.ID] = ch
	if req.Session != "" {
		h.pendingSessions[req.ID] = req.Session
		h.recordSessionLocked(req.Session, 0, "", req.Client, "", req.ConfirmChannel, nil)
	}
	h.outbox[req.ID] = msgs
	h.emitLocked("request", req)
	h.mu.Unlock()

	// 对方断开（编程软件退出、取消了）：读到 EOF（直接读底层 c，不受 LimitReader 影响）
	gone := make(chan struct{})
	safe.Go("确认请求", func() {
		c.SetReadDeadline(time.Time{})
		var one [1]byte
		_, _ = c.Read(one[:])
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
	delete(h.pendingSessions, req.ID)
	delete(h.outbox, req.ID)
	if !d.Approve {
		delete(h.running, req.ID)
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
	delete(h.runningSessions, id)
}

// Decide 用户在桌面版里做了决定。请求已经不在了（超时、取消）时返回错误。
func (h *Hub) Decide(id string, d Decision) error {
	return h.DecideWithBlock(id, d, false)
}

// DecideWithBlock 用户在桌面版里做了决定；blockSession 为 true 且拒绝时，拉黑该请求的 MCP 会话。
func (h *Hub) DecideWithBlock(id string, d Decision, blockSession bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.pending[id]
	if !ok {
		return errors.New("这个请求已经结束了（超时或编程软件取消了）")
	}
	session := h.pendingSessions[id]
	delete(h.pending, id)
	delete(h.pendingSessions, id)
	if d.Approve {
		// 同意时马上开始收进展和结果：桌面版紧接着报结果（如填写密码）也不会丢
		h.running[id] = h.outbox[id]
		if session != "" {
			h.runningSessions[id] = session
		}
	} else if blockSession && session != "" {
		h.blocked[session] = true
		if si, ok := h.sessions[session]; ok {
			si.Blocked = true
		}
		for tok, p := range h.prepared {
			if p.session == session {
				delete(h.prepared, tok)
			}
		}
	}
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
	delete(h.runningSessions, id)
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

func dialHub(ctx context.Context, file string) (net.Conn, info, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, info{}, ErrNoDesktop
	}
	var in info
	if json.Unmarshal(b, &in) != nil || in.Port == 0 {
		return nil, info{}, ErrNoDesktop
	}
	var dialer net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	c, err := dialer.DialContext(dctx, "tcp", fmt.Sprintf("127.0.0.1:%d", in.Port))
	cancel()
	if err != nil {
		return nil, info{}, ErrNoDesktop
	}
	return c, in, nil
}

// Prepare 让运行中的桌面版先做权限检查、权威预览和快照锁定，返回一次性令牌。桌面版未打开时返回 ErrNoDesktop。
func Prepare(ctx context.Context, file string, req Request) (*Prepared, error) {
	c, in, err := dialHub(ctx, file)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if err := json.NewEncoder(c).Encode(wireRequest{Token: in.Token, Op: "prepare", Request: req}); err != nil {
		return nil, ErrNoDesktop
	}
	c.SetReadDeadline(time.Now().Add(45 * time.Second))
	var m message
	if err := json.NewDecoder(c).Decode(&m); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("和 Ferry 桌面版的连接断开了：%v", err)
	}
	if m.Error != "" {
		return nil, errors.New(m.Error)
	}
	if m.Prepared == nil {
		return nil, errors.New("Ferry 桌面版没有返回预检结果")
	}
	return m.Prepared, nil
}

// Cancel 销毁尚未提交的预检令牌（用户在编程软件拒绝或取消时调用）。
func Cancel(ctx context.Context, file, prepareToken, session string) {
	if prepareToken == "" {
		return
	}
	c, in, err := dialHub(ctx, file)
	if err != nil {
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	_ = json.NewEncoder(c).Encode(wireRequest{Token: in.Token, Op: "cancel", PrepareToken: prepareToken, Session: session})
	var m message
	_ = json.NewDecoder(c).Decode(&m)
}

// Hello 向运行中的桌面端上报当前 MCP 连接的进程 PID、目录、客户端信息与协商能力（用于存活探测与诊断展示，不影响正常调用）。
func Hello(ctx context.Context, file, session string, pid int, dir, client, protoVer, channel string, caps []string) {
	if session == "" {
		return
	}
	c, in, err := dialHub(ctx, file)
	if err != nil {
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	_ = json.NewEncoder(c).Encode(wireRequest{
		Token:           in.Token,
		Op:              "hello",
		Session:         session,
		PID:             pid,
		Dir:             dir,
		Client:          client,
		ProtocolVersion: protoVer,
		Channel:         channel,
		Capabilities:    caps,
	})
	var m message
	_ = json.NewDecoder(c).Decode(&m)
}

// Bye 在 ferry mcp 退出时通知桌面端清理本会话的临时预检令牌与内存密码。
func Bye(file, session string) {
	if session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, in, err := dialHub(ctx, file)
	if err != nil {
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	_ = json.NewEncoder(c).Encode(wireRequest{Token: in.Token, Op: "bye", Session: session})
	var m message
	_ = json.NewDecoder(c).Decode(&m)
}

// CancelRun 请求桌面端停止正在执行的指定任务。
func CancelRun(ctx context.Context, file, opID, session string) error {
	c, in, err := dialHub(ctx, file)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewEncoder(c).Encode(wireRequest{Token: in.Token, Op: "cancel-run", OpID: opID, Session: session}); err != nil {
		return err
	}
	var m message
	if err := json.NewDecoder(c).Decode(&m); err != nil {
		return err
	}
	if m.Error != "" {
		return errors.New(m.Error)
	}
	return nil
}

// Commit 消费一次性预检令牌并交给桌面版执行（或回退到桌面端弹窗确认）。
func Commit(ctx context.Context, file, prepareToken, session string, ev ConfirmEvidence, onDecision func(Decision), onProgress func(string)) (Decision, *Outcome, error) {
	return sendAndWait(ctx, file, wireRequest{Op: "commit", PrepareToken: prepareToken, Session: session, Evidence: &ev}, onDecision, onProgress)
}

// Ask 把请求交给桌面版，等用户决定（最多 Timeout）；同意后等桌面版执行完，期间的进展交给 onProgress。
// 拒绝时 Outcome 为空。ctx 取消时放弃：还没决定的，桌面版上的确认框随之关闭；已经在执行的照样执行完。
func Ask(ctx context.Context, file string, req Request, onDecision func(Decision), onProgress func(string)) (Decision, *Outcome, error) {
	return sendAndWait(ctx, file, wireRequest{Request: req}, onDecision, onProgress)
}

func sendAndWait(ctx context.Context, file string, w wireRequest, onDecision func(Decision), onProgress func(string)) (Decision, *Outcome, error) {
	c, in, err := dialHub(ctx, file)
	if err != nil {
		return Decision{}, nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	w.Token = in.Token
	if err := json.NewEncoder(c).Encode(w); err != nil {
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
