package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/remote"
)

// KeyGenResult ferry keygen 的结果。
type KeyGenResult struct {
	Target        string `json:"target"`
	KeyPath       string `json:"keyPath"`
	Generated     bool   `json:"generated"`     // 本次新生成（否则复用已有私钥）
	Installed     bool   `json:"installed"`     // 本次新加到 authorized_keys（否则已经有了）
	Fingerprint   string `json:"fingerprint"`   // 公钥指纹
	ConfigUpdated bool   `json:"configUpdated"` // 已把 identityFile 写回 ferry.yaml
	LocalUpdated  bool   `json:"localUpdated"`  // 已记到本机设置（只对这台电脑）
	ConfigValue   string `json:"configValue"`   // 写进配置的 identityFile 值
}

// 配置好免密登录后把私钥记到哪里（KeyGen 的 write）。
const (
	KeyToNone  = ""      // 不记，只告诉用户怎么写
	KeyToYAML  = "yaml"  // 写回 ferry.yaml（所有电脑共用）
	KeyToLocal = "local" // 本机设置（只对这台电脑）
)

// DefaultKeyPath 默认私钥位置：<密钥目录>/<项目>-<目标>_ed25519（密钥目录见设置，默认数据目录/keys）
func (e *Engine) DefaultKeyPath(t *config.Target) string {
	return filepath.Join(e.Settings.KeyDir(), e.File.Project+"-"+t.Name+"_ed25519")
}

