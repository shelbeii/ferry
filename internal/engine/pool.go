package engine

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/safe"
)

// Pool 到服务器的常驻连接（ferry serve 里用）：同一台服务器、同样的登录方式只建一条连接，
// 部署、查看状态、历史、日志等操作都复用它；空闲超过 Idle 自动断开，下次用到时再连。
type Pool struct {
	mu      sync.Mutex
	entries map[string]*poolEntry
	idle    time.Duration
	quit    chan struct{}
	once    sync.Once
}

type poolEntry struct {
	key      string
	cn       *conn
	users    int // 正在使用它的请求数
	created  time.Time
	lastUsed time.Time
	refs     map[poolRef]bool // 用过它的「配置文件 + 目标」
	dialing  chan struct{}    // 正在建立时不为空，建好或失败后关闭
	stale    bool             // 已失效（配置、凭据变了或手动断开）：没人用时关闭，不再分给新请求
}

type poolRef struct{ config, target string }

// reuseCheckAfter 空闲超过这么久的连接，复用前先确认还通（keepalive 30 秒一次，期间断了未必知道）。
const reuseCheckAfter = 10 * time.Second

// NewPool 创建连接池；idle 为 0 表示不因空闲断开。
func NewPool(idle time.Duration) *Pool {
	p := &Pool{entries: map[string]*poolEntry{}, idle: idle, quit: make(chan struct{})}
	safe.Go("清理空闲连接", p.reap)
	return p
}

// SetIdle 修改空闲断开时间（设置改了之后）。
func (p *Pool) SetIdle(idle time.Duration) {
	p.mu.Lock()
	p.idle = idle
	p.mu.Unlock()
}

// poolKey 决定两次连接能不能共用：服务器、用户、登录方式、私钥（含文件的修改时间，重新生成后不再复用旧连接）、主机公钥、sudo、
// 是否只登录（「新建连接」的服务器不装 agent，不能给部署用）。
func poolKey(f *config.File, t *config.Target) string {
	id := identity(f, t)
	if id != "" {
		if abs, err := filepath.Abs(id); err == nil {
			id = abs
		}
		if fi, err := os.Stat(id); err == nil {
			id = fmt.Sprintf("%s@%d/%d", id, fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return strings.Join([]string{t.User, t.Host, fmt.Sprint(t.Port), t.Auth, id, t.HostKey, fmt.Sprint(t.Sudo), t.FerryDir, fmt.Sprint(t.Plain)}, "|")
}

func refOf(f *config.File, t *config.Target) poolRef {
	path := f.Path
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return poolRef{path, t.Name}
}

// Acquire 取得到目标的连接（复用或新建）；用完调用返回的 release。
func (p *Pool) Acquire(e *Engine, t *config.Target) (*conn, func(), error) {
	key := poolKey(e.File, t)
	ref := refOf(e.File, t)
	for {
		p.mu.Lock()
		ent := p.entries[key]
		if ent != nil && ent.dialing != nil { // 另一个请求正在连同一台服务器：等它的结果
			ch := ent.dialing
			p.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-e.ctx().Done():
				return nil, nil, ErrCanceled
			}
		}
		if ent != nil && ent.stale {
			ent = nil
		}
		if e.Background && (ent == nil || ent.dialing != nil) {
			p.mu.Unlock()
			return nil, nil, ErrNotConnected
		}
		if ent != nil {
			ent.users++
			idleFor := time.Since(ent.lastUsed)
			p.mu.Unlock()
			ok := ent.cn.c.Alive()
			if ok && (ent.cn.broken.Load() || idleFor > reuseCheckAfter) {
				ok = ent.cn.c.Ping(5*time.Second) == nil
			}
			if !ok {
				p.drop(ent)
				if e.Background {
					return nil, nil, ErrNotConnected
				}
				e.Rep.Info("到 %s 的连接已断开，重新连接", ent.cn.label)
				continue
			}
			ent.cn.broken.Store(false)
			p.mu.Lock()
			p.retarget(ent, ref)
			p.mu.Unlock()
			e.Rep.Step("连接 %s（%s，复用已有连接）", t.Name, ent.cn.label)
			return ent.cn, p.releaser(ent, !e.Background), nil
		}
		// 新建
		ent = &poolEntry{key: key, dialing: make(chan struct{}), refs: map[poolRef]bool{}}
		p.entries[key] = ent
		p.mu.Unlock()

		e.Rep.Step("连接 %s（%s@%s:%d）", t.Name, t.User, t.Host, t.Port)
		cn, err := e.dial(t)

		p.mu.Lock()
		close(ent.dialing)
		ent.dialing = nil
		if err != nil {
			if p.entries[key] == ent {
				delete(p.entries, key)
			}
			p.mu.Unlock()
			return nil, nil, err
		}
		ent.cn, ent.users, ent.created, ent.lastUsed = cn, 1, time.Now(), time.Now()
		p.retarget(ent, ref)
		p.mu.Unlock()
		return cn, p.releaser(ent, true), nil
	}
}

// retarget 记下这个目标现在用的连接；它以前用的另一条连接（配置改了）失效。调用时持有锁。
func (p *Pool) retarget(ent *poolEntry, ref poolRef) {
	for _, other := range p.entries {
		if other != ent && other.refs[ref] {
			delete(other.refs, ref)
			if len(other.refs) == 0 {
				p.invalidate(other)
			}
		}
	}
	ent.refs[ref] = true
}

// releaser 用完连接时调用；used 为 false（后台自动刷新）时不算一次使用，不推迟空闲断开。
func (p *Pool) releaser(ent *poolEntry, used bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			ent.users--
			if used {
				ent.lastUsed = time.Now()
			}
			if ent.users == 0 && (ent.stale || !ent.cn.c.Alive()) {
				p.remove(ent)
			}
		})
	}
}

