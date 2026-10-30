package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

// 新建项目向导的「测试连接」：只登录（不装 agent、不改服务器），一次往返查看服务器的情况，
// 给出能不能部署、哪些地方要注意。能 sudo 时用 root 执行，才能看到占用端口的进程。

// ProbeRequest 要检查的内容（来自向导）。
type ProbeRequest struct {
	Root     string      `json:"root"`     // 部署目录
	RunAs    string      `json:"runAs"`    // 运行用户（空 = 不需要）
	FerryDir string      `json:"ferryDir"` // ferry 组件目录
	Ports    []ProbePort `json:"ports"`    // 各应用要用的端口
}

// ProbePort 一个应用要用的端口。
type ProbePort struct {
	App  string `json:"app"`
	Port int    `json:"port"`
}

// ProbeResult 检查结果。Checks 是给人看的结论（按顺序显示），其余字段供界面自动填写。
type ProbeResult struct {
	OS             string                   `json:"os"`
	Arch           string                   `json:"arch"` // amd64 / arm64；不认识的原样
	Kernel         string                   `json:"kernel"`
	Priv           string                   `json:"priv"` // root：root 登录；sudo：能免密 sudo；none：都不行
	MemMB          int64                    `json:"memMB"`
	FreeMB         int64                    `json:"freeMB"` // 部署目录所在分区的剩余空间；-1 = 不知道
	Javas          []ProbeJava              `json:"javas"`
	Nginx          string                   `json:"nginx"` // 服务器上已有的 nginx；空 = 没有
	NginxInstances []proto.SoftwareInstance `json:"nginxInstances"`
	RunAsOK        bool                     `json:"runAsOK"`
	Checks         []ProbeCheck             `json:"checks"`
}

// ProbeJava 服务器上找到的一个 java。
type ProbeJava struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Major   int    `json:"major"`
}

// ProbeCheck 一条结论。Fix 不空时界面可以提供按钮（create-user：创建运行用户）。
type ProbeCheck struct {
	Level string `json:"level"` // ok / warn / error
	Text  string `json:"text"`
	Fix   string `json:"fix,omitempty"`
}

// probeScript 在服务器上执行的检查（sh）。参数已转义。
func probeScript(r ProbeRequest) string {
	if r.FerryDir == "" {
		r.FerryDir = config.DefaultFerryDir
	}
	return fmt.Sprintf(`echo "arch $(uname -m)"
echo "kernel $(uname -r)"
if [ -r /etc/os-release ]; then . /etc/os-release; echo "os ${PRETTY_NAME:-$NAME}"; fi
command -v systemctl >/dev/null 2>&1 && echo "systemd yes"
u=%s; if [ -n "$u" ]; then if id -u "$u" >/dev/null 2>&1; then echo "user yes"; else echo "user no"; fi; fi
d=%s; while [ -n "$d" ] && [ "$d" != / ] && [ ! -e "$d" ]; do d=$(dirname "$d"); done
[ -e %[2]s ] && echo "root yes"
df -Pk "$d" 2>/dev/null | awk 'NR==2{print "disk", $4}'
awk '/^MemTotal:/{print "mem", $2}' /proc/meminfo 2>/dev/null
if command -v ss >/dev/null 2>&1; then ss -ltnp 2>/dev/null | awk 'NR>1{print "listen", $4, $6}'
elif command -v netstat >/dev/null 2>&1; then netstat -ltnp 2>/dev/null | awk 'NR>2{print "listen", $4, $7}'; fi
for j in $(command -v java 2>/dev/null) /usr/lib/jvm/*/bin/java /usr/java/*/bin/java /usr/local/*/bin/java /opt/*/bin/java; do
  [ -x "$j" ] || continue
  printf 'java\t%%s\t%%s\t%%s\n' "$(readlink -f "$j" 2>/dev/null || echo "$j")" "$j" "$("$j" -version 2>&1 | head -n 1)"
done
n=$(command -v nginx 2>/dev/null) && echo "nginx $n"
for f in %[3]s/software/*.json; do [ -r "$f" ] || continue; printf 'software-instance\t'; tr -d '\n' < "$f"; printf '\n'; done
d=%[3]s; while [ -n "$d" ] && [ "$d" != / ]; do if [ -e "$d" ]; then o=$(stat -c %%u "$d"); m=$(stat -c %%a "$d"); if [ "$o" != 0 ] || [ $((0$m & 022)) -ne 0 ]; then echo "unsafe $d"; fi; fi; d=$(dirname "$d"); done
`, shQuote(r.RunAs), shQuote(r.Root), shQuote(r.FerryDir))
}

// privWrap 在登录用户下判断权限：root 直接执行；能免密 sudo 就用 sudo 执行（看得到所有进程）；否则以登录用户执行。
// 脚本从标准输入读入。
const privWrap = `if [ "$(id -u)" = 0 ]; then echo "priv root"; sh -s; elif sudo -n true 2>/dev/null; then echo "priv sudo"; sudo -n sh -s; else echo "priv none"; sh -s; fi`

