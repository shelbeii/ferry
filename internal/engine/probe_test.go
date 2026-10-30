package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

func TestParseProbe(t *testing.T) {
	tg := &config.Target{Name: "prod", Host: "10.0.0.5", Port: 22, User: "deploy"}
	req := ProbeRequest{Root: "/opt/shop", RunAs: "shop", Ports: []ProbePort{{"system", 9201}, {"web", 8081}, {"job", 9203}}}
	lines := []string{
		"priv sudo",
		"arch x86_64",
		"kernel 5.15.0",
		"os Ubuntu 22.04.4 LTS",
		"systemd yes",
		"user no",
		"disk 512000",
		"mem 8048000",
		`listen 0.0.0.0:22 users:(("sshd",pid=812,fd=3))`,
		`listen [::]:9201 users:(("java",pid=13155,fd=45))`,
		"listen 0.0.0.0:9203 ",
		"java\t/usr/lib/jvm/java-8/jre/bin/java\t/usr/bin/java\topenjdk version \"1.8.0_392\"",
		"java\t/usr/lib/jvm/java-8/jre/bin/java\t/usr/lib/jvm/java-8/bin/java\topenjdk version \"1.8.0_392\"",
		"java\t/usr/lib/jvm/jdk-17/bin/java\t/usr/lib/jvm/jdk-17/bin/java\topenjdk version \"17.0.9\" 2023-10-17",
		"nginx /usr/sbin/nginx",
	}
	p := parseProbe(tg, req, lines)
	if p.Arch != "amd64" || p.Priv != "sudo" || p.RunAsOK || p.FreeMB != 500 || p.MemMB != 7859 || p.Nginx != "/usr/sbin/nginx" {
		t.Fatalf("%+v", p)
	}
	if len(p.Javas) != 2 || p.Javas[0].Major != 8 || p.Javas[0].Path != "/usr/bin/java" || p.Javas[1].Major != 17 {
		t.Fatalf("java：%+v", p.Javas)
	}
	var text []string
	fix := ""
	for _, c := range p.Checks {
		text = append(text, c.Level+" "+c.Text)
		if c.Fix != "" {
			fix = c.Fix
		}
	}
	all := strings.Join(text, "\n")
	for _, want := range []string{
		"ok 已连上 deploy@10.0.0.5:22：Ubuntu 22.04.4 LTS，amd64，内存 7.7 GB",
		"ok 登录用户 deploy 可以免密 sudo",
		"warn 运行用户 shop 还不存在",
		"warn 部署目录 /opt/shop 还不存在，部署时自动创建，所在分区剩余 500 MB",
		"warn 端口 9201（system）已被 java（进程 13155）占用",
		"warn 端口 9203（job）已被别的程序占用",
		"ok 端口 8081（web）空闲",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("缺少「%s」：\n%s", want, all)
		}
	}
	if fix != "create-user" {
		t.Errorf("应能创建运行用户：%q", fix)
	}
	if strings.Contains(all, "error") {
		t.Errorf("不应有错误：\n%s", all)
	}

	// 不能 sudo、没有 systemd、组件目录不安全
	p = parseProbe(tg, ProbeRequest{RunAs: "shop"}, []string{"priv none", "arch aarch64", "user no", "unsafe /usr/local/lib"})
	all = ""
	for _, c := range p.Checks {
		all += c.Level + " " + c.Text + "\n"
		if c.Fix != "" {
			t.Errorf("不能 sudo 时不应提供创建用户：%+v", c)
		}
	}
	for _, want := range []string{"error 登录用户 deploy 不是 root", "error 服务器上没有 systemd", "error Ferry 组件目录的上级 /usr/local/lib"} {
		if !strings.Contains(all, want) {
			t.Errorf("缺少「%s」：\n%s", want, all)
		}
	}
	if p.Arch != "arm64" {
		t.Errorf("arch：%s", p.Arch)
	}
}

func TestJavaMajor(t *testing.T) {
	for v, want := range map[string]int{"1.8.0_392": 8, "17.0.9": 17, "21": 21, "11.0.2-ea": 11, "": 0} {
		if got := javaMajor(v); got != want {
			t.Errorf("%q → %d，应为 %d", v, got, want)
		}
	}
}

func TestProbeInstalledNginx(t *testing.T) {
	in := proto.SoftwareInstance{Name: "edge", Kind: "nginx", Hosting: &proto.NginxHosting{Config: "/opt/edge/nginx.conf", Sites: "/opt/edge/sites"}}
	b, _ := json.Marshal(in)
	p := parseProbe(&config.Target{}, ProbeRequest{}, []string{"software-instance\t" + string(b), "software-instance\t{broken}", `software-instance` + "\t" + `{"kind":"nginx","name":"legacy"}`})
	if len(p.NginxInstances) != 1 || p.NginxInstances[0].Name != "edge" || p.NginxInstances[0].Hosting.Config != in.Hosting.Config {
		t.Fatalf("只提供带托管信息的已安装 nginx 实例：%+v", p.NginxInstances)
	}
}

func TestProbeExistingSystemNginxWithoutInstalledInstances(t *testing.T) {
	p := &ProbeResult{Nginx: "/usr/sbin/nginx"}
	addSystemNginx(p, func(cmd string, _ time.Duration) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "systemctl show"):
			return "LoadState=loaded\nExecStart={ path=/usr/sbin/nginx ; argv[]=/usr/sbin/nginx -c /etc/nginx/nginx.conf ; }\n", nil
		case strings.Contains(cmd, " -V "):
			return "nginx version: nginx/1.20.1\nconfigure arguments: --prefix=/usr/share/nginx --user=nginx", nil
		default:
			return "# configuration file /etc/nginx/nginx.conf:\nuser root; http { include /etc/nginx/conf.d/*.conf; }\n", nil
		}
	})
	if len(p.NginxInstances) != 1 || p.NginxInstances[0].Name != proto.SystemNginxInstance || !p.NginxInstances[0].Hosting.System {
		t.Fatalf("系统安装的 nginx 不能因没有 Ferry JSON 记录而被漏掉：%+v", p)
	}
}
