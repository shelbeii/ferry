package agent

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// ---------------------------------------------------------------- 监听地址在哪张网卡上

// reInnerIface 内部的虚拟网卡：libvirt、Docker、Podman、LXC、k8s 的网桥和虚拟网卡，外部的机器连不到上面的地址。
var reInnerIface = regexp.MustCompile(`^(virbr|docker|br-|veth|cni|flannel|cali|kube-|podman|lxcbr|lxdbr|vnet|tunl0|vxlan)`)

// hostIfaces 服务器上每个地址所在的网卡：192.168.122.1 → virbr0。
func hostIfaces() map[string]string {
	out := map[string]string{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, it := range ifs {
		addrs, err := it.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				out[n.IP.String()] = it.Name
			}
		}
	}
	return out
}

// markIfaces 监听的是具体地址时标出所在的网卡；都在内部网卡上的标为 Internal。
func markIfaces(p *proto.PortInfo, ifaces map[string]string) {
	if len(p.Listen) == 0 || len(ifaces) == 0 {
		return
	}
	var names []string
	inner := true
	for _, h := range p.Listen {
		if h == "*" {
			return
		}
		ip := strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
		if strings.HasPrefix(ip, "127.") || ip == "::1" {
			continue
		}
		name := ifaces[ip]
		if name == "" {
			inner = false
			continue
		}
		if !contains(names, name) {
			names = append(names, name)
		}
		inner = inner && reInnerIface.MatchString(name)
	}
	if len(names) == 0 {
		return
	}
	p.Iface = strings.Join(names, " ")
	p.Internal = inner && !p.Docker
}

// ---------------------------------------------------------------- 打开防火墙

// 打开防火墙（fwenable）怎么保证不把自己关在外面：
//  1. 打开之前先写好放行规则：SSH 端口（sshd 监听的和 Ferry 连服务器用的）一定放行，再加上用户勾的端口。
//     firewalld 没运行时用 firewall-offline-cmd 改永久配置，ufw 不启用也能加规则。
//  2. 安排 3 分钟后自动恢复（关掉防火墙）：systemd-run 的临时定时器 ferry-fw-revert。
//  3. 打开防火墙；Docker 在运行、打开的是 firewalld 时重启 Docker（firewalld 启动会动 Docker 的规则，容器的端口映射可能失效）。
//  4. 引擎用新的连接试 SSH 端口，连得上再调 fwconfirm 取消自动恢复；连不上就等它自己恢复。

const revertUnit = "ferry-fw-revert"

// revertDelay 自动恢复前等多久（引擎要在这之前确认）。
const revertDelay = 3 * time.Minute

var rePortProto = regexp.MustCompile(`^([1-9][0-9]{0,4})/(tcp|udp)$`)

