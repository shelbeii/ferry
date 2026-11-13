package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 告警里平时也要做的检查（monitor.go 的 monitorCheck 调用）：
//
//	应用的健康检查（部署时判断就绪用的 HTTP / TCP）平时也探一探：进程还在、接口已经不通（假死）也能发现
//	内存快用完、负载太高、内存不够系统杀了进程（OOM）
//	每次检查完访问一下 PingURL（心跳）：服务器宕机、断网时那边收不到，由那边告警

// appHealth 部署成功时记下的健康检查：<部署目录>/.ferry/apps/<应用>/health.json。
type appHealth struct {
	Unit   string       `json:"unit"`
	Health proto.Health `json:"health"`
}

func healthFile(meta, app string) string { return filepath.Join(meta, "apps", app, "health.json") }

// saveAppHealth 部署（或不停机发布切换）成功后记下这个应用现在用的单元和健康检查；没有健康检查时删掉。
func (a *Agent) saveAppHealth(app, unit string, h proto.Health) {
	p := healthFile(a.meta(), app)
	if h.TCP == "" && h.HTTP == "" {
		os.Remove(p)
		return
	}
	b, _ := json.Marshal(appHealth{Unit: unit, Health: h})
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		writeFileAtomic(p, b, 0o644)
	}
}

// healthOf 这个单元平时要通过的健康检查：先看部署时记下的，没有再看打开告警时 Ferry 传来的。
func healthOf(e monitorEntry, app, unit string) (proto.Health, bool) {
	var h appHealth
	if readJSON(healthFile(filepath.Join(e.Root, dirMeta), app), &h) && h.Unit == unit {
		return h.Health, h.Health.TCP != "" || h.Health.HTTP != ""
	}
	for _, x := range e.Apps {
		if x.App == app && x.Unit == unit {
			return x.Health, x.Health.TCP != "" || x.Health.HTTP != ""
		}
	}
	return proto.Health{}, false
}

// probeAgain 探 3 次（隔 5 秒），都不通才算不通：偶尔一次超时不告警。
func (a *Agent) probeAgain(h proto.Health) error {
	var err error
	for i := 0; i < 3; i++ {
		if i > 0 {
			a.Sys.Sleep(5 * time.Second)
		}
		if err = a.Sys.Probe(h); err == nil {
			return nil
		}
	}
	return err
}

// ---------------------------------------------------------------- 内存、负载、OOM

// topMemory 占内存最多的几个进程：「java 2.1 GB、mysqld 900 MB」。
func topMemory(proc string, n int) string {
	type p struct {
		name string
		rss  int64
	}
	var ps []p
	ents, _ := os.ReadDir(proc)
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(proc, e.Name(), "status"))
		if err != nil {
			continue
		}
		var name string
		var rss int64
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "Name:"); ok {
				name = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
				if f := strings.Fields(v); len(f) > 0 {
					rss, _ = strconv.ParseInt(f[0], 10, 64)
				}
				break
			}
		}
		if rss > 0 {
			ps = append(ps, p{name, rss})
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].rss > ps[j].rss })
	var out []string
	for i := 0; i < len(ps) && i < n; i++ {
		out = append(out, ps[i].name+" "+sizeText(ps[i].rss*1024))
	}
	return strings.Join(out, "、")
}

// loadAvg /proc/loadavg 的 15 分钟平均负载。
func loadAvg(proc string) (float64, bool) {
	b, err := os.ReadFile(filepath.Join(proc, "loadavg"))
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[2], 64)
	return v, err == nil
}

// cpuCount 在线的 CPU 数、procDir 读内存和负载的目录（测试时换掉）。
var (
	cpuCount = runtime.NumCPU
	procDir  = "/proc"
)

// 内核日志里 OOM 杀进程的那行：Out of memory: Killed process 1234 (java) total-vm:..., anon-rss:3355443kB, ...
var reOOM = regexp.MustCompile(`(?:Out of memory|Memory cgroup out of memory): Kill(?:ed)? process (\d+) \(([^)]*)\)(?:.*?anon-rss:(\d+)kB)?`)

