package agent

// 服务器上已有的 nginx / OpenResty（不是 Ferry 为应用单独运行的）：服务器上直接运行的、Docker 容器里的（1Panel、宝塔、手动启动的都算）。
// 按正在运行的主进程找：读它实际在用的配置（nginx -T），列出里面的站点（端口、域名、网站目录）；容器里的网站目录按挂载换成服务器上的路径。
// 选了这样的站点部署时，Ferry 只把每一版的文件换进网站目录（见 sitedir.go），不改 nginx 的配置。
// 只读：不改任何东西。引擎在新建项目、添加应用时经 SSH 调用，agent 在切换运行环境、部署前检查端口时调用。

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type ngxMaster struct {
	pid       int
	argv      []string // master process 后面的命令行
	unit, ctr string
}

// ngxNode nginx 配置里的一条指令；块指令有 children。
type ngxNode struct {
	args     []string
	block    bool
	children []*ngxNode
}

const ngxDumpMark = "@@ferry-nginx-T@@"

// DiscoverNginxSites 列出服务器上正在运行的 nginx / OpenResty 里的站点。notes 是读不出来的原因（给人看）。
func DiscoverNginxSites(run func(string, time.Duration) (string, error)) (sites []proto.NginxSite, notes []string) {
	// 先用一个 grep 找出 nginx 主进程，只对找到的几个再细看：每个进程都 fork 一次 tr 的话，
	// 上万个进程（没回收的僵尸进程）的服务器上要十几秒，还会超时。xargs 分批，进程再多也不会超出命令行长度
	out, _ := run(`ls -f /proc 2>/dev/null | grep -E '^[0-9]+$' | sed 's|.*|/proc/&/cmdline|' |
xargs grep -s -a -l '^nginx: master process' 2>/dev/null | while read -r f; do
p=${f%/cmdline}
c=$(tr '\000' ' ' < "$f" 2>/dev/null) || continue
case "$c" in "nginx: master process"*) ;; *) continue ;; esac
printf 'M\t%s\t%s\t%s\n' "${p#/proc/}" "$c" "$(tr '\n' '|' < "$p/cgroup" 2>/dev/null)"
done
exit 0`, 15*time.Second)
	for _, m := range parseNginxMasters(out) {
		found, note := nginxMasterSites(run, m)
		if note != "" {
			notes = append(notes, note)
		}
		sites = append(sites, found...)
	}
	// 网站目录在服务器上有没有、属主是谁
	var dirs []string
	for _, s := range sites {
		if s.Disabled == "" && s.Dir != "" {
			dirs = append(dirs, shellQuote(s.Dir))
		}
	}
	if len(dirs) > 0 {
		st, _ := run(`for d in `+strings.Join(dirs, " ")+`; do if [ -L "$d" ]; then printf 'L\t%s\n' "$d"; elif [ -d "$d" ]; then printf 'D\t%s\t%s\n' "$d" "$(stat -c '%U:%G' "$d" 2>/dev/null)"; else printf 'N\t%s\n' "$d"; fi; done; exit 0`, 10*time.Second)
		state := map[string][2]string{}
		for _, line := range splitLines(st) {
			f := strings.Split(line, "\t")
			if len(f) >= 2 {
				owner := ""
				if len(f) >= 3 {
					owner = f[2]
				}
				state[f[1]] = [2]string{f[0], owner}
			}
		}
		for i := range sites {
			s := &sites[i]
			if s.Disabled != "" || s.Dir == "" {
				continue
			}
			switch st := state[s.Dir]; st[0] {
			case "D":
				s.Owner = st[1]
			case "L":
				s.Disabled = "网站目录在服务器上是符号链接，请选它指向的目录所在的站点，或改用「只放文件」方式"
			default:
				s.Disabled = "服务器上没有这个网站目录"
			}
		}
	}
	sort.SliceStable(sites, func(i, j int) bool {
		a, b := sites[i], sites[j]
		if (a.Disabled == "") != (b.Disabled == "") {
			return a.Disabled == ""
		}
		pa, pb := 0, 0
		if len(a.Ports) > 0 {
			pa = a.Ports[0]
		}
		if len(b.Ports) > 0 {
			pb = b.Ports[0]
		}
		if pa != pb {
			return pa < pb
		}
		return a.Dir < b.Dir
	})
	return sites, notes
}

