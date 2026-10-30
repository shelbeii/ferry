package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func nginxConfigFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(p)
}

func nginxConfigDump(files ...string) string {
	var out string
	for _, file := range files {
		out += "# configuration file " + file + ":\n# fixture\n"
	}
	return out
}

func TestNginxConfigUsesSelectedMasterInsteadOfDefault(t *testing.T) {
	f, a, _ := configFixture(t)
	dir := t.TempDir()
	main := nginxConfigFile(t, dir, "web-8888/nginx.conf", "# 由 ferry 生成（project=demo app=web）\nhttp {}\n")
	include := nginxConfigFile(t, dir, "web-8888/custom.conf", "server_tokens off;\n")
	other := nginxConfigFile(t, dir, "web-8000/nginx.conf", "# 由 ferry 生成\nhttp {}\n")
	system := nginxConfigFile(t, dir, "system/nginx.conf", "user nginx; http {}\n")
	prefix := filepath.ToSlash(dir) + "/web-8888/data/"
	it := proto.Software{Kind: "nginx", Manager: "Ferry", PID: 42, Unit: "demo-web-8888.service", Path: "/opt/runtimes/nginx/nginx"}
	f.args = map[int][][]string{42: {
		{"nginx: worker process"},
		{"nginx: master process " + it.Path + " -e stderr -p '" + prefix + "' -c '" + main + "'"},
	}}
	scoped := shellQuote(it.Path) + " -T -e 'stderr' -p " + shellQuote(prefix) + " -c " + shellQuote(main) + " 2>&1"
	f.shellOut = map[string]string{
		scoped:                           nginxConfigDump(main, include),
		shellQuote(it.Path) + " -T 2>&1": nginxConfigDump(system, other),
	}
	report := a.discoverConfigs(it, nil)
	if len(report.Files) != 2 || report.Files[0].Path != main || report.Files[0].Source != "main" || report.Files[1].Path != include {
		t.Fatalf("只能列出所选 master 实际加载的文件：%+v", report)
	}
	if report.Files[0].ReadOnly == "" || report.Files[1].ReadOnly != "" {
		t.Fatal("只有真正由 Ferry 生成的文件才应标成由项目管理")
	}
	for _, call := range f.calls {
		if call == "sh "+shellQuote(it.Path)+" -T 2>&1" {
			t.Fatal("不能丢掉启动参数并读取此二进制默认的系统配置")
		}
	}
}

func TestStoppedNginxConfigUsesUnitExecStart(t *testing.T) {
	f, a, _ := configFixture(t)
	dir := t.TempDir()
	main := nginxConfigFile(t, dir, "web/nginx.conf", "# 由 ferry 生成\nhttp {}\n")
	it := proto.Software{Kind: "nginx", Manager: "Ferry", Unit: "demo-web.service", Path: "/opt/nginx/nginx"}
	f.shellOut = map[string]string{
		"systemctl show -p ExecStart --no-pager 'demo-web.service'":  "ExecStart={ path=/opt/nginx/nginx ; argv[]=/opt/nginx/nginx -c " + main + " ; ignore_errors=no ; }\n",
		shellQuote(it.Path) + " -T -c " + shellQuote(main) + " 2>&1": nginxConfigDump(main),
	}
	r := a.discoverConfigs(it, nil)
	if len(r.Files) != 1 || r.Files[0].Path != main {
		t.Fatalf("停止的实例应使用该服务的 ExecStart：%+v", r)
	}
}

