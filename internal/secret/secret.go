// Package secret 管理连接服务器用到的敏感信息：服务器登录密码、私钥密码。
//
// 它们从不写进 ferry.yaml。取值顺序：
//  1. 环境变量（本次进程有效；桌面版把用户刚输入、但不要求记住的值这样传给 ferry，CI 也用它）
//  2. 系统凭据库（Windows 凭据管理器 / macOS 钥匙串 / Linux Secret Service）
//  3. 终端里询问（只在交互终端）
//
// 都取不到时返回 *NeedError，--json 输出里带上环境变量名，桌面版据此弹框询问后重试。
package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

// 凭据种类。
const (
	KindPassword   = "password"   // 服务器登录密码
	KindPassphrase = "passphrase" // 私钥密码
	KindSoftware   = "software"   // Ferry 安装的软件的管理员 / root 密码
	KindAI         = "ai"         // 服务器助手用的大模型 API Key
	KindNotify     = "notify"     // 部署通知的机器人地址、加签密钥
	KindDNS        = "dns"        // HTTPS 证书 DNS 验证用的 DNS 服务商密钥
	KindAIInput    = "ai-input"   // 用户在 AI 助手的输入框里填、并选了记住的密码、账号
)

func service() string {
	if s := os.Getenv("FERRY_KEYRING_SERVICE"); s != "" { // 测试时隔离
		return s
	}
	return "ferry"
}

// PasswordAccount 服务器密码在凭据库里的账户名。
func PasswordAccount(user, host string, port int) string {
	return fmt.Sprintf("password:%s@%s:%d", user, strings.ToLower(host), port)
}

// SoftwareAccount Ferry 安装的软件的密码在凭据库里的账户名：software:<名称>@<服务器>:<SSH 端口>。
func SoftwareAccount(name, host string, port int) string {
	return fmt.Sprintf("software:%s@%s:%d", name, strings.ToLower(host), port)
}

// PassphraseAccount 私钥密码在凭据库里的账户名。
func PassphraseAccount(keyPath string) string { return "passphrase:" + normPath(keyPath) }

// AIKeyAccount 旧版本（只有一个模型时）的大模型 API Key。
const AIKeyAccount = KindAI + ":api-key"

// AIModelKeyAccount 设置 → AI 里某个模型的 API Key。
func AIModelKeyAccount(modelID string) string { return AIKeyAccount + ":" + modelID }

// AIInputAccount AI 助手对话里用户填、并选了记住的一项：ai-input:<对话>:<名字>。
func AIInputAccount(conv, name string) string { return KindAIInput + ":" + conv + ":" + name }

// NotifyURLAccount / NotifySignAccount 部署通知的机器人地址（含令牌）、加签密钥。
func NotifyURLAccount(id string) string  { return KindNotify + ":url:" + id }
func NotifySignAccount(id string) string { return KindNotify + ":sign:" + id }

// DNSAccount DNS 服务商密钥的一项（id、secret、token）。
func DNSAccount(provider, field string) string { return KindDNS + ":" + provider + ":" + field }

// DNSEnv 环境变量名（CI 用）：FERRY_DNS_ALIYUN_ID、FERRY_DNS_CLOUDFLARE_TOKEN……
func DNSEnv(provider, field string) string {
	return "FERRY_DNS_" + strings.ToUpper(provider) + "_" + strings.ToUpper(field)
}

// PasswordEnv 目标的密码环境变量名：FERRY_PASSWORD_<目标名大写，非字母数字换成 _>。
func PasswordEnv(target string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(target) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "FERRY_PASSWORD_" + b.String()
}

// PassphraseEnv 私钥密码的环境变量名：FERRY_PASSPHRASE_<私钥绝对路径 sha256 前 12 位>。
func PassphraseEnv(keyPath string) string {
	sum := sha256.Sum256([]byte(normPath(keyPath)))
	return "FERRY_PASSPHRASE_" + strings.ToUpper(hex.EncodeToString(sum[:])[:12])
}

func normPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// Get 读凭据库。没有时 ok 为 false。
func Get(account string) (string, bool, error) {
	v, err := keyring.Get(service(), account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取系统凭据库失败：%w", err)
	}
	return v, true, nil
}

// Set 写凭据库。
func Set(account, value string) error {
	if value == "" {
		return errors.New("不能保存空值")
	}
	if err := keyring.Set(service(), account, value); err != nil {
		return fmt.Errorf("写入系统凭据库失败：%w", err)
	}
	indexAdd(account, "")
	return nil
}

// SetLabeled 写凭据库，设置页里显示 label（为空时按账户名生成）。
func SetLabeled(account, value, label string) error {
	if err := Set(account, value); err != nil {
		return err
	}
	if label != "" {
		indexAdd(account, label)
	}
	return nil
}

// Delete 删除凭据（不存在不算错）。
func Delete(account string) error {
	err := keyring.Delete(service(), account)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("删除系统凭据失败：%w", err)
	}
	indexRemove(account)
	return nil
}

// Source 值的来源。
type Source int

const (
	FromNone Source = iota
	FromEnv
	FromKeyring
	FromPrompt
)

// Lookup 依次从环境变量（getenv 为空时用 os.Getenv）、凭据库取值。
func Lookup(getenv func(string) string, env, account string) (string, Source, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := getenv(env); v != "" {
		return v, FromEnv, nil
	}
	v, ok, err := Get(account)
	if err != nil {
		return "", FromNone, err
	}
	if ok {
		return v, FromKeyring, nil
	}
	return "", FromNone, nil
}

// NeedError 需要用户提供凭据。
type NeedError struct {
	Kind     string // password / passphrase
	Target   string // 目标名（password）
	Key      string // 私钥路径（passphrase）
	Account  string // 凭据库账户名
	Env      string // 可以用这个环境变量传入
	Rejected bool   // 提供了，但被拒绝（密码错误）
	Prompt   string // 给用户看的说明
}

func (e *NeedError) Error() string {
	what := "服务器 " + e.Target + " 的登录密码"
	if e.Kind == KindPassphrase {
		what = "私钥 " + e.Key + " 的密码"
	}
	if e.Rejected {
		return what + "不正确：用 ferry secret set 重新保存，或通过环境变量 " + e.Env + " 提供"
	}
	return "需要" + what + "：在终端中运行，或先用 ferry secret set 保存到系统凭据库，或通过环境变量 " + e.Env + " 提供"
}

// JSONDetail --json 输出的附加信息。
func (e *NeedError) JSONDetail() map[string]any {
	kind := e.Kind + "-required"
	if e.Rejected {
		kind = e.Kind + "-rejected"
	}
	return map[string]any{"kind": kind, "target": e.Target, "key": e.Key, "env": e.Env, "account": e.Account, "prompt": e.Prompt}
}
