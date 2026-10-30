package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 端口与防火墙（ports 只读；portset 放行 / 关闭一个端口）：
//   - 端口：/proc/net/tcp、tcp6 里正在监听的，按 socket 找到进程和它所在的 systemd 单元；
//     加上 Docker 映射到主机的端口（关了 userland-proxy 时只有 iptables 规则，没有进程在监听），和防火墙放行了、没有程序在用的。
//   - 防火墙：firewalld 在运行就看它（默认路由那张网卡所在的区域），否则看 ufw；
//     都没开时看 INPUT 链里有没有别的拦截规则（iptables / nftables：只显示，不改）。
//   - 只改 firewalld、ufw 的端口规则，运行时和永久配置一起改；SSH 端口不许关。
//     端口范围、富规则、限制来源的规则不拆，让用户在终端处理。

// fwRule 防火墙里的一条端口规则（展开成端口范围）。
type fwRule struct {
	lo, hi int
	proto  string // tcp | udp；空 = 两种都算（ufw 不写协议时）
	allow  bool   // 放行；false = 拦截
	source string // 只对这些来源；空 = 全部
	// kind port（单个端口）| service（firewalld 服务）| app（ufw 应用）| range | list（ufw 的 80,443/tcp）| rich | complex（ufw 带地址、网卡的）
	kind  string
	spec  string // 删除时用：firewalld 的 8080/tcp 或服务名；ufw 的 To 或应用名
	verb  string // ufw：allow | limit
	label string // 给人看
	temp  bool   // 只在运行时（没写进永久配置）
}

func (r *fwRule) covers(port int, pr string) bool {
	return port >= r.lo && port <= r.hi && (r.proto == "" || r.proto == pr)
}

// fwState 防火墙与它的端口规则。
type fwState struct {
	proto.Firewall
	rules []fwRule // 按生效的顺序（ufw 先匹配的生效）
	deflt string   // 没有规则匹配时：blocked | open；空 = 看不出来
}

// decide 端口在防火墙里的状态与起作用的规则。
func (st *fwState) decide(port int, pr string) (string, *fwRule) {
	if !st.Active {
		return "", nil
	}
	var partial *fwRule
	for i := range st.rules {
		r := &st.rules[i]
		if !r.covers(port, pr) {
			continue
		}
		if r.source != "" {
			if r.allow && partial == nil {
				partial = r
			}
			continue
		}
		if r.allow {
			return "open", r
		}
		return "blocked", r
	}
	if partial != nil {
		return "partial", partial
	}
	return st.deflt, nil
}

// fwScan 一次查看。依赖的系统操作都是字段，测试时换成假的。
type fwScan struct {
	proc     string // /proc
	root     string // 读 firewalld 服务定义的根目录（/）
	unitDir  string
	run      func(cmd string, timeout time.Duration) (string, error)
	keep     []int                    // 不许关闭的端口（Ferry 连服务器用的 SSH 端口）
	ferryDir string                   // 拦着的 Docker 端口记在这里（docker-guard.json）
	ifaces   func() map[string]string // 地址 → 所在的网卡；nil = 不看

	ssh map[int]bool // sshd 监听的端口
}

func (a *Agent) fwScanner() *fwScan {
	return &fwScan{proc: "/proc", root: "/", unitDir: a.Sys.UnitDir(), run: a.Sys.Shell, keep: a.req.KeepPorts, ferryDir: a.ferryDir(), ifaces: hostIfaces}
}

func (a *Agent) ports() int {
	rep, _ := a.fwScanner().report()
	return a.result(proto.CodeOK, "", rep)
}

const fwTimeout = 30 * time.Second

