package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func decodeAgentResult(t *testing.T, buf *bytes.Buffer) proto.Result {
	t.Helper()
	var last proto.Result
	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var r proto.Result
		if json.Unmarshal([]byte(line), &r) == nil && r.T == "result" {
			last, found = r, true
		}
	}
	if !found {
		t.Fatalf("未找到 agent result 行：%s", buf.String())
	}
	return last
}

func TestApplyFirewallInventory(t *testing.T) {
	dir := t.TempDir()
	sc := &fwScan{ferryDir: dir}

	// 1. 没有任何防火墙，dnf 可用
	st := &fwState{Firewall: proto.Firewall{Why: "服务器上没有防火墙（firewalld 或 ufw）：所有端口都不拦"}}
	sc.applyFirewallInventory(st, map[string]string{"pkg": "dnf"})
	if st.PkgMgr != "dnf" || len(st.Installed) != 0 || st.Kind != "" {
		t.Fatalf("无防火墙探测异常：%+v", st.Firewall)
	}

	// 2. firewalld 已装（包管理器安装）且 nftables 只是未启用的底座包：只列 firewalld，不误报 nftables
	st = &fwState{Firewall: proto.Firewall{Kind: "firewalld", Active: true, Manage: true}}
	sc.applyFirewallInventory(st, map[string]string{
		"pkg":           "dnf",
		"has_firewalld": "1",
		"pkg_firewalld": "1",
		"nft_svc":       "installed",
		"pkg_nftables":  "1",
	})
	if len(st.Installed) != 1 || st.Installed[0].Kind != "firewalld" || !st.Installed[0].Uninstall || !st.Installed[0].Active {
		t.Fatalf("firewalld 清单异常：%+v", st.Installed)
	}

	// 3. 手动配置 iptables 规则但未装 iptables-services：标为不可自动卸载
	st = &fwState{Firewall: proto.Firewall{Kind: "iptables", Active: true}}
	sc.applyFirewallInventory(st, map[string]string{"pkg": "dnf"})
	if len(st.Installed) != 1 || st.Installed[0].Kind != "iptables" || st.Installed[0].Uninstall || st.Installed[0].Why == "" {
		t.Fatalf("手动 iptables 应不可自动卸载：%+v", st.Installed)
	}

	// 4. Ferry 安装了 nftables（写入 managed 标记）且未配置拦截规则：能识别为已安装且可卸载
	writeManagedFirewall(dir, "nftables", true)
	st = &fwState{Firewall: proto.Firewall{}}
	sc.applyFirewallInventory(st, map[string]string{"pkg": "apt-get", "pkg_nftables": "1"})
	if st.Kind != "nftables" || st.Active || len(st.Installed) != 1 || !st.Installed[0].Uninstall {
		t.Fatalf("Ferry 安装的 nftables 识别异常：%+v", st.Firewall)
	}
	writeManagedFirewall(dir, "nftables", false)

	// 5. 同时装了 firewalld（运行）与 ufw（未启用）：两者都列出
	st = &fwState{Firewall: proto.Firewall{Kind: "firewalld", Active: true, Manage: true}}
	sc.applyFirewallInventory(st, map[string]string{
		"pkg":           "apt-get",
		"has_firewalld": "1",
		"pkg_firewalld": "1",
		"has_ufw":       "1",
		"pkg_ufw":       "1",
	})
	if len(st.Installed) != 2 || st.Installed[0].Kind != "firewalld" || st.Installed[1].Kind != "ufw" {
		t.Fatalf("多防火墙识别异常：%+v", st.Installed)
	}

	// 6. 主探测未识别 ufw（例如 PATH 缺 /usr/sbin 或 ufw status 报错），但包清单发现 ufw 已装：补齐 Kind/Why，绝不与 Installed 矛盾
	st = &fwState{Firewall: proto.Firewall{Why: "服务器上没有防火墙（firewalld 或 ufw）：所有端口都不拦"}}
	sc.applyFirewallInventory(st, map[string]string{"pkg": "apt-get", "has_ufw": "1", "pkg_ufw": "1"})
	if st.Kind != "ufw" || !st.Enable || !strings.Contains(st.Why, "ufw") {
		t.Fatalf("ufw 状态补齐异常：%+v", st.Firewall)
	}

	// 7. 仅检测到 firewalld 软件包但缺少命令：补齐 Kind 并提示可卸载重装（不给 Enable）
	st = &fwState{Firewall: proto.Firewall{Why: "服务器上没有防火墙（firewalld 或 ufw）：所有端口都不拦"}}
	sc.applyFirewallInventory(st, map[string]string{"pkg": "dnf", "pkg_firewalld": "1"})
	if st.Kind != "firewalld" || st.Enable || !strings.Contains(st.Why, "firewall-cmd") {
		t.Fatalf("残缺 firewalld 状态补齐异常：%+v", st.Firewall)
	}
}

