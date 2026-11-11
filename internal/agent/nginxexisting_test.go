package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 1Panel 的 OpenResty：容器、host 网络，www 挂载到 /opt/1panel/apps/openresty/openresty/www。
func TestDiscoverNginxSitesContainer(t *testing.T) {
	const ctr = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dump := `nginx: the configuration file /usr/local/openresty/nginx/conf/nginx.conf syntax is ok
# configuration file /usr/local/openresty/nginx/conf/nginx.conf:
user root;
http {
    include conf.d/*.conf;
    server { listen 80; root html; }
}
stream { server { listen 9000; proxy_pass 127.0.0.1:9001; } }
# configuration file /usr/local/openresty/nginx/conf/conf.d/web.conf:
server {
    listen 8000 ;
    listen [::]:8000;
    server_name web.example.com;
    root /www/sites/web/index;
    location /api/ { proxy_pass http://127.0.0.1:9201/; }
}
server {
    listen 8888 ssl http2;
    server_name _;
    location / { root /www/sites/admin/index; }
}
server { listen 8880; location / { proxy_pass http://127.0.0.1:8080; } }
server { listen 8881; root /var/www/$host; }
server { listen 8882; root /usr/share/nginx/html; }
# configuration file /usr/local/openresty/nginx/conf/conf.d/ferry-abc.conf:
server { listen 8100; root /opt/x/apps/a/running; }
`
	var cmds []string
	run := func(cmd string, _ time.Duration) (string, error) {
		cmds = append(cmds, cmd)
		switch {
		case strings.Contains(cmd, "^nginx: master process"): // 找 nginx 主进程
			return "M\t1234\tnginx: master process /usr/local/openresty/nginx/sbin/nginx -g daemon off; \t0::/system.slice/docker-" + ctr + ".scope|\n" +
				"M\t99\tnginx: master process /opt/shop/runtimes/nginx/nginx -e stderr -p /opt/shop/apps/web/data/ -c /opt/shop/.ferry/apps/web/nginx.conf \t0::/system.slice/shop-web.service|\n", nil
		case strings.HasPrefix(cmd, "docker inspect"):
			return `{"name":"/1Panel-openresty-pPgs","net":"host","mounts":[{"Type":"bind","Source":"/opt/1panel/apps/openresty/openresty/www","Destination":"/www"},{"Type":"bind","Source":"/opt/1panel/apps/openresty/openresty/conf/conf.d","Destination":"/usr/local/openresty/nginx/conf/conf.d"}],"ports":{}}`, nil
		case strings.HasPrefix(cmd, "docker exec"):
			return "nginx version: openresty/1.21.4.3\nconfigure arguments: --prefix=/usr/local/openresty/nginx\n" + ngxDumpMark + "\n" + dump, nil
		case strings.HasPrefix(cmd, "for d in"):
			return "D\t/opt/1panel/apps/openresty/openresty/www/sites/web/index\troot:root\nN\t/opt/1panel/apps/openresty/openresty/www/sites/admin/index\n", nil
		}
		t.Fatalf("没想到的命令：%s", cmd)
		return "", nil
	}
	sites, notes := DiscoverNginxSites(run)
	if len(notes) > 0 {
		t.Fatalf("notes：%v", notes)
	}
	byRoot := map[string]proto.NginxSite{}
	for _, s := range sites {
		byRoot[s.Root] = s
	}
	web := byRoot["/www/sites/web/index"]
	if web.Server != "OpenResty（容器 1Panel-openresty-pPgs）" || web.Container != "1Panel-openresty-pPgs" || !reflect.DeepEqual(web.Ports, []int{8000}) ||
		web.Dir != "/opt/1panel/apps/openresty/openresty/www/sites/web/index" || web.Owner != "root:root" || web.Disabled != "" ||
		!reflect.DeepEqual(web.Names, []string{"web.example.com"}) || !strings.HasSuffix(web.Conf, "conf.d/web.conf") {
		t.Fatalf("web：%+v", web)
	}
	if admin := byRoot["/www/sites/admin/index"]; !admin.SSL || admin.Disabled == "" || len(admin.Names) != 0 {
		t.Fatalf("admin（location / 里的 root，目录不存在）：%+v", admin)
	}
	if def := byRoot["/usr/local/openresty/nginx/html"]; def.Disabled == "" {
		t.Fatalf("没挂载出来的默认站点要不能选：%+v", def)
	}
	if v := byRoot["/var/www/$host"]; v.Disabled == "" {
		t.Fatalf("用了变量的：%+v", v)
	}
	for _, s := range sites {
		if s.Root == "/opt/x/apps/a/running" || strings.Contains(s.Server, "进程 99") {
			t.Fatalf("Ferry 自己的站点不要列出：%+v", s)
		}
		if reflect.DeepEqual(s.Ports, []int{8880}) || reflect.DeepEqual(s.Ports, []int{9000}) {
			t.Fatalf("只做代理的、stream 的不算：%+v", s)
		}
	}
	if sites[0].Root != "/www/sites/web/index" {
		t.Fatalf("能选的排前面：%+v", sites[0])
	}
}

func TestContainerHostPorts(t *testing.T) {
	c := &ngxContainer{Ports: map[string][]struct{ HostIp, HostPort string }{"80/tcp": {{"0.0.0.0", "8080"}, {"::", "8080"}}}}
	if got := containerHostPorts(c, []int{80, 443}); !reflect.DeepEqual(got, []int{8080}) {
		t.Fatal(got)
	}
}

func TestSyncSiteFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("网站目录在 Linux 服务器上")
	}
	src, dst := t.TempDir(), t.TempDir()
	write := func(root, rel, text string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(src, "index.html", "new")
	write(src, "assets/a.js", "a2")
	write(src, "same.txt", "same")
	write(dst, "index.html", "old")
	write(dst, "assets/old.js", "x")
	write(dst, "same.txt", "same")
	write(dst, ".well-known/acme-challenge/t", "token")
	write(dst, "uploads/u.png", "keep me")
	n, err := syncSiteFiles(src, dst, []proto.KeepItem{{Path: "uploads"}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("放了 %d 个", n)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			return "<none>"
		}
		return string(b)
	}
	if read("index.html") != "new" || read("assets/a.js") != "a2" || read("assets/old.js") != "<none>" || read("same.txt") != "same" ||
		read(".well-known/acme-challenge/t") != "token" || read("uploads/u.png") != "keep me" {
		t.Fatal("内容不对")
	}
	if fi, _ := os.Stat(filepath.Join(dst, "index.html")); fi.Mode().Perm()&0o044 != 0o044 {
		t.Fatalf("要让 nginx 能读：%v", fi.Mode())
	}
	entries, _ := os.ReadDir(dst)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ferry-tmp-") {
			t.Fatal("留下了临时文件")
		}
	}
}