func (a *Agent) fwenable() int {
	sc := a.fwScanner()
	_, st := sc.report()
	if st.Active {
		a.info(proto.EvFirewall, "", "防火墙（%s）已经在运行", st.Kind)
		return a.portDone(sc)
	}
	if !st.Enable {
		return a.result(proto.CodeFailed, "服务器上没有装 firewalld 或 ufw：Ferry 不替你装防火墙", nil)
	}
	allow := map[string]bool{}
	for _, p := range a.req.Allow {
		m := rePortProto.FindStringSubmatch(p)
		if m == nil {
			return a.result(proto.CodeBadInput, "端口无效："+p+"（写 8080/tcp 或 53/udp）", nil)
		}
		if n, _ := strconv.Atoi(m[1]); n > 65535 {
			return a.result(proto.CodeBadInput, "端口无效："+p, nil)
		}
		allow[p] = true
	}
	var ssh []string
	for port := range sc.ssh {
		ssh = append(ssh, strconv.Itoa(port)+"/tcp")
	}
	for _, port := range sc.keep {
		ssh = append(ssh, strconv.Itoa(port)+"/tcp")
	}
	if len(ssh) == 0 {
		return a.result(proto.CodeFailed, "找不到 SSH 端口：不敢打开防火墙", nil)
	}
	for _, p := range ssh {
		allow[p] = true
	}
	list := make([]string, 0, len(allow))
	for p := range allow {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.Split(list[i], "/")[0])
		b, _ := strconv.Atoi(strings.Split(list[j], "/")[0])
		return a < b || (a == b && list[i] < list[j])
	})
	docker := st.Kind == "firewalld" && st.Docker
	if docker && !a.req.RestartDocker {
		return a.result(proto.CodeFailed, "Docker 正在运行：打开 firewalld 后容器的端口映射可能失效，要同时重启 Docker（容器会重启一次）", nil)
	}

	run := func(cmd string) error {
		out, err := a.Sys.Shell(cmd+" 2>&1", 2*time.Minute)
		if err != nil {
			return fmt.Errorf("%s：%s", cmd, clipLine(strings.TrimSpace(out)))
		}
		return nil
	}
	fail := func(err error) int {
		a.fail(proto.EvFail, "", "%v", err)
		return a.result(proto.CodeFailed, err.Error(), nil)
	}

	// 1. 先放行
	a.info(proto.EvFirewall, "", "打开 %s 之前先放行：%s（SSH：%s）", st.Kind, strings.Join(list, " "), strings.Join(ssh, " "))
	offline := false
	zone := ""
	if st.Kind == "firewalld" {
		if _, err := a.Sys.Shell("command -v firewall-offline-cmd", 10*time.Second); err == nil {
			offline = true
			out, _ := a.Sys.Shell("LC_ALL=C firewall-offline-cmd --get-default-zone 2>/dev/null", 30*time.Second)
			if zone = strings.TrimSpace(out); !reSafe.MatchString(zone) {
				return a.result(proto.CodeFailed, "读不到 firewalld 的默认区域："+clipLine(zone), nil)
			}
			for _, p := range list {
				if err := run("firewall-offline-cmd --zone=" + zone + " --add-port=" + p); err != nil {
					return fail(err)
				}
			}
		}
	} else {
		for _, p := range list {
			if err := run("ufw allow " + p); err != nil {
				return fail(err)
			}
		}
	}

	// 2. 安排自动恢复
	revert := "ufw --force disable"
	if st.Kind == "firewalld" {
		revert = "systemctl disable --now firewalld"
		if docker {
			revert += "; systemctl restart docker"
		}
	}
	a.Sys.Shell("systemctl stop "+revertUnit+".timer "+revertUnit+".service 2>/dev/null; systemctl reset-failed "+revertUnit+".service 2>/dev/null", 30*time.Second)
	if err := run(fmt.Sprintf("systemd-run --unit=%s --on-active=%d /bin/sh -c %s", revertUnit, int(revertDelay.Seconds()), shellQuote(revert))); err != nil {
		return fail(fmt.Errorf("安排自动恢复失败，没有打开防火墙：%v", err))
	}
	a.info(proto.EvFirewall, "", "已安排 %d 分钟后自动关掉防火墙（新的连接确认能连上 SSH 后取消）", int(revertDelay.Minutes()))
	cancel := func() { a.Sys.Shell("systemctl stop "+revertUnit+".timer 2>/dev/null", 30*time.Second) }

	// 3. 打开
	if st.Kind == "firewalld" {
		if err := run("systemctl enable --now firewalld"); err != nil {
			cancel()
			return fail(err)
		}
		if !offline { // 没有 firewall-offline-cmd：启动后马上放行（已经建立的 SSH 连接不受影响）
			out, _ := a.Sys.Shell("LC_ALL=C firewall-cmd --get-default-zone 2>/dev/null", 30*time.Second)
			zone = strings.TrimSpace(out)
			for _, p := range list {
				if err := run("firewall-cmd --zone=" + zone + " --add-port=" + p + " && firewall-cmd --permanent --zone=" + zone + " --add-port=" + p); err != nil {
					a.Sys.Shell(revert, time.Minute)
					cancel()
					return fail(err)
				}
			}
		}
		a.info(proto.EvFirewall, "", "已打开 firewalld（开机自动启动）")
	} else {
		if err := run("ufw --force enable"); err != nil {
			cancel()
			return fail(err)
		}
		a.info(proto.EvFirewall, "", "已启用 ufw")
	}
	if docker {
		a.info(proto.EvFirewall, "", "重启 Docker，让容器的端口映射重新生效")
		if err := run("systemctl restart docker"); err != nil {
			a.warn(proto.EvWarn, "", "重启 Docker 失败：%v", err)
		}
	}
	a.Sys.Shell(fmt.Sprintf("logger -t ferry -- %s 2>/dev/null", shellQuote(fmt.Sprintf("%s 打开了防火墙 %s，放行 %s", a.req.Actor, st.Kind, strings.Join(list, " ")))), 10*time.Second)

	// 4. 检查：SSH 端口要是放行的
	rep, st2 := sc.report()
	if !st2.Active {
		a.Sys.Shell(revert, time.Minute)
		cancel()
		return a.result(proto.CodeFailed, "防火墙没有起来："+st2.Why, nil)
	}
	for _, p := range ssh {
		port, _ := strconv.Atoi(strings.TrimSuffix(p, "/tcp"))
		if s, _ := st2.decide(port, "tcp"); s != "open" {
			a.Sys.Shell(revert, time.Minute)
			cancel()
			return a.result(proto.CodeFailed, "打开后 SSH 端口 "+p+" 没有放行，已经关掉防火墙恢复原样", nil)
		}
	}
	return a.result(proto.CodeOK, "", rep)
}