func TestShellEnvWithSbin(t *testing.T) {
	got := shellEnvWithSbin([]string{"HOME=/root", "PATH=/usr/local/bin:/usr/bin:/bin"})
	found := ""
	for _, kv := range got {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			found = v
		}
	}
	if found != "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin" {
		t.Fatalf("PATH 未补齐 sbin：%q", found)
	}
	// 已有 /usr/sbin 时不重复添加
	got2 := shellEnvWithSbin([]string{"PATH=/usr/sbin:/usr/bin:/sbin:/usr/local/sbin"})
	if got2[0] != "PATH=/usr/sbin:/usr/bin:/sbin:/usr/local/sbin" {
		t.Fatalf("已有路径被重复追加：%q", got2[0])
	}
}

func TestParsePkgRemoveImpact(t *testing.T) {
	// DNF：只删自身与无用子依赖 → 允许
	dnfOK := `Dependencies resolved.
Removing:
 firewalld                noarch  1.3.4-1.el9    @anaconda   2.1 M
Removing unused dependencies:
 python3-firewall         noarch  1.3.4-1.el9    @anaconda   1.5 M
 nftables                 x86_64  1:1.0.9-1.el9  @baseos     1.1 M

Transaction Summary
Remove  3 Packages
Operation aborted.
`
	if deps, crit := parsePkgRemoveImpact("dnf", dnfOK, fwSpecs["firewalld"]); len(deps) != 0 || len(crit) != 0 {
		t.Fatalf("DNF 正常卸载误报：deps=%v crit=%v", deps, crit)
	}

	// DNF：存在依赖它的第三方包 → 中止
	dnfDep := `Removing:
 firewalld                noarch  1.3.4-1.el9    @anaconda   2.1 M
Removing dependent packages:
 fail2ban-firewalld       noarch  1.0.2-1.el9    @epel       12 k
`
	if deps, crit := parsePkgRemoveImpact("dnf", dnfDep, fwSpecs["firewalld"]); strings.Join(deps, ",") != "fail2ban-firewalld" || len(crit) != 0 {
		t.Fatalf("DNF 依赖包检测异常：deps=%v crit=%v", deps, crit)
	}

	// DNF：连带删除关键系统包 → 阻断
	dnfCrit := `Removing:
 nftables                 x86_64  1.0.9-1.el9    @baseos     1.1 M
Removing dependent packages:
 docker-ce                x86_64  26.1.0         @docker     100 M
`
	if _, crit := parsePkgRemoveImpact("dnf", dnfCrit, fwSpecs["nftables"]); strings.Join(crit, ",") != "docker-ce" {
		t.Fatalf("DNF 关键包检测异常：crit=%v", crit)
	}

	// APT：只删 ufw → 允许
	aptOK := "NOTE: This is only a simulation!\nRemv ufw [0.36.1-4ubuntu0.1]\n"
	if deps, crit := parsePkgRemoveImpact("apt-get", aptOK, fwSpecs["ufw"]); len(deps) != 0 || len(crit) != 0 {
		t.Fatalf("APT 正常卸载误报：deps=%v crit=%v", deps, crit)
	}

	// APT：连带删除关键包或依赖包 → 阻断
	aptBad := "Remv ufw [0.36.1]\nRemv my-panel [1.0]\nRemv openssh-server [1:8.9]\n"
	deps, crit := parsePkgRemoveImpact("apt-get", aptBad, fwSpecs["ufw"])
	if strings.Join(deps, ",") != "my-panel" || strings.Join(crit, ",") != "openssh-server" {
		t.Fatalf("APT 卸载检测异常：deps=%v crit=%v", deps, crit)
	}
}

