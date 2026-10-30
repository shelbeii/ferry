package engine

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/secret"
)

// credentials 一次连接用到的凭据：记录来源，连接成功后决定是否询问「记住」，失败时判断是否是凭据错误。
type credentials struct {
	e        *Engine
	t        *config.Target
	pwSource secret.Source
	password string
	keyPaths map[string]secret.Source // 私钥 → 密码来源
	keyPass  map[string]string
	// forcePassword 本次连接用密码登录（keygen 安装公钥时，不管目标配置的 auth）
	forcePassword bool
	// missingKey 目标指定的私钥在这台电脑上不存在（改用默认私钥、ssh-agent 试）
	missingKey string
}

func (e *Engine) credentials(t *config.Target) *credentials {
	return &credentials{e: e, t: t, keyPaths: map[string]secret.Source{}, keyPass: map[string]string{}}
}

func (c *credentials) passwordNeed(rejected bool) *secret.NeedError {
	t := c.t
	return &secret.NeedError{
		Kind: secret.KindPassword, Target: t.Name, Env: secret.PasswordEnv(t.Name),
		Account: secret.PasswordAccount(t.User, t.Host, t.Port), Rejected: rejected,
		Prompt: fmt.Sprintf("%s@%s:%d 的登录密码", t.User, t.Host, t.Port),
	}
}

// Password 取服务器密码：环境变量 → 系统凭据库 → 终端询问。
func (c *credentials) Password() (string, error) {
	if c.password != "" {
		return c.password, nil // 多路上传的额外连接复用同一个密码，不再询问
	}
	need := c.passwordNeed(false)
	v, src, err := secret.Lookup(c.e.getenv, need.Env, need.Account)
	if err != nil {
		c.e.Rep.Warn("%v", err)
	}
	if src == secret.FromNone {
		if p, ok := c.e.Rep.Secret(need.Prompt + "："); ok {
			v, src = p, secret.FromPrompt
		}
	}
	if src == secret.FromNone {
		return "", need
	}
	c.password, c.pwSource = v, src
	return v, nil
}

// Passphrase 取私钥密码：环境变量 → 系统凭据库 → 终端询问。
func (c *credentials) Passphrase(keyPath string) (string, error) {
	if v := c.keyPass[keyPath]; v != "" {
		return v, nil
	}
	need := c.passphraseNeed(keyPath, false)
	v, src, err := secret.Lookup(c.e.getenv, need.Env, need.Account)
	if err != nil {
		c.e.Rep.Warn("%v", err)
	}
	if src == secret.FromNone {
		if p, ok := c.e.Rep.Secret(need.Prompt + "："); ok {
			v, src = p, secret.FromPrompt
		}
	}
	if src == secret.FromNone {
		return "", need
	}
	c.keyPaths[keyPath], c.keyPass[keyPath] = src, v
	return v, nil
}

func (c *credentials) passphraseNeed(keyPath string, rejected bool) *secret.NeedError {
	return &secret.NeedError{
		Kind: secret.KindPassphrase, Target: c.t.Name, Key: keyPath, Env: secret.PassphraseEnv(keyPath),
		Account: secret.PassphraseAccount(keyPath), Rejected: rejected, Prompt: "私钥 " + keyPath + " 的密码",
	}
}

// translate 把连接错误换成凭据错误（密码 / 私钥密码不对时，告诉 GUI 重新询问）。
func (c *credentials) translate(err error) error {
	var pr *remote.PassphraseRejectedError
	if errors.As(err, &pr) {
		return c.passphraseNeed(pr.Key, true)
	}
	var af *remote.AuthFailedError
	if errors.As(err, &af) && (c.t.Auth == "password" || c.forcePassword) && c.pwSource != secret.FromNone {
		return c.passwordNeed(true)
	}
	if c.missingKey != "" && !c.forcePassword && (errors.As(err, &af) || errors.Is(err, remote.ErrNoKey)) {
		return &MissingKeyError{Target: c.t.Name, Key: c.t.IdentityFile, Path: c.missingKey, Local: c.t.Local != nil && c.t.Local.IdentityFile != ""}
	}
	return err
}

// MissingKeyError 目标指定的私钥在这台电脑上不存在，默认私钥、ssh-agent 也登录不了。
// 常见于把 ferry.yaml 拿到另一台电脑上用：私钥只在配置它的那台电脑上。
type MissingKeyError struct {
	Target string
	Key    string // 配置里写的私钥（可能是 ~/...）
	Path   string // 在这台电脑上的位置
	Local  bool   // 私钥来自本机设置（不是 ferry.yaml）
}

