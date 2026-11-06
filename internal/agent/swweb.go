package agent

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 软件的网页：控制台、管理面板在哪个端口、什么路径（「软件」页「更多 → 打开网页」用）。
// 端口、路径能从命令行、配置文件、环境变量读到的按读到的；读不到按默认值，
// 默认端口不在监听的端口里时（改过端口又没读到），从监听的端口里挑。

// webSrc 认网页用的进程信息。
type webSrc struct {
	exe       string
	container string // 在容器里（docker exec 用的容器 ID）
	args      []string
	root      string                   // 进程的根目录（/proc/<pid>/root）：容器里的配置文件也能读；空 = 读不了
	env       func() map[string]string // 进程的环境变量（用到才读）
	ports     []int                    // 监听的 TCP 端口（容器自己的网络时是容器里的端口）
}

// webOf 一个软件的网页。p 为空时（只从镜像认出的容器）只按端口猜。
func (sc *swScan) webOf(kind string, p *swProc, listen []string) []proto.WebPanel {
	src := webSrc{ports: tcpPorts(listen), env: func() map[string]string { return nil }}
	if p != nil {
		dir := filepath.Join(sc.proc, strconv.Itoa(p.pid))
		src.exe, src.args, src.root, src.container = p.exe, p.args, filepath.Join(dir, "root"), p.container
		var env map[string]string
		src.env = func() map[string]string {
			if env == nil {
				env = readEnviron(filepath.Join(dir, "environ"))
			}
			return env
		}
	}
	return sc.webPanels(kind, src)
}

func (sc *swScan) webPanels(kind string, s webSrc) []proto.WebPanel {
	one := func(label, scheme string, port int, p, note string) []proto.WebPanel {
		if port <= 0 {
			return nil
		}
		if scheme == "" {
			scheme = "http"
		}
		return []proto.WebPanel{{Label: label, Scheme: scheme, Port: port, Path: webPath(p), Note: note}}
	}
	// 默认端口：在监听就用；没在监听的（进程还没起来、读不到端口）也用
	dflt := func(port int) int {
		if len(s.ports) == 0 || hasInt(s.ports, port) {
			return port
		}
		return 0
	}
	switch kind {
	case "nacos":
		return sc.nacosWeb(s)
	case "sentinel":
		port, set := s.springPort(nil, "server.port")
		if !set {
			port = dflt(8080)
			if port == 0 && len(s.ports) > 0 {
				port = s.ports[0] // 另一个是 port+1 的规则推送端口（csp.sentinel.api.port），Java 先监听的是网页端口
				for _, p := range s.ports {
					if !hasInt(s.ports, p-1) {
						port = p
						break
					}
				}
			}
		}
		return one("控制台", "", port, s.spring(nil, "server.servlet.context-path"), "默认账号 sentinel / sentinel")
	case "seata":
		port, set := s.springPort(nil, "server.port")
		if !set {
			port = dflt(7091)
		}
		return one("控制台", "", port, s.spring(nil, "server.servlet.context-path"), "默认账号 seata / seata")
	case "xxljob":
		port, set := s.springPort(nil, "server.port")
		if !set {
			port = dflt(8080)
			if port == 0 && len(s.ports) == 1 {
				port = s.ports[0]
			}
		}
		p := s.spring(nil, "server.servlet.context-path")
		if p == "" {
			p = "/xxl-job-admin"
		}
		return one("调度中心", "", port, p, "默认账号 admin / 123456")
	case "minio":
		if v := argVal(s.args, "--console-address"); v != "" {
			return one("控制台", "", addrPort(v), "", "用 MinIO 的账号（MINIO_ROOT_USER）登录")
		}
		api := 9000
		if v := argVal(s.args, "--address"); v != "" {
			api = addrPort(v)
		}
		for _, p := range s.ports {
			if p != api {
				return one("控制台", "", p, "", "用 MinIO 的账号（MINIO_ROOT_USER）登录")
			}
		}
	case "rabbitmq":
		if hasInt(s.ports, 15672) {
			return one("管理页面", "", 15672, "", "默认账号 guest / guest 只能在服务器本机登录（经 SSH 转发打开可以）")
		}
	case "emqx":
		if hasInt(s.ports, 18083) {
			return one("控制台", "", 18083, "", "默认账号 admin / public")
		}
	case "grafana":
		port := atoiOr(s.env()["GF_SERVER_HTTP_PORT"], 0)
		if port == 0 {
			port = dflt(3000)
		}
		return one("Grafana", strings.ToLower(s.env()["GF_SERVER_PROTOCOL"]), port, "", "默认账号 admin / admin")
	case "prometheus":
		port := dflt(9090)
		if v := argVal(s.args, "--web.listen-address"); v != "" {
			port = addrPort(v)
		}
		return one("Prometheus", "", port, argVal(s.args, "--web.route-prefix"), "")
	case "jenkins":
		port, scheme := dflt(8080), ""
		if v := argVal(s.args, "--httpPort"); v != "" {
			port = atoiOr(v, port)
		}
		if v := argVal(s.args, "--httpsPort"); v != "" && (port <= 0 || argVal(s.args, "--httpPort") == "-1") {
			port, scheme = atoiOr(v, 0), "https"
		}
		return one("Jenkins", scheme, port, argVal(s.args, "--prefix"), "")
	case "1panel":
		if path.Base(s.exe) == "1panel-agent" {
			return nil // 1Panel v2 的 agent 没有网页；面板在 1panel-core
		}
		return sc.onePanelWeb(s)
	case "bt":
		return btWeb(s)
	case "nginx", "openresty", "httpd", "haproxy", "tomcat":
		if kind == "nginx" || kind == "openresty" {
			if ws := sc.nginxWeb(s); len(ws) > 0 {
				return ws
			}
		}
		var out []proto.WebPanel
		for _, p := range s.ports {
			if len(out) == 6 {
				break
			}
			scheme := "http"
			if p == 443 || p == 8443 {
				scheme = "https"
			}
			out = append(out, proto.WebPanel{Label: fmt.Sprintf("网站 :%d", p), Scheme: scheme, Port: p, Path: "/"})
		}
		return out
	}
	return nil
}

