package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/notify"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/settings"
)

// 服务器监控（ferry monitor）：服务器上的 ferry-monitor.timer 每隔几分钟运行 <ferryDir>/ferry-monitor monitor-run，检查
//
//	应用停了、启动失败在反复重启、不是部署引起的重启   （Ferry 生成的单元，按单元文件里的 project=、target=）
//	磁盘快满了                                         （/ 和各部署目录所在的分区）
//	HTTPS 证书快到期、一直是临时证书                   （.ferry/apps/*/tls）
//	防火墙被关了、新出现对外开放的端口
//
// 问题出现时、恢复时各发一次通知（发到开监控时选的地方），不会每次检查都发。
//
//	<ferryDir>/monitor/config.json   各「项目 / 目标」的设置和通知地址（只有 root 能读）
//	<ferryDir>/monitor/state.json    上次检查的时间、现在的问题、单元的启动时间、对外开放的端口

const monitorUnit = "ferry-monitor"

type monitorEntry struct {
	proto.MonitorSpec
	Root string `json:"root"`
}

type monitorConfig struct {
	Entries []monitorEntry `json:"entries"`
}

type monitorState struct {
	LastRun   time.Time                       `json:"lastRun"`
	Problems  map[string]proto.MonitorProblem `json:"problems"`  // 键：<项目>/<目标>|<问题>
	UnitSince map[string]string               `json:"unitSince"` // 单元 → 上次看到的启动时间和主进程号
	Ports     []string                        `json:"ports"`     // 对外开放的端口（8080/tcp），第一次检查时记下
	FwActive  *bool                           `json:"fwActive,omitempty"`
}

func (a *Agent) monitorDir() string { return filepath.Join(a.ferryDir(), "monitor") }

func readJSON(p string, v any) bool {
	b, err := os.ReadFile(p)
	return err == nil && json.Unmarshal(b, v) == nil
}

func (a *Agent) loadMonitor() (monitorConfig, monitorState) {
	var c monitorConfig
	var st monitorState
	readJSON(filepath.Join(a.monitorDir(), "config.json"), &c)
	readJSON(filepath.Join(a.monitorDir(), "state.json"), &st)
	if st.Problems == nil {
		st.Problems = map[string]proto.MonitorProblem{}
	}
	if st.UnitSince == nil {
		st.UnitSince = map[string]string{}
	}
	return c, st
}

func (a *Agent) saveMonitor(c *monitorConfig, st *monitorState) error {
	if err := os.MkdirAll(a.monitorDir(), 0o700); err != nil {
		return err
	}
	if c != nil {
		b, _ := json.MarshalIndent(c, "", "  ")
		if err := writeFileAtomic(filepath.Join(a.monitorDir(), "config.json"), b, 0o600); err != nil {
			return err
		}
	}
	if st != nil {
		b, _ := json.MarshalIndent(st, "", "  ")
		return writeFileAtomic(filepath.Join(a.monitorDir(), "state.json"), b, 0o600)
	}
	return nil
}

func entryKey(project, target string) string { return project + "/" + target }

