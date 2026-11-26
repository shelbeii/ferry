package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// fwSpec 支持通过 Ferry 安装与卸载的 Linux 防火墙类型定义。
type fwSpec struct {
	kind       string
	dnfPkgs    []string
	aptPkgs    []string
	familyPkgs []string // 卸载演练中属于该防火墙自身的包（不视为无关依赖）
	units      []string // 停止 / 禁用时涉及的 systemd 单元
	purgePaths []string // --purge 时额外清理的残留配置目录/文件（均位于 /etc 下）
	canEnable  bool     // 是否支持安装后立即按安全流程启用
	verifyCmd  string   // 安装完成后校验程序/服务就绪
}

var fwSpecs = map[string]fwSpec{
	"firewalld": {
		kind:       "firewalld",
		dnfPkgs:    []string{"firewalld"},
		aptPkgs:    []string{"firewalld"},
		familyPkgs: []string{"firewalld", "firewalld-filesystem", "python3-firewall", "firewall-config", "firewall-applet"},
		units:      []string{"firewalld.service"},
		purgePaths: []string{"/etc/firewalld"},
		canEnable:  true,
		verifyCmd:  "command -v firewall-cmd >/dev/null 2>&1",
	},
	"ufw": {
		kind:       "ufw",
		dnfPkgs:    []string{"ufw"},
		aptPkgs:    []string{"ufw"},
		familyPkgs: []string{"ufw"},
		units:      []string{"ufw.service"},
		purgePaths: []string{"/etc/ufw"},
		canEnable:  true,
		verifyCmd:  "command -v ufw >/dev/null 2>&1",
	},
	"nftables": {
		kind:       "nftables",
		dnfPkgs:    []string{"nftables"},
		aptPkgs:    []string{"nftables"},
		familyPkgs: []string{"nftables", "python3-nftables"},
		units:      []string{"nftables.service"},
		purgePaths: []string{"/etc/nftables.conf"},
		canEnable:  false,
		verifyCmd:  "command -v nft >/dev/null 2>&1",
	},
	"iptables": {
		kind:       "iptables",
		dnfPkgs:    []string{"iptables-services"},
		aptPkgs:    []string{"iptables-persistent"},
		familyPkgs: []string{"iptables-services", "iptables-persistent", "netfilter-persistent"},
		units:      []string{"iptables.service", "ip6tables.service", "netfilter-persistent.service"},
		purgePaths: []string{"/etc/sysconfig/iptables", "/etc/sysconfig/ip6tables", "/etc/iptables"},
		canEnable:  false,
		verifyCmd:  "systemctl cat iptables.service >/dev/null 2>&1 || systemctl cat netfilter-persistent.service >/dev/null 2>&1",
	},
}

func (s fwSpec) packages(mgr string) []string {
	switch mgr {
	case "dnf", "yum":
		return append([]string(nil), s.dnfPkgs...)
	case "apt-get":
		return append([]string(nil), s.aptPkgs...)
	default:
		return nil
	}
}

func (s fwSpec) inFamily(pkg string) bool {
	p := strings.ToLower(strings.TrimSpace(pkg))
	if i := strings.IndexByte(p, ':'); i >= 0 { // apt 多架构后缀如 ufw:amd64
		p = p[:i]
	}
	for _, f := range s.familyPkgs {
		if p == f {
			return true
		}
	}
	return false
}

// criticalSystemPrefixes 卸载防火墙时绝不允许连带删除的关键系统包前缀。
var criticalSystemPrefixes = []string{
	"openssh", "systemd", "docker", "containerd", "glibc", "libc6", "kernel",
	"linux-image", "bash", "sudo", "networkmanager", "netplan", "iproute", "init",
}

func isCriticalPkg(pkg string) bool {
	p := strings.ToLower(strings.TrimSpace(pkg))
	if i := strings.IndexByte(p, ':'); i >= 0 {
		p = p[:i]
	}
	for _, pre := range criticalSystemPrefixes {
		if p == pre || strings.HasPrefix(p, pre+"-") {
			return true
		}
	}
	return false
}

// policyRCPath Debian/Ubuntu 装包期间临时阻止 postinst 自动启动服务的路径（测试可替换）。
var policyRCPath = "/usr/sbin/policy-rc.d"

const (
	fwManagedFile   = "firewall-managed.json"
	fwChallengeFile = "fw-uninstall-challenge.json"
	fwTokenFile     = "fw-uninstall-token.json"
	fwLockFile      = "firewall.lock"

	fwChallengeTTL = 3 * time.Minute
	fwTokenTTL     = 2 * time.Minute
)