// nginxWeb nginx 上的网站：按 nginx -T 的 server 块列，有域名的按域名（同一个端口上的几个网站分得开）。
// 容器里的 nginx 用 docker exec 运行。读不到配置时返回空（按端口列）。
func (sc *swScan) nginxWeb(s webSrc) []proto.WebPanel {
	if s.exe == "" {
		return nil
	}
	flags := nginxConfigFlags(nginxProcessArgs(s.args, s.exe))
	cmd := nginxInvocation{binary: s.exe, flags: flags}.command("-T") + " 2>&1"
	if s.container != "" {
		cmd = "docker exec " + shellQuote(s.container) + " " + cmd
	}
	out, err := sc.run(cmd, 10*time.Second)
	if err != nil || !strings.Contains(out, "# configuration file ") {
		return nil
	}
	var web []proto.WebPanel
	seen := map[string]bool{}
	for _, sv := range parseNginx(out) {
		name := ""
		for _, n := range sv.names {
			// 通配符、正则、IP 不能当地址用
			if strings.ContainsAny(n, "*~") || net.ParseIP(n) != nil || n == "localhost" {
				continue
			}
			name = strings.ToLower(n)
			break
		}
		for _, l := range sv.listen {
			if strings.HasPrefix(l, "unix:") {
				continue
			}
			port := addrPort(l)
			if port <= 0 || (len(s.ports) > 0 && !hasInt(s.ports, port)) {
				continue
			}
			scheme := "http"
			if sv.ssl || port == 443 {
				scheme = "https"
			}
			key := fmt.Sprintf("%s|%d", name, port)
			if seen[key] {
				continue
			}
			seen[key] = true
			label := fmt.Sprintf("网站 :%d", port)
			if name != "" {
				label = "网站 " + name
				if !(scheme == "http" && port == 80) && !(scheme == "https" && port == 443) {
					label += fmt.Sprintf(":%d", port)
				}
			}
			web = append(web, proto.WebPanel{Label: label, Scheme: scheme, Port: port, Path: "/", Host: name})
			break // 一个 server 块列一次（多个 listen 时取第一个）
		}
		if len(web) >= 20 {
			break
		}
	}
	return web
}

