package engine

import (
	"crypto/sha256"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

// 由 Ferry 提供的 nginx 运行的静态站点（static.port）：
//   程序   <root>/runtimes/nginx[-1.30]/nginx（静态链接，部署时装好）
//   站点   <running>/www（running 是运行中的版本，里面只有站点文件与它的日志）
//   配置   <root>/.ferry/apps/<app>/nginx.conf（随版本保存，切换、回滚时由那个版本保存的配置装上）
//   日志   <running>/logs（access.log、error.log：和这个版本放在一起）
//   运行   data/run（pid）、data/tmp（临时文件）
//   单元   systemd 服务，运行用户与 java 服务一样（非 root）

// servedStaticSpec 补全 AppSpec：单元、nginx.conf、健康检查、运行用户等。
func servedStaticSpec(f *config.File, t *config.Target, a *config.App, sc *config.Scope, s *proto.AppSpec) error {
	st := &a.Static
	wrap := func(field string, err error) error { return fmt.Errorf("%s static.%s：%w", a.Name, field, err) }
	s.Unit = a.Service.Unit
	s.User = appUser(t, a)
	if st.NginxInstance != "" {
		var err error
		s.NginxInstance, err = sc.Expand(st.NginxInstance)
		if err != nil {
			return err
		}
		s.SiteNames = st.Domains
		s.SitePort = st.Port
		s.DiagLogs = []string{path.Join(sc.Builtins["data"], "logs", "error.log")}
		if len(s.SiteNames) == 0 && st.HTTPS != nil {
			s.SiteNames = st.HTTPS.Domains
		}
		s.Health, err = health(sc, st.Health)
		if err != nil {
			return err
		}
		if len(s.SiteNames) > 0 {
			s.Health.Host = s.SiteNames[0]
		}
		if h := st.HTTPS; h != nil {
			s.TLS = &proto.TLSSpec{Domains: h.Domains, Email: h.Email, CA: h.CAURL(), DNS: h.DNS, Webroot: path.Join(sc.Builtins["data"], "acme-web")}
		}
		s.NginxConf, err = nginxConf(f, t, a, sc)
		return err
	}
	s.SharedDirs = []string{"run", "tmp"}
	s.SitePort = st.Port
	s.RuntimeNginx = st.NginxLink()
	if h := st.HTTPS; h != nil {
		s.SharedDirs = append(s.SharedDirs, "acme-web") // HTTP 验证的文件
		s.TLS = &proto.TLSSpec{Domains: h.Domains, Email: h.Email, CA: h.CAURL(), DNS: h.DNS,
			Webroot: path.Join(sc.Builtins["data"], "acme-web")}
	}
	shared := sc.Builtins["data"]
	s.DiagLogs = []string{path.Join(sc.Builtins["logs"], "error.log")}
	var err error
	if s.Health, err = health(sc, st.Health); err != nil {
		return wrap("health", err)
	}
	if s.NginxConf, err = nginxConf(f, t, a, sc); err != nil {
		return err
	}
	nginx := t.RuntimeNginx(s.RuntimeNginx)
	conf := path.Join(sc.Builtins["root"], ".ferry", "apps", a.Name, "nginx.conf")
	desc := a.Service.Description
	if desc == "" {
		desc = f.Project + " " + a.Name + "（nginx）"
	}
	if desc, err = sc.Expand(desc); err != nil {
		return wrap("description", err)
	}
	_, port := splitListen(st.Port)

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("%s — 由 ferry 生成，手工修改会在下次部署时被覆盖", agent.ManagedMarker)
	line("# project=%s app=%s target=%s", f.Project, a.Name, t.Name)
	line("[Unit]")
	line("Description=%s", oneLine(desc))
	line("After=network-online.target")
	line("Wants=network-online.target")
	line("")
	line("[Service]")
	line("Type=simple")
	line("User=%s", s.User)
	line("WorkingDirectory=%s", sdEscapePercent(shared))
	// 先检查配置；-e stderr：读配置之前的错误也进 journald
	line("ExecStartPre=%s", sdCommand([]string{nginx, "-t", "-q", "-e", "stderr", "-p", shared + "/", "-c", conf}))
	line("ExecStart=%s", sdCommand([]string{nginx, "-e", "stderr", "-p", shared + "/", "-c", conf}))
	line("ExecReload=/bin/kill -HUP $MAINPID")
	low := port < 1024
	if h := st.HTTPS; h != nil {
		if n, _ := strconv.Atoi(h.HTTPSPort()); n < 1024 {
			low = true
		}
	}
	if low {
		line("AmbientCapabilities=CAP_NET_BIND_SERVICE") // 非 root 监听 1024 以下的端口
	}
	line("Restart=on-failure")
	line("RestartSec=5")
	line("TimeoutStopSec=30")
	line("LimitNOFILE=65536")
	line("")
	line("[Install]")
	line("WantedBy=multi-user.target")
	s.UnitText = b.String()
	return nil
}

// splitListen 8081 → ("", 8081)；127.0.0.1:8081 → ("127.0.0.1", 8081)。
func splitListen(p string) (string, int) {
	host := ""
	if i := strings.LastIndexByte(p, ':'); i >= 0 {
		host, p = p[:i], p[i+1:]
	}
	n, _ := strconv.Atoi(p)
	return host, n
}

// nginxConf 生成站点的 nginx.conf。
func nginxConf(f *config.File, t *config.Target, a *config.App, sc *config.Scope) (string, error) {
	st := &a.Static
	shared := sc.Builtins["data"]
	site := path.Join(sc.Builtins["running"], "www")
	logs := sc.Builtins["logs"]
	hosted := st.NginxInstance != ""
	connectionVar := "$connection_upgrade"
	if hosted {
		site = sc.Builtins["running"]
		logs = path.Join(shared, "logs")
		id := sha256.Sum256([]byte(sc.Builtins["root"] + "\x00" + f.Project + "\x00" + a.Name))
		connectionVar = fmt.Sprintf("$ferry_upgrade_%x", id[:8])
	}
	maxBody := st.MaxBody
	if maxBody == "" {
		maxBody = "100m"
	}
	proxy := map[string]string{}
	for p, to := range st.Proxy {
		v, err := sc.Expand(to)
		if err != nil {
			return "", fmt.Errorf("%s static.proxy %s：%w", a.Name, p, err)
		}
		proxy[p] = v
	}
	server, err := sc.ExpandAll(st.Server)
	if err != nil {
		return "", fmt.Errorf("%s static.server：%w", a.Name, err)
	}

	var b strings.Builder
	w := func(indent int, format string, args ...any) {
		b.WriteString(strings.Repeat("    ", indent))
		fmt.Fprintf(&b, format+"\n", args...)
	}
	w(0, "# 由 ferry 生成（project=%s app=%s target=%s），手工修改会在下次部署时被覆盖", f.Project, a.Name, t.Name)
	w(0, "daemon off;") // 由 systemd 管理
	w(0, "worker_processes auto;")
	w(0, "pid %s;", path.Join(shared, "run", "nginx.pid"))
	w(0, "error_log %s warn;", path.Join(logs, "error.log"))
	w(0, "events { worker_connections 4096; }")
	w(0, "http {")
	w(1, "types {")
	for _, t := range mimeTypes {
		w(2, "%s;", t)
	}
	w(1, "}")
	w(1, "default_type application/octet-stream;")
	w(1, "charset utf-8;")
	w(1, "server_tokens off;")
	w(1, "sendfile on;")
	w(1, "tcp_nopush on;")
	w(1, "keepalive_timeout 65;")
	w(1, "client_max_body_size %s;", maxBody)
	w(1, "access_log %s;", path.Join(logs, "access.log"))
	for _, d := range []string{"client_body", "proxy", "fastcgi", "uwsgi", "scgi"} {
		w(1, "%s_temp_path %s;", d, path.Join(shared, "tmp", d))
	}
	w(1, "gzip on;")
	w(1, "gzip_min_length 1k;")
	w(1, "gzip_types text/plain text/css text/xml application/javascript application/json application/xml image/svg+xml;")
	w(1, "gzip_vary on;")
	if len(proxy) > 0 {
		w(1, "map $http_upgrade $connection_upgrade { default upgrade; '' close; }") // WebSocket
	}
	// 站点本身：文件、前端路由、接口转发、额外的配置行（http、https 两个 server 里一样）
	siteBody := func() {
		if hosted {
			w(2, "access_log %s;", path.Join(logs, "access.log"))
			w(2, "error_log %s warn;", path.Join(logs, "error.log"))
			w(2, "client_max_body_size %s;", maxBody)
		}
		w(2, "root %s;", site)
		w(2, "index index.html;")
		if st.IsSPA() {
			w(2, "location / { try_files $uri $uri/ /index.html; }")
		} else {
			w(2, "location / { try_files $uri $uri/ =404; }")
		}
		// 入口页不缓存：部署新版本后浏览器立即拿到新的资源列表
		w(2, "location = /index.html { add_header Cache-Control \"no-cache\"; }")
		paths := make([]string, 0, len(proxy))
		for p := range proxy {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			w(2, "location %s {", p)
			w(3, "proxy_pass %s;", proxy[p])
			w(3, "proxy_http_version 1.1;")
			w(3, "proxy_set_header Host $http_host;")
			w(3, "proxy_set_header X-Real-IP $remote_addr;")
			w(3, "proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;")
			w(3, "proxy_set_header X-Forwarded-Proto $scheme;")
			w(3, "proxy_set_header Upgrade $http_upgrade;")
			w(3, "proxy_set_header Connection %s;", connectionVar)
			w(3, "proxy_read_timeout 300s;")
			w(3, "proxy_buffering off;") // 流式返回（SSE、大模型逐字输出）不攒到最后才发
			w(2, "}")
		}
		for _, l := range server {
			w(2, "%s", oneLine(l))
		}
	}
	w(1, "server {")
	listen := st.Port
	names := st.Domains
	if len(names) == 0 && st.HTTPS != nil {
		names = st.HTTPS.Domains
	}
	if hosted && len(names) == 0 {
		listen += " default_server"
	}
	w(2, "listen %s;", listen)
	if len(names) > 0 {
		w(2, "server_name %s;", strings.Join(names, " "))
	} else {
		w(2, "server_name _;")
	}
	h := st.HTTPS
	switch {
	case h == nil:
		siteBody()
	default:
		// 证书服务来取验证文件（HTTP 验证）；其余的跳到 https，或照常是站点
		w(2, "location ^~ /.well-known/acme-challenge/ { root %s; default_type text/plain; }", path.Join(shared, "acme-web"))
		if h.IsRedirect() {
			suffix := ""
			if h.HTTPSPort() != "443" {
				suffix = ":" + h.HTTPSPort()
			}
			w(2, "location / { return 301 https://$host%s$request_uri; }", suffix)
		} else {
			siteBody()
		}
	}
	w(1, "}")
	if h != nil {
		tls := path.Join(sc.Builtins["root"], ".ferry", "apps", a.Name, "tls")
		host, _ := splitListen(st.Port)
		listen := h.HTTPSPort()
		if host != "" {
			listen = host + ":" + listen
		}
		w(1, "server {")
		w(2, "listen %s ssl;", listen)
		if !hosted {
			w(2, "http2 on;")
		}
		if len(names) > 0 {
			w(2, "server_name %s;", strings.Join(names, " "))
		} else {
			w(2, "server_name _;")
		}
		w(2, "ssl_certificate %s;", path.Join(tls, "fullchain.pem"))
		w(2, "ssl_certificate_key %s;", path.Join(tls, "privkey.pem"))
		w(2, "ssl_protocols TLSv1.2 TLSv1.3;")
		w(2, "ssl_session_cache shared:SSL:10m;")
		w(2, "ssl_session_timeout 1d;")
		siteBody()
		w(1, "}")
	}
	w(0, "}")
	if hosted {
		text := b.String()
		start := strings.Index(text, "    server {\n")
		if start < 0 {
			return "", fmt.Errorf("没有生成站点配置")
		}
		header := "# managed by ferry; project=" + f.Project + " app=" + a.Name + "\n"
		if len(proxy) > 0 {
			// 站点片段自带独立变量，不依赖或覆盖已有 nginx 主配置里的 map。
			header += fmt.Sprintf("map $http_upgrade %s { default upgrade; '' close; }\n", connectionVar)
		}
		return header + strings.TrimSuffix(text[start:], "}\n"), nil
	}
	return b.String(), nil
}

// mimeTypes 常用的类型（静态 nginx 没有带 mime.types 文件）。
var mimeTypes = []string{
	"text/html html htm shtml",
	"text/css css",
	"text/xml xml",
	"text/plain txt",
	"application/javascript js mjs",
	"application/json json map",
	"application/manifest+json webmanifest",
	"application/wasm wasm",
	"application/pdf pdf",
	"application/zip zip",
	"application/vnd.ms-excel xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet xlsx",
	"application/msword doc",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document docx",
	"image/png png",
	"image/jpeg jpeg jpg",
	"image/gif gif",
	"image/svg+xml svg svgz",
	"image/webp webp",
	"image/avif avif",
	"image/x-icon ico",
	"font/woff woff",
	"font/woff2 woff2",
	"font/ttf ttf",
	"font/otf otf",
	"application/vnd.ms-fontobject eot",
	"audio/mpeg mp3",
	"video/mp4 mp4",
	"video/webm webm",
}