// oomKills since 之后内核因为内存不够杀掉的进程（读 journalctl -k；没有 journald 时什么都不报）。
func (a *Agent) oomKills(since time.Time) []string {
	out, err := a.Sys.Shell(fmt.Sprintf("journalctl -k --since=@%d --no-pager -q -o cat 2>/dev/null | grep -E 'Out of memory|out of memory' | tail -n 20", since.Unix()), 20*time.Second)
	if err != nil && strings.TrimSpace(out) == "" {
		return nil
	}
	var kills []string
	for _, l := range splitLines(out) {
		m := reOOM.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		s := fmt.Sprintf("%s（pid %s", m[2], m[1])
		if kb, err := strconv.ParseInt(m[3], 10, 64); err == nil && kb > 0 {
			s += "，占 " + sizeText(kb*1024)
		}
		kills = append(kills, s+"）")
	}
	return kills
}

// resourceFindings 内存、负载、OOM。memPct、loadX 取各项目里最严的。
func (a *Agent) resourceFindings(entries []monitorEntry, lastRun time.Time) []finding {
	var out []finding
	memPct, loadX := 99, 100
	for _, e := range entries {
		memPct = min(memPct, clamp(e.MemPct, 90, 50, 99))
		loadX = min(loadX, clamp(e.LoadX, 2, 1, 100))
	}
	proc := procDir
	if total, avail := memInfo(filepath.Join(proc, "meminfo")); total > 0 && avail > 0 {
		if pct := int((total - avail) * 100 / total); pct >= memPct {
			text := fmt.Sprintf("内存用了 %d%%，只剩 %s 可用", pct, sizeText(avail<<20))
			if top := topMemory(proc, 3); top != "" {
				text += "（占得最多的：" + top + "）"
			}
			out = append(out, finding{proto.MonitorProblem{Key: "mem", Level: "bad", Text: text}, ""})
		}
	}
	if load, ok := loadAvg(proc); ok {
		cpus := max(cpuCount(), 1)
		if load >= float64(loadX*cpus) {
			out = append(out, finding{proto.MonitorProblem{Key: "load", Level: "warn",
				Text: fmt.Sprintf("负载太高：15 分钟平均 %.1f，是 CPU 核数（%d）的 %.1f 倍", load, cpus, load/float64(cpus))}, ""})
		}
	}
	since := lastRun
	if since.IsZero() || a.Sys.Now().Sub(since) > 24*time.Hour {
		since = a.Sys.Now().Add(-10 * time.Minute) // 第一次检查、很久没查：只看最近的，不翻旧账
	}
	if kills := a.oomKills(since); len(kills) > 0 {
		out = append(out, finding{proto.MonitorProblem{Level: "bad",
			Text: "内存不够，系统杀掉了进程：" + strings.Join(kills, "、") + "。看看是哪个程序占得多，或者加内存、给程序设上限"}, ""})
	}
	return out
}

// ---------------------------------------------------------------- 心跳

// pingClient 访问心跳地址：走系统代理设置之外的直连，10 秒超时。
var pingClient = &http.Client{Timeout: 10 * time.Second}

// ping 访问各项目设的心跳地址（同一个地址只访问一次）；返回各地址的错误（空 = 成功）。
func (a *Agent) ping(entries []monitorEntry) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		u := e.PingURL
		if u == "" {
			continue
		}
		if _, done := out[u]; done {
			continue
		}
		out[u] = ""
		if err := pingURL(u); err != nil {
			out[u] = err.Error()
		}
	}
	return out
}

// pingURL 访问一次心跳地址（测试时换掉）。
var pingURL = pingOnce

func pingOnce(u string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ferry-monitor")
	resp, err := pingClient.Do(req)
	if err != nil {
		return fmt.Errorf("访问不了心跳地址：%v", shortErr(err))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("心跳地址返回 %d", resp.StatusCode)
	}
	return nil
}

// validPingURL 心跳地址只能是 http(s) 的。
func validPingURL(u string) bool {
	return (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) && len(u) <= 2048 && !strings.ContainsAny(u, " \t\r\n")
}