func TestFirewallAuthChallengeAndToken(t *testing.T) {
	f := newFake(t)
	ferryDir := t.TempDir()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	f.now = now

	run := func(cmd string, req *proto.Request) (int, proto.Result) {
		var buf bytes.Buffer
		req.Proto = proto.Version
		req.Root = f.root
		req.FerryDir = ferryDir
		a := &Agent{Sys: f, Out: &buf}
		code := a.Run(cmd, req)
		return code, decodeAgentResult(t, &buf)
	}

	// 1. 申请挑战码
	code, res := run("fwauth-challenge", &proto.Request{Actor: "alice@pc", FirewallKind: "firewalld"})
	if code != proto.CodeOK {
		t.Fatalf("申请挑战失败：%d %s", code, res.Msg)
	}
	var ch proto.FirewallChallenge
	if err := json.Unmarshal(res.Data, &ch); err != nil || len(ch.Code) != 6 || ch.ID == "" {
		t.Fatalf("挑战内容无效：%s", res.Data)
	}

	// 2. 错误验证码被拒（前两次保留，第三次作废）
	for i := 0; i < 3; i++ {
		code, _ = run("fwauth-issue", &proto.Request{
			Actor: "alice@pc", FirewallKind: "firewalld", FirewallAuthMethod: "ssh-key-challenge",
			FirewallChallengeID: ch.ID, FirewallChallengeCode: "WRONG1",
		})
		if code == proto.CodeOK {
			t.Fatal("错误验证码不应通过")
		}
	}
	// 作废后即使填对也失败
	code, _ = run("fwauth-issue", &proto.Request{
		Actor: "alice@pc", FirewallKind: "firewalld", FirewallAuthMethod: "ssh-key-challenge",
		FirewallChallengeID: ch.ID, FirewallChallengeCode: ch.Code,
	})
	if code == proto.CodeOK {
		t.Fatal("多次输错作废后不应再通过")
	}

	// 3. 重新申请并验证通过（不区分大小写）
	_, res = run("fwauth-challenge", &proto.Request{Actor: "alice@pc", FirewallKind: "firewalld"})
	json.Unmarshal(res.Data, &ch)
	code, res = run("fwauth-issue", &proto.Request{
		Actor: "alice@pc", FirewallKind: "firewalld", FirewallAuthMethod: "ssh-key-challenge",
		FirewallChallengeID: ch.ID, FirewallChallengeCode: strings.ToLower(ch.Code),
	})
	if code != proto.CodeOK {
		t.Fatalf("正确验证码应签发令牌：%s", res.Msg)
	}
	var ticket proto.FirewallTicket
	if err := json.Unmarshal(res.Data, &ticket); err != nil || ticket.Token == "" {
		t.Fatalf("令牌无效：%s", res.Data)
	}

	// 4. 挑战码单次有效：不能重复签发
	code, _ = run("fwauth-issue", &proto.Request{
		Actor: "alice@pc", FirewallKind: "firewalld", FirewallAuthMethod: "ssh-key-challenge",
		FirewallChallengeID: ch.ID, FirewallChallengeCode: ch.Code,
	})
	if code == proto.CodeOK {
		t.Fatal("挑战码不应允许重放")
	}

	// 5. 令牌校验：绑定 kind 与 actor
	a := &Agent{Sys: f, req: &proto.Request{FerryDir: ferryDir, Actor: "alice@pc"}}
	if err := a.validateUninstallToken("ufw", ticket.Token); err == nil {
		t.Fatal("kind 不匹配应拒绝")
	}
	aOther := &Agent{Sys: f, req: &proto.Request{FerryDir: ferryDir, Actor: "bob@pc"}}
	if err := aOther.validateUninstallToken("firewalld", ticket.Token); err == nil {
		t.Fatal("actor 不匹配应拒绝")
	}
	if err := a.validateUninstallToken("firewalld", ticket.Token); err != nil {
		t.Fatalf("有效令牌应通过：%v", err)
	}

	// 6. 过期令牌拒绝
	f.now = now.Add(fwTokenTTL + time.Second)
	if err := a.validateUninstallToken("firewalld", ticket.Token); err == nil {
		t.Fatal("过期令牌应被拒绝")
	}
}

type fwMockSys struct {
	*fakeSys
	handler func(cmd string) (string, error)
}

func (m *fwMockSys) Shell(cmd string, timeout time.Duration) (string, error) {
	m.calls = append(m.calls, "sh "+cmd)
	return m.handler(cmd)
}

