package agent

import (
	"encoding/json"
	"errors"
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

// Docker 映射的端口不经过 firewalld / ufw：外部的连接先被 Docker 的 iptables 规则转给容器。
// 要拦外部访问，在 Docker 留给用户的 DOCKER-USER 规则链里加规则（转给容器之前先经过它）：
//
//	iptables -I DOCKER-USER -i eth0 -p tcp -m conntrack --ctorigdstport 3306 --ctdir ORIGINAL -m comment --comment ferry-guard -j DROP
//
//   - 只拦从默认路由那张网卡进来的：服务器本机、同一台服务器上的容器照常能连。
//   - 按转换前的目标端口（主机端口）匹配：到这里时目标已经换成容器的地址和端口了。
//   - Docker 不保存这条链里的规则，重启服务器就没了：拦的端口记在 <ferryDir>/docker-guard.json，
//     规则写成 <ferryDir>/docker-guard.sh，由 ferry-docker-guard.service 在 Docker 启动后（和重启时）重新加上。
//   - 开了 IPv6 的 Docker 有 ip6tables 的 DOCKER-USER，同样加一条。

const (
	guardComment = "ferry-guard"
	guardUnit    = "ferry-docker-guard.service"
)

// guardEntry 拦着的一个端口。
type guardEntry struct {
	Port      int       `json:"port"`
	Proto     string    `json:"proto"`
	Container string    `json:"container,omitempty"` // 关闭时映射它的容器（给人看）
	At        time.Time `json:"at"`
	By        string    `json:"by,omitempty"`
}

type guardState struct {
	Iface string       `json:"iface"`
	Ports []guardEntry `json:"ports"`
}

func guardFile(dir string) string   { return filepath.Join(dir, "docker-guard.json") }
func guardScript(dir string) string { return filepath.Join(dir, "docker-guard.sh") }

func loadGuard(dir string) *guardState {
	st := &guardState{}
	if b, err := os.ReadFile(guardFile(dir)); err == nil {
		json.Unmarshal(b, st)
	}
	return st
}

var (
	reGuardPort  = regexp.MustCompile(`--ctorigdstport (\d+)`)
	reGuardProto = regexp.MustCompile(` -p (tcp|udp)`)
)

// parseGuardRules iptables -S DOCKER-USER 里 Ferry 加的规则：8080/tcp → true。
func parseGuardRules(out string) map[string]bool {
	have := map[string]bool{}
	for _, l := range splitLines(out) {
		if !strings.Contains(l, "--comment "+guardComment) {
			continue
		}
		pm, rm := reGuardPort.FindStringSubmatch(l), reGuardProto.FindStringSubmatch(l)
		if pm != nil && rm != nil {
			have[pm[1]+"/"+rm[1]] = true
		}
	}
	return have
}

// dockerGuard 看能不能拦、哪些端口拦着，填进报告。没有 Docker 映射的端口、也没拦过的时候不看。
func (sc *fwScan) dockerGuard(rep *proto.PortReport, ports map[string]*proto.PortInfo, add func(int, string) *proto.PortInfo) {
	saved := loadGuard(sc.ferryDir)
	any := len(saved.Ports) > 0
	for _, p := range ports {
		any = any || p.Docker
	}
	if !any {
		return
	}
	g := &proto.DockerGuard{Iface: defaultIface(sc.proc)}
	rep.DockerGuard = g
	out, err := sc.run("if command -v iptables >/dev/null 2>&1; then iptables -S DOCKER-USER 2>&1; else echo 'no iptables'; exit 3; fi", fwTimeout)
	switch {
	case err != nil && strings.Contains(out, "no iptables"):
		g.Why = "服务器上没有 iptables 命令：拦不了 Docker 映射的端口"
		return
	case err != nil:
		g.Why = "Docker 没有用 iptables（没有 DOCKER-USER 规则链：可能设了 \"iptables\": false 或改用了 nftables）：Ferry 拦不了它映射的端口"
		return
	case g.Iface == "":
		g.Why = "找不到默认路由的网卡：不知道拦哪张网卡进来的连接"
		return
	}
	g.OK = true
	have := parseGuardRules(out)
	for _, e := range saved.Ports {
		k := strconv.Itoa(e.Port) + "/" + e.Proto
		p := ports[k]
		if p == nil { // 容器已经没了，规则还在：列出来，可以放行（去掉规则）
			p = add(e.Port, e.Proto)
			p.Docker, p.Container = true, e.Container
		}
		if !have[k] {
			rep.Notes = append(rep.Notes, fmt.Sprintf("关闭过的 Docker 端口 %s 现在没有拦着（规则被清掉了，可能重新加载过防火墙）：再点一次「关闭」", k))
		}
	}
	for k := range have {
		if p := ports[k]; p != nil {
			p.Guard = true
		} else if port, pr, ok := strings.Cut(k, "/"); ok {
			n, _ := strconv.Atoi(port)
			p := add(n, pr)
			p.Docker, p.Guard = true, true
		}
	}
}

// guardScriptText 加规则的脚本：先去掉 Ferry 加过的，再按列表加上。
func guardScriptText(iface string, entries []guardEntry) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n" + ManagedMarker + "\n")
	b.WriteString("# 拦住从外部访问这些 Docker 映射端口的连接（在 Ferry 的「端口」里关闭的）。\n")
	b.WriteString("# 由 " + guardUnit + " 在 Docker 启动后执行；改动请在 Ferry 里做，这个文件会被重新生成。\n")
	b.WriteString("for t in iptables ip6tables; do\n")
	b.WriteString("  command -v $t >/dev/null 2>&1 || continue\n")
	b.WriteString("  $t -S DOCKER-USER >/dev/null 2>&1 || continue\n")
	b.WriteString("  $t -S DOCKER-USER | grep -- '--comment " + guardComment + "' | sed 's/^-A /-D /' | while read -r r; do eval \"$t $r\"; done\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "  $t -I DOCKER-USER -i %s -p %s -m conntrack --ctorigdstport %d --ctdir ORIGINAL -m comment --comment %s -j DROP || exit 1\n",
			iface, e.Proto, e.Port, guardComment)
	}
	b.WriteString("done\n")
	return b.String()
}

