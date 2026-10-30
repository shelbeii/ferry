package engine

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/swtpl"
)

func TestHostedNginxSpec(t *testing.T) {
	f, err := loadYAML(t, strings.Replace(servedYAML, "port: 8081", "port: 8081\n      nginxInstance: edge\n      domains: [web.example.test]", 1))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Spec(f, f.Targets["t1"], f.App("admin"))
	if err != nil {
		t.Fatal(err)
	}
	if s.NginxInstance != "edge" || s.Unit != "" || s.RuntimeNginx != "" || s.UnitText != "" || s.Health.Host != "web.example.test" {
		t.Fatalf("应只绑定实例、不安装运行环境或创建服务：%+v", s)
	}
	if strings.Contains(s.NginxConf, "http {") || strings.Contains(s.NginxConf, "daemon off;") || !strings.Contains(s.NginxConf, "root /data/shop/apps/admin/running;") || !strings.Contains(s.NginxConf, "server_name web.example.test;") {
		t.Fatal(s.NginxConf)
	}
}

func TestSystemNginxSpec(t *testing.T) {
	f, err := loadYAML(t, strings.Replace(servedYAML, "port: 8081", "port: 8081\n      nginxInstance: "+proto.SystemNginxInstance, 1))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Spec(f, f.Targets["t1"], f.App("admin"))
	if err != nil || s.NginxInstance != proto.SystemNginxInstance || s.Unit != "" || s.RuntimeNginx != "" {
		t.Fatalf("系统 nginx 应生成站点绑定，而不是安装独立运行环境：%+v %v", s, err)
	}
	if !strings.Contains(s.NginxConf, "map $http_upgrade $ferry_upgrade_") || strings.Contains(s.NginxConf, "Connection $connection_upgrade;") {
		t.Fatal("系统 nginx 的站点片段必须自带代理变量，不能依赖修改全局配置")
	}
}

// 在隔离的 Linux 容器中可用缓存的真实 nginx 验证配置与 HTTP 路由。
func TestHostedNginxReal(t *testing.T) {
	bin := os.Getenv("FERRY_TEST_NGINX")
	if bin == "" {
		t.Skip("未提供测试 nginx")
	}
	dir := t.TempDir()
	// nginx 的 root master 默认将 worker 降为 nobody；测试目录需允许其穿过。
	for _, p := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	port := freeSitePort(t)
	tpl, err := swtpl.Find("", "nginx")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := renderPlan(tpl, &tpl.Variants[0], swtpl.Vars{"dir": dir, "name": "edge", "port": fmt.Sprint(freeSitePort(t)), "host": "127.0.0.1", "user": "root"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"run", "logs", "tmp", "sites", "apps/admin/running", "apps/admin/data/logs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mime := filepath.Join(dir, "mime.types")
	os.WriteFile(mime, []byte("types { text/html html; }\n"), 0o644)
	main := strings.ReplaceAll(plan.Files[0].Content, "/etc/nginx/mime.types", mime)
	main = strings.ReplaceAll(main, "map $http_upgrade $connection_upgrade { default upgrade; '' close; }", "")
	conf := filepath.Join(dir, "nginx.conf")
	os.WriteFile(conf, []byte(main), 0o644)
	// 模拟没有 Ferry 安装记录的系统服务，使用真实 nginx -V / -T 验证识别。
	alias := filepath.Join(dir, "nginx")
	if err := os.Symlink(bin, alias); err != nil {
		t.Fatal(err)
	}
	native, err := agent.DiscoverSystemNginx(func(command string, timeout time.Duration) (string, error) {
		if strings.HasPrefix(command, "systemctl show") {
			return fmt.Sprintf("LoadState=loaded\nUser=\nExecStart={ path=%s ; argv[]=%s -p %s/ -c %s ; }\n", alias, alias, dir, conf), nil
		}
		out, err := exec.Command("sh", "-c", command).CombinedOutput()
		return string(out), err
	})
	if err != nil || native.Name != proto.SystemNginxInstance || native.Hosting.Sites != filepath.Join(dir, "sites") {
		t.Fatalf("真实系统 nginx 识别失败：%+v %v", native, err)
	}
	f, err := loadYAML(t, strings.Replace(servedYAML, "port: 8081", fmt.Sprintf("port: %d\n      nginxInstance: edge\n      domains: [web.example.test]", port), 1))
	if err != nil {
		t.Fatal(err)
	}
	f.Targets["t1"].Root = dir
	s, err := Spec(f, f.Targets["t1"], f.App("admin"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "sites", "web.conf"), []byte(s.NginxConf), 0o644)
	os.WriteFile(filepath.Join(dir, "apps/admin/running/index.html"), []byte("ferry-hosted-site"), 0o644)
	if out, err := exec.Command(bin, "-t", "-p", dir+"/", "-c", conf).CombinedOutput(); err != nil {
		t.Fatalf("真实 nginx 配置校验：%v\n%s\n%s", err, out, s.NginxConf)
	}
	cmd := exec.Command(bin, "-p", dir+"/", "-c", conf)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/front-end-route", port), nil)
		req.Host = "web.example.test"
		res, err := client.Do(req)
		if err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if string(b) == "ferry-hosted-site" {
				break
			}
			t.Fatalf("请求没有到所选站点：%d %s", res.StatusCode, b)
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
func freeSitePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