// report 查看端口与防火墙。
func (sc *fwScan) report() (*proto.PortReport, *fwState) {
	rep := &proto.PortReport{Ports: []proto.PortInfo{}}
	st := sc.firewall()
	rep.Firewall = st.Firewall

	byKey := map[string]*proto.PortInfo{}
	add := func(port int, pr string) *proto.PortInfo {
		k := strconv.Itoa(port) + "/" + pr
		if p := byKey[k]; p != nil {
			return p
		}
		p := &proto.PortInfo{Port: port, Proto: pr}
		byKey[k] = p
		return p
	}

	// 1. 正在监听的
	sc.ssh = map[int]bool{}
	socks := readListen(sc.proc)
	want := map[string]bool{}
	for _, s := range socks {
		want[s.inode] = true
	}
	owners := socketPIDs(sc.proc, want)
	for _, s := range socks {
		p := add(s.port, "tcp")
		p.Listen = addHost(p.Listen, s.host)
		pid := owners[s.inode]
		if pid <= 0 || (p.PID > 0 && p.PID < pid) {
			continue
		}
		p.PID = pid
		b, _ := os.ReadFile(filepath.Join(sc.proc, strconv.Itoa(pid), "comm"))
		p.Process = strings.TrimSpace(string(b))
		if cg, err := os.ReadFile(filepath.Join(sc.proc, strconv.Itoa(pid), "cgroup")); err == nil {
			unit, ctr := cgroupOwner(string(cg))
			p.Unit, p.Container = unit, shortID(ctr)
		}
		p.Ferry = p.Unit != "" && sc.ferryUnit(p.Unit)
		p.Docker = p.Process == "docker-proxy"
		if p.Process == "sshd" {
			sc.ssh[s.port] = true
		}
	}

	// 2. Docker 映射到主机的；用主机网络的容器里的进程换成容器名
	names, note := sc.docker(add)
	if note != "" {
		rep.Notes = append(rep.Notes, note)
	}
	for _, p := range byKey {
		if n := names[p.Container]; n != "" {
			p.Container = n
		}
	}
	sc.dockerGuard(rep, byKey, add)

	// 3. 防火墙放行了的单个端口（没有程序在用的也列出来，可以关掉）
	for _, r := range st.rules {
		if r.allow && r.lo == r.hi && r.kind != "complex" {
			pr := r.proto
			if pr == "" {
				pr = "tcp"
			}
			add(r.lo, pr)
		}
	}

	var ifaces map[string]string
	if sc.ifaces != nil {
		ifaces = sc.ifaces()
	}
	for _, p := range byKey {
		if len(p.Listen) > 0 && p.Listen[0] == "*" {
			p.Listen = []string{"*"}
		}
		markIfaces(p, ifaces)
		state, rule := st.decide(p.Port, p.Proto)
		p.Firewall = state
		if rule != nil {
			p.Rule, p.Temp = rule.label, rule.temp
		}
		p.SSH = p.Proto == "tcp" && sc.protected(p.Port)
		rep.Ports = append(rep.Ports, *p)
	}
	sort.Slice(rep.Ports, func(i, j int) bool {
		a, b := rep.Ports[i], rep.Ports[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Proto < b.Proto
	})
	if os.Geteuid() != 0 && sc.proc == "/proc" {
		rep.Notes = append(rep.Notes, "agent 没有以 root 运行，其他用户的进程可能看不到")
	}
	// 打开、关闭 firewalld 时要知道 Docker 在不在运行
	st.Disable = st.Active && (st.Kind == "firewalld" || st.Kind == "ufw")
	if st.Enable || st.Disable {
		out, _ := sc.run("systemctl is-active docker 2>/dev/null", 10*time.Second)
		st.Docker = strings.TrimSpace(out) == "active"
	}
	rep.Firewall = st.Firewall
	return rep, st
}

// protected SSH 端口：sshd 监听的，和 Ferry 连服务器用的。
func (sc *fwScan) protected(port int) bool {
	if sc.ssh[port] {
		return true
	}
	for _, k := range sc.keep {
		if k == port {
			return true
		}
	}
	return false
}

// addHost 把监听地址加进列表（不重复，排好序）。
func addHost(list []string, h string) []string {
	for _, v := range list {
		if v == h {
			return list
		}
	}
	list = append(list, h)
	sort.Slice(list, func(i, j int) bool { return list[i] == "*" || (list[j] != "*" && list[i] < list[j]) })
	return list
}

func (sc *fwScan) ferryUnit(unit string) bool {
	b, err := os.ReadFile(filepath.Join(sc.unitDir, unit))
	return err == nil && strings.HasPrefix(string(b), ManagedMarker)
}

// ---------------------------------------------------------------- 监听的 socket

type listenSock struct {
	port  int
	host  string // *、127.0.0.1、[::1]……
	inode string
}

// readListen /proc/net/tcp、tcp6 里处于监听状态的 socket（主机的网络；容器自己网络里的看不到）。
func readListen(proc string) []listenSock {
	var out []listenSock
	for _, name := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(filepath.Join(proc, "net", name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" { // 0A = LISTEN
				continue
			}
			hostHex, portHex, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil {
				continue
			}
			out = append(out, listenSock{port: int(port), host: hexHost(hostHex), inode: f[9]})
		}
	}
	return out
}