// KeyGen 生成 ed25519 密钥对（已存在则复用），用密码登录服务器把公钥加入登录用户的 authorized_keys，
// 再只用这把私钥重新登录验证。write 是 KeyToYAML 时把 identityFile 写回 ferry.yaml（并去掉 auth: password），
// KeyToLocal 时记到本机设置。密码只用于这一次安装，不会保存。
func (e *Engine) KeyGen(target, keyPath, write string) (*KeyGenResult, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	if keyPath == "" {
		keyPath = e.DefaultKeyPath(t)
	}
	if keyPath, err = filepath.Abs(keyPath); err != nil {
		return nil, err
	}
	res := &KeyGenResult{Target: t.Name, KeyPath: keyPath}

	// 1. 私钥
	cred := e.credentials(t)
	var signer ssh.Signer
	if _, err := os.Stat(keyPath); err == nil {
		signer, err = remote.LoadKey(keyPath, cred.Passphrase)
		if err != nil {
			return nil, cred.translate(err)
		}
		e.Rep.Info("使用已有的私钥 %s", keyPath)
	} else {
		signer, err = generateKey(keyPath, fmt.Sprintf("ferry:%s %s %s", actor(), t.Name, time.Now().Format("2006-01-02")))
		if err != nil {
			return nil, err
		}
		res.Generated = true
		e.Rep.Info("已生成私钥 %s（公钥 %s.pub）", keyPath, keyPath)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if data, err := os.ReadFile(keyPath + ".pub"); err == nil {
		pub = strings.TrimSpace(string(data)) // 带注释的那一行
	}
	res.Fingerprint = ssh.FingerprintSHA256(signer.PublicKey())

	// 2. 用密码登录，安装公钥
	e.Rep.Step("用密码登录 %s@%s:%d 安装公钥", t.User, t.Host, t.Port)
	opts := e.dialOptions(t, cred)
	opts.Auth, opts.IdentityFile = "password", ""
	cred.forcePassword = true
	c, err := remote.Dial(opts)
	if err != nil {
		return nil, cred.translate(err)
	}
	script := installKeyScript(pub)
	out, rr, err := c.Output("sh -c " + shQuote(script))
	c.Close()
	if err != nil {
		return nil, err
	}
	if rr.Code != 0 {
		return nil, fmt.Errorf("安装公钥失败：%s", strings.TrimSpace(rr.Stderr))
	}
	res.Installed = strings.Contains(out, "ADDED")
	if res.Installed {
		e.Rep.Info("已把公钥加入 %s 的 ~/.ssh/authorized_keys", t.User)
	} else {
		e.Rep.Info("服务器上已经有这个公钥")
	}

	// 3. 只用这把私钥重新登录
	e.Rep.Step("用新密钥登录验证")
	vopts := e.dialOptions(t, cred)
	vopts.Auth, vopts.IdentityFile, vopts.OnlyIdentity = "key", keyPath, true
	vc, err := remote.Dial(vopts)
	if err != nil {
		return nil, fmt.Errorf("公钥已安装，但用私钥登录失败（服务器可能禁用了公钥登录，或 sshd 的 AuthorizedKeysFile 不是默认位置）：%w", err)
	}
	if _, rr, err := vc.Output("true"); err != nil || rr.Code != 0 {
		vc.Close()
		return nil, fmt.Errorf("用私钥登录后执行命令失败：%v", err)
	}
	vc.Close()
	e.Rep.Info("免密登录可用")

	// 4. 写回配置
	res.ConfigValue = configPath(keyPath)
	switch write {
	case KeyToYAML:
		if err := config.SetTargetIdentity(e.File.Path, t.Name, res.ConfigValue); err != nil {
			return res, fmt.Errorf("免密登录已可用，但写回配置失败：%w", err)
		}
		res.ConfigUpdated = true
		e.Rep.Info("已更新 %s：targets.%s.identityFile = %s", e.File.Path, t.Name, res.ConfigValue)
		if t.Local != nil {
			e.Rep.Warn("这台电脑的本机设置里另外指定了 %s 的登录方式，在这台电脑上仍按本机设置；要按 ferry.yaml，用 ferry local unset %s", t.Name, t.Name)
		}
	case KeyToLocal:
		if err := e.SetLocalTarget(t.Name, &config.LocalTarget{Auth: "key", IdentityFile: keyPath}); err != nil {
			return res, fmt.Errorf("免密登录已可用，但记到本机设置失败：%w", err)
		}
		res.LocalUpdated = true
		e.Rep.Info("已记到这台电脑的本机设置：%s 用私钥 %s 登录（ferry.yaml 没改）", t.Name, keyPath)
	}
	return res, nil
}

// SetLocalTarget 改这台电脑上某个目标的登录方式（nil = 按 ferry.yaml）；这台服务器在本机设置里的变量不变。
func (e *Engine) SetLocalTarget(target string, lt *config.LocalTarget) error {
	l, err := config.ReadLocal(e.File.Project, e.File.Path)
	if err != nil {
		return err
	}
	if l.Targets == nil {
		l.Targets = map[string]*config.LocalTarget{}
	}
	// 只改登录方式：这台服务器在本机设置里的变量留着
	var vars map[string]string
	if old := l.Targets[target]; old != nil {
		vars = old.Vars
	}
	if lt == nil {
		lt = &config.LocalTarget{}
	}
	c := *lt
	c.Vars = vars
	l.Targets[target] = &c
	if err := config.CheckLocal(e.File, l); err != nil {
		return err
	}
	return config.WriteLocal(e.File, l)
}

// generateKey 生成 OpenSSH 格式的 ed25519 私钥（不加密）与 .pub。
func generateKey(path, comment string) (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + comment + "\n"
	if err := os.WriteFile(path+".pub", []byte(line), 0o644); err != nil {
		return nil, err
	}
	return signer, nil
}

// installKeyScript 把公钥加入登录用户的 authorized_keys（已有则不重复），修正权限与 SELinux 上下文。
func installKeyScript(pub string) string {
	key := strings.Fields(pub)
	if len(key) < 2 {
		return "echo 公钥格式错误 >&2; exit 1"
	}
	match := key[0] + " " + key[1] // 按「类型 + 公钥」判断是否已存在，忽略注释
	return strings.Join([]string{
		"set -e",
		"umask 077",
		`mkdir -p "$HOME/.ssh"`,
		`touch "$HOME/.ssh/authorized_keys"`,
		`if grep -qF ` + shQuote(match) + ` "$HOME/.ssh/authorized_keys"; then echo EXISTS; else printf '%s\n' ` + shQuote(pub) + ` >> "$HOME/.ssh/authorized_keys"; echo ADDED; fi`,
		`chmod 700 "$HOME/.ssh"`,
		`chmod 600 "$HOME/.ssh/authorized_keys"`,
		`command -v restorecon >/dev/null 2>&1 && restorecon -R "$HOME/.ssh" >/dev/null 2>&1 || true`,
	}, "\n")
}

// configPath 写进配置的私钥路径：在用户目录下时写成 ~/...，统一用 /。
func configPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}
