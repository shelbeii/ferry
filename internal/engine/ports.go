package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/safe"
	"golang.org/x/crypto/ssh"
)

// Ports 服务器上的端口与防火墙（只读查看）。
func (e *Engine) Ports(target string) (*proto.PortReport, error) {
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		if err := s.callData("ports", &proto.Request{KeepPorts: keepPorts(s)}, &out); err != nil {
			return err
		}
		out.Firewall.UninstallAuth = e.firewallUninstallAuth(s.t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PortSet 用服务器的防火墙（firewalld / ufw）放行（open）或关闭（close）一个端口，返回改完的情况。
func (e *Engine) PortSet(target string, port int, pr, action string) (*proto.PortReport, error) {
	if action != "open" && action != "close" {
		return nil, fmt.Errorf("未知操作：%s（open 或 close）", action)
	}
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		if s.t.NeedConfirm() && !e.Yes {
			q := fmt.Sprintf("在 %s 的防火墙上放行端口 %d/%s？之后外部都能连这个端口", s.t.Name, port, pr)
			if action == "close" {
				q = fmt.Sprintf("在 %s 的防火墙上关闭端口 %d/%s？之后外部连不上这个端口（服务器本机不受影响）", s.t.Name, port, pr)
			}
			if !e.Rep.Confirm(q) {
				return ErrCanceled
			}
		}
		if err := s.callData("portset", &proto.Request{Port: port, PortProto: pr, Action: action, KeepPorts: keepPorts(s)}, &out); err != nil {
			return err
		}
		out.Firewall.UninstallAuth = e.firewallUninstallAuth(s.t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// keepPorts Ferry 连这台服务器用的端口：不许关闭。
func keepPorts(s *session) []int {
	if s.t.Port > 0 {
		return []int{s.t.Port}
	}
	return []int{22}
}

// PortProbe 从本机连服务器的一个端口的结果。
type PortProbe struct {
	Port int   `json:"port"`
	OK   bool  `json:"ok"`
	Ms   int64 `json:"ms"`
	// Why 连不上的原因：refused（服务器回绝：没有程序在监听，或防火墙拒绝）| timeout（没有回应：多半被防火墙、安全组丢弃）| error
	Why string `json:"why,omitempty"`
	Msg string `json:"msg,omitempty"`
}

// PortCheck 从本机（运行 Ferry 的电脑）连服务器的这些 TCP 端口，看能不能连上（不连 SSH，不改动服务器）。
func (e *Engine) PortCheck(target string, ports []int, timeout time.Duration) ([]PortProbe, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	if len(ports) == 0 {
		return nil, errors.New("要指定端口")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	out := make([]PortProbe, len(ports))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, p := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Recover("测端口", nil)
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = probePort(t.Host, p, timeout)
		}()
	}
	wg.Wait()
	return out, nil
}

func probePort(host string, port int, timeout time.Duration) PortProbe {
	r := PortProbe{Port: port}
	start := time.Now()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	r.Ms = time.Since(start).Milliseconds()
	if err == nil {
		c.Close()
		r.OK = true
		return r
	}
	r.Msg = err.Error()
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(r.Msg, "refused"):
		r.Why = "refused"
	case errors.As(err, &ne) && ne.Timeout():
		r.Why = "timeout"
	default:
		r.Why = "error"
	}
	return r
}

// FirewallEnable 打开服务器上装了没开的防火墙（firewalld / ufw）：agent 先放行 SSH 端口和 allow 里的端口、安排自动恢复，
// 再打开；这里用新的连接试 SSH 端口，连得上才取消自动恢复（连不上时防火墙过几分钟自己关掉，恢复原样）。
func (e *Engine) FirewallEnable(target string, allow []string, restartDocker bool) (*proto.PortReport, error) {
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		if s.t.NeedConfirm() && !e.Yes {
			q := fmt.Sprintf("打开 %s 的防火墙？之后只有 SSH 端口和 %s 能从外部连", s.t.Name, strings.Join(allow, "、"))
			if len(allow) == 0 {
				q = fmt.Sprintf("打开 %s 的防火墙？之后外部只能连 SSH 端口", s.t.Name)
			}
			if !e.Rep.Confirm(q) {
				return ErrCanceled
			}
		}
		if err := s.callData("fwenable", &proto.Request{Allow: allow, RestartDocker: restartDocker, KeepPorts: keepPorts(s)}, &out); err != nil {
			return err
		}
		if err := e.verifyAndConfirmFirewallEnable(s, &out); err != nil {
			return err
		}
		out.Firewall.UninstallAuth = e.firewallUninstallAuth(s.t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// verifyAndConfirmFirewallEnable 用新的 TCP 连接确认 SSH 端口可达，连得上才取消 3 分钟自动恢复定时器。
func (e *Engine) verifyAndConfirmFirewallEnable(s *session, out *proto.PortReport) error {
	port := s.t.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(s.t.Host, strconv.Itoa(port))
	e.Rep.Info("用新的连接试 %s 能不能连上", addr)
	var last error
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err == nil {
			c.Close()
			last = nil
			break
		}
		last = err
		time.Sleep(2 * time.Second)
	}
	if last != nil {
		return fmt.Errorf("打开防火墙后，新的连接连不上 %s（%v）：几分钟内防火墙会自动关掉，恢复原样", addr, last)
	}
	return s.callData("fwconfirm", &proto.Request{KeepPorts: keepPorts(s)}, out)
}

// FirewallDisable 关掉服务器上正在运行的防火墙（firewalld / ufw），开机也不再启动；放行规则留着，再打开时照旧。
// restartDocker：Docker 在运行、关的是 firewalld 时要同时重启 Docker。
func (e *Engine) FirewallDisable(target string, restartDocker bool) (*proto.PortReport, error) {
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		if s.t.NeedConfirm() && !e.Yes {
			q := fmt.Sprintf("关闭 %s 的防火墙？之后所有端口都不拦，服务器上对外监听的端口外部都能连", s.t.Name)
			if !e.Rep.Confirm(q) {
				return ErrCanceled
			}
		}
		if err := s.callData("fwdisable", &proto.Request{RestartDocker: restartDocker, KeepPorts: keepPorts(s)}, &out); err != nil {
			return err
		}
		out.Firewall.UninstallAuth = e.firewallUninstallAuth(s.t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------- 防火墙安装、认证与卸载

// FirewallInstallOptions 安装防火墙的参数。
type FirewallInstallOptions struct {
	Kind          string   // firewalld | ufw | nftables | iptables
	Enable        bool     // 安装后立即打开（仅 firewalld / ufw）
	Allow         []string // 立即打开时先放行的端口
	RestartDocker bool     // 打开 firewalld 且 Docker 运行中时重启 Docker
	Network       string   // auto | local | server
	AllowChanges  bool     // 允许包管理器顺带升级/改动已装系统包
}

// FirewallAuthOptions 卸载防火墙前的身份认证参数（凭据只从本次输入获取，不复用已保存密码）。
type FirewallAuthOptions struct {
	Kind        string // firewalld | ufw | nftables | iptables
	Method      string // 可选：仅在免密密钥目标上允许显式切换为 ssh-password
	Secret      string // sudo 密码 / SSH 登录密码 / 私钥密码 / 一次性认证码
	ChallengeID string // ssh-key-challenge 对应的挑战 ID
}

// FirewallUninstallOptions 卸载防火墙的参数。
type FirewallUninstallOptions struct {
	Kind          string // firewalld | ufw | nftables | iptables
	Purge         bool   // 同时清除防火墙配置文件
	RestartDocker bool   // 卸载运行中的 firewalld 且 Docker 运行中时重启 Docker
	Token         string // FirewallUninstallAuth 签发的一次性卸载令牌
}

// FirewallAuthError 卸载防火墙时的身份认证失败或缺少认证。
type FirewallAuthError struct {
	Method string
	Msg    string
}

func (e *FirewallAuthError) Error() string { return e.Msg }

// JSONDetail --json 输出的结构化错误，桌面版据此在认证弹窗内提示并允许重试。
func (e *FirewallAuthError) JSONDetail() map[string]any {
	return map[string]any{"kind": "fw-auth-failed", "method": e.Method}
}

func validFirewallKind(kind string) bool {
	switch kind {
	case "firewalld", "ufw", "nftables", "iptables":
		return true
	}
	return false
}

// FirewallUninstallAuthInfo 返回目标卸载防火墙时所需的认证方式（不建立连接即可计算）。
func (e *Engine) FirewallUninstallAuthInfo(target string) (*proto.FirewallAuth, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	return e.firewallUninstallAuth(t), nil
}

func (e *Engine) firewallUninstallAuth(t *config.Target) *proto.FirewallAuth {
	return firewallUninstallAuthForTarget(t, e.encryptedIdentityPath(t))
}

func firewallUninstallAuthForTarget(t *config.Target, encryptedKeyPath string) *proto.FirewallAuth {
	port := t.Port
	if port == 0 {
		port = 22
	}
	base := proto.FirewallAuth{
		User:       t.User,
		Host:       t.Host,
		Port:       port,
		Production: t.Production,
	}
	switch {
	case t.User != "root" && t.SudoAuth == "password":
		base.Method = "sudo-password"
		base.Prompt = fmt.Sprintf("请输入 %s@%s:%d 的 sudo 密码以验证身份", t.User, t.Host, port)
	case t.Auth == "password":
		base.Method = "ssh-password"
		base.Prompt = fmt.Sprintf("请输入 %s@%s:%d 的 SSH 登录密码以验证身份", t.User, t.Host, port)
	case encryptedKeyPath != "":
		base.Method = "key-passphrase"
		base.Prompt = fmt.Sprintf("请输入 SSH 私钥 %s 的密码以验证身份", encryptedKeyPath)
	default:
		base.Method = "ssh-key-challenge"
		base.Prompt = fmt.Sprintf("请输入服务端签发的一次性认证码，并实时校验 %s@%s:%d 的 SSH 密钥身份", t.User, t.Host, port)
		base.CanPasswordFallback = true
	}
	return &base
}

func (e *Engine) encryptedIdentityPath(t *config.Target) string {
	if t.Auth == "password" {
		return ""
	}
	if t.IdentityFile != "" {
		p := identity(e.File, t)
		if p != "" && remote.KeyEncrypted(p) {
			return p
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", n)
		if remote.KeyEncrypted(p) {
			return p
		}
	}
	return ""
}

// resolveFirewallAuthMethod 决定本次认证实际采用的方法（严格禁止从密码/私钥密码认证降级为挑战码）。
func resolveFirewallAuthMethod(required *proto.FirewallAuth, requested string) (string, error) {
	req := strings.TrimSpace(requested)
	if req == "" || req == required.Method {
		return required.Method, nil
	}
	if req == "ssh-password" && required.CanPasswordFallback {
		return "ssh-password", nil
	}
	return "", &FirewallAuthError{
		Method: required.Method,
		Msg:    fmt.Sprintf("此服务器卸载防火墙要求使用 %s 认证，不允许切换为 %s", required.Method, req),
	}
}

// FirewallInstall 在服务器上安装指定类型的防火墙；可选安装后立即安全打开（仅 firewalld / ufw）。
func (e *Engine) FirewallInstall(target string, o FirewallInstallOptions) (*proto.PortReport, error) {
	kind := strings.TrimSpace(o.Kind)
	if !validFirewallKind(kind) {
		return nil, fmt.Errorf("不支持的防火墙类型：%q（支持 firewalld、ufw、nftables、iptables）", kind)
	}
	if o.Enable && kind != "firewalld" && kind != "ufw" {
		return nil, fmt.Errorf("%s 不支持由 Ferry 自动启用，请仅安装后在终端配置规则", kind)
	}
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		if s.t.NeedConfirm() && !e.Yes {
			q := fmt.Sprintf("在 %s 上安装防火墙 %s？", s.t.Name, kind)
			if o.Enable {
				q = fmt.Sprintf("在 %s 上安装并打开防火墙 %s？打开后只放行 SSH 和指定端口", s.t.Name, kind)
			}
			if !e.Rep.Confirm(q) {
				return ErrCanceled
			}
		}
		proxy, proxyURL, noProxy, err := e.packageNetwork(s, o.Network)
		if err != nil {
			return err
		}
		closedProxy := false
		closeProxy := func() {
			if proxy != nil && !closedProxy {
				closedProxy = true
				e.closeNetProxy(proxy)
			}
		}
		defer closeProxy()

		e.Rep.Step("在 %s 上安装防火墙 %s", s.t.Name, kind)
		r, err := s.call("fwinstall", &proto.Request{
			FirewallKind:     kind,
			FirewallEnable:   o.Enable,
			FirewallProxy:    proxyURL,
			FirewallNoProxy:  noProxy,
			FirewallAllowChg: o.AllowChanges,
			Allow:            o.Allow,
			RestartDocker:    o.RestartDocker,
			KeepPorts:        keepPorts(s),
		})
		closeProxy()
		if err != nil {
			return err
		}
		switch r.RC {
		case proto.CodeOK:
			if len(r.Data) > 0 {
				if err := json.Unmarshal(r.Data, &out); err != nil {
					return err
				}
			}
		case proto.CodeConfirm:
			var c proto.SwChanges
			json.Unmarshal(r.Data, &c)
			return &PkgChangesError{Target: s.t.Name, Packages: c.Packages, Changes: c.Changes, Msg: r.Msg}
		default:
			return &AgentError{Cmd: "fwinstall", RC: r.RC, Msg: r.Msg}
		}
		if o.Enable && (kind == "firewalld" || kind == "ufw") {
			if err := e.verifyAndConfirmFirewallEnable(s, &out); err != nil {
				return err
			}
		}
		out.Firewall.UninstallAuth = e.firewallUninstallAuth(s.t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FirewallUninstallChallenge 向服务器申请免密密钥卸载认证用的一次性挑战码（3 分钟有效）。
func (e *Engine) FirewallUninstallChallenge(target, kind string) (*proto.FirewallChallenge, error) {
	kind = strings.TrimSpace(kind)
	if !validFirewallKind(kind) {
		return nil, fmt.Errorf("不支持的防火墙类型：%q（支持 firewalld、ufw、nftables、iptables）", kind)
	}
	var out proto.FirewallChallenge
	err := e.withTarget(target, func(s *session) error {
		return s.callData("fwauth-challenge", &proto.Request{FirewallKind: kind}, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FirewallUninstallAuth 对卸载防火墙执行实时身份认证，通过后由服务端签发一次性卸载令牌（2 分钟有效）。
// 全程不使用已保存的系统凭据库密码或会话缓存密码，确保必须由当前操作人实时完成认证。
func (e *Engine) FirewallUninstallAuth(target string, o FirewallAuthOptions) (*proto.FirewallTicket, error) {
	kind := strings.TrimSpace(o.Kind)
	if !validFirewallKind(kind) {
		return nil, fmt.Errorf("不支持的防火墙类型：%q（支持 firewalld、ufw、nftables、iptables）", kind)
	}
	var ticket proto.FirewallTicket
	err := e.withTarget(target, func(s *session) error {
		required := e.firewallUninstallAuth(s.t)
		method, err := resolveFirewallAuthMethod(required, o.Method)
		if err != nil {
			return err
		}
		if o.Secret == "" || strings.ContainsAny(o.Secret, "\r\n\x00") {
			return &FirewallAuthError{Method: method, Msg: "认证凭据不能为空，也不能包含换行或非法字符"}
		}

		switch method {
		case "sudo-password":
			used := false
			res, err := s.c.RunSudo(e.ctx(), "true", nil, func() (string, error) {
				used = true
				return o.Secret, nil
			}, nil)
			if err != nil {
				if errors.Is(err, remote.ErrSudoRejected) {
					return &FirewallAuthError{Method: method, Msg: "sudo 密码不正确，认证未通过"}
				}
				return &FirewallAuthError{Method: method, Msg: "sudo 密码认证失败：" + err.Error()}
			}
			if res.Code != 0 {
				return &FirewallAuthError{Method: method, Msg: "sudo 密码认证失败：" + strings.TrimSpace(res.Stderr)}
			}
			if !used {
				// 服务器实际允许免密 sudo，未校验密码：绝不能放行任意字符串，改为实时校验 SSH 密码
				if err := e.verifySSHPassword(s.t, s.c.HostFingerprint, o.Secret); err != nil {
					return &FirewallAuthError{Method: method, Msg: "服务器当前未要求 sudo 密码，且 SSH 密码校验未通过，请核对密码或调整目标的 sudo 认证设置"}
				}
			}

		case "ssh-password":
			if err := e.verifySSHPassword(s.t, s.c.HostFingerprint, o.Secret); err != nil {
				return &FirewallAuthError{Method: method, Msg: "SSH 登录密码不正确，认证未通过"}
			}

		case "key-passphrase":
			keyPath := e.encryptedIdentityPath(s.t)
			if keyPath == "" {
				return &FirewallAuthError{Method: method, Msg: "未找到需要密码的 SSH 私钥文件，无法完成私钥密码认证"}
			}
			if err := e.verifyKeyPassphrase(s.t, s.c.HostFingerprint, keyPath, o.Secret); err != nil {
				return &FirewallAuthError{Method: method, Msg: err.Error()}
			}

		case "ssh-key-challenge":
			if strings.TrimSpace(o.ChallengeID) == "" {
				return &FirewallAuthError{Method: method, Msg: "缺少一次性认证挑战 ID，请重新获取认证码"}
			}
			// 先实时重连一次 SSH，确保当前本机 SSH 密钥/ssh-agent 仍有效且被服务器授权
			if err := e.verifySSHKeyLive(s.t, s.c.HostFingerprint); err != nil {
				return &FirewallAuthError{Method: method, Msg: "实时校验 SSH 密钥身份失败：" + err.Error()}
			}
		}

		req := &proto.Request{
			FirewallKind:       kind,
			FirewallAuthMethod: method,
		}
		if method == "ssh-key-challenge" {
			req.FirewallChallengeID = strings.TrimSpace(o.ChallengeID)
			req.FirewallChallengeCode = strings.TrimSpace(o.Secret)
		}
		if err := s.callData("fwauth-issue", req, &ticket); err != nil {
			var ae *AgentError
			if errors.As(err, &ae) {
				return &FirewallAuthError{Method: method, Msg: ae.Msg}
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ticket, nil
}

// verifySSHPassword 用用户刚刚输入的密码建立全新 SSH 认证，绝不读取系统凭据库或会话缓存。
func (e *Engine) verifySSHPassword(t *config.Target, wantFP, password string) error {
	c, err := remote.Dial(remote.Options{
		Host:    t.Host,
		Port:    t.Port,
		User:    t.User,
		HostKey: t.HostKey,
		Auth:    "password",
		Timeout: e.Settings.Timeout(),
		Password: func() (string, error) {
			return password, nil
		},
	})
	if err != nil {
		return err
	}
	defer c.Close()
	if wantFP != "" && c.HostFingerprint != "" && c.HostFingerprint != wantFP {
		return errors.New("服务器主机公钥指纹与当前会话不一致，已中止认证")
	}
	return nil
}

// verifyKeyPassphrase 解密私钥并用该私钥实时重连服务器，确认口令正确且密钥仍获授权。
func (e *Engine) verifyKeyPassphrase(t *config.Target, wantFP, keyPath, passphrase string) error {
	pem, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("读取私钥 %s 失败：%v", keyPath, err)
	}
	if _, err := ssh.ParsePrivateKeyWithPassphrase(pem, []byte(passphrase)); err != nil {
		return errors.New("SSH 私钥密码不正确，认证未通过")
	}
	c, err := remote.Dial(remote.Options{
		Host:         t.Host,
		Port:         t.Port,
		User:         t.User,
		HostKey:      t.HostKey,
		Auth:         "key",
		KeyData:      pem,
		KeyName:      keyPath,
		OnlyIdentity: true,
		Timeout:      e.Settings.Timeout(),
		Passphrase: func(string) (string, error) {
			return passphrase, nil
		},
	})
	if err != nil {
		return fmt.Errorf("使用该私钥实时认证服务器失败：%v", err)
	}
	defer c.Close()
	if wantFP != "" && c.HostFingerprint != "" && c.HostFingerprint != wantFP {
		return errors.New("服务器主机公钥指纹与当前会话不一致，已中止认证")
	}
	return nil
}

// verifySSHKeyLive 使用目标配置的私钥 / ssh-agent 建立全新 SSH 握手，验证当前仍具备实时登录权限。
func (e *Engine) verifySSHKeyLive(t *config.Target, wantFP string) error {
	cred := e.credentials(t)
	opts := e.dialOptions(t, cred)
	opts.Auth = "key"
	c, err := remote.Dial(opts)
	if err != nil {
		return err
	}
	defer c.Close()
	if wantFP != "" && c.HostFingerprint != "" && c.HostFingerprint != wantFP {
		return errors.New("服务器主机公钥指纹与当前会话不一致，已中止认证")
	}
	return nil
}

// FirewallUninstall 使用认证后签发的一次性令牌卸载指定类型的防火墙。
func (e *Engine) FirewallUninstall(target string, o FirewallUninstallOptions) (*proto.PortReport, error) {
	kind := strings.TrimSpace(o.Kind)
	if !validFirewallKind(kind) {
		return nil, fmt.Errorf("不支持的防火墙类型：%q（支持 firewalld、ufw、nftables、iptables）", kind)
	}
	token := strings.TrimSpace(o.Token)
	if token == "" {
		return nil, &FirewallAuthError{Msg: "卸载防火墙前必须先完成身份认证"}
	}
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		e.Rep.Step("在 %s 上卸载防火墙 %s", s.t.Name, kind)
		if err := s.callData("fwuninstall", &proto.Request{
			FirewallKind:  kind,
			Purge:         o.Purge,
			RestartDocker: o.RestartDocker,
			FirewallToken: token,
			KeepPorts:     keepPorts(s),
		}, &out); err != nil {
			var ae *AgentError
			if errors.As(err, &ae) && strings.Contains(ae.Msg, "认证") {
				return &FirewallAuthError{Msg: ae.Msg}
			}
			return err
		}
		out.Firewall.UninstallAuth = e.firewallUninstallAuth(s.t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
