package agent

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// DiscoverSystemNginx 只读识别已有系统服务及其 http include 目录。
// 引擎的向导探测和 agent 发布时使用同一套规则；不改主配置、不创建安装记录。
func DiscoverSystemNginx(run func(string, time.Duration) (string, error)) (*proto.SoftwareInstance, error) {
	show, err := run("systemctl show nginx.service -p LoadState -p ExecStart -p User --no-pager", 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("读取 nginx.service 失败：%w", err)
	}
	var execStart, masterUser string
	for _, line := range strings.Split(show, "\n") {
		if line == "LoadState=not-found" {
			return nil, errors.New("未找到 nginx.service；旧项目专用或容器内的 nginx 需单独接入")
		}
		if v, ok := strings.CutPrefix(line, "ExecStart="); ok {
			execStart = v
		}
		if v, ok := strings.CutPrefix(line, "User="); ok {
			masterUser = strings.TrimSpace(v)
		}
	}
	match := regexp.MustCompile(`(?:^|[ {])path=([^ ;}]+)`).FindStringSubmatch(execStart)
	if len(match) != 2 || !path.IsAbs(match[1]) || path.Base(match[1]) != "nginx" {
		return nil, errors.New("无法确认 nginx.service 实际执行的 nginx 程序")
	}
	bin := match[1]
	version, err := run(shellQuote(bin)+" -V 2>&1", 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("读取 nginx 编译参数失败：%w", err)
	}
	flags := nginxTokens(execStart)
	value := func(flag, fallback string) string {
		for i, token := range flags {
			if token == flag && i+1 < len(flags) {
				return flags[i+1]
			}
		}
		return fallback
	}
	prefix := value("-p", nginxBuildArg(version, "prefix", "/usr/local/nginx"))
	if !path.IsAbs(prefix) {
		return nil, errors.New("nginx 的运行前缀不是绝对路径")
	}
	prefix = path.Clean(prefix)
	conf := value("-c", nginxBuildArg(version, "conf-path", "conf/nginx.conf"))
	if !path.IsAbs(conf) {
		conf = path.Join(prefix, conf)
	}
	conf = path.Clean(conf)
	h := &proto.NginxHosting{Binary: bin, Prefix: prefix, Config: conf, System: true}
	dump, err := run(shellQuote(bin)+" -T -p "+shellQuote(prefix+"/")+" -c "+shellQuote(conf)+" 2>&1", 15*time.Second)
	if err != nil {
		for _, line := range strings.Split(dump, "\n") {
			if strings.HasPrefix(line, "nginx:") && (strings.Contains(line, "[emerg]") || strings.Contains(line, "[alert]")) {
				return nil, fmt.Errorf("现有 nginx 配置检查未通过：%s", line)
			}
		}
		return nil, fmt.Errorf("现有 nginx 配置检查未通过：%w", err)
	}
	main := nginxDumpFile(dump, conf)
	if main == "" {
		return nil, fmt.Errorf("nginx 未返回主配置 %s 的内容", conf)
	}
	user, sites := nginxHTTPInclude(main, path.Dir(conf))
	if sites == "" {
		return nil, errors.New("主配置的 http 中没有可用的 include *.conf 或 sites-enabled/*；请先配置独立站点目录，或安装 Ferry nginx 实例")
	}
	h.Sites = sites
	if masterUser != "" && masterUser != "root" {
		user = masterUser
	}
	if user == "" {
		user = nginxBuildArg(version, "user", "nobody")
	}
	if user != "root" && !reUserName.MatchString(user) {
		return nil, fmt.Errorf("nginx 运行用户无效：%q", user)
	}
	in := &proto.SoftwareInstance{Name: proto.SystemNginxInstance, Kind: "nginx", Title: "系统 Nginx", Dir: path.Dir(conf), Unit: "nginx.service", User: user, Hosting: h}
	if m := regexp.MustCompile(`nginx/([^\s]+)`).FindStringSubmatch(version); len(m) == 2 {
		in.Version = m[1]
	}
	return in, nil
}

func nginxBuildArg(version, key, fallback string) string {
	re := regexp.MustCompile(`(?:^|\s)--` + regexp.QuoteMeta(key) + `=(?:'([^']*)'|"([^"]*)"|([^\s]+))`)
	if m := re.FindStringSubmatch(version); len(m) > 0 {
		for _, v := range m[1:] {
			if v != "" {
				return v
			}
		}
	}
	return fallback
}

func nginxDumpFile(dump, file string) string {
	marker := "# configuration file " + file + ":\n"
	_, tail, ok := strings.Cut(strings.ReplaceAll(dump, "\r\n", "\n"), marker)
	if !ok {
		return ""
	}
	if next := strings.Index(tail, "\n# configuration file "); next >= 0 {
		tail = tail[:next]
	}
	return tail
}

// 只使用 http 块直属的 include，不能将 server 或 stream 内的目录误作站点目录。
func nginxHTTPInclude(text, confDir string) (user, sites string) {
	var stack, stmt []string
	for _, token := range nginxTokens(text) {
		switch token {
		case "{":
			name := ""
			if len(stmt) > 0 {
				name = stmt[0]
			}
			stack = append(stack, name)
			stmt = nil
		case "}":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			stmt = nil
		case ";":
			if len(stmt) >= 2 && len(stack) == 0 && stmt[0] == "user" {
				user = stmt[1]
			}
			if len(stmt) == 2 && len(stack) == 1 && stack[0] == "http" && stmt[0] == "include" && sites == "" {
				p := stmt[1]
				if !path.IsAbs(p) {
					p = path.Join(confDir, p)
				}
				base, dir := path.Base(p), path.Dir(p)
				if (base == "*.conf" || base == "*") && !strings.ContainsAny(dir, "*?[$\n\r") && path.IsAbs(dir) && dir != "/" {
					sites = path.Clean(dir)
				}
			}
			stmt = nil
		default:
			stmt = append(stmt, token)
		}
	}
	return
}

func (a *Agent) loadHostingInstance(name string) (*proto.SoftwareInstance, error) {
	if name == proto.SystemNginxInstance {
		return DiscoverSystemNginx(a.Sys.Shell)
	}
	return a.loadInstance(name)
}