// nacosWeb Nacos 3.x 的控制台单独一个端口（nacos.console.port，默认 8080，路径 /）；
// 2.x 在主端口上（server.port，默认 8848，路径 /nacos）。
func (sc *swScan) nacosWeb(s webSrc) []proto.WebPanel {
	var files []string
	if v := argVal(s.args, "--spring.config.additional-location", "--spring.config.location"); v != "" {
		for _, f := range strings.Split(v, ",") {
			f = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(f), "optional:"), "file:")
			if strings.HasSuffix(f, "/") {
				f += "application.properties"
			}
			if strings.HasSuffix(f, ".properties") {
				files = append(files, f)
			}
		}
	}
	if home := argVal(s.args, "-Dnacos.home"); home != "" {
		files = append(files, path.Join(home, "conf", "application.properties"))
	}
	if len(files) == 0 && s.root != "" {
		files = append(files, "/home/nacos/conf/application.properties") // 官方镜像
	}
	props := s.readProps(files)
	v3 := props["nacos.console.port"] != "" || props["nacos.server.main.port"] != "" || argVal(s.args, "-Dnacos.deployment.type") != ""
	if v3 {
		port, set := s.springPort(props, "nacos.console.port")
		if !set {
			port = 8080
		}
		p := s.spring(props, "nacos.console.contextPath")
		return []proto.WebPanel{{Label: "控制台", Scheme: "http", Port: port, Path: webPath(p), Note: "账号 nacos（3.x 第一次打开时要设置密码）"}}
	}
	main := 8848
	if p, set := s.springPort(props, "server.port"); set {
		main = p
	}
	// 读不到配置时：2.x 只监听主端口和 +1000、+1001（gRPC）、-1000（集群）；还有别的端口的多半是 3.x 的控制台
	if len(props) == 0 && len(s.ports) > 0 {
		if hasInt(s.ports, 8080) {
			return []proto.WebPanel{{Label: "控制台", Scheme: "http", Port: 8080, Path: "/", Note: "账号 nacos"}}
		}
		for _, p := range s.ports {
			if hasInt(s.ports, p+1000) && hasInt(s.ports, p+1001) {
				main = p
				break
			}
		}
		for _, p := range s.ports {
			if p != main && p != main+1000 && p != main+1001 && p != main-1000 {
				return []proto.WebPanel{{Label: "控制台", Scheme: "http", Port: p, Path: "/", Note: "账号 nacos"}}
			}
		}
	}
	p := s.spring(props, "server.servlet.contextPath", "server.servlet.context-path")
	if p == "" {
		p = "/nacos"
	}
	return []proto.WebPanel{{Label: "控制台", Scheme: "http", Port: main, Path: webPath(p), Note: "默认账号 nacos / nacos（没开鉴权时不用登录）"}}
}

var (
	reOnePanelURL  = regexp.MustCompile(`(https?)://[^\s/]+?:(\d+)(/[^\s]*)?`)
	reOnePanelUser = regexp.MustCompile(`(?mi)^\s*(?:面板用户|panel user(?:name)?)\s*[:：]\s*(\S+)`)
)

// onePanelWeb 1Panel 的地址和安全入口：1pctl user-info 输出「面板地址: http://$LOCAL_IP:10001/入口」。
func (sc *swScan) onePanelWeb(s webSrc) []proto.WebPanel {
	port := 0
	if len(s.ports) > 0 {
		port = s.ports[0]
	}
	w := proto.WebPanel{Label: "面板", Scheme: "http", Port: port, Path: "/",
		Note: "设置了安全入口时要在地址后面加上入口（在服务器上运行 1pctl user-info 查看）"}
	if out, err := sc.run("1pctl user-info 2>&1", 10*time.Second); err == nil {
		if m := reOnePanelURL.FindStringSubmatch(out); m != nil {
			w.Scheme, w.Port, w.Path = m[1], atoiOr(m[2], port), webPath(m[3])
			w.Note = "忘了密码可以在服务器上运行 1pctl update password"
			if u := reOnePanelUser.FindStringSubmatch(out); u != nil {
				w.Note = "账号 " + u[1] + "；" + w.Note
			}
		}
	}
	if w.Port <= 0 {
		return nil
	}
	return []proto.WebPanel{w}
}