func guardUnitText(script string) string {
	return ManagedMarker + `
[Unit]
Description=Ferry: block outside access to Docker ports closed in Ferry
After=docker.service
PartOf=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh ` + script + `

[Install]
WantedBy=docker.service
`
}

// setGuard 拦（block）或不再拦一个 Docker 映射的端口：改列表，重新生成脚本并执行；没有要拦的端口了就去掉服务。
func (a *Agent) setGuard(iface string, e guardEntry, block bool) error {
	if !reSafe.MatchString(iface) {
		return fmt.Errorf("网卡名无效：%q", iface)
	}
	dir := a.ferryDir()
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("ferry 目录无效：%q", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	st := loadGuard(dir)
	st.Iface = iface
	var keep []guardEntry
	for _, x := range st.Ports {
		if x.Port != e.Port || x.Proto != e.Proto {
			keep = append(keep, x)
		}
	}
	if block {
		keep = append(keep, e)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].Port < keep[j].Port })
	st.Ports = keep

	script := guardScript(dir)
	if err := writeFileAtomic(script, []byte(guardScriptText(iface, keep)), 0o700); err != nil {
		return err
	}
	if out, err := a.Sys.Shell("/bin/sh "+shellQuote(script)+" 2>&1", time.Minute); err != nil {
		return errors.New("加 iptables 规则失败：" + clipLine(strings.TrimSpace(out)))
	}
	unit := a.unitPath(guardUnit)
	if len(keep) == 0 {
		a.Sys.Systemctl("disable", guardUnit)
		os.Remove(unit)
		a.Sys.Systemctl("daemon-reload")
		os.Remove(script)
		os.Remove(guardFile(dir))
		return nil
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := writeFileAtomic(guardFile(dir), append(b, '\n'), 0o600); err != nil {
		return err
	}
	if !sameFile(unit, []byte(guardUnitText(script))) {
		if err := writeFileAtomic(unit, []byte(guardUnitText(script)), 0o644); err != nil {
			return err
		}
		a.Sys.Systemctl("daemon-reload")
	}
	// 开机（Docker 启动）时加回规则；enable 失败不影响这次已经加上的规则
	if err := a.Sys.Systemctl("enable", guardUnit); err != nil {
		a.warn(proto.EvWarn, "", "设置开机自动加回规则失败（%v）：重启服务器后要在 Ferry 里再关闭一次", err)
	}
	return nil
}