// monitor 命令：开、关、查看、立即检查、发测试告警。
func (a *Agent) monitor() int {
	m := a.req.Monitor
	if m == nil || m.Project == "" || m.Target == "" {
		return a.result(proto.CodeBadInput, "缺少监控的设置", nil)
	}
	cfg, st := a.loadMonitor()
	key := entryKey(m.Project, m.Target)
	idx := slices.IndexFunc(cfg.Entries, func(e monitorEntry) bool { return entryKey(e.Project, e.Target) == key })
	switch m.Action {
	case "on":
		if len(m.Channels) == 0 {
			return a.result(proto.CodeBadInput, "没有选告警发到哪（在设置 → 通知里添加）", nil)
		}
		e := monitorEntry{MonitorSpec: *m, Root: a.req.Root}
		e.Action = ""
		e.IntervalMin = clamp(e.IntervalMin, 5, 1, 1440)
		e.DiskPct = clamp(e.DiskPct, 90, 50, 99)
		e.CertDays = clamp(e.CertDays, 14, 1, 60)
		if idx >= 0 {
			cfg.Entries[idx] = e
		} else {
			cfg.Entries = append(cfg.Entries, e)
		}
		if err := a.saveMonitor(&cfg, nil); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		if err := a.installMonitorTimer(cfg); err != nil {
			return a.result(proto.CodeFailed, "装定时器失败："+err.Error(), nil)
		}
		a.info(proto.EvInfo, "", "监控已打开：每 %d 分钟检查一次，有问题发到 %s", e.IntervalMin, channelNames(e.Channels))
	case "off":
		if idx >= 0 {
			cfg.Entries = slices.Delete(cfg.Entries, idx, idx+1)
			for k := range st.Problems {
				if strings.HasPrefix(k, key+"|") {
					delete(st.Problems, k)
				}
			}
			if err := a.saveMonitor(&cfg, &st); err != nil {
				return a.result(proto.CodeFailed, err.Error(), nil)
			}
		}
		if err := a.installMonitorTimer(cfg); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		a.info(proto.EvInfo, "", "监控已关闭")
	case "check":
		e := monitorEntry{MonitorSpec: *m, Root: a.req.Root}
		if idx >= 0 {
			e = cfg.Entries[idx]
		}
		e.DiskPct, e.CertDays = clamp(e.DiskPct, 90, 50, 99), clamp(e.CertDays, 14, 1, 60)
		probs, _ := a.monitorCheck([]monitorEntry{e}, &monitorState{UnitSince: map[string]string{}})
		out := proto.MonitorStatus{On: idx >= 0, Problems: []proto.MonitorProblem{}}
		for _, p := range probs {
			out.Problems = append(out.Problems, p.MonitorProblem)
		}
		return a.result(proto.CodeOK, "", out)
	case "test":
		if idx < 0 {
			return a.result(proto.CodeBadInput, "这台服务器上还没打开监控", nil)
		}
		e := cfg.Entries[idx]
		ev := notify.Event{Action: "alert", Project: e.Project, Target: e.Target, Host: e.Host, Time: a.Sys.Now(),
			Lines: []string{"这是一条测试告警：服务器上的 Ferry 能把告警发到这里。"}}
		out := proto.MonitorStatus{On: true, Problems: []proto.MonitorProblem{}, Sent: a.sendAlert(e.Channels, ev)}
		return a.result(proto.CodeOK, "", out)
	}
	return a.result(proto.CodeOK, "", a.monitorStatus(cfg, st, key))
}

func clamp(v, def, lo, hi int) int {
	if v <= 0 {
		return def
	}
	return min(max(v, lo), hi)
}

func channelNames(cs []proto.MonitorChannel) string {
	var n []string
	for _, c := range cs {
		n = append(n, c.Name)
	}
	return strings.Join(n, "、")
}

func (a *Agent) monitorStatus(cfg monitorConfig, st monitorState, key string) proto.MonitorStatus {
	out := proto.MonitorStatus{Problems: []proto.MonitorProblem{}, LastRun: st.LastRun}
	for _, e := range cfg.Entries {
		if entryKey(e.Project, e.Target) == key {
			out.On, out.IntervalMin, out.DiskPct, out.CertDays = true, e.IntervalMin, e.DiskPct, e.CertDays
			for _, c := range e.Channels {
				out.Channels = append(out.Channels, c.Name)
			}
		}
	}
	keys := make([]string, 0, len(st.Problems))
	for k := range st.Problems {
		if strings.HasPrefix(k, key+"|") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Problems = append(out.Problems, st.Problems[k])
	}
	return out
}

// installMonitorTimer 装上（或去掉）定时器：间隔取各项目里最短的。没有项目开着就停掉。
func (a *Agent) installMonitorTimer(cfg monitorConfig) error {
	if len(cfg.Entries) == 0 {
		a.Sys.Systemctl("disable", "--now", monitorUnit+".timer")
		return nil
	}
	interval := 1440
	for _, e := range cfg.Entries {
		interval = min(interval, e.IntervalMin)
	}
	bin, err := a.selfCopy(monitorUnit)
	if err != nil {
		return err
	}
	svc := ManagedMarker + " — 由 ferry 生成：检查应用、磁盘、证书、端口，有问题发通知\n[Unit]\nDescription=Ferry 服务器监控\nAfter=network-online.target\n\n" +
		"[Service]\nType=oneshot\nExecStart=" + bin + " monitor-run\n"
	timer := ManagedMarker + " — 由 ferry 生成\n[Unit]\nDescription=Ferry 服务器监控（定时）\n\n" +
		fmt.Sprintf("[Timer]\nOnBootSec=2min\nOnUnitActiveSec=%dmin\nAccuracySec=30s\n\n", interval) +
		"[Install]\nWantedBy=timers.target\n"
	if err := a.installUnit("", monitorUnit+".service", []byte(svc)); err != nil {
		return err
	}
	if err := a.installUnit("", monitorUnit+".timer", []byte(timer)); err != nil {
		return err
	}
	a.Sys.Systemctl("restart", monitorUnit+".timer") // 间隔改了：重新开始计时
	return a.Sys.Systemctl("enable", "--now", monitorUnit+".timer")
}