func (e *MissingKeyError) Error() string {
	from := "ferry.yaml 里 targets." + e.Target + ".identityFile 指定的私钥 " + e.Key
	if e.Local {
		from = "本机设置里的私钥 " + e.Key
	}
	where := ""
	if e.Path != e.Key {
		where = "（在这台电脑上是 " + e.Path + "）"
	}
	return fmt.Sprintf("这台电脑上没有%s%s。私钥只在配置它的那台电脑上，不会跟着 ferry.yaml 走。在这台电脑上任选一种："+
		"配置这台电脑自己的免密登录（ferry keygen %s --local，推荐）；选这台电脑上的一个私钥（ferry local set %s --key <私钥>）；"+
		"改用密码登录（ferry local set %s --password）。这些只记在这台电脑上，不改 ferry.yaml",
		from, where, e.Target, e.Target, e.Target)
}

// JSONDetail --json 输出的附加信息。
func (e *MissingKeyError) JSONDetail() map[string]any {
	return map[string]any{"kind": "key-missing", "target": e.Target, "key": e.Path, "local": e.Local}
}

// offerRemember 连接成功后：终端里输入的密码，问一次要不要保存到系统凭据库。
func (c *credentials) offerRemember() {
	if c.pwSource == secret.FromPrompt {
		need := c.passwordNeed(false)
		if c.e.Rep.Confirm("把 " + need.Prompt + "保存到系统凭据库（以后不再询问）？") {
			if err := secret.Set(need.Account, c.password); err != nil {
				c.e.Rep.Warn("%v", err)
			} else {
				c.e.Rep.Info("已保存，可用 ferry secret delete %s 删除", c.t.Name)
			}
		}
	}
	for key, src := range c.keyPaths {
		if src != secret.FromPrompt {
			continue
		}
		need := c.passphraseNeed(key, false)
		if c.e.Rep.Confirm("把" + need.Prompt + "保存到系统凭据库？") {
			if err := secret.Set(need.Account, c.keyPass[key]); err != nil {
				c.e.Rep.Warn("%v", err)
			}
		}
	}
}

// dialOptions 目标的连接参数（不含主机公钥相关之外的特殊设置）。
// 指定的私钥在这台电脑上不存在时改用默认私钥、ssh-agent 试（登录不了时报 MissingKeyError）。
func (e *Engine) dialOptions(t *config.Target, cred *credentials) remote.Options {
	key := identity(e.File, t)
	if p := e.File.KeyMissing(t); p != "" {
		cred.missingKey, key = p, ""
	}
	o := remote.Options{
		Host: t.Host, Port: t.Port, User: t.User, HostKey: t.HostKey, AcceptNew: e.AcceptNewHostKey,
		IdentityFile: key, Auth: t.Auth,
		Password: cred.Password, Passphrase: cred.Passphrase,
		Timeout:      e.Settings.Timeout(),
		OnNewHostKey: func(fp string) { e.Rep.Warn("首次连接，已记录主机公钥 %s", fp) },
	}
	if t.Auth != "password" {
		if name, data := envKey(e.getenv, t.Name); data != nil {
			cred.missingKey = ""
			o.IdentityFile, o.KeyData, o.KeyName = "", data, "环境变量 "+name
			o.Passphrase = func(string) (string, error) {
				if p := e.getenv(name + "_PASSPHRASE"); p != "" {
					return p, nil
				}
				return "", fmt.Errorf("环境变量 %s 里的私钥有密码：密码放在 %s_PASSPHRASE", name, name)
			}
		}
	}
	return o
}

// envKey CI 里用的私钥：环境变量 FERRY_SSH_KEY_<目标名大写> 或 FERRY_SSH_KEY 的内容（私钥文件原文，
// 也可以是 base64，或把换行写成了 \n 的一行）。设置了就只用它，不看 identityFile。
func envKey(getenv func(string) string, target string) (string, []byte) {
	for _, name := range []string{"FERRY_SSH_KEY_" + strings.TrimPrefix(secret.PasswordEnv(target), "FERRY_PASSWORD_"), "FERRY_SSH_KEY"} {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			continue
		}
		if !strings.Contains(v, "-----BEGIN") {
			if b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(v), "")); err == nil {
				v = strings.TrimSpace(string(b))
			}
		}
		if !strings.Contains(v, "\n") && strings.Contains(v, `\n`) {
			v = strings.ReplaceAll(v, `\n`, "\n")
		}
		return name, []byte(v + "\n")
	}
	return "", nil
}