// fwInventoryProbeCmd 一次探测包管理器、各防火墙命令、服务与包安装情况。
const fwInventoryProbeCmd = `PATH="$PATH:/usr/local/sbin:/usr/sbin:/sbin"
pkg=""
for m in dnf yum apt-get; do
  if command -v "$m" >/dev/null 2>&1; then pkg="$m"; break; fi
done
echo "pkg=$pkg"
command -v firewall-cmd >/dev/null 2>&1 && echo "has_firewalld=1"
command -v ufw >/dev/null 2>&1 && echo "has_ufw=1"
if systemctl is-active --quiet nftables 2>/dev/null; then echo "nft_svc=active"
elif systemctl is-enabled --quiet nftables 2>/dev/null; then echo "nft_svc=enabled"
elif systemctl cat nftables.service >/dev/null 2>&1; then echo "nft_svc=installed"
fi
if systemctl is-active --quiet iptables 2>/dev/null || systemctl is-active --quiet netfilter-persistent 2>/dev/null; then echo "ipt_svc=active"
elif systemctl is-enabled --quiet iptables 2>/dev/null || systemctl is-enabled --quiet netfilter-persistent 2>/dev/null; then echo "ipt_svc=enabled"
elif systemctl cat iptables.service >/dev/null 2>&1 || systemctl cat netfilter-persistent.service >/dev/null 2>&1; then echo "ipt_svc=installed"
fi
if [ "$pkg" = "apt-get" ]; then
  dpkg-query -W -f='${Status}\n' firewalld 2>/dev/null | grep -q 'install ok installed' && echo "pkg_firewalld=1"
  dpkg-query -W -f='${Status}\n' ufw 2>/dev/null | grep -q 'install ok installed' && echo "pkg_ufw=1"
  dpkg-query -W -f='${Status}\n' nftables 2>/dev/null | grep -q 'install ok installed' && echo "pkg_nftables=1"
  dpkg-query -W -f='${Status}\n' iptables-persistent 2>/dev/null | grep -q 'install ok installed' && echo "pkg_iptables=1"
elif [ -n "$pkg" ]; then
  rpm -q --quiet firewalld 2>/dev/null && echo "pkg_firewalld=1"
  rpm -q --quiet ufw 2>/dev/null && echo "pkg_ufw=1"
  rpm -q --quiet nftables 2>/dev/null && echo "pkg_nftables=1"
  rpm -q --quiet iptables-services 2>/dev/null && echo "pkg_iptables=1"
fi
true`