// drop 复用前发现连接已断：不再使用它。
func (p *Pool) drop(ent *poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ent.users--
	ent.stale = true
	if ent.users == 0 {
		p.remove(ent)
	}
}

// remove 关闭并移出连接池。调用时持有锁。
func (p *Pool) remove(ent *poolEntry) {
	if p.entries[ent.key] == ent {
		delete(p.entries, ent.key)
	}
	if ent.cn != nil {
		go ent.cn.c.Close()
	}
}

// invalidate 让连接失效：没人用就立即关闭，否则用完后关闭。调用时持有锁。
func (p *Pool) invalidate(ent *poolEntry) {
	if ent.dialing != nil {
		return
	}
	ent.stale = true
	if ent.users == 0 {
		p.remove(ent)
	}
}

// InvalidateAll 全部连接失效（凭据、主机公钥、数据目录等变了）：下次用到时重新建立。
func (p *Pool) InvalidateAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ent := range p.entries {
		p.invalidate(ent)
	}
}

// Disconnect 断开目标当前的连接；返回是否有连接被断开。
func (p *Pool) Disconnect(f *config.File, t *config.Target) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ent := p.entries[poolKey(f, t)]
	if ent == nil || ent.dialing != nil || ent.stale {
		return false
	}
	p.invalidate(ent)
	return true
}

// Close 关闭全部连接并停止后台检查（ferry serve 退出时）。
func (p *Pool) Close() {
	p.once.Do(func() { close(p.quit) })
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ent := range p.entries {
		if ent.cn != nil {
			ent.cn.c.Close()
		}
	}
	p.entries = map[string]*poolEntry{}
}

// reap 定期关闭空闲太久或已经断开的连接。
func (p *Pool) reap() {
	for {
		p.mu.Lock()
		every := 15 * time.Second
		if p.idle > 0 && p.idle/4 < every {
			every = max(p.idle/4, 200*time.Millisecond)
		}
		p.mu.Unlock()
		select {
		case <-p.quit:
			return
		case <-time.After(every):
		}
		p.mu.Lock()
		for _, ent := range p.entries {
			if ent.dialing != nil || ent.users > 0 {
				continue
			}
			if !ent.cn.c.Alive() || (p.idle > 0 && time.Since(ent.lastUsed) >= p.idle) {
				p.remove(ent)
			}
		}
		p.mu.Unlock()
	}
}

// ConnStatus 一个目标的连接状态。
type ConnStatus struct {
	Target    string    `json:"target"`
	Connected bool      `json:"connected"`
	Label     string    `json:"label,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
	InUse     int       `json:"inUse"`
	// RTT 到服务器的往返时间（毫秒）；Slow 量的时候超过 LatencyTimeout 还没回来
	RTT  float64 `json:"rtt,omitempty"`
	Slow bool    `json:"slow,omitempty"`
}

// 量往返时间：最多等多久；多久以内量过的直接用（桌面版几个地方同时问时不重复量）。
const (
	LatencyTimeout = 3 * time.Second
	latencyMaxAge  = 2 * time.Second
)

// Status 配置文件里各目标的连接状态，已连接的顺便量一下到服务器的往返时间（同时量，不占着连接池）。
func (p *Pool) Status(f *config.File) []ConnStatus {
	p.mu.Lock()
	out := []ConnStatus{}
	var clients []*remote.Client
	for _, name := range slices.Sorted(maps.Keys(f.Targets)) {
		t := f.Targets[name]
		st := ConnStatus{Target: name}
		var c *remote.Client
		if ent := p.entries[poolKey(f, t)]; ent != nil && ent.dialing == nil && !ent.stale && ent.cn.c.Alive() {
			st.Connected, st.Label, st.Since, st.LastUsed, st.InUse = true, ent.cn.label, ent.created, ent.lastUsed, ent.users
			c = ent.cn.c
		}
		out = append(out, st)
		clients = append(clients, c)
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for i, c := range clients {
		if c == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Recover("测延迟", nil)
			d, err := c.Latency(LatencyTimeout, latencyMaxAge)
			switch {
			case err == remote.ErrSlow:
				out[i].Slow = true
			case err != nil:
				out[i].Connected = false // 量的时候发现已经断了
			default:
				out[i].RTT = latencyMs(d)
			}
		}()
	}
	wg.Wait()
	return out
}

// latencyMs 往返时间的毫秒数（保留一位小数）。
func latencyMs(d time.Duration) float64 {
	return float64(d.Round(100*time.Microsecond).Microseconds()) / 1000
}

// PoolEntry 连接池里的一条连接。
type PoolEntry struct {
	Label    string    `json:"label"`
	Targets  []string  `json:"targets"`
	Since    time.Time `json:"since"`
	LastUsed time.Time `json:"lastUsed"`
	InUse    int       `json:"inUse"`
}

// List 全部连接。
func (p *Pool) List() []PoolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []PoolEntry{}
	for _, ent := range p.entries {
		if ent.dialing != nil || ent.stale {
			continue
		}
		pe := PoolEntry{Label: ent.cn.label, Since: ent.created, LastUsed: ent.lastUsed, InUse: ent.users, Targets: []string{}}
		for r := range ent.refs {
			pe.Targets = append(pe.Targets, r.target)
		}
		sort.Strings(pe.Targets)
		out = append(out, pe)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}
