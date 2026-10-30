package ai

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// 用户在输入框里填的密码、账号（安全输入）：值只放在本机 Ferry 进程的内存里（按对话分开），不写进对话记录、改动记录，
// 模型只拿到代号 {{secret:名字}}。执行时在本机换成真值：
//   - 命令里用环境变量 $FERRY_SECRET_<名字大写>：值经 SSH 标准输入传过去，不出现在命令行里（服务器的 ps、日志看不到）；
//     命令里写了代号的也换成这个环境变量
//   - 写文件的内容、装软件的参数里的代号直接换成真值（它们本来就不在命令行上）
//
// 交回模型的输出里出现这些值（或它们的 base64）时遮住。

// SecretField 要用户填的一项。
type SecretField struct {
	Name    string `json:"name"`              // 代号里的名字：小写字母开头，小写字母、数字、下划线
	Label   string `json:"label"`             // 给用户看的说明，如「MySQL root 新密码」
	Kind    string `json:"kind,omitempty"`    // password（默认，输入时不显示）| text（账号等，显示）
	Confirm bool   `json:"confirm,omitempty"` // 要输两遍（设新密码时）
}

// MaxSecretFields 一次最多让用户填几项。
const MaxSecretFields = 5

var (
	reSecretName  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	reSecretToken = regexp.MustCompile(`\{\{\s*secret:([a-z][a-z0-9_]{0,39})\s*\}\}`)
	reSecretEnv   = regexp.MustCompile(`FERRY_SECRET_([A-Z][A-Z0-9_]{0,39})\b`)
)

// ValidSecretName 名字能不能用作代号。
func ValidSecretName(name string) bool { return reSecretName.MatchString(name) }

// SecretToken 代号：{{secret:名字}}。
func SecretToken(name string) string { return "{{secret:" + name + "}}" }

// SecretEnv 命令里取值用的环境变量名：FERRY_SECRET_<名字大写>。
func SecretEnv(name string) string { return "FERRY_SECRET_" + strings.ToUpper(name) }

// CheckSecretFields 检查模型要用户填的项。
func CheckSecretFields(fs []SecretField) error {
	if len(fs) == 0 {
		return fmt.Errorf("fields 至少要有一项")
	}
	if len(fs) > MaxSecretFields {
		return fmt.Errorf("一次最多填 %d 项", MaxSecretFields)
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if !ValidSecretName(f.Name) {
			return fmt.Errorf("名字 %q 不对：小写字母开头，只能有小写字母、数字、下划线，最长 40 个字符", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("名字 %q 重复了", f.Name)
		}
		seen[f.Name] = true
		if strings.TrimSpace(f.Label) == "" {
			return fmt.Errorf("%s 要写 label（给用户看的说明）", f.Name)
		}
		if f.Kind != "" && f.Kind != "password" && f.Kind != "text" {
			return fmt.Errorf("%s 的 kind 只能是 password 或 text", f.Name)
		}
	}
	return nil
}

var secretStore = struct {
	sync.Mutex
	m map[string]map[string]string // 对话 → 名字 → 值
}{m: map[string]map[string]string{}}

// SetSecret 记下对话里用户填的一项（只在内存里）。
func SetSecret(conv, name, value string) {
	secretStore.Lock()
	defer secretStore.Unlock()
	if secretStore.m[conv] == nil {
		secretStore.m[conv] = map[string]string{}
	}
	secretStore.m[conv][name] = value
}

// Secret 对话里用户填的一项。
func Secret(conv, name string) (string, bool) {
	secretStore.Lock()
	defer secretStore.Unlock()
	v, ok := secretStore.m[conv][name]
	return v, ok
}

// ForgetSecret 忘掉对话里用户填的一项（在「设置 → 凭据」里删掉记住的值时）。
func ForgetSecret(conv, name string) {
	secretStore.Lock()
	defer secretStore.Unlock()
	delete(secretStore.m[conv], name)
}

// ForgetSecrets 忘掉对话里用户填的（删除对话时）。
func ForgetSecrets(conv string) {
	secretStore.Lock()
	defer secretStore.Unlock()
	delete(secretStore.m, conv)
}

// SecretNames 文本里用到了哪些代号（{{secret:名字}} 和 FERRY_SECRET_名字），按出现顺序、不重复。
func SecretNames(s string) []string {
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, m := range reSecretToken.FindAllStringSubmatch(s, -1) {
		add(m[1])
	}
	for _, m := range reSecretEnv.FindAllStringSubmatch(s, -1) {
		add(strings.ToLower(m[1]))
	}
	return names
}

// UsesSecrets 文本里有没有用到代号。
func UsesSecrets(s string) bool { return len(SecretNames(s)) > 0 }

func missingSecret(name string) error {
	return fmt.Errorf("没有 %s 的值：用户没填过，或者 Ferry 重启过（没让记住的值只在内存里）。用 ask_secret 让用户重新填", SecretToken(name))
}

// FillSecrets 把文本里的代号 {{secret:名字}} 换成用户填的值（写文件、装软件参数用）。
func FillSecrets(conv, s string) (string, error) {
	var err error
	out := reSecretToken.ReplaceAllStringFunc(s, func(m string) string {
		name := reSecretToken.FindStringSubmatch(m)[1]
		v, ok := Secret(conv, name)
		if !ok {
			if err == nil {
				err = missingSecret(name)
			}
			return m
		}
		return v
	})
	return out, err
}

// CommandSecrets 命令里用到的代号：{{secret:名字}} 换成 ${FERRY_SECRET_名字}，返回改好的命令和要传过去的环境变量。
func CommandSecrets(conv, cmd string) (string, map[string]string, error) {
	names := SecretNames(cmd)
	if len(names) == 0 {
		return cmd, nil, nil
	}
	env := map[string]string{}
	for _, n := range names {
		v, ok := Secret(conv, n)
		if !ok {
			return "", nil, missingSecret(n)
		}
		env[SecretEnv(n)] = v
	}
	cmd = reSecretToken.ReplaceAllStringFunc(cmd, func(m string) string {
		return "${" + SecretEnv(reSecretToken.FindStringSubmatch(m)[1]) + "}"
	})
	return cmd, env, nil
}

// SecretEnvLine 经标准输入传给服务器的一行：base64 编码的 export 语句（服务器上 read 一行、解码后 eval）。
func SecretEnvLine(env map[string]string) []byte {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export " + k + "=" + ShellQuote(env[k]) + "\n")
	}
	return []byte(base64.StdEncoding.EncodeToString([]byte(b.String())) + "\n")
}

// minMaskLen 太短的值不遮（会把输出里不相干的地方也遮掉）。
const minMaskLen = 4

// MaskSecrets 遮住文本里对话中用户填过的值（原样的和 base64 的）。
func MaskSecrets(conv, s string) string {
	secretStore.Lock()
	var vals []string
	for _, v := range secretStore.m[conv] {
		if len(v) < minMaskLen {
			continue
		}
		vals = append(vals, v, base64.StdEncoding.EncodeToString([]byte(v)), base64.RawStdEncoding.EncodeToString([]byte(v)))
	}
	secretStore.Unlock()
	if len(vals) == 0 || s == "" {
		return s
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) }) // 长的先遮
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, mask)
	}
	return s
}
