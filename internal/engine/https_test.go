package engine

import (
	"strings"
	"testing"
)

func TestHTTPSSpec(t *testing.T) {
	f, err := loadYAML(t, `
version: 1
project: shop
targets:
  prod: {host: 10.0.0.1, runAs: shop}
apps:
  - name: web
    type: static
    artifact: {path: dist}
    static:
      port: 80
      proxy: {/api/: "http://127.0.0.1:8080/"}
      https: {domains: [WWW.Example.com, example.com], email: ops@example.com}
  - name: admin
    type: static
    artifact: {path: admin}
    static:
      port: 8081
      https: {domains: ["*.example.com"], dns: aliyun, port: 8443, redirect: false, ca: letsencrypt-staging}
`)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("prod")
	s, err := Spec(f, tg, f.App("web"))
	if err != nil {
		t.Fatal(err)
	}
	if s.TLS == nil || s.TLS.Domains[0] != "www.example.com" || s.TLS.CA != "https://acme-v02.api.letsencrypt.org/directory" ||
		s.TLS.Webroot != "/opt/shop/apps/web/data/acme-web" || !contains(s.SharedDirs, "acme-web") {
		t.Fatalf("%+v %v", s.TLS, s.SharedDirs)
	}
	conf := s.NginxConf
	for _, want := range []string{
		"location ^~ /.well-known/acme-challenge/ { root /opt/shop/apps/web/data/acme-web;",
		"location / { return 301 https://$host$request_uri; }",
		"listen 443 ssl;", "http2 on;",
		"ssl_certificate /opt/shop/.ferry/apps/web/tls/fullchain.pem;",
		"ssl_certificate_key /opt/shop/.ferry/apps/web/tls/privkey.pem;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("缺 %q：\n%s", want, conf)
		}
	}
	// 站点和接口转发在 https 的 server 里（http 的只跳转）
	https := conf[strings.Index(conf, "listen 443 ssl;"):]
	if !strings.Contains(https, "location /api/ {") || !strings.Contains(https, "try_files $uri $uri/ /index.html;") {
		t.Errorf("https 里要有站点：\n%s", https)
	}
	if strings.Count(conf, "location /api/ {") != 1 {
		t.Error("跳转时 http 里不再放站点")
	}
	if !strings.Contains(s.UnitText, "AmbientCapabilities=CAP_NET_BIND_SERVICE") {
		t.Error("监听 80、443 要能绑定低端口")
	}

	a, err := Spec(f, tg, f.App("admin"))
	if err != nil {
		t.Fatal(err)
	}
	if a.TLS.DNS != "aliyun" || a.TLS.CA != "https://acme-staging-v02.api.letsencrypt.org/directory" {
		t.Fatalf("%+v", a.TLS)
	}
	if !strings.Contains(a.NginxConf, "listen 8443 ssl;") || strings.Contains(a.NginxConf, "return 301") || strings.Count(a.NginxConf, "try_files") != 2 {
		t.Errorf("不跳转时 http、https 都是站点：\n%s", a.NginxConf)
	}
	if strings.Contains(a.UnitText, "AmbientCapabilities") {
		t.Error("8081、8443 不用低端口权限")
	}
}

func TestHTTPSConfigErrors(t *testing.T) {
	for st, want := range map[string]string{
		`{port: 80, https: {}}`:                                        "domains 要写",
		`{port: 80, https: {domains: [bad_domain]}}`:                   "不是域名",
		`{port: 80, https: {domains: ["*.a.com"]}}`:                    "通配符域名",
		`{port: 8081, https: {domains: [a.com]}}`:                      "HTTP 验证要从外网访问 80 端口",
		`{port: 80, https: {domains: [a.com], dns: godaddy}}`:          "dns 只能是",
		`{port: 80, https: {domains: [a.com], port: "80"}}`:            "和 static.port 不一样",
		`{port: 80, https: {domains: [a.com], email: x}}`:              "不是邮箱",
		`{port: 80, https: {domains: [a.com], ca: "http://ca/dir"}}`:   "ca 写",
		`{link: /srv/www/x, https: {domains: [a.com]}}`:                "只在设置了 static.port",
		`{port: 80, https: {domains: [a.com], dns: cloudflare, x: 1}}`: "x",
	} {
		_, err := loadYAML(t, `
version: 1
project: shop
targets:
  prod: {host: 10.0.0.1, runAs: shop}
apps:
  - name: web
    type: static
    artifact: {path: dist}
    static: `+st+`
`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s：期望含 %q，得到 %v", st, want, err)
		}
	}
}