// socketPIDs 这些 socket 属于哪个进程（有多个时取进程号最小的）。
func socketPIDs(proc string, want map[string]bool) map[string]int {
	out := map[string]int{}
	if len(want) == 0 {
		return out
	}
	ents, _ := os.ReadDir(proc)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(proc, e.Name(), "fd")
		fds, _ := os.ReadDir(dir)
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			ino := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			if want[ino] {
				if cur, ok := out[ino]; !ok || pid < cur {
					out[ino] = pid
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- Docker

// docker 运行中的容器映射到主机的端口，以及容器是怎么建的（1Panel、compose）。返回容器短 ID → 容器名，读不到时返回说明。
func (sc *fwScan) docker(add func(int, string) *proto.PortInfo) (map[string]string, string) {
	out, err := sc.run("command -v docker >/dev/null 2>&1 || exit 0; docker ps --no-trunc --format '{{.ID}}\t{{.Names}}\t{{.Ports}}\t{{.Labels}}' 2>&1", 15*time.Second)
	if err != nil {
		msg := strings.TrimSpace(out)
		if strings.Contains(msg, "Cannot connect to the Docker daemon") || strings.Contains(msg, "Is the docker daemon running") {
			return nil, ""
		}
		return nil, "读取 Docker 容器的端口映射失败：" + clipLine(msg)
	}
	names := map[string]string{}
	for _, line := range splitLines(out) {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 2 {
			continue
		}
		name := strings.TrimPrefix(strings.Split(f[1], ",")[0], "/")
		names[shortID(f[0])] = name
		if len(f) < 3 {
			continue
		}
		var labels map[string]string
		if len(f) == 4 {
			labels = parseLabels(f[3])
		}
		for _, m := range dockerMaps(f[2]) {
			p := add(m.port, m.proto)
			p.Listen = addHost(p.Listen, m.host)
			p.Docker, p.Container, p.CtrPort = true, name, m.ctr
			switch {
			case labels["createdBy"] == "1Panel" || strings.HasPrefix(name, "1Panel-"):
				p.Manager = "1Panel"
			case labels["com.docker.compose.project"] != "":
				p.Manager = "compose"
				p.ComposeFile, _, _ = strings.Cut(labels["com.docker.compose.project.config_files"], ",")
				p.ComposeDir = labels["com.docker.compose.project.working_dir"]
				p.ComposeService = labels["com.docker.compose.service"]
			}
		}
	}
	return names, ""
}

// dockerMap 一条端口映射：主机地址:端口 → 容器端口。
type dockerMap struct {
	host      string // *、127.0.0.1、[::1]
	port, ctr int
	proto     string
}

// dockerMaps docker ps 的 Ports：0.0.0.0:3306->3306/tcp, :::3306->3306/tcp, 33060/tcp, 0.0.0.0:8000-8002->8000-8002/tcp
// → 映射到主机的每个端口（范围展开，最多 100 个）。
func dockerMaps(s string) []dockerMap {
	var out []dockerMap
	seen := map[string]bool{}
	rng := func(v string) (int, int, bool) {
		a, b, isRange := strings.Cut(v, "-")
		lo, err1 := strconv.Atoi(a)
		hi, err2 := lo, error(nil)
		if isRange {
			hi, err2 = strconv.Atoi(b)
		}
		return lo, hi, err1 == nil && err2 == nil && lo > 0 && lo <= hi && hi <= 65535
	}
	for _, part := range strings.Split(s, ",") {
		host, ctr, ok := strings.Cut(strings.TrimSpace(part), "->")
		if !ok {
			continue
		}
		pr := "tcp"
		if c, p, ok := strings.Cut(ctr, "/"); ok {
			ctr, pr = c, p
		}
		i := strings.LastIndexByte(host, ':')
		if i < 0 {
			continue
		}
		addr := host[:i]
		switch addr {
		case "0.0.0.0", "::", "[::]", "":
			addr = "*"
		}
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]"
		}
		hlo, hhi, ok1 := rng(host[i+1:])
		clo, chi, ok2 := rng(ctr)
		if !ok1 || !ok2 || hhi-hlo != chi-clo || hhi-hlo > 100 {
			continue
		}
		for k := 0; k <= hhi-hlo; k++ {
			key := fmt.Sprintf("%s:%d/%s", addr, hlo+k, pr)
			if !seen[key] {
				seen[key] = true
				out = append(out, dockerMap{host: addr, port: hlo + k, ctr: clo + k, proto: pr})
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- 防火墙

// firewall 看服务器用的是哪种防火墙，读出端口规则。
func (sc *fwScan) firewall() *fwState {
	fwd, _ := sc.run("if command -v firewall-cmd >/dev/null 2>&1; then LC_ALL=C firewall-cmd --state 2>&1; else echo none; fi", fwTimeout)
	fwd = strings.TrimSpace(fwd)
	if fwd == "running" {
		return sc.firewalld()
	}
	kind := ""
	if fwd != "none" && fwd != "" {
		kind = "firewalld" // 装了没运行
	}
	if out, err := sc.run("if command -v ufw >/dev/null 2>&1; then LC_ALL=C ufw status verbose 2>&1; else echo none; fi", fwTimeout); err == nil {
		if st, lines := parseUFW(out); st != nil {
			if st.Active {
				sc.ufwRules(st, lines)
				return st
			}
			kind = "ufw"
		}
	}
	// 都没开：INPUT 链里有没有别的拦截规则
	if out, err := sc.run("iptables -S INPUT 2>/dev/null", fwTimeout); err == nil && iptablesFiltering(out) {
		return &fwState{Firewall: proto.Firewall{Kind: "iptables", Active: true, Why: "这台服务器用 iptables 规则拦截端口（不是 firewalld 或 ufw）：Ferry 只显示，不修改"}}
	}
	if out, err := sc.run("nft list ruleset 2>/dev/null", fwTimeout); err == nil && nftFiltering(out) {
		return &fwState{Firewall: proto.Firewall{Kind: "nftables", Active: true, Why: "这台服务器用 nftables 规则拦截端口（不是 firewalld 或 ufw）：Ferry 只显示，不修改"}}
	}
	why := "服务器上没有防火墙（firewalld 或 ufw）：所有端口都不拦"
	switch kind {
	case "firewalld":
		why = "firewalld 装了但没有运行：所有端口都不拦"
	case "ufw":
		why = "ufw 装了但没有启用：所有端口都不拦"
	}
	return &fwState{Firewall: proto.Firewall{Kind: kind, Why: why, Enable: kind != ""}}
}

// iptablesFiltering iptables -S INPUT 里有没有拦截：默认策略是 DROP，或有不限来源的 DROP / REJECT。
// fail2ban 之类只封某些地址的不算。
func iptablesFiltering(out string) bool {
	for _, l := range splitLines(out) {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == "-P" && f[1] == "INPUT" && f[2] == "DROP" {
			return true
		}
		if strings.HasPrefix(l, "-A INPUT ") && !strings.Contains(l, " -s ") &&
			(strings.HasSuffix(l, "-j DROP") || strings.Contains(l, "-j REJECT")) {
			return true
		}
	}
	return false
}

// nftFiltering nft list ruleset 里有没有默认丢弃的 input 链。
func nftFiltering(out string) bool {
	for _, l := range splitLines(out) {
		if strings.Contains(l, "hook input") && strings.Contains(l, "policy drop") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- firewalld

func (sc *fwScan) firewalld() *fwState {
	st := &fwState{Firewall: proto.Firewall{Kind: "firewalld", Active: true, Manage: true}, deflt: "blocked"}
	zone := ""
	if ifc := defaultIface(sc.proc); ifc != "" && reSafe.MatchString(ifc) {
		if out, err := sc.run("LC_ALL=C firewall-cmd --get-zone-of-interface="+ifc+" 2>/dev/null", fwTimeout); err == nil {
			zone = strings.TrimSpace(out)
		}
	}
	if zone == "" {
		out, _ := sc.run("LC_ALL=C firewall-cmd --get-default-zone 2>&1", fwTimeout)
		zone = strings.TrimSpace(out)
	}
	if !reSafe.MatchString(zone) {
		st.Manage, st.deflt, st.Why = false, "", "读不到 firewalld 的区域："+clipLine(zone)
		return st
	}
	st.Zone = zone
	out, err := sc.run("LC_ALL=C firewall-cmd --zone="+zone+" --list-all 2>&1", fwTimeout)
	if err != nil {
		st.Manage, st.deflt, st.Why = false, "", "读取 firewalld 的规则失败："+clipLine(strings.TrimSpace(out))
		return st
	}
	z := parseZone(out)
	var perm *fwZone // 永久配置：对比出只在运行时的规则
	if out, err := sc.run("LC_ALL=C firewall-cmd --permanent --zone="+zone+" --list-all 2>&1", fwTimeout); err == nil {
		perm = parseZone(out)
	}
	temp := func(list func(*fwZone) []string, v string) bool { return perm != nil && !contains(list(perm), v) }
	if z.target == "ACCEPT" {
		st.deflt, st.Manage = "open", false
		st.Why = "区域 " + zone + " 设成了放行全部（target: ACCEPT），不需要逐个放行端口"
	}

	// 富规则里不限来源的拦截先生效；其余都是放行，顺序无所谓
	var rich []fwRule
	for _, rr := range z.rich {
		for _, r := range sc.richRule(rr) {
			r.temp = temp(func(z *fwZone) []string { return z.rich }, rr)
			rich = append(rich, r)
		}
	}
	for _, r := range rich {
		if !r.allow && r.source == "" {
			st.rules = append(st.rules, r)
		}
	}
	for _, svc := range z.services {
		ports := sc.servicePorts(svc)
		label := "服务 " + svc
		if len(ports) > 0 {
			label += "（" + strings.Join(ports, " ") + "）"
		}
		st.Rules = append(st.Rules, label)
		for _, p := range ports {
			if lo, hi, pr, ok := parsePortProto(p, "-"); ok {
				st.rules = append(st.rules, fwRule{lo: lo, hi: hi, proto: pr, allow: true, kind: "service", spec: svc, label: label,
					temp: temp(func(z *fwZone) []string { return z.services }, svc)})
			}
		}
	}
	for _, p := range z.ports {
		lo, hi, pr, ok := parsePortProto(p, "-")
		if !ok {
			continue
		}
		r := fwRule{lo: lo, hi: hi, proto: pr, allow: true, kind: "port", spec: p, label: p, temp: temp(func(z *fwZone) []string { return z.ports }, p)}
		if lo != hi {
			r.kind, r.label = "range", "范围 "+p
		}
		st.Rules = append(st.Rules, r.label)
		st.rules = append(st.rules, r)
	}
	for _, r := range rich {
		if r.allow || r.source != "" {
			st.rules = append(st.rules, r)
		}
		if !contains(st.Rules, r.label) {
			st.Rules = append(st.Rules, r.label)
		}
	}
	return st
}

// fwZone firewall-cmd --list-all 的内容。
type fwZone struct {
	target                string
	services, ports, rich []string
}

func parseZone(s string) *fwZone {
	z := &fwZone{}
	inRich := false
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if inRich {
			if strings.HasPrefix(t, "rule ") {
				z.rich = append(z.rich, t)
				continue
			}
			inRich = false
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "target":
			z.target = v
		case "services":
			z.services = strings.Fields(v)
		case "ports":
			z.ports = strings.Fields(v)
		case "rich rules":
			inRich = true
		}
	}
	return z
}

var (
	reRichPort = regexp.MustCompile(`port port="?([0-9]+(?:-[0-9]+)?)"? protocol="?(tcp|udp)"?`)
	reRichSvc  = regexp.MustCompile(`service name="?([A-Za-z0-9._-]+)"?`)
	reRichSrc  = regexp.MustCompile(`source (not )?address="?([^" ]+)"?`)
	reSvcPort  = regexp.MustCompile(`<port\s+([^>]*)/?>`)
	reXMLAttr  = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// richRule 富规则里的端口（不认识的、只拦某些来源的不算）。
func (sc *fwScan) richRule(rr string) []fwRule {
	allow := strings.HasSuffix(rr, " accept") || strings.Contains(rr, " accept ")
	if !allow && !strings.Contains(rr, " reject") && !strings.Contains(rr, " drop") {
		return nil
	}
	src := ""
	if m := reRichSrc.FindStringSubmatch(rr); m != nil {
		if m[1] != "" { // source not address：太复杂，不算
			return nil
		}
		src = m[2]
	}
	if !allow && src != "" {
		return nil
	}
	var ports []string
	if m := reRichPort.FindStringSubmatch(rr); m != nil {
		ports = []string{m[1] + "/" + m[2]}
	} else if m := reRichSvc.FindStringSubmatch(rr); m != nil {
		ports = sc.servicePorts(m[1])
	}
	var out []fwRule
	for _, p := range ports {
		lo, hi, pr, ok := parsePortProto(p, "-")
		if !ok {
			continue
		}
		label := p
		switch {
		case !allow:
			label = "拦截 " + p + "（富规则）"
		case src != "":
			label = p + " 只允许 " + src
		default:
			label = p + "（富规则）"
		}
		out = append(out, fwRule{lo: lo, hi: hi, proto: pr, allow: allow, source: src, kind: "rich", spec: rr, label: label})
	}
	return out
}

// servicePorts firewalld 服务带的端口（22/tcp……）：先看 /etc/firewalld/services，再看 /usr/lib/firewalld/services。
func (sc *fwScan) servicePorts(svc string) []string {
	if !reSafe.MatchString(svc) {
		return nil
	}
	for _, d := range []string{"etc/firewalld/services", "usr/lib/firewalld/services"} {
		b, err := os.ReadFile(filepath.Join(sc.root, d, svc+".xml"))
		if err != nil {
			continue
		}
		var out []string
		for _, m := range reSvcPort.FindAllStringSubmatch(string(b), -1) {
			at := map[string]string{}
			for _, kv := range reXMLAttr.FindAllStringSubmatch(m[1], -1) {
				at[kv[1]] = kv[2]
			}
			if at["port"] != "" && (at["protocol"] == "tcp" || at["protocol"] == "udp") {
				out = append(out, at["port"]+"/"+at["protocol"])
			}
		}
		return out
	}
	// 没有定义文件（老版本放在别处）：问 firewall-cmd
	out, err := sc.run("LC_ALL=C firewall-cmd --info-service="+svc+" 2>/dev/null", fwTimeout)
	if err != nil {
		return nil
	}
	for _, l := range splitLines(out) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "ports:"); ok {
			return strings.Fields(v)
		}
	}
	return nil
}

// parsePortProto 8080/tcp、8000-8100/tcp（ufw 的范围用 :）→ 范围与协议；没写协议时 pr 为空。
func parsePortProto(s, rangeSep string) (lo, hi int, pr string, ok bool) {
	p, pr, _ := strings.Cut(s, "/")
	if pr != "" && pr != "tcp" && pr != "udp" {
		return 0, 0, "", false
	}
	a, b, isRange := strings.Cut(p, rangeSep)
	lo, err1 := strconv.Atoi(a)
	hi, err2 := lo, error(nil)
	if isRange {
		hi, err2 = strconv.Atoi(b)
	}
	if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, "", false
	}
	return lo, hi, pr, true
}

// defaultIface 默认路由走的网卡（/proc/net/route 里目标为 0 的那条）。
func defaultIface(proc string) string {
	b, err := os.ReadFile(filepath.Join(proc, "net", "route"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(l)
		if len(f) >= 2 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

// ---------------------------------------------------------------- ufw

// ufwLine ufw status 里的一条入站规则。
type ufwLine struct {
	to, app, verb, from string
}

var (
	reUFWRule  = regexp.MustCompile(`^(.*?)\s+(ALLOW|DENY|REJECT|LIMIT)(?: (IN|OUT|FWD))?\s+(.*)$`)
	reUFWPorts = regexp.MustCompile(`^[0-9][0-9,:]*(/(tcp|udp))?$`)
)

// parseUFW ufw status verbose 的结果；不是 ufw 的输出时为 nil。
func parseUFW(out string) (*fwState, []ufwLine) {
	st := &fwState{Firewall: proto.Firewall{Kind: "ufw"}, deflt: "blocked"}
	found, inRules := false, false
	var lines []ufwLine
	for _, l := range splitLines(out) {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "Status:"):
			found = true
			st.Active = strings.TrimSpace(strings.TrimPrefix(t, "Status:")) == "active"
		case strings.HasPrefix(t, "Default:"):
			// Default: deny (incoming), allow (outgoing), disabled (routed)
			for _, part := range strings.Split(strings.TrimPrefix(t, "Default:"), ",") {
				if f := strings.Fields(part); len(f) >= 2 && f[1] == "(incoming)" && f[0] == "allow" {
					st.deflt = "open"
				}
			}
		case strings.HasPrefix(t, "--"):
			inRules = true
		case inRules && t != "":
			m := reUFWRule.FindStringSubmatch(t)
			if m == nil || (m[3] != "" && m[3] != "IN") {
				continue
			}
			to := strings.TrimSuffix(strings.TrimSpace(m[1]), " (v6)")
			from := strings.TrimSuffix(strings.TrimSpace(m[4]), " (v6)")
			ln := ufwLine{to: to, verb: strings.ToLower(m[2]), from: from}
			// 80,443/tcp (Nginx Full)：应用的规则，verbose 时带展开的端口
			if i := strings.LastIndex(to, " ("); i > 0 && strings.HasSuffix(to, ")") {
				ln.to, ln.app = to[:i], to[i+2:len(to)-1]
			}
			if from == "Anywhere" {
				ln.from = ""
			}
			lines = append(lines, ln)
		}
	}
	if !found {
		return nil, nil
	}
	if st.deflt == "open" {
		st.Why = "ufw 设成了默认放行所有入站连接，不需要逐个放行端口"
	} else {
		st.Manage = st.Active
	}
	return st, lines
}

// ufwRules 把 ufw 的规则展开成端口（应用名要问 ufw app info）。
func (sc *fwScan) ufwRules(st *fwState, lines []ufwLine) {
	for _, ln := range lines {
		if strings.HasPrefix(ln.to, "Anywhere") { // 只按来源放行（某个地址的全部端口）：不是端口规则
			continue
		}
		allow := ln.verb == "allow" || ln.verb == "limit"
		specs := []string{ln.to}
		kind, spec := "port", ln.to
		switch {
		case ln.app != "":
			kind, spec = "app", ln.app
		case ln.to != "" && (ln.to[0] < '0' || ln.to[0] > '9'): // 只写了应用名
			kind, spec = "app", ln.to
			specs = sc.ufwApp(ln.to)
		case strings.ContainsAny(ln.to, " "): // 带目标地址、网卡：192.168.1.5 22/tcp、22/tcp on eth0
			kind, specs = "complex", nil
			for _, f := range strings.Fields(ln.to) {
				if reUFWPorts.MatchString(f) {
					specs = []string{f}
				}
			}
		}
		var ports []string
		for _, s := range specs {
			p, pr, _ := strings.Cut(s, "/")
			for _, one := range strings.Split(p, ",") {
				if pr != "" {
					one += "/" + pr
				}
				ports = append(ports, one)
			}
		}
		if kind == "port" && len(ports) > 1 {
			kind = "list"
		}
		label := ln.to
		if ln.app != "" || kind == "app" {
			label = "应用 " + spec
			if len(ports) > 0 {
				label += "（" + strings.Join(ports, " ") + "）"
			}
		}
		if ln.from != "" {
			label += " 只允许 " + ln.from
		}
		if !allow {
			label = "拦截 " + label
		}
		for _, p := range ports {
			lo, hi, pr, ok := parsePortProto(p, ":")
			if !ok {
				continue
			}
			k := kind
			if k == "port" && lo != hi {
				k = "range"
			}
			st.rules = append(st.rules, fwRule{lo: lo, hi: hi, proto: pr, allow: allow, source: ln.from, kind: k, spec: spec, verb: ln.verb, label: label})
		}
		if !contains(st.Rules, label) {
			st.Rules = append(st.Rules, label)
		}
	}
}

// ufwApp ufw 应用的端口（ufw app info 的 Ports）。
func (sc *fwScan) ufwApp(name string) []string {
	out, err := sc.run("LC_ALL=C ufw app info "+shellQuote(name)+" 2>/dev/null", fwTimeout)
	if err != nil {
		return nil
	}
	var ports []string
	in := false
	for _, l := range splitLines(out) {
		t := strings.TrimSpace(l)
		switch {
		case t == "Ports:" || t == "Port:":
			in = true
		case in && t != "":
			ports = append(ports, t)
		}
	}
	return ports
}

// ---------------------------------------------------------------- 放行 / 关闭

func (a *Agent) portset() int {
	port, pr, action := a.req.Port, a.req.PortProto, a.req.Action
	if pr == "" {
		pr = "tcp"
	}
	switch {
	case port < 1 || port > 65535:
		return a.result(proto.CodeBadInput, fmt.Sprintf("端口无效：%d", port), nil)
	case pr != "tcp" && pr != "udp":
		return a.result(proto.CodeBadInput, "协议只能是 tcp 或 udp："+pr, nil)
	case action != "open" && action != "close":
		return a.result(proto.CodeBadInput, "未知操作："+action, nil)
	}
	sc := a.fwScanner()
	rep, st := sc.report()
	what := fmt.Sprintf("%d/%s", port, pr)
	// Docker 映射的端口不经过防火墙：在 DOCKER-USER 里拦
	for i := range rep.Ports {
		if p := &rep.Ports[i]; p.Port == port && p.Proto == pr && p.Docker {
			return a.dockerPortSet(sc, rep, p, action)
		}
	}
	if !st.Manage {
		why := st.Why
		if why == "" {
			why = "Ferry 改不了这台服务器的防火墙"
		}
		if st.Enable {
			why += "：先打开防火墙（ferry port enable，或桌面版「端口」里的「打开防火墙」）"
		}
		return a.result(proto.CodeFailed, why, nil)
	}
	if action == "close" && pr == "tcp" && sc.protected(port) {
		return a.result(proto.CodeBadInput, fmt.Sprintf("%d 是 SSH 端口（Ferry 用它连服务器），关掉就连不上了：不能关闭", port), nil)
	}
	state, rule := st.decide(port, pr)
	var cmds []string
	label := map[string]string{"open": "放行", "close": "关闭"}[action]
	if action == "open" {
		switch {
		case state == "open":
			a.info(proto.EvFirewall, "", "%s 已经放行了", what)
			return a.portDone(sc)
		case rule != nil && !rule.allow:
			return a.result(proto.CodeFailed, fmt.Sprintf("有一条拦截 %s 的规则（%s）：Ferry 不改拦截规则，请在终端处理", what, rule.label), nil)
		}
		cmds = openCmds(st, port, pr)
	} else {
		switch {
		case state == "blocked" || state == "":
			a.info(proto.EvFirewall, "", "%s 本来就没有放行", what)
			return a.portDone(sc)
		case state == "partial":
			return a.result(proto.CodeFailed, fmt.Sprintf("%s 只对部分来源放行（%s）：Ferry 不改这种规则，请在终端处理", what, rule.label), nil)
		case rule == nil:
			return a.result(proto.CodeFailed, "找不到放行 "+what+" 的规则", nil)
		}
		switch rule.kind {
		case "port", "service", "app":
		case "range":
			return a.result(proto.CodeFailed, fmt.Sprintf("%d 在放行的端口范围里（%s）：Ferry 不拆分范围，请在终端处理", port, rule.label), nil)
		case "list":
			return a.result(proto.CodeFailed, fmt.Sprintf("%d 和别的端口写在同一条规则里（%s）：Ferry 不拆这种规则，请在终端处理", port, rule.label), nil)
		default:
			return a.result(proto.CodeFailed, fmt.Sprintf("放行 %s 的是一条复杂规则（%s）：Ferry 不改，请在终端处理", what, rule.label), nil)
		}
		// 服务、应用带着别的端口：一起关掉；其中有 SSH 端口时不许
		if rule.kind != "port" {
			var others []string
			for _, r := range st.rules {
				if r.kind != rule.kind || r.spec != rule.spec {
					continue
				}
				for p := r.lo; p <= r.hi; p++ {
					if (r.proto == "" || r.proto == "tcp") && sc.protected(p) {
						return a.result(proto.CodeBadInput, fmt.Sprintf("放行 %s 的规则（%s）也放行了 SSH 端口 %d，关掉就连不上了：不能关闭", what, rule.label, p), nil)
					}
				}
				if r.lo != port || (r.proto != "" && r.proto != pr) {
					others = append(others, portText(r))
				}
			}
			if len(others) > 0 {
				a.warn(proto.EvFirewall, "", "%s 是「%s」放行的：这条规则带的 %s 也会一起关闭", what, rule.label, strings.Join(others, "、"))
			}
		}
		if rule.kind == "port" && rule.proto == "" {
			a.warn(proto.EvFirewall, "", "放行 %s 的规则（%s）没写协议，tcp、udp 会一起关闭", what, rule.label)
		}
		cmds = closeCmds(st, rule)
	}
	a.info(proto.EvFirewall, "", "%s %s（%s）", label, what, fwWhere(st))
	for _, c := range cmds {
		out, err := a.Sys.Shell(c+" 2>&1", 2*time.Minute)
		if err != nil {
			msg := strings.TrimSpace(out)
			a.fail(proto.EvFail, "", "%s：%s", c, clipLine(msg))
			return a.result(proto.CodeFailed, label+"失败："+clipLine(msg), nil)
		}
	}
	a.Sys.Shell(fmt.Sprintf("logger -t ferry -- %s 2>/dev/null", shellQuote(fmt.Sprintf("%s 用防火墙%s %s", a.req.Actor, label, what))), 10*time.Second)
	rep, st2 := sc.report()
	if now, _ := st2.decide(port, pr); (action == "open") != (now == "open") {
		a.warn(proto.EvWarn, "", "改完再看，%s 现在是 %s：可能还有别的规则在起作用", what, fwStateText(now))
	} else {
		a.info(proto.EvFirewall, "", "已%s %s", label, what)
	}
	return a.result(proto.CodeOK, "", rep)
}

// portText 规则的端口：8080/tcp、8000-8100/tcp；没写协议时只写端口。
func portText(r fwRule) string {
	s := strconv.Itoa(r.lo)
	if r.hi != r.lo {
		s += "-" + strconv.Itoa(r.hi)
	}
	if r.proto != "" {
		s += "/" + r.proto
	}
	return s
}

func fwStateText(s string) string {
	if s == "" {
		return "（看不出来）"
	}
	return s
}

// portDone 不需要改：返回现在的情况。
func (a *Agent) portDone(sc *fwScan) int {
	rep, _ := sc.report()
	return a.result(proto.CodeOK, "", rep)
}

func fwWhere(st *fwState) string {
	if st.Kind == "firewalld" {
		return "firewalld 区域 " + st.Zone + "，运行时和永久配置"
	}
	return st.Kind
}

// openCmds 放行端口的命令（firewalld 运行时、永久配置各一条）。
func openCmds(st *fwState, port int, pr string) []string {
	p := fmt.Sprintf("%d/%s", port, pr)
	if st.Kind == "firewalld" {
		return []string{
			"firewall-cmd --zone=" + st.Zone + " --add-port=" + p,
			"firewall-cmd --permanent --zone=" + st.Zone + " --add-port=" + p,
		}
	}
	return []string{"ufw allow " + p}
}

// closeCmds 去掉放行它的规则。只在永久配置（或只在运行时）里有的那一边，firewall-cmd 只报个 NOT_ENABLED，不算失败。
func closeCmds(st *fwState, r *fwRule) []string {
	if st.Kind == "firewalld" {
		opt := "--remove-port=" + r.spec
		if r.kind == "service" {
			opt = "--remove-service=" + r.spec
		}
		return []string{
			"firewall-cmd --zone=" + st.Zone + " " + opt,
			"firewall-cmd --permanent --zone=" + st.Zone + " " + opt,
		}
	}
	verb := r.verb
	if verb != "limit" {
		verb = "allow"
	}
	return []string{"ufw --force delete " + verb + " " + shellQuote(r.spec)}
}