// selfCopy 把自己复制成 <ferryDir>/<name>（定时器用固定的路径；agent 按内容命名、旧的会被清理）。
func (a *Agent) selfCopy(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(a.ferryDir(), name)
	if sameContent(exe, dst) {
		return dst, nil
	}
	if err := copyFile(exe, dst+".tmp", 0o755); err != nil {
		return "", err
	}
	return dst, os.Rename(dst+".tmp", dst)
}

// ---------------------------------------------------------------- 检查

// finding 一次检查发现的：持续的问题（Key 不为空，恢复时再通知一次）或一次性的事（Key 为空：重启过、新端口）。
type finding struct {
	proto.MonitorProblem
	entry string // 属于哪个「项目 / 目标」；空 = 整台服务器的（发给所有开着的）
}

// managedUnits Ferry 生成的应用单元：单元 → 项目、应用、目标（单元文件第二行 # project=… app=… target=…）。
func (a *Agent) managedUnits() map[string][3]string {
	out := map[string][3]string{}
	ents, _ := os.ReadDir(a.Sys.UnitDir())
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".service") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(a.Sys.UnitDir(), e.Name()))
		if err != nil || !strings.HasPrefix(string(b), ManagedMarker) {
			continue
		}
		var p, app, t string
		for i, l := range strings.Split(string(b), "\n") {
			if i > 3 {
				break
			}
			if strings.HasPrefix(l, "# project=") {
				for _, kv := range strings.Fields(strings.TrimPrefix(l, "# ")) {
					k, v, _ := strings.Cut(kv, "=")
					switch k {
					case "project":
						p = v
					case "app":
						app = v
					case "target":
						t = v
					}
				}
			}
		}
		if p != "" && app != "" {
			out[e.Name()] = [3]string{p, app, t}
		}
	}
	return out
}

// plannedSince 这个应用在 since 之后有没有部署、回滚、启停（部署历史里有）：有的话重启是计划内的。
func plannedSince(root, app string, since time.Time) bool {
	lines, _ := tailFile(filepath.Join(root, ".ferry", "history.jsonl"), 200)
	for _, l := range lines {
		var h proto.HistoryEntry
		if json.Unmarshal([]byte(l), &h) == nil && h.App == app && !h.Time.Before(since) {
			return true
		}
	}
	return false
}

