package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

const servedYAML = `
version: 1
project: shop
targets:
  t1: {host: 10.0.0.1, root: /data/shop, runAs: web}
apps:
  - name: admin
    type: static
    artifact: {path: dist}
    static:
      port: 8081
      proxy:
        /prod-api/: http://127.0.0.1:8080/
      server:
        - "location /files/ { alias /data/files/; }"
  - name: low
    type: static
    artifact: {path: dist2}
    static: {port: "127.0.0.1:80", nginx: "1.30", spa: false, maxBody: 20m}
`

func loadYAML(t *testing.T, text string) (*config.File, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), config.DefaultFileName)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Load(p)
}

func TestServedStaticSpec(t *testing.T) {
	f, err := loadYAML(t, servedYAML)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("t1")
	s, err := Spec(f, tg, f.App("admin"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Unit != "shop-admin.service" || s.User != "web" || s.RuntimeNginx != "nginx" || s.Health.HTTP != "http://127.0.0.1:8081/" {
		t.Fatalf("%+v", s)
	}
	for _, want := range []string{
		"ExecStartPre=/data/shop/runtimes/nginx/nginx -t -q -e stderr -p /data/shop/apps/admin/data/ -c /data/shop/.ferry/apps/admin/nginx.conf",
		"ExecStart=/data/shop/runtimes/nginx/nginx -e stderr -p /data/shop/apps/admin/data/ -c /data/shop/.ferry/apps/admin/nginx.conf",
		"User=web",
	} {
		if !strings.Contains(s.UnitText, want) {
			t.Errorf("单元缺少 %q：\n%s", want, s.UnitText)
		}
	}
	if strings.Contains(s.UnitText, "CAP_NET_BIND_SERVICE") {
		t.Error("8081 不需要特权端口能力")
	}
	for _, want := range []string{
		"daemon off;",
		"pid /data/shop/apps/admin/data/run/nginx.pid;",
		"error_log /data/shop/apps/admin/running/logs/error.log warn;",
		"access_log /data/shop/apps/admin/running/logs/access.log;",
		"listen 8081;",
		"root /data/shop/apps/admin/running/www;",
		"try_files $uri $uri/ /index.html;",
		"location /prod-api/ {",
		"proxy_pass http://127.0.0.1:8080/;",
		"location /files/ { alias /data/files/; }",
		"client_max_body_size 100m;",
		"proxy_temp_path /data/shop/apps/admin/data/tmp/proxy;",
	} {
		if !strings.Contains(s.NginxConf, want) {
			t.Errorf("nginx.conf 缺少 %q：\n%s", want, s.NginxConf)
		}
	}
	if strings.Count(s.NginxConf, "{") != strings.Count(s.NginxConf, "}") {
		t.Errorf("括号不成对：\n%s", s.NginxConf)
	}

	low, err := Spec(f, tg, f.App("low"))
	if err != nil {
		t.Fatal(err)
	}
	if low.RuntimeNginx != "nginx-1.30" || !strings.Contains(low.UnitText, "/data/shop/runtimes/nginx-1.30/nginx") ||
		!strings.Contains(low.UnitText, "AmbientCapabilities=CAP_NET_BIND_SERVICE") || low.Health.HTTP != "http://127.0.0.1:80/" {
		t.Fatalf("%+v\n%s", low, low.UnitText)
	}
	if !strings.Contains(low.NginxConf, "listen 127.0.0.1:80;") || !strings.Contains(low.NginxConf, "try_files $uri $uri/ =404;") ||
		strings.Contains(low.NginxConf, "$connection_upgrade") || !strings.Contains(low.NginxConf, "client_max_body_size 20m;") {
		t.Fatalf("%s", low.NginxConf)
	}
}

func TestServedStaticValidation(t *testing.T) {
	_, err := loadYAML(t, `
version: 1
project: shop
targets:
  t1: {host: 10.0.0.1, root: /data/shop, runAs: web}
apps:
  - {name: a, type: static, artifact: {path: d}, static: {port: abc}}
  - {name: b, type: static, artifact: {path: d}, static: {port: 8082, nginx: latest, reload: "nginx -s reload"}}
  - {name: c, type: static, artifact: {path: d}, static: {port: 8083, proxy: {api: "ftp://x"}}}
  - {name: d, type: static, artifact: {path: d}, static: {proxy: {/api/: "http://127.0.0.1:1/"}}}
  - {name: e, type: static, artifact: {path: d}, static: {port: 8084}, service: {jvmArgs: [-Xmx1g]}}
`)
	if err == nil {
		t.Fatal("应报错")
	}
	for _, want := range []string{"static.port 要写端口", "1.30（当前 \"latest\"）", "不需要 static.reload", "要以 / 开头", "http://地址:端口", "只在设置了 static.port", "只能设置 unit、user、description"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("缺少错误 %q：\n%v", want, err)
		}
	}
}
