package engine

import (
	"crypto/sha256"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

func backendHTTPSSpec(f *config.File, t *config.Target, a *config.App, sc *config.Scope, s *proto.AppSpec) error {
	h := a.HTTPS
	meta := path.Join(t.Root, ".ferry", "apps", a.Name)
	port, _ := strconv.Atoi(h.HTTPSPort())
	p := &proto.BackendHTTPS{
		Port: port, HTTP: h.HasHTTP(), Instance: h.NginxInstance, Cert: h.Cert, Key: h.Key,
		TLS: proto.TLSSpec{Domains: h.Domains, Email: h.Email, DNS: h.DNS, CA: h.CAURL(), Webroot: path.Join(meta, "acme-web")},
	}
	if s.ZD == nil {
		address, err := sc.Expand(h.Upstream)
		if err != nil {
			return fmt.Errorf("%s https.upstream：%w", a.Name, err)
		}
		p.Upstream, err = config.HTTPSUpstream(address, s.Health.TCP, s.Health.HTTP)
		if err != nil {
			return fmt.Errorf("%s：%w", a.Name, err)
		}
		host, _, _ := net.SplitHostPort(p.Upstream)
		ip := net.ParseIP(host)
		_, upstreamPort := splitListen(p.Upstream)
		if (strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()) &&
			(upstreamPort == port || p.HTTP && upstreamPort == 80) {
			return fmt.Errorf("%s：HTTPS 代理的监听端口与后端端口 %d 冲突", a.Name, upstreamPort)
		}
	} else {
		p.Upstream = "127.0.0.1:" + proto.ProxyPort
	}
	listen := ""
	if p.Instance == "" {
		p.Unit = a.HTTPSUnit()
		p.Prefix = path.Join(meta, "https-nginx")
		conf := path.Join(meta, "https.conf")
		if s.RuntimeNginx == "" {
			s.RuntimeNginx = (&config.Static{}).NginxLink()
		}
		p.Binary = t.RuntimeNginx(s.RuntimeNginx)
		if s.ZD != nil {
			listen = a.Service.ZeroDowntime.Listen
			if host, port := splitListen(listen); host == "" && port == 80 {
				listen = "80"
			}
			conf = path.Join(meta, "proxy.conf")
		}
		_, plainPort := splitListen(listen)
		p.UnitText = proxyUnit(f, t, a, s.User, p.Binary, conf, p.Prefix, port < 1024 || p.HTTP || (plainPort > 0 && plainPort < 1024))
		if s.ZD != nil && s.ZD.ProxyUnit == "" {
			// 同一个单元和配置路径，切换时只更新正在运行的槽位。
			s.ZD.ProxyUnit = p.Unit
			s.ZD.ProxyUnitText = p.UnitText
			s.ZD.ProxyConf = "# HTTPS 由后端代理配置生成\n# " + proto.ProxyPort
		}
	}
	p.Config = backendHTTPSConfig(f, t, a, p, meta, listen)
	s.HTTPS = p
	return nil
}

func backendHTTPSConfig(f *config.File, t *config.Target, a *config.App, p *proto.BackendHTTPS, meta, listen string) string {
	h := a.HTTPS
	var b strings.Builder
	w := func(indent int, format string, args ...any) {
		b.WriteString(strings.Repeat("    ", indent))
		fmt.Fprintf(&b, format+"\n", args...)
	}
	w(0, "# managed by ferry; project=%s app=%s target=%s", f.Project, a.Name, t.Name)
	if p.Instance == "" {
		w(0, "daemon off;")
		w(0, "worker_processes auto;")
		w(0, "pid %s;", nginxString(path.Join(p.Prefix, "run", "nginx.pid")))
		w(0, "error_log stderr warn;")
		w(0, "events { worker_connections 4096; }")
		w(0, "http {")
		w(1, "server_tokens off;")
		w(1, "access_log off;")
		for _, d := range []string{"client_body", "proxy", "fastcgi", "uwsgi", "scgi"} {
			w(1, "%s_temp_path %s;", d, nginxString(path.Join(p.Prefix, "tmp", d)))
		}
	}
	connection := ""
	if h.WebSocket == nil || *h.WebSocket {
		id := sha256.Sum256([]byte(t.Root + "\x00" + f.Project + "\x00" + a.Name))
		connection = fmt.Sprintf("$ferry_https_upgrade_%x", id[:8])
		w(1, "map $http_upgrade %s { default upgrade; '' close; }", connection)
	}
	maxBody := h.MaxBody
	if maxBody == "" {
		maxBody = "100m"
	}
	proxy := func() {
		w(2, "client_max_body_size %s;", maxBody)
		proxyLocation(w, "/", "http://"+proto.BackendHTTPSUpstream, connection)
	}
	if p.HTTP {
		w(1, "server {")
		w(2, "listen 80;")
		w(2, "server_name %s;", strings.Join(h.Domains, " "))
		if h.Cert == "" && h.DNS == "" {
			w(2, "location ^~ /.well-known/acme-challenge/ { root %s; default_type text/plain; }", nginxString(p.TLS.Webroot))
		}
		if h.IsRedirect() {
			suffix := ""
			if p.Port != 443 {
				suffix = ":" + strconv.Itoa(p.Port)
			}
			w(2, "location / { return 301 https://$host%s$request_uri; }", suffix)
		} else {
			proxy()
		}
		w(1, "}")
	}
	if listen != "" && !(listen == "80" && p.HTTP) {
		w(1, "server {")
		w(2, "listen %s;", listen)
		proxy()
		w(1, "}")
	}
	certDir := "tls"
	if h.Cert != "" {
		certDir = "tls-custom"
	}
	cert, key := path.Join(meta, certDir, "fullchain.pem"), path.Join(meta, certDir, "privkey.pem")
	w(1, "server {")
	w(2, "listen %d ssl;", p.Port)
	w(2, "server_name %s;", strings.Join(h.Domains, " "))
	w(2, "ssl_certificate %s;", nginxString(cert))
	w(2, "ssl_certificate_key %s;", nginxString(key))
	w(2, "ssl_protocols TLSv1.2 TLSv1.3;")
	proxy()
	w(1, "}")
	if p.Instance == "" {
		w(0, "}")
	}
	return b.String()
}

func nginxString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`).Replace(s) + `"`
}