// ---------------------------------------------------------------- 关闭防火墙

// fwdisable 关掉正在运行的防火墙（firewalld / ufw），开机也不再启动。放行规则留在配置里，再打开时照旧生效。
// 关掉只会多放开端口，不会把 SSH 关在外面，所以不用安排自动恢复。
func (a *Agent) fwdisable() int {
	sc := a.fwScanner()
	_, st := sc.report()
	if !st.Active {
		a.info(proto.EvFirewall, "", "防火墙已经是关着的")
		return a.portDone(sc)
	}
	if !st.Disable {
		return a.result(proto.CodeFailed, "Ferry 只能关 firewalld 或 ufw："+st.Why, nil)
	}
	docker := st.Kind == "firewalld" && st.Docker
	if docker && !a.req.RestartDocker {
		return a.result(proto.CodeFailed, "Docker 正在运行：关掉 firewalld 时可能清掉 Docker 的网络规则，容器的端口映射会失效，要同时重启 Docker（容器会重启一次）", nil)
	}
	cmd := "ufw --force disable"
	if st.Kind == "firewalld" {
		cmd = "systemctl disable --now firewalld"
	}
	if out, err := a.Sys.Shell(cmd+" 2>&1", 2*time.Minute); err != nil {
		msg := fmt.Sprintf("%s：%s", cmd, clipLine(strings.TrimSpace(out)))
		a.fail(proto.EvFail, "", "%s", msg)
		return a.result(proto.CodeFailed, msg, nil)
	}
	if st.Kind == "firewalld" {
		a.info(proto.EvFirewall, "", "已关掉 firewalld（开机不再启动）")
	} else {
		a.info(proto.EvFirewall, "", "已停用 ufw（开机不再启用）")
	}
	if docker {
		a.info(proto.EvFirewall, "", "重启 Docker，让容器的端口映射重新生效")
		if out, err := a.Sys.Shell("systemctl restart docker 2>&1", 2*time.Minute); err != nil {
			a.warn(proto.EvWarn, "", "重启 Docker 失败：%s", clipLine(strings.TrimSpace(out)))
		}
	}
	a.Sys.Shell(fmt.Sprintf("logger -t ferry -- %s 2>/dev/null", shellQuote(fmt.Sprintf("%s 关闭了防火墙 %s", a.req.Actor, st.Kind))), 10*time.Second)

	rep, st2 := sc.report()
	if st2.Active {
		return a.result(proto.CodeFailed, "关了以后防火墙（"+st2.Kind+"）还在运行", nil)
	}
	return a.result(proto.CodeOK, "", rep)
}

// fwconfirm 新的连接能连上：取消自动恢复。
func (a *Agent) fwconfirm() int {
	a.Sys.Shell("systemctl stop "+revertUnit+".timer 2>/dev/null; systemctl reset-failed "+revertUnit+".service 2>/dev/null", 30*time.Second)
	a.info(proto.EvFirewall, "", "新的连接能连上 SSH：取消自动恢复，防火墙保持打开")
	return a.portDone(a.fwScanner())
}
