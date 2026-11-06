package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func webText(ws []proto.WebPanel) string {
	var out []string
	for _, w := range ws {
		s := fmt.Sprintf("%s %s://:%d%s", w.Label, w.Scheme, w.Port, w.Path)
		if w.Inner > 0 {
			s += fmt.Sprintf(" inner=%d", w.Inner)
		}
		if w.Local {
			s += " local"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

func TestWebPanels(t *testing.T) {
	fp := &fakeProc{dir: t.TempDir(), links: map[string]string{}}
	host := "net:[1]"
	put := func(pid int, file, body string) {
		p := filepath.Join(fp.dir, strconv.Itoa(pid), "root", filepath.FromSlash(file))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	// Nacos 3：控制台端口在配置文件里，用了环境变量占位
	fp.add(t, 10, 1, "/usr/bin/java", "", host, "java", "-Dnacos.home=/opt/nacos", "-jar", "/opt/nacos/target/nacos-server.jar",
		"--spring.config.additional-location=file:/opt/nacos/conf/")
	put(10, "/opt/nacos/conf/application.properties", "nacos.server.main.port=8848\nnacos.console.port=${CONSOLE_PORT:8080}\n# x\nnacos.console.contextPath=\n")
	os.WriteFile(filepath.Join(fp.dir, "10", "environ"), []byte("PATH=/bin\x00CONSOLE_PORT=18080\x00"), 0o644)
	// Nacos 2：主端口、/nacos
	fp.add(t, 11, 1, "/usr/bin/java", "", host, "java", "-Dnacos.home=/home/nacos", "-jar", "nacos-server.jar")
	put(11, "/home/nacos/conf/application.properties", "server.servlet.contextPath=/nacos\nserver.port=8849\n")
	// Nacos 读不到配置：从端口猜（3.x 的控制台是多出来的那个）
	fp.add(t, 12, 1, "/usr/bin/java", "", host, "java", "-jar", "nacos-server.jar")
	fp.add(t, 20, 1, "/usr/bin/java", "", host, "java", "-Dserver.port=8718", "-jar", "sentinel-dashboard.jar")
	fp.add(t, 21, 1, "/usr/bin/java", "", host, "java", "-jar", "sentinel-dashboard.jar")
	fp.add(t, 30, 1, "/opt/minio", "", host, "/opt/minio", "server", "/data", "--address", ":9000", "--console-address", "127.0.0.1:9001")
	fp.add(t, 31, 1, "/opt/minio", "", host, "/opt/minio", "server", "/data")
	fp.add(t, 40, 1, "/opt/1panel/bin/1panel-core", "", host)
	fp.add(t, 41, 1, "/opt/1panel/bin/1panel-agent", "", host)
	fp.add(t, 50, 1, "/usr/sbin/nginx", "", host)
	fp.add(t, 60, 1, "/usr/bin/java", "", host, "java", "-jar", "xxl-job-admin.jar", "--server.port=9090")

	userInfo := "面板地址: https://$LOCAL_IP:10001/ab12cd\n面板用户:  admin\n面板密码:  ******\n提示：修改密码可执行命令：1pctl update password\n"
	sc := &swScan{proc: fp.dir, run: func(cmd string, _ time.Duration) (string, error) {
		if cmd == "1pctl user-info 2>&1" {
			return userInfo, nil
		}
		return "", errors.New("exit status 127")
	}}
	cases := []struct {
		kind   string
		pid    int
		listen []string
		want   string
	}{
		{"nacos", 10, []string{"*:8848", "*:9848", "*:9849", "*:18080"}, "控制台 http://:18080/"},
		{"nacos", 11, []string{"*:8849"}, "控制台 http://:8849/nacos"},
		{"nacos", 12, []string{"*:7848", "*:8848", "*:9848", "*:9849", "*:18080"}, "控制台 http://:18080/"},
		{"nacos", 12, []string{"*:8848", "*:9848", "*:9849"}, "控制台 http://:8848/nacos"},
		{"sentinel", 20, []string{"*:8718", "*:8719"}, "控制台 http://:8718/"},
		{"sentinel", 21, []string{"*:8858", "*:8719"}, "控制台 http://:8858/"},
		{"minio", 30, []string{"*:9000", "127.0.0.1:9001"}, "控制台 http://:9001/"},
		{"minio", 31, []string{"*:9000", "*:34567"}, "控制台 http://:34567/"},
		{"1panel", 40, []string{"*:10001"}, "面板 https://:10001/ab12cd"},
		{"1panel", 41, nil, ""},
		{"nginx", 50, []string{"*:80", "*:443", "*:53/udp"}, "网站 :80 http://:80/; 网站 :443 https://:443/"},
		{"xxljob", 60, []string{"*:9090"}, "调度中心 http://:9090/xxl-job-admin"},
		{"redis", 50, []string{"*:6379"}, ""},
	}
	for _, c := range cases {
		got := webText(sc.webOf(c.kind, sc.procOf(c.pid), c.listen))
		if got != c.want {
			t.Errorf("%s（%d）：%q，期望 %q", c.kind, c.pid, got, c.want)
		}
	}
	if w := sc.webOf("1panel", sc.procOf(40), []string{"*:10001"}); len(w) != 1 || !strings.Contains(w[0].Note, "账号 admin") {
		t.Errorf("1Panel 的账号：%+v", w)
	}
	// 1pctl 不能用：用监听的端口，提示怎么查安全入口
	sc.run = func(string, time.Duration) (string, error) { return "", errors.New("not found") }
	if w := sc.webOf("1panel", sc.procOf(40), []string{"*:10001"}); webText(w) != "面板 http://:10001/" || !strings.Contains(w[0].Note, "1pctl user-info") {
		t.Errorf("1pctl 不能用：%+v", w)
	}
}

// procOf 测试用：读出假的 /proc 里的一个进程
func (sc *swScan) procOf(pid int) *swProc {
	dir := filepath.Join(sc.proc, strconv.Itoa(pid))
	cmd, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
	args := strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
	return &swProc{pid: pid, args: args, exe: args[0]}
}

func TestWebDockerAndLocal(t *testing.T) {
	m := dockerPortMap("0.0.0.0:18848->8848/tcp, :::18848->8848/tcp, 0.0.0.0:9848->9848/tcp, 0.0.0.0:53->53/udp")
	if m[8848] != 18848 || m[9848] != 9848 || m[53] != 0 {
		t.Fatalf("端口映射：%v", m)
	}
	web := []proto.WebPanel{{Label: "a", Port: 8848}, {Label: "b", Port: 8080}}
	mapWeb(web, m)
	if web[0].Port != 18848 || web[1].Port != 0 || web[1].Inner != 8080 {
		t.Errorf("换成主机端口：%+v", web)
	}
	it := proto.Software{Listen: []string{"127.0.0.1:9001", "*:9000", "[::1]:8000", "*:8000"},
		Web: []proto.WebPanel{{Port: 9001}, {Port: 9000}, {Port: 8000}, {Port: 7000}}}
	markLocal(&it)
	if !it.Web[0].Local || it.Web[1].Local || it.Web[2].Local || it.Web[3].Local {
		t.Errorf("只在本机监听：%+v", it.Web)
	}
	ws := instanceWeb(proto.SoftwareInstance{Kind: "nacos", Account: "nacos", Ports: map[string]int{"port": 8848, "console": 8080}})
	if webText(ws) != "控制台 http://:8080/" || !strings.Contains(ws[0].Note, "密码") {
		t.Errorf("Ferry 装的 Nacos：%+v", ws)
	}
}

func TestNginxWeb(t *testing.T) {
	dump := "# configuration file /etc/nginx/nginx.conf:\nhttp { include /etc/nginx/conf.d/*.conf; }\n" +
		"# configuration file /etc/nginx/conf.d/a.conf:\n" +
		"server { listen 80 default_server; server_name _; root /srv/default; }\n" +
		"server { listen 80; listen [::]:80; server_name shop.example.com www.shop.example.com; root /srv/shop; }\n" +
		"server { listen 80; server_name *.wild.com ~^re; }\n" +
		"server { listen 443 ssl; server_name admin.example.com; }\n" +
		"server { listen 8081; server_name api.example.com; }\n" +
		"server { listen 9999; server_name off.example.com; }\n"
	var ran []string
	sc := &swScan{run: func(cmd string, _ time.Duration) (string, error) {
		ran = append(ran, cmd)
		if strings.Contains(cmd, " -T") {
			return dump, nil
		}
		return "", errors.New("no")
	}}
	got := webText(sc.webPanels("nginx", webSrc{exe: "/usr/sbin/nginx", args: []string{"nginx: master process /usr/sbin/nginx -c /etc/nginx/nginx.conf"},
		env: func() map[string]string { return nil }, ports: []int{80, 443, 8081}}))
	want := "网站 :80 http://:80/; 网站 shop.example.com http://:80/; 网站 admin.example.com https://:443/; 网站 api.example.com:8081 http://:8081/"
	if got != want {
		t.Errorf("网站：\n%s\n期望\n%s", got, want)
	}
	if len(ran) == 0 || !strings.Contains(ran[0], "'/usr/sbin/nginx' -T -c '/etc/nginx/nginx.conf'") {
		t.Errorf("命令：%v", ran)
	}
	// 容器里的：docker exec
	ran = nil
	sc.webPanels("nginx", webSrc{exe: "/usr/sbin/nginx", container: "abc", args: []string{"nginx: master process nginx -g daemon off;"}, env: func() map[string]string { return nil }})
	if len(ran) == 0 || !strings.HasPrefix(ran[0], "docker exec 'abc' '/usr/sbin/nginx' -T") {
		t.Errorf("容器：%v", ran)
	}
}