func TestSystemNginxConfigIncludesRemainEditable(t *testing.T) {
	f, a, _ := configFixture(t)
	dir := t.TempDir()
	main := nginxConfigFile(t, dir, "system/nginx.conf", "user nginx; http {}\n")
	include := nginxConfigFile(t, dir, "system/conf.d/default.conf", "server { listen 80; }\n")
	it := proto.Software{Kind: "nginx", Unit: "nginx.service", Path: "/usr/sbin/nginx"}
	f.shellOut = map[string]string{
		"systemctl show -p ExecStart --no-pager 'nginx.service'":         "ExecStart={ path=/usr/sbin/nginx ; argv[]=/usr/sbin/nginx -g daemon on; master_process on; ; ignore_errors=no ; }\n",
		"'/usr/sbin/nginx' -T -g 'daemon on ; master_process on ;' 2>&1": nginxConfigDump(main, include),
	}
	r := a.discoverConfigs(it, nil)
	if len(r.Files) != 2 || r.Files[0].ReadOnly != "" || r.Files[1].ReadOnly != "" {
		t.Fatalf("系统 nginx 仍应返回实际的主配置和 include，且不能误标为项目生成：%+v", r)
	}
}

func TestNginxApplyChecksSameSelectedConfiguration(t *testing.T) {
	f, a, _ := configFixture(t)
	it := proto.Software{Kind: "nginx", PID: 42, Unit: "custom-nginx.service", Path: "/usr/sbin/nginx"}
	f.args = map[int][][]string{42: {{"nginx: master process /usr/sbin/nginx -p /srv/edge/ -c /srv/edge/nginx.conf"}}}
	f.calls = nil
	if err := a.applySoftwareConfig(it, nil, proto.SoftwareConfigFile{Apply: "reload"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[0] != "sh '/usr/sbin/nginx' -t -p '/srv/edge/' -c '/srv/edge/nginx.conf' 2>&1" || f.calls[1] != "reload custom-nginx.service" {
		t.Fatalf("配置生效必须校验所选实例，而不是默认 nginx：%v", f.calls)
	}
}

func TestBrokenNginxConfigDoesNotFallBackToOtherInstances(t *testing.T) {
	f, a, _ := configFixture(t)
	dir := t.TempDir()
	main := nginxConfigFile(t, dir, "web/nginx.conf", "# 由 ferry 生成\nbroken;\n")
	it := proto.Software{Kind: "nginx", PID: 42, Path: "/opt/nginx/nginx"}
	f.args = map[int][][]string{42: {{it.Path, "-c", main}}}
	f.shellFail[shellQuote(it.Path)+" -T -c "+shellQuote(main)+" 2>&1"] = true
	r := a.discoverConfigs(it, nil)
	if len(r.Files) != 1 || filepath.Clean(r.Files[0].Path) != filepath.Clean(main) || len(r.Notes) == 0 {
		t.Fatalf("配置错误时应保留其主配置并说明原因：%+v", r)
	}
}

func TestNginxConfigWithoutStartupEvidenceDoesNotGuess(t *testing.T) {
	_, a, _ := configFixture(t)
	r := a.discoverConfigs(proto.Software{Kind: "nginx", Manager: "Ferry", Path: "/opt/nginx/nginx"}, nil)
	if len(r.Files) != 0 || !strings.Contains(strings.Join(r.Notes, "\n"), "未使用系统默认配置") {
		t.Fatalf("无法确认来源时应说明原因，而不是展示默认系统文件：%+v", r)
	}
}

func TestNginxFlagsPreserveGlobalDirectivesAndArgumentBoundaries(t *testing.T) {
	argv := nginxProcessArgs([]string{"nginx: master process /usr/sbin/nginx -g daemon on; master_process on; -c /etc/nginx/custom.conf"}, "/usr/sbin/nginx")
	inv := nginxInvocation{binary: "/usr/sbin/nginx", flags: nginxConfigFlags(argv)}
	if inv.flag("-g") != "daemon on ; master_process on ;" || inv.flag("-c") != "/etc/nginx/custom.conf" {
		t.Fatal(inv)
	}
	inv.flags = nginxConfigFlags([]string{"/usr/sbin/nginx", "-c", "/opt/config with spaces/nginx.conf", "-p/opt/prefix/", "-s", "stop"})
	if inv.flag("-c") != "/opt/config with spaces/nginx.conf" || inv.flag("-p") != "/opt/prefix/" || strings.Contains(inv.command("-T"), " -s ") {
		t.Fatal(inv.command("-T"))
	}
}
