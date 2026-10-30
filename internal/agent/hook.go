package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// HookExec 一条部署钩子怎么执行。
type HookExec struct {
	Cmd     string
	Dir     string
	UID     int
	GID     int
	Groups  []int
	Env     []string
	Timeout time.Duration
}

const (
	hookDefaultTimeout = 5 * time.Minute
	hookMaxLines       = 200 // 过程里最多显示这么多行，再多的只保留最后几行用来说明失败原因
	hookTailLines      = 12
	hookLineMax        = 400
)

// hookPhase 钩子在部署中的位置。
type hookPhase struct {
	name  string // beforeSwitch / afterSwitch
	label string // 给人看的
	dir   string // 工作目录
}

// runHooks 依次执行一组钩子。不是 Optional 的失败时返回错误（后面的不再执行）。
func (a *Agent) runHooks(s *proto.AppSpec, ph hookPhase, hooks []proto.Hook, release, prev string) error {
	if len(hooks) == 0 {
		return nil
	}
	env, err := a.hookEnv(s, ph, release, prev)
	if err != nil {
		return fmt.Errorf("%s：%v", ph.label, err)
	}
	for i, h := range hooks {
		what := fmt.Sprintf("%s第 %d 条", ph.label, i+1)
		if len(hooks) == 1 {
			what = ph.label
		}
		if err := a.runHook(s, h, ph, env, what); err != nil {
			if h.Optional {
				a.warn(proto.EvHook, s.Name, "%s失败（设了 optional，接着部署）：%v", what, err)
				continue
			}
			return fmt.Errorf("%s失败：%v", what, err)
		}
	}
	return nil
}

func (a *Agent) runHook(s *proto.AppSpec, h proto.Hook, ph hookPhase, env []string, what string) error {
	user := h.User
	if user == "" {
		user = s.User
	}
	if user == "" {
		return fmt.Errorf("不知道以哪个用户执行：设置 user，或目标的 runAs")
	}
	x := HookExec{Cmd: h.Run, Dir: ph.dir, Timeout: time.Duration(h.TimeoutSec) * time.Second}
	if x.Timeout <= 0 {
		x.Timeout = hookDefaultTimeout
	}
	home := "/root"
	if user != "root" {
		uid, gid, err := a.Sys.LookupUser(user)
		if err != nil {
			return err
		}
		_, groups, err := a.Sys.UserGroups(user)
		if err != nil {
			groups = []int{gid}
		}
		x.UID, x.GID, x.Groups = uid, gid, groups
		home = userHome(user)
	}
	x.Env = append([]string{"HOME=" + home, "USER=" + user, "LOGNAME=" + user}, env...)
	a.info(proto.EvHook, s.Name, "%s（以 %s 执行，最多 %s）：%s", what, user, humanDur(x.Timeout), oneLineCmd(h.Run))

	var tail []string
	n := 0
	start := a.Sys.Now()
	err := a.Sys.RunHook(x, func(l string) {
		l = strings.TrimRight(l, "\r")
		if len(l) > hookLineMax {
			l = l[:hookLineMax] + "…"
		}
		tail = append(tail, l)
		if len(tail) > hookTailLines {
			tail = tail[1:]
		}
		n++
		if n <= hookMaxLines {
			a.info(proto.EvHook, s.Name, "  %s", l)
		} else if n == hookMaxLines+1 {
			a.info(proto.EvHook, s.Name, "  …（输出太多，后面的不再显示）")
		}
	})
	if err != nil {
		// 输出已经显示过的，失败时只在显示不全时补上最后几行
		if n > hookMaxLines {
			for _, l := range tail {
				a.fail(proto.EvDiag, s.Name, "%s", l)
			}
		}
		return err
	}
	a.info(proto.EvHook, s.Name, "%s完成（%ds）", what, int(a.Sys.Now().Sub(start).Seconds()))
	return nil
}

// hookEnv 钩子的环境变量：PATH、服务的 env / envFile，再加上 FERRY_* 说明这次部署。
func (a *Agent) hookEnv(s *proto.AppSpec, ph hookPhase, release, prev string) ([]string, error) {
	vars := map[string]string{}
	for _, f := range s.HookEnvFiles {
		optional := strings.HasPrefix(f, "-")
		f = strings.TrimPrefix(f, "-")
		kv, err := readEnvFile(f)
		if err != nil {
			if optional && os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("读取 envFile %s 失败：%v", f, err)
		}
		for k, v := range kv {
			vars[k] = v
		}
	}
	for k, v := range s.HookEnv {
		vars[k] = v
	}
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	// 应用用的 java 放在最前面：钩子里直接写 java 就是同一个
	if s.RuntimeVersion != "" {
		path = filepath.Join(a.runtimesDir(), proto.JavaPinLink(s.RuntimeVersion), "bin") + ":" + path
	} else if s.RuntimeJava > 0 {
		path = filepath.Join(a.runtimesDir(), "java-"+strconv.Itoa(s.RuntimeJava), "bin") + ":" + path
	} else if s.Java != "" && s.Java != "/usr/bin/java" {
		path = filepath.Dir(s.Java) + ":" + path
	}
	if _, ok := vars["PATH"]; !ok {
		vars["PATH"] = path
	}
	if _, ok := vars["LANG"]; !ok {
		vars["LANG"] = "C.UTF-8"
	}
	for k, v := range map[string]string{
		"FERRY_PROJECT":     a.req.Project,
		"FERRY_TARGET":      a.req.Target,
		"FERRY_APP":         s.Name,
		"FERRY_PHASE":       ph.name,
		"FERRY_RELEASE":     release,
		"FERRY_PREVIOUS":    prev,
		"FERRY_VERSION_DIR": a.releaseDir(s.Name, release),
		"FERRY_RUNNING_DIR": a.runningDir(s.Name),
		"FERRY_APP_DIR":     a.appDir(s.Name),
		"FERRY_DATA_DIR":    a.sharedDir(s.Name),
		"FERRY_ACTOR":       a.req.Actor,
	} {
		vars[k] = v
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+vars[k])
	}
	return out, nil
}

// readEnvFile 按 systemd 的 EnvironmentFile 读：KEY=VALUE，# 和 ; 开头的是注释，值两边的引号去掉；
// 也认 export KEY=VALUE（常见的 shell 写法）。
func readEnvFile(p string) (map[string]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || l[0] == '#' || l[0] == ';' {
			continue
		}
		l = strings.TrimPrefix(l, "export ")
		k, v, ok := strings.Cut(l, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			q := v[0]
			v = v[1 : len(v)-1]
			if q == '"' {
				v = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\$`, `$`).Replace(v)
			}
		}
		out[k] = v
	}
	return out, sc.Err()
}

// userHome 用户的主目录（/etc/passwd）；查不到时用 /
func userHome(name string) string {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "/"
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) >= 6 && f[0] == name && f[5] != "" {
			return f[5]
		}
	}
	return "/"
}

// humanDur 5 分钟、90 秒、1 小时 30 分钟
func humanDur(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%d 小时", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%d 分钟", m))
	}
	if s > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d 秒", s))
	}
	return strings.Join(parts, " ")
}

func oneLineCmd(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ; ")), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
