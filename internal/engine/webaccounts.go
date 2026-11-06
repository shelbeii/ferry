package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/secret"
)

// 「打开网页」自动登录用的账号：列出能用的账号让用户选，选好后内置浏览器替用户填好、点登录。
// 来源（按顺序，重复的去掉）：用户在 Ferry 里填过并记住的 → Ferry 安装时保存的 → 进程 / 容器的环境变量、启动参数 →
// 软件的默认账号（可能已经改过）。只读服务器，不改动。

// WebQuery 哪个软件的哪个网页。
type WebQuery struct {
	Kind      string `json:"kind"`
	Container string `json:"container,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Account   string `json:"account,omitempty"` // Ferry 装的：安装时的账号
	Port      int    `json:"port"`              // 网页在服务器上（或容器里）的端口：记住的账号按它分
	Label     string `json:"label,omitempty"`   // 设置 → 凭据里显示的名字，如「Nacos 控制台」
	// 试账号用：网页的地址（主机上的端口、容器里的端口、http / https、路径、域名）
	HostPort  int    `json:"hostPort,omitempty"`
	InnerPort int    `json:"innerPort,omitempty"`
	Scheme    string `json:"scheme,omitempty"`
	Path      string `json:"path,omitempty"`
	VHost     string `json:"vhost,omitempty"`
}

// WebAccount 一个能登录的账号。
type WebAccount struct {
	User     string `json:"user"`
	Password string `json:"password"`
	From     string `json:"from"`
	// Saved 用户在 Ferry 里记住的（可以删掉）；Default 软件的默认账号（可能已经改过）
	Saved   bool `json:"saved,omitempty"`
	Default bool `json:"default,omitempty"`
	// Check 打开前试过：ok 能登录 | bad 登录不上；空 = 没试（不认得这种控制台、连不上）
	Check string `json:"check,omitempty"`
}

type savedWeb struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// webAccountKey 记住的账号在凭据库里的名字：web:<软件>@<服务器>:<SSH 端口>/<网页端口>[/<容器>]。
func (e *Engine) webAccountKey(target string, q WebQuery) (string, string, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return "", "", err
	}
	if q.Port <= 0 {
		return "", "", fmt.Errorf("端口不对")
	}
	key := fmt.Sprintf("%s:%s@%s:%d/%d", secret.KindWeb, firstNonEmptyStr(q.Kind, "web"), strings.ToLower(t.Host), t.Port, q.Port)
	if q.Container != "" {
		key += "/" + q.Container
	}
	label := fmt.Sprintf("%s（%s:%d）", firstNonEmptyStr(q.Label, q.Kind, "网页"), t.Host, q.Port)
	return key, label, nil
}

func readSaved(key string) []savedWeb {
	v, ok, err := secret.Get(key)
	if err != nil || !ok {
		return nil
	}
	var list []savedWeb
	if json.Unmarshal([]byte(v), &list) != nil {
		return nil
	}
	return list
}

// SaveWebAccount 记住一个账号（同一个用户名的换成新密码，放在最前面，最多 10 个）。
func (e *Engine) SaveWebAccount(target string, q WebQuery, user, password string) error {
	key, label, err := e.webAccountKey(target, q)
	if err != nil {
		return err
	}
	list := []savedWeb{{User: user, Password: password}}
	for _, s := range readSaved(key) {
		if s.User != user && len(list) < 10 {
			list = append(list, s)
		}
	}
	b, _ := json.Marshal(list)
	return secret.SetLabeled(key, string(b), label)
}

// ForgetWebAccount 不再记住这个用户名（最后一个也删掉时删掉凭据）。
func (e *Engine) ForgetWebAccount(target string, q WebQuery, user string) error {
	key, label, err := e.webAccountKey(target, q)
	if err != nil {
		return err
	}
	var keep []savedWeb
	for _, s := range readSaved(key) {
		if s.User != user {
			keep = append(keep, s)
		}
	}
	if len(keep) == 0 {
		return secret.Delete(key)
	}
	b, _ := json.Marshal(keep)
	return secret.SetLabeled(key, string(b), label)
}

// WebAccounts 能用来登录的账号。连不上服务器时只返回记住的和 Ferry 保存的（不算错误）。
func (e *Engine) WebAccounts(target string, q WebQuery) ([]WebAccount, error) {
	var out []WebAccount
	seen := map[string]bool{}
	add := func(a WebAccount) {
		if a.Password == "" && !a.Default {
			return
		}
		k := a.User + "\x00" + a.Password
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, a)
	}
	key, _, err := e.webAccountKey(target, q)
	if err != nil {
		return nil, err
	}
	for _, s := range readSaved(key) {
		add(WebAccount{User: s.User, Password: s.Password, From: "在 Ferry 里记住的", Saved: true})
	}
	if q.Instance != "" {
		if pw, err := e.SoftwarePassword(target, q.Instance); err == nil {
			add(WebAccount{User: firstNonEmptyStr(q.Account, webDefaultUser[q.Kind]), Password: pw, From: "Ferry 安装时保存的"})
		}
	}
	// 进程 / 容器的环境变量、启动参数；再对认得的控制台试一下每个账号
	var env map[string]string
	var args []string
	if q.Container != "" && !reSafeName.MatchString(q.Container) {
		return nil, fmt.Errorf("容器名 %q 不合法", q.Container)
	}
	needEnv := q.Container != "" || q.PID > 0
	if needEnv || webLoginChecks[q.Kind] != nil {
		err := e.withTarget(target, func(s *session) error {
			if needEnv {
				run := func(cmd string) (string, bool) {
					o, r, err := s.c.Output("timeout 15 " + s.sudo + cmd)
					return o, err == nil && r.Code == 0
				}
				env, args = processEnv(q, run)
			}
			for _, a := range webEnvAccounts(q.Kind, env, args) {
				add(a)
			}
			if d, ok := webDefaults[q.Kind]; ok && q.Instance == "" {
				add(WebAccount{User: d[0], Password: d[1], From: "默认账号（可能已经改过）", Default: true})
			}
			e.checkWebAccounts(s, q, out)
			return nil
		})
		if err != nil {
			e.Rep.Warn("没能从服务器上读账号、试账号：%v", err)
		} else {
			return sortChecked(out), nil
		}
	}
	// 用户没在别处设过账号时，默认账号多半能用；设过的话默认账号多半不能用了，但也列出来
	if d, ok := webDefaults[q.Kind]; ok && q.Instance == "" {
		add(WebAccount{User: d[0], Password: d[1], From: "默认账号（可能已经改过）", Default: true})
	}
	return out, nil
}

// sortChecked 试过能登录的排前面，登录不上的排最后（其他保持原来的顺序）。
func sortChecked(list []WebAccount) []WebAccount {
	rank := func(a WebAccount) int { return map[string]int{"ok": 0, "": 1, "bad": 2}[a.Check] }
	out := make([]WebAccount, 0, len(list))
	for r := 0; r <= 2; r++ {
		for _, a := range list {
			if rank(a) == r {
				out = append(out, a)
			}
		}
	}
	return out
}

// processEnv 进程（或容器）的环境变量和启动参数。
func processEnv(q WebQuery, run func(string) (string, bool)) (map[string]string, []string) {
	env := map[string]string{}
	if q.Container != "" {
		out, ok := run("docker inspect " + shQuote(q.Container))
		var list []dockerInspect
		if ok && json.Unmarshal([]byte(out), &list) == nil && len(list) > 0 {
			for _, kv := range list[0].Config.Env {
				if k, v, ok := strings.Cut(kv, "="); ok {
					env[k] = v
				}
			}
			return env, append([]string{list[0].Path}, list[0].Args...)
		}
	}
	if q.PID <= 0 {
		return env, nil
	}
	p := strconv.Itoa(q.PID)
	if out, ok := run("cat /proc/" + p + "/environ"); ok {
		for _, kv := range strings.Split(strings.TrimRight(out, "\n"), "\x00") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
	}
	var args []string
	if out, ok := run("cat /proc/" + p + "/cmdline"); ok {
		args = strings.Split(strings.TrimRight(out, "\x00\n"), "\x00")
	}
	return env, args
}

var webDefaultUser = map[string]string{"nacos": "nacos", "sentinel": "sentinel", "minio": "minioadmin", "grafana": "admin"}

// webDefaults 软件自带的默认账号（装好后多半会改）。
var webDefaults = map[string][2]string{
	"sentinel": {"sentinel", "sentinel"},
	"seata":    {"seata", "seata"},
	"xxljob":   {"admin", "123456"},
	"rabbitmq": {"guest", "guest"},
	"emqx":     {"admin", "public"},
	"grafana":  {"admin", "admin"},
	"nacos":    {"nacos", "nacos"}, // 2.x；3.x 第一次打开时自己设
}

// webEnvAccounts 环境变量、启动参数里的账号。
func webEnvAccounts(kind string, env map[string]string, args []string) []WebAccount {
	pair := func(uk, pk, from string) []WebAccount {
		if env[pk] == "" {
			return nil
		}
		return []WebAccount{{User: firstNonEmptyStr(env[uk], webDefaultUser[kind]), Password: env[pk], From: from}}
	}
	switch kind {
	case "minio":
		if a := pair("MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD", "环境变量 MINIO_ROOT_USER / MINIO_ROOT_PASSWORD"); a != nil {
			return a
		}
		if a := pair("MINIO_ACCESS_KEY", "MINIO_SECRET_KEY", "环境变量 MINIO_ACCESS_KEY / MINIO_SECRET_KEY"); a != nil {
			return a
		}
		if len(env) > 0 || len(args) > 0 {
			return []WebAccount{{User: "minioadmin", Password: "minioadmin", From: "没设账号时 MinIO 的默认账号", Default: true}}
		}
	case "sentinel":
		u, p := argValue(args, "-Dsentinel.dashboard.auth.username"), argValue(args, "-Dsentinel.dashboard.auth.password")
		if p != "" {
			return []WebAccount{{User: firstNonEmptyStr(u, "sentinel"), Password: p, From: "启动参数 -Dsentinel.dashboard.auth.*"}}
		}
	case "grafana":
		return pair("GF_SECURITY_ADMIN_USER", "GF_SECURITY_ADMIN_PASSWORD", "环境变量 GF_SECURITY_ADMIN_*")
	case "rabbitmq":
		return pair("RABBITMQ_DEFAULT_USER", "RABBITMQ_DEFAULT_PASS", "环境变量 RABBITMQ_DEFAULT_USER / PASS")
	case "emqx":
		return pair("EMQX_DASHBOARD__DEFAULT_USERNAME", "EMQX_DASHBOARD__DEFAULT_PASSWORD", "环境变量 EMQX_DASHBOARD__DEFAULT_*")
	}
	return nil
}

func argValue(args []string, name string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