// Probe 测试连接并检查服务器（target 必须是只登录的目标，见 config.Target.Plain）。
func (e *Engine) Probe(target string, r ProbeRequest) (*ProbeResult, error) {
	t, err := e.plainTarget(target)
	if err != nil {
		return nil, err
	}
	s, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	var lines []string
	res, err := s.c.Run(privWrap, []byte(probeScript(r)), func(l string) { lines = append(lines, l) })
	if err != nil {
		return nil, err
	}
	if res.Code != 0 && len(lines) == 0 {
		return nil, fmt.Errorf("在服务器上执行检查失败：%s", strings.TrimSpace(res.Stderr))
	}
	p := parseProbe(t, r, lines)
	if p.Priv == "root" || p.Priv == "sudo" {
		addSystemNginx(p, func(cmd string, timeout time.Duration) (string, error) {
			if p.Priv == "sudo" {
				cmd = "sudo -n sh -c " + shQuote(cmd)
			}
			ctx, cancel := context.WithTimeout(e.ctx(), timeout)
			defer cancel()
			var out []string
			res, err := s.c.RunContext(ctx, cmd, nil, func(l string) { out = append(out, l) })
			if err != nil {
				return "", err
			}
			if res.Code != 0 {
				return "", fmt.Errorf("命令退出码 %d：%s", res.Code, strings.TrimSpace(res.Stderr))
			}
			return strings.Join(out, "\n") + "\n", nil
		})
	}
	return p, nil
}

func addSystemNginx(p *ProbeResult, run func(string, time.Duration) (string, error)) {
	in, err := agent.DiscoverSystemNginx(run)
	if err != nil {
		if p.Nginx != "" {
			p.Checks = append(p.Checks, ProbeCheck{Level: "warn", Text: "检测到系统 nginx，但暂不能接入：" + err.Error()})
		}
		return
	}
	p.NginxInstances = append(p.NginxInstances, *in)
	p.Checks = append(p.Checks, ProbeCheck{Level: "ok", Text: fmt.Sprintf("已识别系统 nginx %s（%s），站点目录 %s", in.Version, in.Unit, in.Hosting.Sites)})
}

// CreateRunUser 在服务器上创建运行用户（系统用户、不能登录，有家目录）。
func (e *Engine) CreateRunUser(target, name string) error {
	if !reUserName.MatchString(name) {
		return fmt.Errorf("用户名 %q 不合法：只能用小写字母、数字、_ -，以字母或 _ 开头", name)
	}
	t, err := e.plainTarget(target)
	if err != nil {
		return err
	}
	s, err := e.connect(t)
	if err != nil {
		return err
	}
	defer s.close()
	script := fmt.Sprintf(`u=%s; if id -u "$u" >/dev/null 2>&1; then exit 0; fi
n=/sbin/nologin; [ -x "$n" ] || n=/usr/sbin/nologin
useradd -r -m -s "$n" "$u"`, shQuote(name))
	var out []string
	res, err := s.c.Run(privWrap, []byte(script), func(l string) { out = append(out, l) })
	if err != nil {
		return err
	}
	if len(out) > 0 && out[0] == "priv none" {
		return fmt.Errorf("登录用户 %s 不是 root，也不能免密 sudo，没法创建用户", t.User)
	}
	if res.Code != 0 {
		return fmt.Errorf("创建运行用户 %s 失败：%s", name, strings.TrimSpace(res.Stderr))
	}
	return nil
}

var reUserName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func (e *Engine) plainTarget(name string) (*config.Target, error) {
	t := e.File.Targets[name]
	if t == nil {
		return nil, fmt.Errorf("没有目标 %s", name)
	}
	if !t.Plain {
		return nil, fmt.Errorf("目标 %s 不是只登录的服务器：测试连接只用于新建项目", name)
	}
	return t, nil
}

var (
	reJavaVersion = regexp.MustCompile(`version "([^"]+)"`)
	reSSProcess   = regexp.MustCompile(`\(\("([^"]+)",pid=(\d+)`)
)

// javaMajor "1.8.0_392" → 8，"17.0.9" → 17。
func javaMajor(v string) int {
	v = strings.TrimPrefix(v, "1.")
	end := strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' })
	if end >= 0 {
		v = v[:end]
	}
	n, _ := strconv.Atoi(v)
	return n
}

// listenPort "0.0.0.0:8080"、"[::]:8080"、"*:8080" → 8080。
func listenPort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(addr[i+1:])
	return n
}

// listenProcess ss 的 users:(("java",pid=123,fd=4)) 或 netstat 的 123/java → 「java（进程 123）」。
func listenProcess(s string) string {
	if m := reSSProcess.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("%s（进程 %s）", m[1], m[2])
	}
	if pid, name, ok := strings.Cut(s, "/"); ok && pid != "" && pid != "-" {
		return fmt.Sprintf("%s（进程 %s）", name, pid)
	}
	return ""
}

