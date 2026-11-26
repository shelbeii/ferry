package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/ui"
)

var portUsage = errors.New(`用法：
  ferry port <目标>                           服务器上的端口（谁在用、监听哪些地址）和防火墙放行了哪些
  ferry port open|close <目标> <端口>[/udp]    用服务器的防火墙（firewalld / ufw）放行 / 关闭端口（运行时和永久配置一起改）；
                                             Docker 映射的端口在 Docker 的 DOCKER-USER 规则链里拦外部访问（开机自动加回）
  ferry port check <目标> <端口>… [--timeout 3]  从本机连服务器的这些端口，看能不能连上（不改动服务器）
  ferry port enable <目标> [--allow 8080,443/tcp] [--restart-docker]
                                             打开装了没开的防火墙：先放行 SSH 端口和 --allow 的端口；
                                             打开后用新的连接试 SSH，连不上时 3 分钟后自动关掉、恢复原样
  ferry port disable <目标> [--restart-docker]  关闭正在运行的防火墙（开机也不再启动）；放行规则留着，再打开时照旧
  ferry port install <目标> <类型> [--enable] [--allow 80,443/tcp] [--restart-docker] [--network auto|local|server] [--allow-changes]
                                             安装防火墙（类型：firewalld | ufw | nftables | iptables）；
                                             默认仅安装不启用；加上 --enable 时安装后按防锁死流程立即打开（仅 firewalld / ufw）
  ferry port uninstall <目标> <类型> [--purge] [--restart-docker]
                                             卸载防火墙（必须实时身份认证后方可卸载）；--purge 同时清除配置文件`)