func (a *Agent) monitorCheck(entries []monitorEntry, st *monitorState) ([]finding, map[string]string) {
	var out []finding
	now := a.Sys.Now()
	byProject := map[string]monitorEntry{}
	for _, e := range entries {
		byProject[entryKey(e.Project, e.Target)] = e
	}
	since := map[string]string{}
	// 1. 应用
	units := a.managedUnits()
	names := make([]string, 0, len(units))
	for u := range units {
		names = append(names, u)
	}
	sort.Strings(names)
	for _, u := range names {
		info := units[u]
		ek := entryKey(info[0], info[2])
		e, ok := byProject[ek]
		if !ok {
			continue
		}
		enabled, _ := a.Sys.Shell("systemctl is-enabled "+shellQuote(u)+" 2>/dev/null", 10*time.Second)
		if strings.TrimSpace(enabled) != "enabled" {
			continue // 没设开机自启的（不停机发布停着的槽位、手动停的不算）
		}
		p := a.Sys.UnitProps(u)
		switch {
		case p.Sub == "auto-restart" || p.Active == "activating" && p.Sub == "auto-restart":
			out = append(out, finding{proto.MonitorProblem{Key: "unit:" + u, Level: "bad", Text: fmt.Sprintf("应用 %s 启动失败，正在反复重启（%s）", info[1], u)}, ek})
		case p.Active == "failed" || p.Active == "inactive":
			out = append(out, finding{proto.MonitorProblem{Key: "unit:" + u, Level: "bad", Text: fmt.Sprintf("应用 %s 停了（%s，%s）", info[1], u, p.Active)}, ek})
		case p.Active == "active":
			// 启动时间只精确到秒：同一秒里重启过看不出来，加上主进程号
			cur := fmt.Sprintf("%s pid %d", p.Since, p.PID)
			if p.Since != "" {
				since[u] = cur
			}
			old, ok := st.UnitSince[u]
			if ok && !strings.Contains(old, " pid ") { // 老版本只记了启动时间
				cur = p.Since
			}
			if ok && old != cur && p.Since != "" && !plannedSince(e.Root, info[1], st.LastRun) {
				out = append(out, finding{proto.MonitorProblem{Level: "warn", Text: fmt.Sprintf("应用 %s 重启过（%s 起），不是部署、启停引起的：可能崩溃后自动重启了，看看日志", info[1], sinceShort(p.Since))}, ek})
			}
		}
	}
	// 1b. Ferry 装的软件
	out = append(out, a.swMonitor()...)
	// 2. 磁盘
	paths := []string{"/"}
	maxPct := 99
	for _, e := range entries {
		paths = append(paths, e.Root)
		maxPct = min(maxPct, e.DiskPct)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		mount, pct, free, err := diskUsage(p)
		if err != nil || seen[mount] {
			continue
		}
		seen[mount] = true
		if pct >= maxPct {
			out = append(out, finding{proto.MonitorProblem{Key: "disk:" + mount, Level: "bad", Text: fmt.Sprintf("磁盘 %s 用了 %d%%，只剩 %s", mount, pct, sizeText(int64(free)))}, ""})
		}
	}
	// 3. 证书
	for _, e := range entries {
		dirs, _ := filepath.Glob(filepath.Join(e.Root, ".ferry", "apps", "*", "tls"))
		for _, d := range dirs {
			app := filepath.Base(filepath.Dir(d))
			c, err := certInfo(d)
			if err != nil {
				continue
			}
			days := int(c.NotAfter.Sub(now).Hours() / 24)
			switch {
			case c.Subject.CommonName == tempCertName && now.Sub(c.NotBefore) > 24*time.Hour:
				out = append(out, finding{proto.MonitorProblem{Key: "cert:" + app, Level: "warn", Text: fmt.Sprintf("%s 的 HTTPS 一直是临时证书（正式证书申请不下来，看 journalctl -u ferry-acme）", app)}, entryKey(e.Project, e.Target)})
			case c.Subject.CommonName != tempCertName && days < e.CertDays:
				out = append(out, finding{proto.MonitorProblem{Key: "cert:" + app, Level: "warn", Text: fmt.Sprintf("%s 的 HTTPS 证书 %d 天后到期（续期没成功？看 journalctl -u ferry-acme）", app, days)}, entryKey(e.Project, e.Target)})
			}
		}
	}
	// 4. 数据备份失败了
	for _, e := range entries {
		for _, p := range backupProblems(e.Root, e.Project, e.Target) {
			out = append(out, finding{p, entryKey(e.Project, e.Target)})
		}
	}
	// 5. 防火墙、对外开放的端口
	rep, _ := a.fwScanner().report()
	if rep != nil {
		fw := rep.Firewall
		if st.FwActive != nil && *st.FwActive && fw.Kind != "" && !fw.Active {
			out = append(out, finding{proto.MonitorProblem{Key: "firewall", Level: "bad", Text: "防火墙（" + fw.Kind + "）被关了：所有端口都能从外面连"}, ""})
		}
		active := fw.Active
		if st.FwActive == nil || active || !(st.FwActive != nil && *st.FwActive) {
			st.FwActive = &active // 关着的一直关着不再报；打开后再关才报
		}
		var open []string
		desc := map[string]string{}
		for _, p := range rep.Ports {
			if exposed(p) {
				k := fmt.Sprintf("%d/%s", p.Port, p.Proto)
				open = append(open, k)
				desc[k] = strings.TrimSpace(p.Process + " " + p.Unit + " " + p.Container)
			}
		}
		if st.Ports != nil {
			for _, k := range open {
				if !slices.Contains(st.Ports, k) {
					out = append(out, finding{proto.MonitorProblem{Level: "warn", Text: fmt.Sprintf("新出现对外开放的端口 %s（%s）：不是有意开的话在 Ferry 的「端口」里关掉", k, orNone(desc[k]))}, ""})
				}
			}
		}
		if open == nil {
			open = []string{}
		}
		st.Ports = open
	}
	return out, since
}

