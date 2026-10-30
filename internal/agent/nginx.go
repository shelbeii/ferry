package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 静态站点自己没有进程，端口要看 nginx：读 nginx -T 的完整配置，
// 找出 root / alias 指向这个站点的 server 块，取它们的 listen 与 server_name。

// ErrNoNginx 服务器上没有 nginx。
var ErrNoNginx = errors.New("没有找到 nginx")

// nginxServer nginx 配置里的一个 server 块。
type nginxServer struct {
	file   string
	listen []string // 如 *:80、127.0.0.1:8080
	ssl    bool
	names  []string
	roots  []nginxRoot
}

// nginxRoot server 块里的一个 root / alias（location 里的带上 location）。
type nginxRoot struct {
	path     string
	location string
}

// parseNginx 解析 nginx -T 的输出。输出里每个配置文件以「# configuration file 路径:」开头，
// include 进来的文件单独列出（不内联），所以逐个文件分别解析。
func parseNginx(dump string) []nginxServer {
	var out []nginxServer
	file := ""
	var buf strings.Builder
	flush := func() {
		out = append(out, parseNginxFile(file, buf.String())...)
		buf.Reset()
	}
	for _, line := range strings.SplitAfter(dump, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# configuration file ") && strings.HasSuffix(t, ":") {
			flush()
			file = strings.TrimSuffix(strings.TrimPrefix(t, "# configuration file "), ":")
			continue
		}
		buf.WriteString(line)
	}
	flush()
	return out
}

// nginxTokens 切成词：普通词、引号字符串、{ } ;，去掉 # 注释。
func nginxTokens(s string) []string {
	var toks []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == ';':
			toks = append(toks, string(c))
			i++
		case c == '"' || c == '\'':
			j := i + 1
			var w strings.Builder
			for j < len(s) && s[j] != c {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				w.WriteByte(s[j])
				j++
			}
			toks = append(toks, w.String())
			i = j + 1
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\r\n{};\"'", rune(s[j])) {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks
}

func parseNginxFile(file, text string) []nginxServer {
	toks := nginxTokens(text)
	var out []nginxServer
	type frame struct {
		name     string
		args     []string
		server   int // 所在 server 块在 out 里的下标，-1 = 不在 server 里
		location string
	}
	stack := []frame{{server: -1}}
	var stmt []string
	for _, t := range toks {
		top := stack[len(stack)-1]
		switch t {
		case "{":
			f := frame{server: top.server, location: top.location}
			if len(stmt) > 0 {
				f.name, f.args = stmt[0], stmt[1:]
			}
			switch {
			case f.name == "server" && top.name != "stream" && top.name != "mail" && top.server < 0:
				out = append(out, nginxServer{file: file})
				f.server = len(out) - 1
			case f.name == "location" && len(f.args) > 0:
				f.location = f.args[len(f.args)-1]
			}
			stack = append(stack, f)
			stmt = nil
		case "}":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			stmt = nil
		case ";":
			if len(stmt) > 0 && top.server >= 0 {
				sv := &out[top.server]
				args := stmt[1:]
				switch stmt[0] {
				case "listen":
					if top.name == "server" && len(args) > 0 {
						if l := nginxListen(args[0]); !slices.Contains(sv.listen, l) { // IPv4、IPv6 各写一行时只留一个
							sv.listen = append(sv.listen, l)
						}
						for _, a := range args[1:] {
							if a == "ssl" {
								sv.ssl = true
							}
						}
					}
				case "server_name":
					if top.name == "server" {
						for _, n := range args {
							if n != "_" && n != "" && n != `""` {
								sv.names = append(sv.names, n)
							}
						}
					}
				case "root", "alias":
					if len(args) > 0 {
						sv.roots = append(sv.roots, nginxRoot{path: args[0], location: top.location})
					}
				}
			}
			stmt = nil
		default:
			stmt = append(stmt, t)
		}
	}
	for i := range out {
		if len(out[i].listen) == 0 {
			out[i].listen = []string{"*:80"} // 没写 listen：nginx 默认 80
		}
	}
	return out
}

// nginxListen listen 的地址写成与进程端口一样的形式：80 → *:80，[::]:80 → *:80。
func nginxListen(a string) string {
	if strings.HasPrefix(a, "unix:") {
		return a
	}
	if _, err := strconv.Atoi(a); err == nil {
		return "*:" + a
	}
	i := strings.LastIndexByte(a, ':')
	if i < 0 || strings.HasSuffix(a, "]") { // 只有地址，端口默认 80
		a, i = a+":80", len(a)
	}
	host, port := a[:i], a[i+1:]
	switch host {
	case "*", "0.0.0.0", "[::]":
		host = "*"
	case "localhost":
		host = "127.0.0.1"
	}
	return host + ":" + port
}