// portCmd ferry port <目标> | open | close | check | enable | disable | install | uninstall | uninstall-challenge | uninstall-auth。
func portCmd(c *cli, cio *cliIO, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	ok := func(data any) int { return ok0(js, data) }
	if len(c.pos) == 0 {
		return fail(portUsage)
	}
	switch sub := c.pos[0]; sub {
	case "enable":
		if len(c.pos) < 2 {
			return fail(portUsage)
		}
		allow, err := parseAllowFlag(c.flags["allow"])
		if err != nil {
			return fail(err)
		}
		rep, err := e.FirewallEnable(c.pos[1], allow, c.has("restart-docker"))
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(rep)
		}
		con.OK("防火墙已打开（%s）", rep.Firewall.Kind)
		return 0

	case "disable":
		if len(c.pos) < 2 {
			return fail(portUsage)
		}
		rep, err := e.FirewallDisable(c.pos[1], c.has("restart-docker"))
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(rep)
		}
		con.OK("防火墙已关闭：所有端口都不拦")
		return 0

	case "install":
		if len(c.pos) < 3 {
			return fail(portUsage)
		}
		allow, err := parseAllowFlag(c.flags["allow"])
		if err != nil {
			return fail(err)
		}
		rep, err := e.FirewallInstall(c.pos[1], engine.FirewallInstallOptions{
			Kind:          c.pos[2],
			Enable:        c.has("enable"),
			Allow:         allow,
			RestartDocker: c.has("restart-docker"),
			Network:       c.flags["network"],
			AllowChanges:  c.has("allow-changes"),
		})
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(rep)
		}
		if c.has("enable") {
			con.OK("防火墙已安装并打开（%s）", c.pos[2])
		} else {
			con.OK("防火墙已安装（%s，尚未启用）", c.pos[2])
		}
		return 0

	case "uninstall-challenge":
		if len(c.pos) < 3 {
			return fail(portUsage)
		}
		ch, err := e.FirewallUninstallChallenge(c.pos[1], c.pos[2])
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(ch)
		}
		con.Info("一次性认证码（%s）：%s（3 分钟内有效）", ch.Kind, ch.Code)
		return 0

	case "uninstall-auth":
		if len(c.pos) < 3 {
			return fail(portUsage)
		}
		secretVal := cio.getenv("FERRY_FW_AUTH_SECRET")
		if secretVal == "" && js == nil {
			auth, err := e.FirewallUninstallAuthInfo(c.pos[1])
			if err != nil {
				return fail(err)
			}
			v, okSec := con.Secret(auth.Prompt + "：")
			if !okSec {
				return fail(engine.ErrCanceled)
			}
			secretVal = v
		}
		ticket, err := e.FirewallUninstallAuth(c.pos[1], engine.FirewallAuthOptions{
			Kind:        c.pos[2],
			Method:      c.flags["method"],
			Secret:      secretVal,
			ChallengeID: c.flags["challenge-id"],
		})
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(ticket)
		}
		con.OK("身份认证已通过（%s），令牌 2 分钟内有效", ticket.Method)
		return 0

	case "uninstall":
		if len(c.pos) < 3 {
			return fail(portUsage)
		}
		target, kind := c.pos[1], c.pos[2]
		token := strings.TrimSpace(cio.getenv("FERRY_FW_UNINSTALL_TOKEN"))
		if token == "" {
			if js != nil {
				return fail(&engine.FirewallAuthError{Msg: "卸载防火墙前必须先通过身份认证（缺少 FERRY_FW_UNINSTALL_TOKEN）"})
			}
			t, err := cliFirewallUninstallAuth(e, con, target, kind, c.flags["method"])
			if err != nil {
				return fail(err)
			}
			token = t
		}
		rep, err := e.FirewallUninstall(target, engine.FirewallUninstallOptions{
			Kind:          kind,
			Purge:         c.has("purge"),
			RestartDocker: c.has("restart-docker"),
			Token:         token,
		})
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(rep)
		}
		con.OK("防火墙已卸载（%s）", kind)
		return 0

	case "open", "close":
		if len(c.pos) < 3 {
			return fail(portUsage)
		}
		port, pr, err := parsePortArg(c.pos[2])
		if err != nil {
			return fail(err)
		}
		rep, err := e.PortSet(c.pos[1], port, pr, sub)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(rep)
		}
		con.OK("%s %d/%s 完成", map[string]string{"open": "放行", "close": "关闭"}[sub], port, pr)
		return 0

	case "check":
		if len(c.pos) < 3 {
			return fail(portUsage)
		}
		var ports []int
		for _, a := range c.pos[2:] {
			for _, s := range strings.Split(a, ",") {
				p, pr, err := parsePortArg(s)
				if err != nil {
					return fail(err)
				}
				if pr != "tcp" {
					return fail(fmt.Errorf("只能测 TCP 端口：%s", s))
				}
				ports = append(ports, p)
			}
		}
		timeout := 3 * time.Second
		if v := c.flags["timeout"]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 30 {
				return fail(errors.New("--timeout 是 1～30 秒"))
			}
			timeout = time.Duration(n) * time.Second
		}
		res, err := e.PortCheck(c.pos[1], ports, timeout)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(res)
		}
		for _, r := range res {
			switch {
			case r.OK:
				con.OK("%d 能连上（%d ms）", r.Port, r.Ms)
			case r.Why == "refused":
				con.Warn("%d 连不上：服务器回绝了（没有程序在监听，或防火墙拒绝）", r.Port)
			case r.Why == "timeout":
				con.Warn("%d 连不上：没有回应（多半被防火墙或云服务商的安全组挡住了）", r.Port)
			default:
				con.Warn("%d 连不上：%s", r.Port, r.Msg)
			}
		}
		return 0
	}
	rep, err := e.Ports(c.pos[0])
	if err != nil {
		return fail(err)
	}
	if js != nil {
		return ok(rep)
	}
	printPorts(con, rep)
	return 0
}

func parseAllowFlag(v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}
	var allow []string
	for _, s := range strings.Split(v, ",") {
		port, pr, err := parsePortArg(s)
		if err != nil {
			return nil, err
		}
		allow = append(allow, fmt.Sprintf("%d/%s", port, pr))
	}
	return allow, nil
}