// exposed 外部能连上的端口：监听的不只是本机、不是只在内部网卡上，防火墙没拦（Docker 映射的端口 Ferry 没拦）。
func exposed(p proto.PortInfo) bool {
	if len(p.Listen) == 0 || p.Internal || p.Firewall == "blocked" || (p.Docker && p.Guard) {
		return false
	}
	for _, h := range p.Listen {
		if !strings.HasPrefix(h, "127.") && h != "[::1]" && h != "::1" && h != "localhost" {
			return true
		}
	}
	return false
}

func sinceShort(s string) string {
	if m := strings.Fields(s); len(m) >= 3 {
		return m[1] + " " + m[2]
	}
	return s
}

// sendAlert 发到这些地方，返回各自的结果（名字 → 错误，空 = 成功）。
func (a *Agent) sendAlert(chs []proto.MonitorChannel, ev notify.Event) map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, c := range chs {
		err := notify.Send(ctx, notify.Target{Channel: settings.NotifyChannel{Name: c.Name, Kind: c.Kind}, URL: c.URL, Sign: c.Sign}, ev)
		out[c.Name] = ""
		if err != nil {
			out[c.Name] = err.Error()
		}
	}
	return out
}

// MonitorRun ferry-monitor monitor-run：定时器每隔几分钟运行一次。
func MonitorRun(sys System, out io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	a := &Agent{Sys: sys, Out: out, Plain: true, req: &proto.Request{FerryDir: filepath.Dir(exe)}, appRoots: map[string]string{}}
	return a.monitorRun(a.sendAlert)
}

// monitorRun 检查一次，新出现的问题、恢复了的问题各发一次通知（send），记下这次的状态。
func (a *Agent) monitorRun(send func([]proto.MonitorChannel, notify.Event) map[string]string) int {
	out := a.Out
	cfg, st := a.loadMonitor()
	if len(cfg.Entries) == 0 {
		return 0
	}
	a.req.Root = cfg.Entries[0].Root
	found, since := a.monitorCheck(cfg.Entries, &st)
	sys := a.Sys
	now := sys.Now()
	// 按「项目 / 目标」分：新出现的问题、恢复了的问题、一次性的事
	newBy, goneBy := map[string][]string{}, map[string][]string{}
	current := map[string]proto.MonitorProblem{}
	for _, e := range cfg.Entries {
		ek := entryKey(e.Project, e.Target)
		for _, f := range found {
			if f.entry != "" && f.entry != ek {
				continue
			}
			if f.Key == "" {
				newBy[ek] = append(newBy[ek], f.Text)
				continue
			}
			k := ek + "|" + f.Key
			p := f.MonitorProblem
			if old, ok := st.Problems[k]; ok {
				p.Since = old.Since
			} else {
				p.Since = now
				newBy[ek] = append(newBy[ek], f.Text)
			}
			current[k] = p
		}
	}
	for k, p := range st.Problems {
		if _, ok := current[k]; !ok {
			ek, _, _ := strings.Cut(k, "|")
			goneBy[ek] = append(goneBy[ek], "已恢复："+p.Text)
		}
	}
	for _, e := range cfg.Entries {
		ek := entryKey(e.Project, e.Target)
		for action, lines := range map[string][]string{"alert": newBy[ek], "recovered": goneBy[ek]} {
			if len(lines) == 0 {
				continue
			}
			ev := notify.Event{Action: action, Project: e.Project, Target: e.Target, Host: e.Host, Time: now, Lines: lines}
			for name, err := range send(e.Channels, ev) {
				if err != "" {
					fmt.Fprintf(out, "%s：告警没有发到「%s」：%s\n", ek, name, err)
				}
			}
			fmt.Fprintf(out, "%s %s：%s\n", ek, map[string]string{"alert": "告警", "recovered": "恢复"}[action], strings.Join(lines, "；"))
		}
	}
	st.Problems, st.UnitSince, st.LastRun = current, since, now
	if err := a.saveMonitor(nil, &st); err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	return 0
}