// onlyLocal 只监听本机（127.0.0.1、[::1]）。
func onlyLocal(listen []string) bool {
	for _, h := range listen {
		if !strings.HasPrefix(h, "127.") && h != "[::1]" {
			return false
		}
	}
	return len(listen) > 0
}

// dockerPortSet 关闭 / 放行 Docker 映射的端口：在 DOCKER-USER 里加 / 去掉规则（不经过防火墙）。
func (a *Agent) dockerPortSet(sc *fwScan, rep *proto.PortReport, p *proto.PortInfo, action string) int {
	what := fmt.Sprintf("%d/%s", p.Port, p.Proto)
	g := rep.DockerGuard
	if g == nil || !g.OK {
		why := "Ferry 拦不了这个 Docker 映射的端口"
		if g != nil && g.Why != "" {
			why = g.Why
		}
		if action == "open" && !p.Guard {
			a.info(proto.EvFirewall, "", "%s 是 Docker 映射的端口，本来就对外开放", what)
			return a.portDone(sc)
		}
		return a.result(proto.CodeFailed, why, nil)
	}
	saved := false
	for _, e := range loadGuard(a.ferryDir()).Ports {
		saved = saved || (e.Port == p.Port && e.Proto == p.Proto)
	}
	ctr := p.Container
	if ctr == "" {
		ctr = "（容器已经不在了）"
	}
	if action == "close" {
		switch {
		case p.Guard && saved:
			a.info(proto.EvFirewall, "", "%s 已经拦着了", what)
			return a.portDone(sc)
		case onlyLocal(p.Listen):
			a.info(proto.EvFirewall, "", "%s 只映射到本机（%s），外部本来就连不上", what, strings.Join(p.Listen, " "))
			return a.portDone(sc)
		}
		a.info(proto.EvFirewall, "", "关闭 %s（容器 %s）：在 Docker 的 DOCKER-USER 规则链里拦住从 %s 进来的连接，服务器本机和其他容器照常能连", what, ctr, g.Iface)
	} else {
		if !p.Guard && !saved {
			a.info(proto.EvFirewall, "", "%s 是 Docker 映射的端口，本来就对外开放", what)
			return a.portDone(sc)
		}
		a.info(proto.EvFirewall, "", "放行 %s（容器 %s）：去掉 DOCKER-USER 里拦它的规则", what, ctr)
	}
	e := guardEntry{Port: p.Port, Proto: p.Proto, Container: p.Container, At: a.Sys.Now(), By: a.req.Actor}
	if err := a.setGuard(g.Iface, e, action == "close"); err != nil {
		a.fail(proto.EvFail, "", "%v", err)
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	label := map[string]string{"open": "放行", "close": "关闭"}[action]
	a.Sys.Shell(fmt.Sprintf("logger -t ferry -- %s 2>/dev/null", shellQuote(fmt.Sprintf("%s 在 DOCKER-USER 里%s Docker 端口 %s", a.req.Actor, label, what))), 10*time.Second)
	rep2, _ := sc.report()
	for _, q := range rep2.Ports {
		if q.Port == p.Port && q.Proto == p.Proto && q.Guard != (action == "close") {
			a.warn(proto.EvWarn, "", "改完再看，%s 的规则不对：请在终端用 iptables -S DOCKER-USER 看看", what)
			return a.result(proto.CodeOK, "", rep2)
		}
	}
	if action == "close" {
		a.info(proto.EvFirewall, "", "已关闭 %s；开机、Docker 重启后由 %s 自动加回规则", what, guardUnit)
	} else {
		a.info(proto.EvFirewall, "", "已放行 %s", what)
	}
	return a.result(proto.CodeOK, "", rep2)
}
