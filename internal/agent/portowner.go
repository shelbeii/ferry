package agent

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

// portOwner 谁在监听这个 TCP 端口：进程号，以及它所在的 systemd 单元或 Docker 容器（测试时替换）。没找到时 pid 为 0。
var portOwner = func(port int) (pid int, unit, container string) {
	want := map[string]bool{}
	for _, l := range readListen("/proc") {
		if l.port == port {
			want[l.inode] = true
		}
	}
	for _, p := range socketPIDs("/proc", want) {
		if pid == 0 || p < pid {
			pid = p
		}
	}
	if pid == 0 {
		return 0, "", ""
	}
	cg, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	unit, container = cgroupOwner(string(cg))
	return pid, unit, container
}

// portBusy 端口被别人占着时，说明是谁、怎么处理；没人监听，或监听的是 mine 里的单元时返回空。
func (a *Agent) portBusy(port int, mine ...string) string {
	if port <= 0 || !listeningPorts()[port] {
		return ""
	}
	pid, unit, ctr := portOwner(port)
	for _, u := range mine {
		if unit != "" && unit == u {
			return ""
		}
	}
	switch {
	case ctr != "":
		return fmt.Sprintf("端口 %d 已被 Docker 容器 %s 占用", port, shortID(ctr))
	case unit == "docker.service":
		return fmt.Sprintf("端口 %d 已被 Docker 容器映射占用", port)
	case unit != "":
		if app, ok := a.managedUnits()[unit]; ok {
			return fmt.Sprintf("端口 %d 已被 Ferry 部署的应用 %s（项目 %s，服务 %s）占用：如果它已经不用了（比如在配置里改了名），先在服务器上停掉它：systemctl disable --now %s",
				port, app[1], app[0], unit, unit)
		}
		for _, in := range a.instances() {
			if in.Unit == unit {
				return fmt.Sprintf("端口 %d 已被 Ferry 装的软件 %s（服务 %s）占用", port, in.Name, unit)
			}
		}
		return fmt.Sprintf("端口 %d 已被服务 %s 占用", port, unit)
	case pid > 0:
		comm, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
		return fmt.Sprintf("端口 %d 已被进程 %d（%s）占用", port, pid, strings.TrimSpace(string(comm)))
	}
	return fmt.Sprintf("端口 %d 已被别的程序占用", port)
}

// sitePorts 前端站点监听的端口（static.port）。
func sitePorts(s *proto.AppSpec) []int {
	if s.Type != proto.TypeStatic || s.SitePort == "" {
		return nil
	}
	if _, p := splitSiteListen(s.SitePort); p > 0 {
		return []int{p}
	}
	return nil
}

var (
	reInUse     = regexp.MustCompile(`(?i)address (already )?in use|already in use`)
	reInUsePort = regexp.MustCompile(`(?i):(\d{2,5})\b|port (\d{2,5})\b`)
)

// inUseHints 启动失败的日志里有「端口被占用」时，找出占着端口的是谁。
func (a *Agent) inUseHints(s *proto.AppSpec, lines []string, unit string) []string {
	ports := map[int]bool{}
	for _, l := range lines {
		if !reInUse.MatchString(l) {
			continue
		}
		for _, m := range reInUsePort.FindAllStringSubmatch(l, -1) {
			if p, _ := strconv.Atoi(m[1] + m[2]); p > 0 && p < 65536 {
				ports[p] = true
			}
		}
		for _, p := range sitePorts(s) {
			ports[p] = true
		}
	}
	var list []int
	for p := range ports {
		list = append(list, p)
	}
	sort.Ints(list)
	var out []string
	for _, p := range list {
		if msg := a.portBusy(p, unit); msg != "" {
			out = append(out, msg)
		}
	}
	return out
}

// portCheck 部署前检查：各应用要用的端口（前端站点的 static.port、服务健康检查的端口）是不是被别的程序占着。
// 第一次部署、服务器上原来就有程序在用这个端口时最常见；提前告诉用户是谁占的，不用等到切换那一步才失败。
// 占着前端站点端口的是 nginx / OpenResty 时，一起给出这个端口上已有的站点（可以改成只换它网站目录里的文件）。
func (a *Agent) portCheck() int {
	out := []proto.PortConflict{}
	var sites []proto.NginxSite
	discovered := false
	for i := range a.req.Apps {
		s := &a.req.Apps[i]
		if s.ZD != nil || s.SiteDir != "" {
			continue // 不停机发布的两个槽位轮流用端口；只换网站目录的不占端口
		}
		mine := append([]string{s.Unit}, s.Conflict...)
		if s.Type == proto.TypeStatic {
			if s.NginxInstance != "" {
				if in, err := a.loadHostingInstance(s.NginxInstance); err == nil {
					mine = append(mine, in.Unit)
				}
			}
			mine = append(mine, a.standaloneUnits(s)...)
		}
		ports := sitePorts(s)
		if s.Type != proto.TypeStatic {
			ports = healthPorts(s.Health)
		}
		for _, p := range ports {
			msg := a.portBusy(p, mine...)
			if msg == "" {
				continue
			}
			c := proto.PortConflict{App: s.Name, Port: p, Owner: msg}
			if s.Type == proto.TypeStatic {
				if !discovered {
					sites, _ = DiscoverNginxSites(a.Sys.Shell)
					discovered = true
				}
				for _, site := range sites {
					for _, sp := range site.Ports {
						if sp == p {
							c.Sites = append(c.Sites, site)
							break
						}
					}
				}
				if len(c.Sites) > 0 {
					c.Owner = fmt.Sprintf("端口 %d 已被 %s占用", p, c.Sites[0].Server)
				}
			}
			out = append(out, c)
		}
	}
	return a.result(proto.CodeOK, "", out)
}

// healthPorts 健康检查访问本机的端口（服务自己监听的）。
func healthPorts(h proto.Health) []int {
	var out []int
	add := func(host, port string) {
		switch strings.Trim(host, "[]") {
		case "", "127.0.0.1", "localhost", "0.0.0.0", "::1", "::":
		default:
			return
		}
		if p, err := strconv.Atoi(port); err == nil && p > 0 && p < 65536 {
			for _, x := range out {
				if x == p {
					return
				}
			}
			out = append(out, p)
		}
	}
	if h.TCP != "" {
		if i := strings.LastIndexByte(h.TCP, ':'); i >= 0 {
			add(h.TCP[:i], h.TCP[i+1:])
		}
	}
	if h.HTTP != "" {
		if u, err := url.Parse(h.HTTP); err == nil {
			port := u.Port()
			if port == "" {
				port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
			}
			add(u.Hostname(), port)
		}
	}
	return out
}