// nginxSites 指向这个静态站点的 server：root / alias 等于站点目录（current、link 或当前版本目录）或在它下面。
func (a *Agent) nginxSites(s *proto.AppSpec, servers []nginxServer) []proto.Site {
	targets := []string{a.runningDir(s.Name)}
	if s.Link != "" {
		targets = append(targets, s.Link)
	}
	if cur := a.current(s.Name); cur != "" {
		targets = append(targets, a.releaseDir(s.Name, cur))
	}
	for _, t := range targets[:] {
		if r, err := filepath.EvalSymlinks(t); err == nil {
			targets = append(targets, r)
		}
	}
	under := func(p string) bool {
		if strings.Contains(p, "$") { // 带变量的路径看不出指向哪里
			return false
		}
		cands := []string{filepath.Clean(p)}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			cands = append(cands, r)
		}
		for _, c := range cands {
			for _, t := range targets {
				t = filepath.Clean(t)
				if c == t || strings.HasPrefix(c, t+string(filepath.Separator)) {
					return true
				}
			}
		}
		return false
	}
	var out []proto.Site
	for _, sv := range servers {
		for _, r := range sv.roots {
			if under(r.path) {
				out = append(out, proto.Site{Listen: sv.listen, SSL: sv.ssl, Names: sv.names, Location: r.location, File: sv.file})
				break
			}
		}
	}
	return out
}

// staticServing 静态站点的 nginx 信息：sites 与所有监听地址（按端口排序去重），找不到时给出原因。
func (a *Agent) staticServing(s *proto.AppSpec, dump func() (string, error)) (sites []proto.Site, listen []string, note string) {
	text, err := dump()
	if err != nil {
		if errors.Is(err, ErrNoNginx) {
			return nil, nil, "服务器上没有找到 nginx"
		}
		return nil, nil, "读不到 nginx 配置：" + err.Error()
	}
	sites = a.nginxSites(s, parseNginx(text))
	if len(sites) == 0 {
		where := a.runningDir(s.Name)
		if s.Link != "" {
			where = s.Link
		}
		return nil, nil, "nginx 里没有站点指向这里（root 或 alias 应为 " + filepath.ToSlash(where) + "）"
	}
	seen := map[string]bool{}
	for _, st := range sites {
		for _, l := range st.Listen {
			if !seen[l] {
				seen[l] = true
				listen = append(listen, l)
			}
		}
	}
	sort.SliceStable(listen, func(i, j int) bool { return portNum(listen[i]) < portNum(listen[j]) })
	return sites, listen, ""
}

func portNum(l string) int {
	n, _ := strconv.Atoi(l[strings.LastIndexByte(l, ':')+1:])
	return n
}

// ---------------------------------------------------------------- 读 nginx 配置（真实系统）

// NginxDump 运行 nginx -T。优先用正在运行的 master 进程的程序与 -c 配置；
// master 在别的挂载命名空间（容器里）时读不到它的配置。
func (realSystem) NginxDump() (string, error) {
	bin, conf := "", ""
	if pid, args := nginxMaster("/proc"); pid > 0 {
		if self, _ := os.Readlink("/proc/self/ns/mnt"); self != "" {
			if ns, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/mnt"); ns != "" && ns != self {
				return "", errors.New("nginx 运行在容器里，看不到它的配置")
			}
		}
		if exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe"); err == nil {
			bin = strings.TrimSuffix(exe, " (deleted)")
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-c" {
				conf = args[i+1]
			}
		}
	}
	if bin == "" {
		for _, p := range []string{"nginx", "/usr/sbin/nginx", "/usr/local/sbin/nginx", "/usr/local/nginx/sbin/nginx", "/usr/local/openresty/nginx/sbin/nginx"} {
			if b, err := exec.LookPath(p); err == nil {
				bin = b
				break
			}
		}
	}
	if bin == "" {
		return "", ErrNoNginx
	}
	args := []string{"-T"}
	if conf != "" {
		args = append(args, "-c", conf)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", errors.New("nginx -T 超时")
		}
		// 错误原因在 stderr（如配置有错）：只要第一行
		if msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return stdout.String(), nil
}

// nginxMaster 找 nginx master 进程：cmdline 为「nginx: master process 原命令行」。
func nginxMaster(proc string) (int, []string) {
	ents, _ := os.ReadDir(proc)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(proc, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cl := strings.TrimRight(strings.ReplaceAll(string(b), "\x00", " "), " ")
		if rest, ok := strings.CutPrefix(cl, "nginx: master process "); ok {
			return pid, strings.Fields(rest)
		}
	}
	return 0, nil
}

// checkStaticServing 只放文件的静态站点（没有 static.port）：服务器上没有 nginx、或 nginx 没有指向它时提示。
func (a *Agent) checkStaticServing(s *proto.AppSpec, p *proto.Preflight) {
	_, _, note := a.staticServing(s, a.Sys.NginxDump)
	if note == "" {
		return
	}
	hint := "。可以在配置里给它设置 static.port（如 8081），由 Ferry 自带的 nginx 单独运行这个站点（装在部署目录里，不影响服务器上已有的 nginx）"
	p.Warnings = append(p.Warnings, s.Name+"："+note+hint)
}
