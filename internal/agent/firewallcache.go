package agent

import (
	"sync"
	"time"

	"github.com/captain/ferry/internal/safe"
)

// 防火墙结果能用多久：端口页要让刚关掉的端口马上消失，应用状态卡片上的提示旧一点没关系。
const (
	fwFullFresh   = 5 * time.Second
	fwStatusFresh = 90 * time.Second
)

// fwCache 常驻 agent 里跨请求共用的防火墙结果：应用状态每分钟刷新时不再每次都起 firewall-cmd
// （firewall-cmd 一次约 0.65 秒）。改动端口的命令是另一个进程，缓存随它自己的进程结束，不会留下旧结果。
type fwCache struct {
	mu      sync.Mutex
	full    fwCacheEntry // 端口页：运行时和永久配置都读
	runtime fwCacheEntry // 应用状态：只读运行时
}

type fwCacheEntry struct {
	st   *fwState
	at   time.Time
	busy bool // 正在后台刷新
}

func (c *fwCache) entry(runtimeOnly bool) *fwCacheEntry {
	if runtimeOnly {
		return &c.runtime
	}
	return &c.full
}

func (c *fwCache) lookup(runtimeOnly bool) (*fwState, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entry(runtimeOnly)
	if e.st == nil {
		return nil, time.Time{}, false
	}
	return e.st, e.at, true
}

func (c *fwCache) store(runtimeOnly bool, st *fwState) {
	if st == nil {
		return
	}
	c.mu.Lock()
	e := c.entry(runtimeOnly)
	e.st, e.at = st, time.Now()
	c.mu.Unlock()
}

func (c *fwCache) begin(runtimeOnly bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entry(runtimeOnly)
	if e.busy {
		return false
	}
	e.busy = true
	return true
}

func (c *fwCache) end(runtimeOnly bool) {
	c.mu.Lock()
	c.entry(runtimeOnly).busy = false
	c.mu.Unlock()
}

// firewallState 查防火墙（带缓存）：应用状态先用旧结果、后台刷新；端口页要新结果，过期就现查。
func (a *Agent) firewallState(runtimeOnly bool) *fwState {
	sc := a.fwScanner()
	sc.runtimeOnly = runtimeOnly
	if a.fw == nil {
		return sc.firewall()
	}
	fresh := fwFullFresh
	if runtimeOnly {
		fresh = fwStatusFresh
	}
	if st, at, ok := a.fw.lookup(runtimeOnly); ok {
		if time.Since(at) <= fresh {
			return st
		}
		if runtimeOnly {
			a.refreshFirewall()
			return st
		}
	}
	st := sc.firewall()
	a.fw.store(runtimeOnly, st)
	return st
}

// refreshFirewall 后台重读应用状态要的防火墙（只读运行时）；同一时间只跑一个。
func (a *Agent) refreshFirewall() {
	if a.fw == nil || a.bgSys == nil || !a.fw.begin(true) {
		return
	}
	sys := a.bgSys
	safe.Go("刷新防火墙", func() {
		defer a.fw.end(true)
		sc := &fwScan{proc: "/proc", root: "/", unitDir: sys.UnitDir(), run: sys.Shell, runtimeOnly: true, ifaces: hostIfaces}
		a.fw.store(true, sc.firewall())
	})
}
