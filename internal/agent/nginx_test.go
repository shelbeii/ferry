package agent

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

const ngDump = `# configuration file /etc/nginx/nginx.conf:
user nginx;
events { worker_connections 1024; }
http {
    include /etc/nginx/conf.d/*.conf;
    upstream api { server 127.0.0.1:9201; }
    server {
        listen 80 default_server;
        listen [::]:80 default_server;
        server_name _;
        root /usr/share/nginx/html;
    }
}
stream { server { listen 3307; proxy_pass db; } }

# configuration file /etc/nginx/conf.d/site.conf:
server {
    listen 443 ssl http2;
    server_name example.com  www.example.com; # 注释 { 不影响
    location / {
        root "@ROOT@/dist";
    }
    location /api/ { proxy_pass http://api; }
}
server {
    listen 127.0.0.1:8088;
    location /admin/ { alias @LINK@/; }
}
server {
    server_name other.com;
    root /var/www/$host;
}
`

func TestParseNginx(t *testing.T) {
	sv := parseNginx(strings.NewReplacer("@ROOT@", "/srv/web/current", "@LINK@", "/var/www/web").Replace(ngDump))
	if len(sv) != 4 {
		t.Fatalf("应有 4 个 http server（upstream 里的 server、stream 里的不算）：%+v", sv)
	}
	if !reflect.DeepEqual(sv[0].listen, []string{"*:80"}) || len(sv[0].names) != 0 || sv[0].file != "/etc/nginx/nginx.conf" {
		t.Fatalf("%+v", sv[0])
	}
	s := sv[1]
	if !reflect.DeepEqual(s.listen, []string{"*:443"}) || !s.ssl || !reflect.DeepEqual(s.names, []string{"example.com", "www.example.com"}) ||
		!reflect.DeepEqual(s.roots, []nginxRoot{{"/srv/web/current/dist", "/"}}) || s.file != "/etc/nginx/conf.d/site.conf" {
		t.Fatalf("%+v", s)
	}
	if !reflect.DeepEqual(sv[2].listen, []string{"127.0.0.1:8088"}) || sv[2].roots[0].location != "/admin/" {
		t.Fatalf("%+v", sv[2])
	}
	if !reflect.DeepEqual(sv[3].listen, []string{"*:80"}) {
		t.Fatalf("没写 listen 默认 80：%+v", sv[3])
	}
	for in, want := range map[string]string{"8080": "*:8080", "0.0.0.0:80": "*:80", "[::]:443": "*:443", "localhost:81": "127.0.0.1:81",
		"10.0.0.2": "10.0.0.2:80", "[::1]:82": "[::1]:82", "unix:/run/x.sock": "unix:/run/x.sock"} {
		if got := nginxListen(in); got != want {
			t.Errorf("%s → %s，应为 %s", in, got, want)
		}
	}
}

func TestStaticStatusShowsNginxSites(t *testing.T) {
	f := newFake(t)
	link := filepath.Join(t.TempDir(), "html", "web")
	web := proto.AppSpec{Name: "web", Type: proto.TypeStatic, Link: link, KeepReleases: 3}
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{web}, id1, map[string][]byte{"web": []byte("<h1>1</h1>")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	status := func() proto.AppStatus {
		r := exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{web}})
		var st []proto.AppStatus
		if err := json.Unmarshal(r.result.Data, &st); err != nil || len(st) != 1 {
			t.Fatal(r.dump())
		}
		return st[0]
	}
	if st := status(); st.Serving != "服务器上没有找到 nginx" || st.Listen != nil {
		t.Fatalf("%+v", st)
	}
	f.nginx = strings.NewReplacer("@ROOT@", filepath.ToSlash(filepath.Join(f.root, "apps", "web", "running")), "@LINK@", filepath.ToSlash(link)).Replace(ngDump)
	st := status()
	if st.Serving != "" || len(st.Sites) != 2 || !reflect.DeepEqual(st.Listen, []string{"*:443", "127.0.0.1:8088"}) ||
		st.Sites[0].Names[0] != "example.com" || st.Sites[1].Location != "/admin/" {
		t.Fatalf("%+v", st)
	}
	f.nginx = "# configuration file /etc/nginx/nginx.conf:\nhttp { server { root /usr/share/nginx/html; } }\n"
	if st := status(); !strings.Contains(st.Serving, "没有站点指向这里") || !strings.Contains(st.Serving, filepath.ToSlash(link)) {
		t.Fatalf("%+v", st)
	}
}