func TestFirewallInstallAndUninstallFlow(t *testing.T) {
	f := newFake(t)
	ferryDir := t.TempDir()
	policyTmp := filepath.Join(t.TempDir(), "policy-rc.d")
	prevPolicy := policyRCPath
	policyRCPath = policyTmp
	defer func() { policyRCPath = prevPolicy }()

	installedUfw := false
	policySawDuringInstall := false
	removedCalled := false
	aptDryRunUpgrade := false

	sys := &fwMockSys{
		fakeSys: f,
		handler: func(cmd string) (string, error) {
			switch {
			case strings.Contains(cmd, "pkg=\"\""):
				if installedUfw {
					return "pkg=apt-get\nhas_ufw=1\npkg_ufw=1\n", nil
				}
				return "pkg=apt-get\n", nil
			case strings.Contains(cmd, "firewall-cmd --state"):
				return "none\n", nil
			case strings.Contains(cmd, "ufw status verbose"):
				if installedUfw {
					return "Status: inactive\n", nil
				}
				return "none\n", nil
			case strings.Contains(cmd, "iptables -S INPUT"), strings.Contains(cmd, "nft list ruleset"):
				return "", nil
			case strings.Contains(cmd, "systemctl is-active docker"):
				return "inactive\n", nil
			case strings.Contains(cmd, "apt-get -s install"):
				if aptDryRunUpgrade {
					return "NOTE: This is only a simulation!\nInst libssl3 [3.0.1] (3.0.2 Ubuntu)\nInst ufw (0.36 Ubuntu)\n", nil
				}
				return "NOTE: This is only a simulation!\nInst ufw (0.36 Ubuntu)\n", nil
			case strings.Contains(cmd, "apt-get install -y"):
				if _, err := os.Stat(policyTmp); err == nil {
					policySawDuringInstall = true
				}
				installedUfw = true
				return "Setting up ufw...\n", nil
			case strings.Contains(cmd, "command -v ufw"):
				if installedUfw {
					return "/usr/sbin/ufw\n", nil
				}
				return "", errors.New("not found")
			case strings.Contains(cmd, "dpkg-query -W -f='${Status}' 'ufw'"):
				if installedUfw {
					return "install ok installed\n", nil
				}
				return "", errors.New("not installed")
			case strings.Contains(cmd, "apt-get -s remove"):
				return "NOTE: This is only a simulation!\nRemv ufw [0.36]\n", nil
			case strings.Contains(cmd, "apt-get remove -y"), strings.Contains(cmd, "apt-get purge -y"):
				removedCalled = true
				installedUfw = false
				return "Removing ufw...\n", nil
			default:
				return "", nil
			}
		},
	}

	run := func(cmd string, req *proto.Request) (int, proto.Result) {
		var buf bytes.Buffer
		req.Proto = proto.Version
		req.Root = f.root
		req.FerryDir = ferryDir
		req.Actor = "admin@pc"
		a := &Agent{Sys: sys, Out: &buf}
		code := a.Run(cmd, req)
		return code, decodeAgentResult(t, &buf)
	}

	// 1. 不支持类型拒绝
	if code, _ := run("fwinstall", &proto.Request{FirewallKind: "unknown"}); code != proto.CodeBadInput {
		t.Fatalf("非法防火墙类型应返回 CodeBadInput，实际 %d", code)
	}

	// 2. 演练检测到顺带升级系统包时停下来返回 CodeConfirm
	aptDryRunUpgrade = true
	if code, res := run("fwinstall", &proto.Request{FirewallKind: "ufw"}); code != proto.CodeConfirm {
		t.Fatalf("应返回 CodeConfirm 等待同意，实际 %d (%s)", code, res.Msg)
	}
	if installedUfw {
		t.Fatal("未同意包变更前不能执行安装")
	}
	aptDryRunUpgrade = false

	// 3. 正式安装 ufw（验证 Debian policy-rc.d 防自启生效并在结束后清理）
	code, res := run("fwinstall", &proto.Request{FirewallKind: "ufw"})
	if code != proto.CodeOK {
		t.Fatalf("安装 ufw 失败：%d %s", code, res.Msg)
	}
	if !installedUfw || !policySawDuringInstall {
		t.Fatalf("安装状态异常：installed=%v policySaw=%v", installedUfw, policySawDuringInstall)
	}
	if _, err := os.Stat(policyTmp); !os.IsNotExist(err) {
		t.Fatal("安装结束后应删除临时 policy-rc.d")
	}

	// 4. 冲突检测：已装 ufw 时再装 firewalld 应拒绝
	if code, res := run("fwinstall", &proto.Request{FirewallKind: "firewalld"}); code != proto.CodeFailed || !strings.Contains(res.Msg, "冲突") {
		t.Fatalf("应阻止防火墙冲突安装：%d %s", code, res.Msg)
	}

	// 5. 未认证直接卸载必须被拒绝
	if code, res := run("fwuninstall", &proto.Request{FirewallKind: "ufw"}); code != proto.CodeFailed || !strings.Contains(res.Msg, "认证") {
		t.Fatalf("无令牌卸载必须失败：%d %s", code, res.Msg)
	}
	if removedCalled {
		t.Fatal("认证未通过时绝不能执行卸载")
	}

	// 6. 签发一次性卸载令牌后成功卸载
	code, res = run("fwauth-issue", &proto.Request{FirewallKind: "ufw", FirewallAuthMethod: "sudo-password"})
	if code != proto.CodeOK {
		t.Fatalf("签发令牌失败：%s", res.Msg)
	}
	var ticket proto.FirewallTicket
	json.Unmarshal(res.Data, &ticket)

	code, res = run("fwuninstall", &proto.Request{FirewallKind: "ufw", FirewallToken: ticket.Token})
	if code != proto.CodeOK || !removedCalled || installedUfw {
		t.Fatalf("卸载 ufw 失败：code=%d msg=%s removed=%v installed=%v", code, res.Msg, removedCalled, installedUfw)
	}

	// 7. 令牌已被消费：重放必须失败
	if code, _ := run("fwuninstall", &proto.Request{FirewallKind: "ufw", FirewallToken: ticket.Token}); code == proto.CodeOK {
		t.Fatal("单次令牌不能重放")
	}
}