// cliFirewallUninstallAuth 命令行交互式卸载认证：支持 sudo/SSH/私钥密码及免密密钥挑战码。
func cliFirewallUninstallAuth(e *engine.Engine, con *ui.Console, target, kind, methodOverride string) (string, error) {
	auth, err := e.FirewallUninstallAuthInfo(target)
	if err != nil {
		return "", err
	}
	method := auth.Method
	if methodOverride == "ssh-password" && auth.CanPasswordFallback {
		method = "ssh-password"
	}
	con.Warn("卸载 %s 的防火墙（%s）需要实时身份认证", target, kind)
	var secretVal, challengeID string
	if method == "ssh-key-challenge" {
		ch, err := e.FirewallUninstallChallenge(target, kind)
		if err != nil {
			return "", err
		}
		challengeID = ch.ID
		con.Info("服务端一次性认证码：%s（3 分钟内有效）", ch.Code)
		fmt.Fprint(os.Stderr, "请输入上面的认证码以确认卸载：")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return "", engine.ErrCanceled
		}
		secretVal = strings.TrimSpace(line)
	} else {
		prompt := auth.Prompt
		if method == "ssh-password" && auth.Method != "ssh-password" {
			prompt = fmt.Sprintf("请输入 %s@%s:%d 的 SSH 登录密码以验证身份", auth.User, auth.Host, auth.Port)
		}
		v, ok := con.Secret(prompt + "：")
		if !ok {
			return "", engine.ErrCanceled
		}
		secretVal = v
	}
	ticket, err := e.FirewallUninstallAuth(target, engine.FirewallAuthOptions{
		Kind:        kind,
		Method:      method,
		Secret:      secretVal,
		ChallengeID: challengeID,
	})
	if err != nil {
		return "", err
	}
	return ticket.Token, nil
}

// localOnly 只监听本机（127.0.0.1、[::1]）。
func localOnly(listen []string) bool {
	for _, h := range listen {
		if !strings.HasPrefix(h, "127.") && h != "[::1]" {
			return false
		}
	}
	return len(listen) > 0
}

// parsePortArg 8080、8080/tcp、53/udp。
func parsePortArg(s string) (int, string, error) {
	p, pr, _ := strings.Cut(strings.TrimSpace(s), "/")
	if pr == "" {
		pr = "tcp"
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 || (pr != "tcp" && pr != "udp") {
		return 0, "", fmt.Errorf("端口无效：%s（写 8080、8080/tcp 或 53/udp）", s)
	}
	return n, pr, nil
}

func printPorts(con *ui.Console, rep *proto.PortReport) {
	fw := rep.Firewall
	switch {
	case fw.Active && fw.Kind == "firewalld":
		con.Info("防火墙：firewalld（区域 %s）", fw.Zone)
	case fw.Active:
		con.Info("防火墙：%s", fw.Kind)
	}
	if fw.Why != "" {
		con.Warn("%s", fw.Why)
	}
	t := &ui.Table{Header: []string{"端口", "监听", "程序", "防火墙", "规则"}}
	for _, p := range rep.Ports {
		listen := strings.Join(p.Listen, " ")
		switch {
		case listen == "":
			listen = "没有程序在用"
		case p.Iface != "":
			listen += "（" + p.Iface + "）"
		}
		who := p.Process
		switch {
		case p.Docker:
			who = "Docker 容器 " + p.Container
		case p.Unit != "":
			who = strings.TrimSpace(who + " " + p.Unit)
		}
		if p.SSH {
			who = strings.TrimSpace(who + "（SSH）")
		}
		state := map[string]string{"open": "放行", "blocked": "拦截", "partial": "部分来源", "": "—"}[p.Firewall]
		switch {
		case localOnly(p.Listen):
			state = "只监听本机" // 防火墙怎么设外部都连不上
		case p.Internal:
			state = "只在内部网卡"
		case p.Docker && p.Guard:
			state = "Ferry 在 DOCKER-USER 里拦着外部"
		case p.Docker && len(p.Listen) > 0:
			state = "对外开放（Docker 映射，不经过防火墙）"
		}
		rule := p.Rule
		if p.Docker && rule != "" {
			rule += "（对 Docker 映射不起作用）"
		}
		if p.Temp {
			rule += "（临时，重启后失效）"
		}
		t.Rows = append(t.Rows, []string{fmt.Sprintf("%d/%s", p.Port, p.Proto), listen, who, state, rule})
	}
	t.Write(os.Stdout)
	if g := rep.DockerGuard; g != nil && !g.OK {
		con.Warn("Docker 映射的端口：%s", g.Why)
	}
	for _, n := range rep.Notes {
		con.Warn("%s", n)
	}
}