// btWeb 宝塔面板：端口、安全入口、是否 https 都记在 /www/server/panel/data 下的文件里。
func btWeb(s webSrc) []proto.WebPanel {
	data := "/www/server/panel/data/"
	port := atoiOr(s.readFile(data+"port.pl"), 8888)
	scheme := "http"
	if _, err := os.Stat(s.at(data + "ssl.pl")); err == nil {
		scheme = "https"
	}
	return []proto.WebPanel{{Label: "面板", Scheme: scheme, Port: port, Path: webPath(s.readFile(data + "admin_path.pl")),
		Note: "账号密码在服务器上运行 bt default 查看"}}
}

// instanceWeb Ferry 装的软件：端口按安装时的参数（比读配置可靠）。
func instanceWeb(in proto.SoftwareInstance) []proto.WebPanel {
	note := ""
	if in.Account != "" {
		note = "账号 " + in.Account + "，密码在「更多 → 密码」里看"
	}
	p := in.Ports
	switch in.Kind {
	case "nacos":
		if p["console"] > 0 {
			return []proto.WebPanel{{Label: "控制台", Scheme: "http", Port: p["console"], Path: "/", Note: firstNonEmpty(note, "账号 nacos，密码在「更多 → 密码」里看")}}
		}
	case "minio":
		if p["console"] > 0 {
			return []proto.WebPanel{{Label: "控制台", Scheme: "http", Port: p["console"], Path: "/", Note: note}}
		}
	case "sentinel":
		if p["port"] > 0 {
			return []proto.WebPanel{{Label: "控制台", Scheme: "http", Port: p["port"], Path: "/", Note: note}}
		}
	case "zlmediakit":
		if p["https"] > 0 {
			return []proto.WebPanel{{Label: "WebRTC 测试页", Scheme: "https", Port: p["https"], Path: "/webrtc/", Note: "用的是自签名证书，浏览器会提示不安全，选继续访问"}}
		}
	}
	return nil
}

// mapWeb 容器用自己的网络：网页的端口换成映射到主机的端口；没映射的只记容器里的端口。
func mapWeb(web []proto.WebPanel, ports map[int]int) {
	for i := range web {
		w := &web[i]
		if w.Port <= 0 {
			continue
		}
		if h, ok := ports[w.Port]; ok {
			w.Port = h
		} else {
			w.Inner, w.Port = w.Port, 0
		}
	}
}

// markLocal 只在 127.0.0.1 上监听的网页：这台电脑直接连不上。
func markLocal(it *proto.Software) {
	for i := range it.Web {
		w := &it.Web[i]
		if w.Port <= 0 {
			continue
		}
		seen, local := false, true
		for _, l := range it.Listen {
			j := strings.LastIndexByte(l, ':')
			if j < 0 || strings.Contains(l[j:], "/") || atoiOr(l[j+1:], 0) != w.Port {
				continue
			}
			seen = true
			h := strings.Trim(l[:j], "[]")
			if !(strings.HasPrefix(h, "127.") || h == "::1" || h == "localhost") {
				local = false
			}
		}
		w.Local = seen && local
	}
}

// dockerPortMap docker ps 的端口映射：容器里的 TCP 端口 → 主机端口（同一端口映射了几次取第一个）。
func dockerPortMap(s string) map[int]int {
	out := map[int]int{}
	for _, part := range strings.Split(s, ",") {
		host, inner, ok := strings.Cut(strings.TrimSpace(part), "->")
		if !ok {
			continue
		}
		p, pr, _ := strings.Cut(inner, "/")
		if pr != "" && pr != "tcp" {
			continue
		}
		i := strings.LastIndexByte(host, ':')
		if i < 0 {
			continue
		}
		ip, hp := atoiOr(p, 0), atoiOr(host[i+1:], 0)
		if _, dup := out[ip]; !dup && ip > 0 && hp > 0 {
			out[ip] = hp
		}
	}
	return out
}

