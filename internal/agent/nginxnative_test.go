package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func nativeNginxRunner(t *testing.T, main string) func(string, time.Duration) (string, error) {
	t.Helper()
	return func(cmd string, _ time.Duration) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "systemctl show nginx.service"):
			return "LoadState=loaded\nUser=\nExecStart={ path=/usr/sbin/nginx ; argv[]=/usr/sbin/nginx -c /etc/nginx/nginx.conf ; ignore_errors=no ; }\n", nil
		case cmd == "'/usr/sbin/nginx' -V 2>&1":
			return "nginx version: nginx/1.20.1\nconfigure arguments: --prefix='/usr/share/nginx' --conf-path=/etc/nginx/nginx.conf --user=nginx\n", nil
		case cmd == "'/usr/sbin/nginx' -T -p '/usr/share/nginx/' -c '/etc/nginx/nginx.conf' 2>&1":
			return "nginx: configuration file /etc/nginx/nginx.conf test is successful\n# configuration file /etc/nginx/nginx.conf:\n" + main + "\n# configuration file /etc/nginx/mime.types:\ntypes {}\n", nil
		default:
			t.Errorf("意外执行命令：%s", cmd)
			return "", errors.New("unexpected command")
		}
	}
}

func TestDiscoverSystemNginxWithoutFerryRecord(t *testing.T) {
	in, err := DiscoverSystemNginx(nativeNginxRunner(t, "user root;\nhttp { include /etc/nginx/conf.d/*.conf; include mime.types; }"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Name != proto.SystemNginxInstance || in.Hosting == nil || !in.Hosting.System || in.Hosting.Sites != "/etc/nginx/conf.d" || in.Hosting.Prefix != "/usr/share/nginx" || in.User != "root" || in.Version != "1.20.1" {
		t.Fatalf("应识别现有系统 nginx，而不依赖 Ferry 软件记录：%+v", in)
	}
}

func TestSystemNginxRequiresHTTPInclude(t *testing.T) {
	for _, main := range []string{
		`stream { include /etc/nginx/conf.d/*.conf; } http { server { include /etc/nginx/server/*.conf; } }`,
		`http { # include /etc/nginx/conf.d/*.conf;
server { listen 80; } }`,
		`http { include /etc/nginx/*/conf/*.conf; }`,
	} {
		if _, err := DiscoverSystemNginx(nativeNginxRunner(t, main)); err == nil || !strings.Contains(err.Error(), "include") {
			t.Fatalf("不能将非 http 直属或注释配置当作站点目录：%s, %v", main, err)
		}
	}
	user, sites := nginxHTTPInclude(`user www-data; http { include "sites-enabled/*"; }`, "/etc/nginx")
	if user != "www-data" || sites != "/etc/nginx/sites-enabled" {
		t.Fatalf("配置相对路径应相对主配置目录：%s %s", user, sites)
	}
}

func TestSystemNginxDiscoveryDoesNotRunWrites(t *testing.T) {
	called := 0
	_, err := DiscoverSystemNginx(func(cmd string, _ time.Duration) (string, error) {
		called++
		if !strings.HasPrefix(cmd, "systemctl show nginx.service") {
			t.Fatal(cmd)
		}
		return "LoadState=not-found\n", nil
	})
	if err == nil || called != 1 {
		t.Fatal("没有系统服务时应停止识别，并给出说明")
	}
}