func parseKVOutput(out string) map[string]string {
	m := map[string]string{}
	for _, l := range splitLines(out) {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if ok && k != "" {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}

// applyFirewallInventory 补齐服务器包管理器与已安装防火墙清单（用于安装/卸载与冲突检测）。
func (sc *fwScan) applyFirewallInventory(st *fwState, kv map[string]string) {
	if st == nil {
		return
	}
	if kv == nil {
		kv = map[string]string{}
	}
	pkgMgr := kv["pkg"]
	st.PkgMgr = pkgMgr
	managed := readManagedFirewalls(sc.ferryDir)

	hasFwd := st.Kind == "firewalld" || kv["has_firewalld"] == "1" || kv["pkg_firewalld"] == "1"
	hasUfw := st.Kind == "ufw" || kv["has_ufw"] == "1" || kv["pkg_ufw"] == "1"
	nftSvc := kv["nft_svc"]
	iptSvc := kv["ipt_svc"]
	hasNft := st.Kind == "nftables" || nftSvc == "active" || nftSvc == "enabled" || managed["nftables"]
	hasIpt := st.Kind == "iptables" || iptSvc != "" || kv["pkg_iptables"] == "1" || managed["iptables"]

	var items []proto.FirewallItem
	canUninstall := func(pkgKey, manualMsg string) (bool, string) {
		if pkgMgr == "" {
			return false, "服务器上没有 dnf / yum / apt-get，无法通过 Ferry 自动卸载"
		}
		if kv[pkgKey] != "1" {
			return false, manualMsg
		}
		return true, ""
	}
	if hasFwd {
		ok, why := canUninstall("pkg_firewalld", "firewalld 不是通过系统软件包安装的，无法通过包管理器自动卸载")
		items = append(items, proto.FirewallItem{
			Kind:      "firewalld",
			Active:    st.Kind == "firewalld" && st.Active,
			Manage:    st.Kind == "firewalld" && st.Manage,
			Uninstall: ok,
			Why:       why,
		})
	}
	if hasUfw {
		ok, why := canUninstall("pkg_ufw", "ufw 不是通过系统软件包安装的，无法通过包管理器自动卸载")
		items = append(items, proto.FirewallItem{
			Kind:      "ufw",
			Active:    st.Kind == "ufw" && st.Active,
			Manage:    st.Kind == "ufw" && st.Manage,
			Uninstall: ok,
			Why:       why,
		})
	}
	if hasNft {
		ok, why := canUninstall("pkg_nftables", "nftables 不是通过系统软件包安装的，无法通过包管理器自动卸载")
		items = append(items, proto.FirewallItem{
			Kind:      "nftables",
			Active:    st.Kind == "nftables" && st.Active,
			Uninstall: ok,
			Why:       why,
		})
	}
	if hasIpt {
		ok, why := canUninstall("pkg_iptables", "检测到手动配置的 iptables 规则（未安装 iptables-services / iptables-persistent），请在终端直接清理规则")
		items = append(items, proto.FirewallItem{
			Kind:      "iptables",
			Active:    st.Kind == "iptables" && st.Active,
			Uninstall: ok,
			Why:       why,
		})
	}
	if len(items) > 0 {
		st.Installed = items
	}
	// 若主探测未识别出防火墙，但清单中发现了已安装的防火墙，补齐 Kind / Why / Enable，避免界面出现自相矛盾
	if st.Kind == "" {
		switch {
		case hasFwd:
			st.Kind = "firewalld"
			if kv["has_firewalld"] == "1" {
				st.Why = "firewalld 装了但没有运行：所有端口都不拦"
				st.Enable = true
			} else {
				st.Why = "检测到 firewalld 软件包已安装，但未找到 firewall-cmd 命令（可卸载后重新安装）"
			}
		case hasUfw:
			st.Kind = "ufw"
			if kv["has_ufw"] == "1" {
				st.Why = "ufw 装了但没有启用：所有端口都不拦"
				st.Enable = true
			} else {
				st.Why = "检测到 ufw 软件包已安装，但未找到 ufw 命令（可卸载后重新安装）"
			}
		case hasNft:
			st.Kind = "nftables"
			st.Why = "nftables 已安装但未配置拦截规则：所有端口都不拦（可通过终端编辑 /etc/nftables.conf 管理）"
		case hasIpt:
			st.Kind = "iptables"
			st.Why = "iptables 持久化服务已安装但未配置拦截规则：所有端口都不拦（可通过终端管理规则）"
		}
	}
}

// ---------------------------------------------------------------- Ferry 安装标记

func readManagedFirewalls(ferryDir string) map[string]bool {
	out := map[string]bool{}
	if ferryDir == "" {
		return out
	}
	b, err := os.ReadFile(filepath.Join(ferryDir, fwManagedFile))
	if err != nil {
		return out
	}
	var list []string
	if json.Unmarshal(b, &list) == nil {
		for _, k := range list {
			out[k] = true
		}
	}
	return out
}

func writeManagedFirewall(ferryDir, kind string, add bool) {
	if ferryDir == "" {
		return
	}
	cur := readManagedFirewalls(ferryDir)
	if add {
		cur[kind] = true
	} else {
		delete(cur, kind)
	}
	path := filepath.Join(ferryDir, fwManagedFile)
	if len(cur) == 0 {
		os.Remove(path)
		return
	}
	list := make([]string, 0, len(cur))
	for k := range cur {
		list = append(list, k)
	}
	sort.Strings(list)
	b, _ := json.Marshal(list)
	os.MkdirAll(ferryDir, 0o755)
	writeAtomicFile(path, b, 0o600)
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".fw-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// ---------------------------------------------------------------- 防火墙安装

func (a *Agent) fwLock() (func(), error) {
	dir := a.ferryDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return a.Sys.Lock(filepath.Join(dir, fwLockFile), 10*time.Second)
}

// fwinstall 安装指定类型的防火墙（firewalld | ufw | nftables | iptables），
// 可选安装后立即按防锁死流程打开（仅 firewalld / ufw）。
func (a *Agent) fwinstall() int {
	kind := strings.TrimSpace(a.req.FirewallKind)
	spec, ok := fwSpecs[kind]
	if !ok {
		return a.result(proto.CodeBadInput, fmt.Sprintf("不支持的防火墙类型：%q（支持 firewalld、ufw、nftables、iptables）", kind), nil)
	}
	if a.req.FirewallEnable && !spec.canEnable {
		return a.result(proto.CodeBadInput, fmt.Sprintf("%s 不支持由 Ferry 自动启用，请安装后在终端配置规则并启用", kind), nil)
	}

	unlock, err := a.fwLock()
	if err != nil {
		return a.result(proto.CodeLocked, "另一个防火墙操作正在进行，请稍后再试", nil)
	}
	defer unlock()

	sc := a.fwScanner()
	_, st := sc.report()

	mgr := st.PkgMgr
	if mgr == "" {
		mgr = a.pkgManager()
	}
	if mgr == "" {
		return a.result(proto.CodeFailed, "服务器上没有 dnf / yum / apt-get，无法自动安装防火墙", nil)
	}
	pkgs := spec.packages(mgr)
	if len(pkgs) == 0 {
		return a.result(proto.CodeFailed, fmt.Sprintf("当前软件包管理器（%s）不支持安装 %s", mgr, kind), nil)
	}

	// 检查是否已安装或与其他防火墙冲突
	for _, item := range st.Installed {
		if item.Kind == kind {
			return a.result(proto.CodeFailed, fmt.Sprintf("服务器上已经安装了 %s，无需重复安装", kind), nil)
		}
		// 另一个防火墙正在运行，或者已安装另一款同类防火墙，避免规则冲突
		if item.Active || item.Kind == "firewalld" || item.Kind == "ufw" {
			return a.result(proto.CodeFailed, fmt.Sprintf("服务器上已安装防火墙 %s，为避免防火墙规则冲突，请先卸载 %s 后再安装 %s", item.Kind, item.Kind, kind), nil)
		}
	}

	// 若选择了「安装后立即启用」，在动系统包之前先把端口参数、SSH 端口与 Docker 要求校验完
	if a.req.FirewallEnable {
		if _, _, _, code, msg := a.prepareEnablePorts(sc, kind, st.Docker); code != proto.CodeOK {
			return a.result(code, msg, nil)
		}
	}

	env := proxyEnvPrefix(&proto.SoftwarePlan{Proxy: a.req.FirewallProxy, NoProxy: a.req.FirewallNoProxy})
	via := "服务器自己的网络"
	if env != "" {
		via = "下载经这台电脑"
	}

	run := func(phase, cmd string, timeout time.Duration) (string, error) {
		return a.installShell("firewall-"+kind, phase, cmd, timeout)
	}

	// 1. 演练安装：检查软件源是否有包，以及是否会顺带改动已装的系统包
	a.installPhase("firewall-"+kind, fmt.Sprintf("检查软件源和依赖（%s，%s）", mgr, via))
	changes, err := pkgChanges(run, mgr, env, pkgs, nil, false)
	if err != nil {
		if kind == "ufw" && (mgr == "dnf" || mgr == "yum") && strings.Contains(err.Error(), "软件源里没有") {
			return a.result(proto.CodeFailed, fmt.Sprintf("%v（CentOS / RHEL / Rocky 等系统的默认软件源通常不包含 ufw，请先启用 EPEL 源，或改选 firewalld）", err), nil)
		}
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	if len(changes) > 0 && !a.req.FirewallAllowChg {
		pc := &pkgChangesError{pkgs: pkgs, changes: changes}
		return a.result(proto.CodeConfirm, pc.Error(), proto.SwChanges{Packages: pkgs, Changes: changes})
	}
	if len(changes) > 0 {
		a.warn(proto.EvWarn, "", "安装 %s 会顺带改动已装的系统包：%s（已同意）", strings.Join(pkgs, " "), strings.Join(changes, "、"))
	}

	// 2. 安装系统包（Debian/Ubuntu 下临时阻止 postinst 在未放行 SSH 端口前自动启动防火墙）
	a.installPhase("firewall-"+kind, fmt.Sprintf("下载并安装 %s（%s，%s）", kind, mgr, via))
	installErr := a.withNoAutoStart(mgr, func() error {
		if kind == "iptables" && mgr == "apt-get" {
			// iptables-persistent 在 dpkg 配置阶段有 debconf 交互，预置为不自动覆写当前规则
			a.Sys.Shell("if command -v debconf-set-selections >/dev/null 2>&1; then "+
				"printf 'iptables-persistent iptables-persistent/autosave_v4 boolean false\\niptables-persistent iptables-persistent/autosave_v6 boolean false\\n' | debconf-set-selections; fi", 15*time.Second)
		}
		cmd := env + mgr + " install -y " + packageArguments(pkgs, nil)
		if mgr == "apt-get" {
			cmd = env + "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-upgrade " + packageArguments(pkgs, nil)
		}
		out, err := a.installShell("firewall-"+kind, fmt.Sprintf("安装 %s", kind), cmd+" 2>&1", 20*time.Minute)
		if err != nil {
			lines := splitLines(out)
			if len(lines) > 8 {
				lines = lines[len(lines)-8:]
			}
			hint := "服务器连不上软件源时，安装时把下载方式选成「经这台电脑」"
			if env != "" {
				hint = "下载经这台电脑：这台电脑也要能访问这些软件源"
			}
			return fmt.Errorf("安装 %s 失败（%s）：%v %s", kind, hint, err, safePackageOutput(strings.Join(lines, " / ")))
		}
		return nil
	})
	if installErr != nil {
		a.fail(proto.EvFail, "", "%v", installErr)
		return a.result(proto.CodeFailed, installErr.Error(), nil)
	}

	// 3. 校验安装结果
	if spec.verifyCmd != "" {
		if _, err := a.Sys.Shell(spec.verifyCmd, 15*time.Second); err != nil {
			return a.result(proto.CodeFailed, fmt.Sprintf("%s 安装包已安装，但未找到对应的防火墙命令或系统服务", kind), nil)
		}
	}

	// 4. 若未选择立即启用，确保不会因为发行版默认 postinst 意外开启防火墙而阻断现有服务或 SSH
	if !a.req.FirewallEnable {
		a.ensureFirewallStopped(kind)
	}
	writeManagedFirewall(a.ferryDir(), kind, true)
	a.Sys.Shell(fmt.Sprintf("logger -t ferry -- %s 2>/dev/null", shellQuote(fmt.Sprintf("%s 安装了防火墙 %s", a.req.Actor, kind))), 10*time.Second)
	a.info(proto.EvFirewall, "", "已安装防火墙 %s", kind)

	// 5. 若选择安装后立即启用（仅 firewalld / ufw），按与 fwenable 相同的安全防锁死流程启动
	if a.req.FirewallEnable {
		_, st2 := sc.report()
		if st2.Active && st2.Kind == kind {
			a.info(proto.EvFirewall, "", "防火墙（%s）已经在运行", kind)
			return a.portDone(sc)
		}
		if st2.Kind != kind {
			st2.Kind = kind
			st2.Enable = true
		}
		return a.enableFirewall(sc, st2)
	}

	return a.portDone(sc)
}

func (a *Agent) ensureFirewallStopped(kind string) {
	switch kind {
	case "firewalld":
		a.Sys.Shell("systemctl disable --now firewalld >/dev/null 2>&1 || true", 30*time.Second)
	case "ufw":
		a.Sys.Shell("ufw --force disable >/dev/null 2>&1 || true; systemctl disable --now ufw >/dev/null 2>&1 || true", 30*time.Second)
	case "nftables":
		a.Sys.Shell("systemctl disable --now nftables >/dev/null 2>&1 || true", 30*time.Second)
	case "iptables":
		a.Sys.Shell("systemctl disable --now iptables ip6tables netfilter-persistent >/dev/null 2>&1 || true", 30*time.Second)
	}
}

// withNoAutoStart 在 Debian/Ubuntu 装包期间临时放置 policy-rc.d（返回 101），
// 避免 postinst 在放行 SSH 端口前擅自启动防火墙。
func (a *Agent) withNoAutoStart(mgr string, fn func() error) error {
	if mgr != "apt-get" || policyRCPath == "" {
		return fn()
	}
	if _, err := os.Stat(policyRCPath); err == nil {
		return fn()
	}
	if _, err := os.Stat(filepath.Dir(policyRCPath)); err != nil {
		return fn()
	}
	if err := os.WriteFile(policyRCPath, []byte("#!/bin/sh\nexit 101\n"), 0o755); err != nil {
		return fn()
	}
	defer os.Remove(policyRCPath)
	return fn()
}

// ---------------------------------------------------------------- 卸载认证挑战与一次性令牌

type fwChallengeRecord struct {
	ID        string    `json:"id"`
	CodeHash  string    `json:"codeHash"`
	Kind      string    `json:"kind"`
	Actor     string    `json:"actor"`
	ExpiresAt time.Time `json:"expiresAt"`
	Attempts  int       `json:"attempts"`
}

type fwTokenRecord struct {
	TokenHash string    `json:"tokenHash"`
	Kind      string    `json:"kind"`
	Actor     string    `json:"actor"`
	Method    string    `json:"method"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randomChallengeCode 生成 6 位不易混淆的大写字母数字验证码（去掉 0/O/1/I）。
func randomChallengeCode(n int) (string, error) {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out), nil
}

func (a *Agent) fwauthChallenge() int {
	kind := strings.TrimSpace(a.req.FirewallKind)
	if _, ok := fwSpecs[kind]; !ok {
		return a.result(proto.CodeBadInput, fmt.Sprintf("不支持的防火墙类型：%q", kind), nil)
	}
	id, err := randomHex(12)
	if err != nil {
		return a.result(proto.CodeFailed, "生成认证挑战失败："+err.Error(), nil)
	}
	code, err := randomChallengeCode(6)
	if err != nil {
		return a.result(proto.CodeFailed, "生成认证码失败："+err.Error(), nil)
	}
	exp := a.Sys.Now().Add(fwChallengeTTL)
	rec := fwChallengeRecord{
		ID:        id,
		CodeHash:  sha256Hex(code),
		Kind:      kind,
		Actor:     a.req.Actor,
		ExpiresAt: exp,
	}
	b, _ := json.Marshal(rec)
	if err := writeAtomicFile(filepath.Join(a.ferryDir(), fwChallengeFile), b, 0o600); err != nil {
		return a.result(proto.CodeFailed, "保存卸载认证挑战失败："+err.Error(), nil)
	}
	return a.result(proto.CodeOK, "", proto.FirewallChallenge{
		ID:        id,
		Code:      code,
		Kind:      kind,
		ExpiresAt: exp,
	})
}

func (a *Agent) fwauthIssue() int {
	kind := strings.TrimSpace(a.req.FirewallKind)
	if _, ok := fwSpecs[kind]; !ok {
		return a.result(proto.CodeBadInput, fmt.Sprintf("不支持的防火墙类型：%q", kind), nil)
	}
	method := strings.TrimSpace(a.req.FirewallAuthMethod)
	switch method {
	case "sudo-password", "ssh-password", "key-passphrase", "ssh-key-challenge":
	default:
		return a.result(proto.CodeBadInput, fmt.Sprintf("不支持的认证方式：%q", method), nil)
	}
	if method == "ssh-key-challenge" {
		if err := a.verifyAndConsumeChallenge(kind, a.req.FirewallChallengeID, a.req.FirewallChallengeCode); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
	}
	token, err := randomHex(32)
	if err != nil {
		return a.result(proto.CodeFailed, "生成卸载认证令牌失败："+err.Error(), nil)
	}
	exp := a.Sys.Now().Add(fwTokenTTL)
	rec := fwTokenRecord{
		TokenHash: sha256Hex(token),
		Kind:      kind,
		Actor:     a.req.Actor,
		Method:    method,
		ExpiresAt: exp,
	}
	b, _ := json.Marshal(rec)
	if err := writeAtomicFile(filepath.Join(a.ferryDir(), fwTokenFile), b, 0o600); err != nil {
		return a.result(proto.CodeFailed, "保存卸载认证令牌失败："+err.Error(), nil)
	}
	return a.result(proto.CodeOK, "", proto.FirewallTicket{
		Token:     token,
		Kind:      kind,
		Method:    method,
		ExpiresAt: exp,
	})
}

func (a *Agent) verifyAndConsumeChallenge(kind, id, code string) error {
	path := filepath.Join(a.ferryDir(), fwChallengeFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return errors.New("一次性认证码不存在或已失效，请重新获取认证码")
	}
	var rec fwChallengeRecord
	if json.Unmarshal(b, &rec) != nil {
		os.Remove(path)
		return errors.New("认证挑战数据已损坏，请重新获取认证码")
	}
	if a.Sys.Now().After(rec.ExpiresAt) {
		os.Remove(path)
		return errors.New("一次性认证码已过期，请重新获取")
	}
	if rec.ID != strings.TrimSpace(id) || rec.Kind != kind || rec.Actor != a.req.Actor {
		return errors.New("一次性认证码与当前卸载请求不匹配，请重新获取")
	}
	gotHash := sha256Hex(strings.ToUpper(strings.TrimSpace(code)))
	if subtle.ConstantTimeCompare([]byte(gotHash), []byte(rec.CodeHash)) != 1 {
		rec.Attempts++
		if rec.Attempts >= 3 {
			os.Remove(path)
			return errors.New("一次性认证码多次输入错误，已作废，请重新获取")
		}
		if nb, err := json.Marshal(rec); err == nil {
			writeAtomicFile(path, nb, 0o600)
		}
		return errors.New("一次性认证码不正确，请核对后重试")
	}
	os.Remove(path)
	return nil
}

func (a *Agent) validateUninstallToken(kind, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("卸载防火墙需要先通过认证（缺少卸载认证令牌）")
	}
	path := filepath.Join(a.ferryDir(), fwTokenFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return errors.New("卸载认证令牌无效或已使用，请重新认证")
	}
	var rec fwTokenRecord
	if json.Unmarshal(b, &rec) != nil {
		os.Remove(path)
		return errors.New("卸载认证令牌无效，请重新认证")
	}
	if a.Sys.Now().After(rec.ExpiresAt) {
		os.Remove(path)
		return errors.New("卸载认证已过期，请重新认证")
	}
	if rec.Kind != kind || rec.Actor != a.req.Actor {
		return errors.New("卸载认证与当前目标防火墙或操作人不匹配，请重新认证")
	}
	gotHash := sha256Hex(token)
	if subtle.ConstantTimeCompare([]byte(gotHash), []byte(rec.TokenHash)) != 1 {
		return errors.New("卸载认证令牌校验失败，请重新认证")
	}
	return nil
}

func (a *Agent) consumeUninstallToken() {
	os.Remove(filepath.Join(a.ferryDir(), fwTokenFile))
}

// ---------------------------------------------------------------- 防火墙卸载

func (a *Agent) fwuninstall() int {
	kind := strings.TrimSpace(a.req.FirewallKind)
	spec, ok := fwSpecs[kind]
	if !ok {
		return a.result(proto.CodeBadInput, fmt.Sprintf("不支持的防火墙类型：%q（支持 firewalld、ufw、nftables、iptables）", kind), nil)
	}

	unlock, err := a.fwLock()
	if err != nil {
		return a.result(proto.CodeLocked, "另一个防火墙操作正在进行，请稍后再试", nil)
	}
	defer unlock()

	// 1. 强校验服务端一次性卸载令牌（未通过实时认证前拒绝一切操作）
	if err := a.validateUninstallToken(kind, a.req.FirewallToken); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}

	sc := a.fwScanner()
	_, st := sc.report()

	mgr := st.PkgMgr
	if mgr == "" {
		mgr = a.pkgManager()
	}
	if mgr == "" {
		return a.result(proto.CodeFailed, "服务器上没有 dnf / yum / apt-get，无法自动卸载防火墙", nil)
	}

	var found *proto.FirewallItem
	for i := range st.Installed {
		if st.Installed[i].Kind == kind {
			found = &st.Installed[i]
			break
		}
	}
	if found == nil {
		return a.result(proto.CodeFailed, fmt.Sprintf("服务器上没有安装 %s", kind), nil)
	}
	if !found.Uninstall {
		why := found.Why
		if why == "" {
			why = fmt.Sprintf("%s 无法通过包管理器自动卸载", kind)
		}
		return a.result(proto.CodeFailed, why, nil)
	}

	// Docker 正在运行且卸载运行中的 firewalld 时要求确认重启 Docker
	docker := kind == "firewalld" && found.Active && st.Docker
	if docker && !a.req.RestartDocker {
		return a.result(proto.CodeFailed, "Docker 正在运行：卸载运行中的 firewalld 可能清掉 Docker 的网络规则，容器的端口映射会失效，要同时重启 Docker（容器会重启一次）", nil)
	}

	pkgs := spec.packages(mgr)
	var installedPkgs []string
	for _, p := range pkgs {
		if a.pkgInstalled(mgr, p) {
			installedPkgs = append(installedPkgs, p)
		}
	}
	if len(installedPkgs) == 0 {
		return a.result(proto.CodeFailed, fmt.Sprintf("包管理器（%s）中未找到已安装的 %s 软件包", mgr, strings.Join(pkgs, " ")), nil)
	}

	// 2. 卸载前演练：防止连带删除其他依赖服务或关键系统包
	run := func(phase, cmd string, timeout time.Duration) (string, error) {
		return a.installShell("firewall-remove-"+kind, phase, cmd, timeout)
	}
	a.installPhase("firewall-remove-"+kind, fmt.Sprintf("检查卸载 %s 的包依赖（%s）", kind, mgr))
	if err := pkgRemoveCheck(run, mgr, spec, installedPkgs); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}

	// 3. 所有前置检查通过后立即消费令牌（单次有效，防止重放）
	a.consumeUninstallToken()

	// 4. 先停止并禁用防火墙服务，同时清理残留的自动恢复定时器
	a.info(proto.EvFirewall, "", "停止并禁用防火墙服务（%s）", kind)
	if kind == "firewalld" || kind == "ufw" {
		a.Sys.Shell("systemctl stop "+revertUnit+".timer "+revertUnit+".service 2>/dev/null; systemctl reset-failed "+revertUnit+".service 2>/dev/null", 15*time.Second)
	}
	a.ensureFirewallStopped(kind)

	// 5. 调用系统包管理器卸载软件包
	a.installPhase("firewall-remove-"+kind, fmt.Sprintf("卸载 %s 软件包（%s）", kind, mgr))
	var cmd string
	if mgr == "apt-get" {
		action := "remove"
		if a.req.Purge {
			action = "purge"
		}
		cmd = "DEBIAN_FRONTEND=noninteractive apt-get " + action + " -y " + packageArguments(installedPkgs, nil)
	} else {
		cmd = mgr + " remove -y " + packageArguments(installedPkgs, nil)
	}
	out, err := a.installShell("firewall-remove-"+kind, fmt.Sprintf("卸载 %s", kind), cmd+" 2>&1", 15*time.Minute)
	if err != nil {
		lines := splitLines(out)
		if len(lines) > 8 {
			lines = lines[len(lines)-8:]
		}
		msg := fmt.Sprintf("卸载 %s 失败：%v %s", kind, err, safePackageOutput(strings.Join(lines, " / ")))
		a.fail(proto.EvFail, "", "%s", msg)
		return a.result(proto.CodeFailed, msg, nil)
	}

	// 6. 可选清除残留配置文件
	if a.req.Purge {
		for _, p := range spec.purgePaths {
			if strings.HasPrefix(p, "/etc/") && len(p) > len("/etc/") {
				os.RemoveAll(p)
			}
		}
		a.info(proto.EvFirewall, "", "已清除 %s 的配置文件", kind)
	}

	// 7. 如有需要重启 Docker 恢复容器网络规则
	if docker {
		a.info(proto.EvFirewall, "", "重启 Docker，让容器的端口映射重新生效")
		if out, err := a.Sys.Shell("systemctl restart docker 2>&1", 2*time.Minute); err != nil {
			a.warn(proto.EvWarn, "", "重启 Docker 失败：%s", clipLine(strings.TrimSpace(out)))
		}
	}

	writeManagedFirewall(a.ferryDir(), kind, false)
	a.Sys.Shell("systemctl daemon-reload >/dev/null 2>&1 || true", 15*time.Second)
	a.Sys.Shell(fmt.Sprintf("logger -t ferry -- %s 2>/dev/null", shellQuote(fmt.Sprintf("%s 卸载了防火墙 %s（purge=%v）", a.req.Actor, kind, a.req.Purge))), 10*time.Second)
	a.info(proto.EvFirewall, "", "已卸载防火墙 %s", kind)

	return a.portDone(sc)
}

// pkgRemoveCheck 卸载演练：确保卸载防火墙不会连带删除依赖它的其他服务或关键系统包。
func pkgRemoveCheck(run packageShell, mgr string, spec fwSpec, pkgs []string) error {
	list := packageArguments(pkgs, nil)
	var cmd string
	if mgr == "apt-get" {
		cmd = "LC_ALL=C DEBIAN_FRONTEND=noninteractive apt-get -s remove " + list + " 2>&1"
	} else {
		cmd = "LC_ALL=C " + mgr + " remove --assumeno " + list + " 2>&1"
	}
	out, err := run("检查卸载依赖", cmd, 5*time.Minute)
	refused := strings.Contains(out, "Operation aborted") || strings.Contains(out, "Exiting on user command")
	if err != nil && !((mgr == "dnf" || mgr == "yum") && strings.Contains(out, "Transaction Summary") && refused) {
		lines := splitLines(out)
		if len(lines) > 6 {
			lines = lines[len(lines)-6:]
		}
		return fmt.Errorf("检查卸载 %s 的依赖失败：%v %s", spec.kind, err, safePackageOutput(strings.Join(lines, " / ")))
	}
	deps, critical := parsePkgRemoveImpact(mgr, out, spec)
	if len(critical) > 0 {
		return fmt.Errorf("卸载 %s 会影响关键系统软件包（%s），为保护服务器安全已中止卸载", spec.kind, strings.Join(critical, "、"))
	}
	if len(deps) > 0 {
		return fmt.Errorf("卸载 %s 会连带移除依赖它的软件包：%s。为避免误删其他服务，已中止自动卸载，请在终端确认后手动处理", spec.kind, strings.Join(deps, "、"))
	}
	return nil
}

// parsePkgRemoveImpact 从 dnf/yum remove --assumeno 或 apt-get -s remove 输出中识别非防火墙自身的受影响包。
func parsePkgRemoveImpact(mgr, out string, spec fwSpec) (dependent, critical []string) {
	depSeen := map[string]bool{}
	critSeen := map[string]bool{}
	addDep := func(name string) {
		if name == "" || spec.inFamily(name) || depSeen[name] {
			return
		}
		depSeen[name] = true
		dependent = append(dependent, name)
	}
	addCrit := func(name string) {
		if name == "" || critSeen[name] {
			return
		}
		critSeen[name] = true
		critical = append(critical, name)
	}

	if mgr == "apt-get" {
		for _, line := range splitLines(out) {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == "Remv" {
				name := f[1]
				if isCriticalPkg(name) {
					addCrit(name)
				} else {
					addDep(name)
				}
			}
		}
		return dependent, critical
	}

	section := ""
	wrapped := false
	for _, raw := range strings.Split(out, "\n") {
		t := strings.TrimSpace(raw)
		if t == "" {
			section, wrapped = "", false
			continue
		}
		if strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t") {
			if section == "" {
				continue
			}
			f := strings.Fields(t)
			if len(f) == 0 {
				continue
			}
			if wrapped {
				wrapped = false
				continue
			}
			name := f[0]
			if len(f) == 1 {
				wrapped = true
			}
			if isCriticalPkg(name) {
				addCrit(name)
			} else if section == "dependent" || section == "removing" {
				addDep(name)
			}
			continue
		}
		wrapped = false
		switch {
		case strings.HasPrefix(t, "Removing dependent packages:"):
			section = "dependent"
		case strings.HasPrefix(t, "Removing unused dependencies:"):
			section = "unused"
		case strings.HasPrefix(t, "Removing:"):
			section = "removing"
		default:
			section = ""
		}
	}
	return dependent, critical
}