// ---------------------------------------------------------------- 读端口、配置

// spring Spring 的配置项：命令行（--key=、-Dkey=）> 环境变量（KEY_NAME）> 配置文件。
func (s webSrc) spring(props map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := argVal(s.args, "--"+k, "-D"+k); v != "" {
			return v
		}
	}
	env := s.env()
	for _, k := range keys {
		if v := env[relaxedEnv(k)]; v != "" {
			return v
		}
	}
	for _, k := range keys {
		if v := props[k]; v != "" {
			return v
		}
	}
	return ""
}

// springPort 读端口；set 为 false 表示没有配置（用默认值）。
func (s webSrc) springPort(props map[string]string, keys ...string) (int, bool) {
	p := atoiOr(s.spring(props, keys...), 0)
	return p, p > 0
}

func relaxedEnv(k string) string {
	return strings.ToUpper(strings.NewReplacer(".", "_", "-", "").Replace(k))
}

// at 进程看到的路径在主机上的位置。
func (s webSrc) at(p string) string {
	if s.root == "" {
		return ""
	}
	return filepath.Join(s.root, filepath.FromSlash(p))
}

func (s webSrc) readFile(p string) string {
	if s.root == "" {
		return ""
	}
	b, err := os.ReadFile(s.at(p))
	if err != nil || len(b) > 64<<10 {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readProps 读 .properties（后面的文件覆盖前面的），${名字:默认值} 换成环境变量、其他配置项或默认值。
func (s webSrc) readProps(files []string) map[string]string {
	props := map[string]string{}
	for _, f := range files {
		if s.root == "" {
			break
		}
		fh, err := os.Open(s.at(f))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || line[0] == '#' || line[0] == '!' {
				continue
			}
			i := strings.IndexAny(line, "=:")
			if i <= 0 {
				continue
			}
			props[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
		fh.Close()
	}
	for k, v := range props {
		props[k] = s.resolve(v, props, 0)
	}
	return props
}

var rePlaceholder = regexp.MustCompile(`\$\{([^}:]+)(?::([^}]*))?\}`)

func (s webSrc) resolve(v string, props map[string]string, depth int) string {
	if depth > 3 || !strings.Contains(v, "${") {
		return v
	}
	return rePlaceholder.ReplaceAllStringFunc(v, func(m string) string {
		g := rePlaceholder.FindStringSubmatch(m)
		env := s.env()
		if x := env[g[1]]; x != "" {
			return x
		}
		if x := env[relaxedEnv(g[1])]; x != "" {
			return x
		}
		if x, ok := props[g[1]]; ok && x != v {
			return s.resolve(x, props, depth+1)
		}
		return g[2]
	})
}

func readEnviron(file string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(file)
	if err != nil {
		return out
	}
	for _, kv := range strings.Split(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// argVal 命令行参数的值：name=value 或 name value（-D 开头的只认 =）。
func argVal(args []string, names ...string) string {
	for _, n := range names {
		for i, a := range args {
			if v, ok := strings.CutPrefix(a, n+"="); ok {
				return strings.Trim(v, `"'`)
			}
			if a == n && !strings.HasPrefix(n, "-D") && i+1 < len(args) {
				return strings.Trim(args[i+1], `"'`)
			}
		}
	}
	return ""
}

// addrPort ":9001"、"0.0.0.0:9001"、"9001" 的端口。
func addrPort(v string) int {
	if i := strings.LastIndexByte(v, ':'); i >= 0 {
		v = v[i+1:]
	}
	return atoiOr(v, 0)
}

// tcpPorts 监听地址（*:80、127.0.0.1:6379、*:53/udp）里的 TCP 端口，去重，保持顺序。
func tcpPorts(listen []string) []int {
	var out []int
	for _, l := range listen {
		if strings.Contains(l, "/") {
			continue
		}
		if p := addrPort(l); p > 0 && !hasInt(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func webPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func hasInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func atoiOr(s string, d int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return d
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