func parseProbe(t *config.Target, r ProbeRequest, lines []string) *ProbeResult {
	p := &ProbeResult{FreeMB: -1, Priv: "none"}
	var systemd, rootExists, userKnown bool
	var unsafe []string
	listen := map[int]string{} // 端口 → 占用的进程（看不到时为空）
	seenJava := map[string]bool{}
	for _, l := range lines {
		if raw, ok := strings.CutPrefix(l, "software-instance\t"); ok {
			var in proto.SoftwareInstance
			if json.Unmarshal([]byte(raw), &in) == nil && in.Kind == "nginx" && in.Hosting != nil {
				p.NginxInstances = append(p.NginxInstances, in)
			}
			continue
		}
		if strings.HasPrefix(l, "java\t") {
			f := strings.SplitN(l, "\t", 4)
			if len(f) < 4 || seenJava[f[1]] {
				continue
			}
			seenJava[f[1]] = true
			j := ProbeJava{Path: f[2], Version: strings.TrimSpace(f[3])}
			if m := reJavaVersion.FindStringSubmatch(f[3]); m != nil {
				j.Version, j.Major = m[1], javaMajor(m[1])
			}
			p.Javas = append(p.Javas, j)
			continue
		}
		key, val, _ := strings.Cut(l, " ")
		switch key {
		case "priv":
			p.Priv = val
		case "arch":
			p.Arch = map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[val]
			if p.Arch == "" {
				p.Arch = val
			}
		case "kernel":
			p.Kernel = val
		case "os":
			p.OS = val
		case "systemd":
			systemd = true
		case "user":
			userKnown, p.RunAsOK = true, val == "yes"
		case "root":
			rootExists = true
		case "disk":
			if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
				p.FreeMB = n / 1024
			}
		case "mem":
			if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
				p.MemMB = n / 1024
			}
		case "listen":
			addr, proc, _ := strings.Cut(val, " ")
			if port := listenPort(addr); port > 0 && listen[port] == "" {
				listen[port] = listenProcess(proc)
			}
		case "nginx":
			p.Nginx = val
		case "unsafe":
			unsafe = append(unsafe, val)
		}
	}

	add := func(level, fix, format string, a ...any) {
		p.Checks = append(p.Checks, ProbeCheck{Level: level, Text: fmt.Sprintf(format, a...), Fix: fix})
	}
	sys := p.OS
	if sys == "" {
		sys = "Linux " + p.Kernel
	}
	mem := ""
	if p.MemMB > 0 {
		mem = fmt.Sprintf("，内存 %.1f GB", float64(p.MemMB)/1024)
	}
	add("ok", "", "已连上 %s@%s:%d：%s，%s%s", t.User, t.Host, t.Port, sys, p.Arch, mem)

	switch p.Priv {
	case "root":
	case "sudo":
		add("ok", "", "登录用户 %s 可以免密 sudo：部署时用 sudo 执行", t.User)
	default:
		add("error", "", "登录用户 %s 不是 root，也不能免密 sudo：部署需要 root 权限。请用 root 登录，或在服务器上给 %s 配置免密 sudo", t.User, t.User)
	}
	if !systemd {
		add("error", "", "服务器上没有 systemd：Ferry 用它运行和管理服务")
	}

	if r.RunAs != "" && userKnown {
		if p.RunAsOK {
			add("ok", "", "运行用户 %s 已存在", r.RunAs)
		} else {
			fix := "create-user"
			if p.Priv == "none" {
				fix = ""
			}
			add("warn", fix, "运行用户 %s 还不存在：部署时会自动创建，也可以现在创建", r.RunAs)
		}
	}

	if r.Root != "" {
		free := ""
		if p.FreeMB >= 0 {
			free = fmt.Sprintf("，所在分区剩余 %s", sizeMB(p.FreeMB))
		}
		level := "ok"
		if p.FreeMB >= 0 && p.FreeMB < 1024 {
			level = "warn"
		}
		if rootExists {
			add(level, "", "部署目录 %s 已存在%s", r.Root, free)
		} else {
			add(level, "", "部署目录 %s 还不存在，部署时自动创建%s", r.Root, free)
		}
	}

	var free []string
	ports := append([]ProbePort(nil), r.Ports...)
	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	for _, pp := range ports {
		proc, busy := listen[pp.Port]
		if !busy {
			free = append(free, fmt.Sprintf("%d（%s）", pp.Port, pp.App))
			continue
		}
		who := "别的程序"
		if proc != "" {
			who = " " + proc
		}
		add("warn", "", "端口 %d（%s）已被%s占用：如果就是这个应用以前用别的方式部署的，部署前要先停掉它；否则请换一个端口", pp.Port, pp.App, who)
	}
	if len(free) == 1 {
		add("ok", "", "端口 %s空闲", free[0])
	} else if len(free) > 1 {
		add("ok", "", "端口 %s都空闲", strings.Join(free, "、"))
	}

	for _, d := range unsafe {
		add("error", "", "Ferry 组件目录的上级 %s 不归 root 或别的用户可写：agent 以 root 运行，不能放在这里，请换一个 Ferry 组件目录", d)
	}
	return p
}

func sizeMB(mb int64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}
