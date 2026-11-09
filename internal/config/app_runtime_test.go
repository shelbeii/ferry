package config

import (
	"strings"
	"testing"
)

func TestInvalidTargetRuntimeOverrides(t *testing.T) {
	base := `version: 1
project: demo
targets:
  a: {host: 192.0.2.1, runAs: app, appRuntimes: {api: OVERRIDE}}
apps:
  - {name: api, type: java, artifact: {path: api.jar}}
`
	for _, value := range []string{"{}", "{java: relative}", "{java: /bin/java, node: /bin/node}", "{python: /bin/python}", "{java: /bin/java, port: 8081}"} {
		if _, err := load(t, strings.Replace(base, "OVERRIDE", value, 1)); err == nil {
			t.Fatalf("应拒绝无效覆盖：%s", value)
		}
	}
}

// 前端站点改成只换服务器上已有站点网站目录里的文件（1Panel 的 OpenResty 等）。
func TestSiteDirRuntime(t *testing.T) {
	base := `version: 1
project: demo
targets:
  a: {host: 192.0.2.1, runAs: app, appRuntimes: {web: OVERRIDE}}
  b: {host: 192.0.2.2, runAs: app}
apps:
  - {name: web, type: static, artifact: {path: dist}, static: {port: "8000", proxy: {/api/: "http://127.0.0.1:9201/"}}}
`
	f, err := load(t, strings.Replace(base, "OVERRIDE", "{siteDir: /opt/1panel/apps/openresty/openresty/www/sites/web/index}", 1))
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.RuntimeApp(f.Targets["a"], f.Apps[0])
	if err != nil {
		t.Fatal(err)
	}
	st := a.Static
	if st.SiteDir != "/opt/1panel/apps/openresty/openresty/www/sites/web/index" || st.Served() || len(st.Proxy) > 0 || a.Service.Unit != "" {
		t.Fatalf("%+v %+v", st, a.Service)
	}
	if st.Health.HTTP != "http://127.0.0.1:8000/" || len(st.Health.Status) != 0 {
		t.Fatalf("还是检查原来端口的首页，状态码放宽：%+v", st.Health)
	}
	if b, _ := f.RuntimeApp(f.Targets["b"], f.Apps[0]); !b.Static.Served() || b.Static.SiteDir != "" {
		t.Fatal("别的服务器不受影响")
	}
	// 指定已有站点的端口：检查那个端口
	f, err = load(t, strings.Replace(base, "OVERRIDE", "{siteDir: /www/wwwroot/web, port: \"8888\"}", 1))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ = f.RuntimeApp(f.Targets["a"], f.Apps[0]); a.Static.Health.HTTP != "http://127.0.0.1:8888/" {
		t.Fatalf("%+v", a.Static.Health)
	}
	for _, value := range []string{"{siteDir: relative/dir}", "{siteDir: /etc/nginx}", "{siteDir: /www}", "{siteDir: /www/a, nginxInstance: web}"} {
		if _, err := load(t, strings.Replace(base, "OVERRIDE", value, 1)); err == nil {
			t.Fatalf("应拒绝：%s", value)
		}
	}
	// 直接写在应用里
	direct := `version: 1
project: demo
targets: {a: {host: 192.0.2.1, runAs: app}}
apps:
  - {name: web, type: static, artifact: {path: dist}, static: STATIC}
`
	if _, err := load(t, strings.Replace(direct, "STATIC", "{siteDir: /www/wwwroot/web}", 1)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"{siteDir: /www/wwwroot/web, port: \"8000\"}", "{siteDir: /www/wwwroot/web, link: /www/x/y}", "{siteDir: /usr/bin/x}"} {
		if _, err := load(t, strings.Replace(direct, "STATIC", value, 1)); err == nil {
			t.Fatalf("应拒绝：%s", value)
		}
	}
}