func parseNginxMasters(out string) []ngxMaster {
	var masters []ngxMaster
	for _, line := range splitLines(out) {
		f := strings.SplitN(line, "\t", 4)
		if len(f) == 4 && f[0] == "M" {
			pid, err := strconv.Atoi(f[1])
			if err != nil || pid <= 0 {
				continue
			}
			argv := strings.Fields(strings.TrimPrefix(f[2], "nginx: master process"))
			if len(argv) == 0 {
				continue
			}
			m := ngxMaster{pid: pid, argv: argv}
			m.unit, m.ctr = cgroupOwner(strings.ReplaceAll(f[3], "|", "\n"))
			if strings.Contains(nginxArg(argv, "-c"), "/.ferry/apps/") {
				continue // Ferry 为应用单独运行的 nginx
			}
			masters = append(masters, m)
		}
	}
	return masters
}

// nginxArg 命令行里某个选项（-c、-p）的值。
func nginxArg(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

type ngxContainer struct {
	Name   string
	Net    string
	Mounts []struct{ Type, Source, Destination string }
	Ports  map[string][]struct{ HostIp, HostPort string }
}

// nginxMasterSites 一个主进程里的站点。
func nginxMasterSites(run func(string, time.Duration) (string, error), m ngxMaster) ([]proto.NginxSite, string) {
	bin := m.argv[0]
	var opts []string
	for _, flag := range []string{"-c", "-p"} {
		if v := nginxArg(m.argv, flag); v != "" {
			opts = append(opts, flag, shellQuote(v))
		}
	}
	where := fmt.Sprintf("（进程 %d）", m.pid)
	var ctr *ngxContainer
	var exec string
	if m.ctr != "" {
		info, err := run(`docker inspect --format '{"name":{{json .Name}},"net":{{json .HostConfig.NetworkMode}},"mounts":{{json .Mounts}},"ports":{{json .NetworkSettings.Ports}}}' `+shellQuote(m.ctr), 10*time.Second)
		var c ngxContainer
		if err != nil || json.Unmarshal([]byte(strings.TrimSpace(info)), &c) != nil {
			return nil, fmt.Sprintf("容器 %s 里有 nginx，但读不到容器的信息（只认得 Docker 的容器）", shortID(m.ctr))
		}
		c.Name = strings.TrimPrefix(c.Name, "/")
		ctr = &c
		where = "（容器 " + c.Name + "）"
		exec = "docker exec " + shellQuote(m.ctr) + " "
	} else {
		if m.unit != "" {
			where = "（" + m.unit + "）"
		}
		if !path.IsAbs(bin) {
			bin = fmt.Sprintf("/proc/%d/exe", m.pid)
		}
	}
	cmd := exec + shellQuote(bin) + " -V 2>&1; echo " + ngxDumpMark + "; " + exec + shellQuote(bin) + " -T " + strings.Join(opts, " ") + " 2>&1; exit 0"
	out, err := run(cmd, 20*time.Second)
	version, dump, ok := strings.Cut(strings.ReplaceAll(out, "\r\n", "\n"), ngxDumpMark+"\n")
	kind := "Nginx"
	if strings.Contains(strings.ToLower(bin+" "+version), "openresty") {
		kind = "OpenResty"
	}
	server := kind + where
	if err != nil || !ok || !strings.Contains(dump, "# configuration file ") {
		last := ""
		for _, l := range lastLines(dump, 3) {
			if strings.Contains(l, "[emerg]") || strings.Contains(l, "[alert]") || last == "" {
				last = l
			}
		}
		if last == "" && err != nil {
			last = err.Error()
		}
		return nil, server + " 的配置读不出来：" + strings.TrimSpace(last)
	}
	prefix := nginxArg(m.argv, "-p")
	if prefix == "" {
		prefix = nginxBuildArg(version, "prefix", "/usr/local/nginx")
	}
	var sites []proto.NginxSite
	for _, f := range nginxDumpFiles(dump) {
		if strings.HasPrefix(path.Base(f.name), "ferry-") {
			continue // Ferry 往共用的 nginx 里加的站点
		}
		for _, srv := range nginxServers(parseNginxConf(f.text), false) {
			site, ok := nginxSiteOf(srv, prefix)
			if !ok {
				continue
			}
			site.Server, site.PID, site.Unit, site.Conf = server, m.pid, m.unit, f.name
			if ctr != nil {
				site.Container, site.Unit = ctr.Name, ""
				site.Dir = containerHostPath(ctr, site.Root)
				if site.Dir == "" && site.Disabled == "" {
					site.Disabled = "网站目录只在容器里（没有挂载到服务器上），Ferry 放不进去"
				}
				if ctr.Net != "host" {
					site.Ports = containerHostPorts(ctr, site.Ports)
				}
			} else {
				site.Dir = site.Root
			}
			if site.Disabled == "" && site.Dir != "" {
				if err := proto.CheckSiteDir(site.Dir); err != nil {
					site.Disabled = err.Error()
				}
			}
			sites = append(sites, site)
		}
	}
	// 同一个端口上有几个站点：靠域名区分，健康检查访问 127.0.0.1 不一定到这个站点
	count := map[int]int{}
	for _, s := range sites {
		for _, p := range s.Ports {
			count[p]++
		}
	}
	for i := range sites {
		for _, p := range sites[i].Ports {
			if count[p] > 1 {
				sites[i].Shared = true
			}
		}
	}
	return sites, ""
}

type ngxFile struct{ name, text string }

// nginxDumpFiles nginx -T 的输出按文件分开。
func nginxDumpFiles(dump string) []ngxFile {
	var files []ngxFile
	const marker = "# configuration file "
	for _, part := range strings.Split("\n"+dump, "\n"+marker)[1:] {
		head, body, _ := strings.Cut(part, "\n")
		files = append(files, ngxFile{name: strings.TrimSuffix(strings.TrimSpace(head), ":"), text: body})
	}
	return files
}

// parseNginxConf 配置文字 → 指令树。
func parseNginxConf(text string) []*ngxNode {
	root := &ngxNode{block: true}
	stack := []*ngxNode{root}
	var stmt []string
	for _, tok := range nginxTokens(text) {
		cur := stack[len(stack)-1]
		switch tok {
		case ";":
			if len(stmt) > 0 {
				cur.children = append(cur.children, &ngxNode{args: stmt})
			}
			stmt = nil
		case "{":
			n := &ngxNode{args: stmt, block: true}
			cur.children = append(cur.children, n)
			stack = append(stack, n)
			stmt = nil
		case "}":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			stmt = nil
		default:
			stmt = append(stmt, tok)
		}
	}
	return root.children
}

// nginxServers http 里的 server 块（stream、mail 里的不要）。
func nginxServers(nodes []*ngxNode, inServer bool) []*ngxNode {
	var out []*ngxNode
	for _, n := range nodes {
		if !n.block || len(n.args) == 0 {
			continue
		}
		switch n.args[0] {
		case "stream", "mail", "location", "if", "upstream", "map", "types", "geo", "split_clients", "limit_except":
			continue
		case "server":
			if !inServer {
				out = append(out, n)
			}
			continue
		}
		out = append(out, nginxServers(n.children, inServer)...)
	}
	return out
}

func ngxDirective(nodes []*ngxNode, name string) *ngxNode {
	for _, n := range nodes {
		if !n.block && len(n.args) > 0 && n.args[0] == name {
			return n
		}
	}
	return nil
}

// nginxSiteOf 一个 server 块 → 站点。没有网站目录的（只做反向代理的）不算。
func nginxSiteOf(srv *ngxNode, prefix string) (proto.NginxSite, bool) {
	var s proto.NginxSite
	root := ""
	if r := ngxDirective(srv.children, "root"); r != nil && len(r.args) >= 2 {
		root = r.args[1]
	}
	if root == "" {
		for _, n := range srv.children {
			if n.block && len(n.args) >= 2 && n.args[0] == "location" && n.args[len(n.args)-1] == "/" && (len(n.args) == 2 || n.args[1] == "^~" || n.args[1] == "=") {
				if r := ngxDirective(n.children, "root"); r != nil && len(r.args) >= 2 {
					root = r.args[1]
				}
			}
		}
	}
	if root == "" {
		return s, false
	}
	ports := map[int]bool{}
	listens := 0
	for _, n := range srv.children {
		if n.block || len(n.args) < 2 || n.args[0] != "listen" {
			continue
		}
		listens++
		addr := n.args[1]
		if strings.HasPrefix(addr, "unix:") {
			continue
		}
		p := 80
		if v, err := strconv.Atoi(addr); err == nil {
			p = v
		} else if i := strings.LastIndexByte(addr, ':'); i >= 0 && !strings.HasSuffix(addr, "]") {
			if v, err := strconv.Atoi(addr[i+1:]); err == nil {
				p = v
			}
		}
		ports[p] = true
		for _, o := range n.args[2:] {
			if o == "ssl" {
				s.SSL = true
			}
		}
	}
	if listens == 0 {
		ports[80] = true
	}
	for p := range ports {
		s.Ports = append(s.Ports, p)
	}
	sort.Ints(s.Ports)
	if n := ngxDirective(srv.children, "server_name"); n != nil {
		for _, name := range n.args[1:] {
			if name != "_" && name != "" && name != `""` {
				s.Names = append(s.Names, name)
			}
		}
	}
	if strings.Contains(root, "$") {
		s.Root, s.Disabled = root, "网站目录里用了变量，Ferry 认不出是哪个目录"
		return s, true
	}
	if !path.IsAbs(root) {
		root = path.Join(prefix, root)
	}
	s.Root = path.Clean(root)
	return s, true
}

// containerHostPath 容器里的路径在服务器上是哪里（按挂载）；没挂载出来的返回空。
func containerHostPath(c *ngxContainer, p string) string {
	best, out := -1, ""
	for _, m := range c.Mounts {
		if (m.Type != "bind" && m.Type != "volume") || m.Source == "" || !path.IsAbs(m.Destination) {
			continue
		}
		dest := path.Clean(m.Destination)
		if p != dest && !strings.HasPrefix(p, strings.TrimSuffix(dest, "/")+"/") {
			continue
		}
		if len(dest) > best {
			best, out = len(dest), path.Join(m.Source, strings.TrimPrefix(p, dest))
		}
	}
	return out
}

// containerHostPorts 容器里监听的端口映射到服务器上的端口（没映射的不算）。
func containerHostPorts(c *ngxContainer, ports []int) []int {
	var out []int
	seen := map[int]bool{}
	for _, p := range ports {
		for _, b := range c.Ports[strconv.Itoa(p)+"/tcp"] {
			if v, err := strconv.Atoi(b.HostPort); err == nil && v > 0 && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	sort.Ints(out)
	return out
}

// nginxSiteDesc 给人看的一行：端口、域名、网站目录。
func nginxSiteDesc(s proto.NginxSite) string {
	var parts []string
	if len(s.Ports) > 0 {
		var ps []string
		for _, p := range s.Ports {
			ps = append(ps, strconv.Itoa(p))
		}
		parts = append(parts, "端口 "+strings.Join(ps, "、"))
	}
	if len(s.Names) > 0 {
		parts = append(parts, "域名 "+strings.Join(s.Names, " "))
	}
	return strings.Join(parts, " · ")
}
