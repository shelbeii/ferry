package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func TestCgroupOwner(t *testing.T) {
	id := strings.Repeat("ab", 32)
	cases := []struct{ cg, unit, container string }{
		{"0::/system.slice/mysqld.service\n", "mysqld.service", ""},
		{"0::/system.slice/docker-" + id + ".scope\n", "", id},
		{"12:pids:/docker/" + id + "\n1:name=systemd:/docker/" + id + "\n", "", id},
		// 在容器里跑 systemd（取最里层）
		{"0::/system.slice/docker-" + id + ".scope/system.slice/redis.service\n", "redis.service", ""},
		{"0::/user.slice/user-0.slice/session-3.scope\n", "", ""},
		{"0::/user.slice/user-0.slice/user@0.service/app.slice/x.scope\n", "", ""},
	}
	for _, c := range cases {
		u, ct := cgroupOwner(c.cg)
		if u != c.unit || ct != c.container {
			t.Errorf("%q → %q %q，期望 %q %q", c.cg, u, ct, c.unit, c.container)
		}
	}
}

func TestMatchRule(t *testing.T) {
	kind := func(exe string, args ...string) string {
		if len(args) == 0 {
			args = []string{exe}
		}
		if r := matchRule(exe, args); r >= 0 {
			return swRules[r].kind
		}
		return ""
	}
	cases := []struct{ got, want string }{
		{kind("/usr/sbin/mysqld"), "mysql"},
		{kind("/usr/local/openresty/nginx/sbin/nginx"), "openresty"},
		{kind("/usr/sbin/nginx", "nginx: master process /usr/sbin/nginx"), "nginx"},
		{kind("/usr/bin/java", "java", "-Dnacos.home=/home/nacos", "-jar", "/home/nacos/target/nacos-server.jar"), "nacos"},
		{kind("/usr/bin/java", "java", "-Dproject.name=sentinel-dashboard", "-jar", "sentinel-dashboard-1.8.6.jar"), "sentinel"},
		// 用 Nacos、Sentinel 的业务服务不算
		{kind("/usr/bin/java", "java", "-Dspring.cloud.nacos.server-addr=1.2.3.4:8848", "-Dcsp.sentinel.dashboard.server=x", "-jar", "app.jar"), ""},
		{kind("/usr/lib/erlang/erts-13/bin/beam.smp", "beam.smp", "--", "-root", "/usr/lib/erlang", "-s", "rabbit", "boot"), "rabbitmq"},
		{kind("/www/server/panel/pyenv/bin/python3.7", "/www/server/panel/BT-Panel"), "bt"},
		{kind("/usr/bin/sleep", "sleep", "100"), ""},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("#%d：%q，期望 %q", i, c.got, c.want)
		}
	}
}

func TestDockerHelpers(t *testing.T) {
	if got := dockerPorts("0.0.0.0:3306->3306/tcp, :::3306->3306/tcp, 33060/tcp, 127.0.0.1:8080->80/tcp, 0.0.0.0:5353->5353/udp"); !reflect.DeepEqual(got, []string{"*:3306", "127.0.0.1:8080", "*:5353/udp"}) {
		t.Errorf("dockerPorts：%v", got)
	}
	for img, want := range map[string]string{
		"mysql:8.4.11":                         "mysql 8.4.11",
		"registry.local:5000/1panel/openresty": "openresty ",
		"nacos/nacos-server:v2.3.2@sha256:ab":  "nacos-server v2.3.2",
		"sha256:0123":                          " ",
	} {
		r, tag := splitImage(img)
		if r+" "+tag != want {
			t.Errorf("splitImage(%q) = %q %q", img, r, tag)
		}
	}
	if m := containerManager("1Panel-mysql-AbCd", ""); m != "1Panel" {
		t.Errorf("1Panel 容器：%q", m)
	}
	if m := containerManager("x", "createdBy=1Panel,com.docker.compose.project=mysql"); m != "1Panel" {
		t.Errorf("1Panel 标签：%q", m)
	}
	if m := containerManager("web-1", "com.docker.compose.config-hash=a,b,com.docker.compose.project=shop"); m != "Docker Compose（shop）" {
		t.Errorf("compose：%q", m)
	}
	if r := imageRule("mysql-server"); r < 0 || swRules[r].kind != "mysql" {
		t.Errorf("mysql-server 没认出")
	}
	if r := imageRule("mysqlx"); r >= 0 {
		t.Errorf("mysqlx 不该认成 %s", swRules[r].kind)
	}
	if m := reJarVer.FindStringSubmatch("/opt/sentinel-dashboard-1.8.6.jar"); m == nil || m[1] != "1.8.6" {
		t.Errorf("jar 版本：%v", m)
	}
}

// fakeProc 在临时目录里造一个 /proc。
type fakeProc struct {
	dir   string
	links map[string]string
}

func (f *fakeProc) add(t *testing.T, pid, ppid int, exe, cgroup, net string, args ...string) {
	t.Helper()
	d := filepath.Join(f.dir, strconv.Itoa(pid))
	os.MkdirAll(d, 0o755)
	if len(args) == 0 {
		args = []string{exe}
	}
	os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o644)
	// 启动时间：开机后 5000 个时钟周期 = 50 秒
	stat := fmt.Sprintf("%d (%s) S %d %s5000 0 0\n", pid, filepath.Base(exe), ppid, strings.Repeat("0 ", 17))
	os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644)
	os.WriteFile(filepath.Join(d, "cgroup"), []byte(cgroup), 0o644)
	f.links[filepath.Join(d, "exe")] = exe
	f.links[filepath.Join(d, "ns", "net")] = net
}

func TestSoftwareScan(t *testing.T) {
	fp := &fakeProc{dir: t.TempDir(), links: map[string]string{}}
	os.WriteFile(filepath.Join(fp.dir, "stat"), []byte("cpu 1 2 3\nbtime 1700000000\n"), 0o644)
	host := "net:[4026531840]"
	redisID, appID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	fp.add(t, 1, 0, "/usr/lib/systemd/systemd", "0::/init.scope\n", host)
	fp.add(t, 100, 1, "/usr/sbin/mysqld", "0::/system.slice/mysqld.service\n", host)
	fp.add(t, 200, 1, "/usr/sbin/nginx", "0::/system.slice/nginx.service\n", host, "nginx: master process /usr/sbin/nginx")
	fp.add(t, 201, 200, "/usr/sbin/nginx", "0::/system.slice/nginx.service\n", host, "nginx: worker process")
	fp.add(t, 300, 1, "/usr/lib/jvm/bin/java", "0::/user.slice/user-0.slice/session-3.scope\n", host,
		"java", "-Dproject.name=sentinel-dashboard", "-jar", "/opt/sentinel/sentinel-dashboard-1.8.6.jar")
	fp.add(t, 400, 1, "/usr/bin/containerd-shim-runc-v2", "0::/system.slice/containerd.service\n", host)
	fp.add(t, 401, 400, "/usr/local/bin/redis-server", "0::/system.slice/docker-"+redisID+".scope\n", "net:[4026532000]", "redis-server *:6379")
	fp.add(t, 500, 400, "/app/server", "0::/system.slice/docker-"+appID+".scope\n", host)
	fp.add(t, 501, 500, "/app/worker", "0::/system.slice/docker-"+appID+".scope\n", host)
	fp.add(t, 600, 1, "/opt/1panel/bin/1panel-agent", "0::/system.slice/1panel-agent.service\n", host)
	fp.add(t, 610, 1, "/opt/1panel/bin/1panel-core", "0::/system.slice/1panel-core.service\n", host)
	fp.add(t, 700, 1, "/usr/bin/dockerd", "0::/system.slice/docker.service\n", host)

	unitDir := t.TempDir()
	os.WriteFile(filepath.Join(unitDir, "nginx.service"), []byte(ManagedMarker+"\n[Unit]\n"), 0o644)

	ps := strings.Join([]string{
		`{"ID":"` + redisID + `","Names":"1Panel-redis-AbCd","Image":"redis:8.0.1","State":"running","Status":"Up 3 weeks","Ports":"0.0.0.0:6379->6379/tcp","Labels":"createdBy=1Panel"}`,
		`{"ID":"` + strings.Repeat("c", 64) + `","Names":"1Panel-mysql-XyZ","Image":"mysql:8.4.11","State":"exited","Status":"Exited (0) 2 days ago","Ports":"","Labels":"createdBy=1Panel"}`,
		`{"ID":"` + appID + `","Names":"myapp","Image":"myapp:1.0","Status":"Up 2 hours","Ports":"","Labels":""}`,
	}, "\n")
	var ran []string
	outputs := map[string]string{
		"command -v docker":                             "/usr/bin/docker",
		"docker ps -a --no-trunc --format '{{json .}}'": ps + "\n",
		"systemctl list-unit-files --type=service --no-legend --no-pager": "mysqld.service enabled enabled\nnginx.service enabled enabled\n" +
			"redis.service disabled enabled\ntomcat.service disabled disabled\nmysql.service alias -\ngetty@.service static -\nsshd.service enabled enabled\n",
		"systemctl show -p ExecStart --no-pager 'redis.service'":  "ExecStart={ path=/usr/bin/redis-server ; argv[]=/usr/bin/redis-server /etc/redis.conf ; }\n",
		"'/usr/sbin/mysqld' --version 2>&1":                       "/usr/sbin/mysqld  Ver 8.0.36 for Linux on x86_64 (MySQL Community Server - GPL)\n",
		"'/usr/sbin/nginx' -v 2>&1":                               "nginx version: nginx/1.24.0\n",
		"systemctl show -p ExecStart --no-pager 'tomcat.service'": "ExecStart={ path=/usr/libexec/tomcat/server ; argv[]=/usr/libexec/tomcat/server start ; }\n",
		"'/opt/1panel/bin/1panel-core' version 2>&1":              "系统版本: v2.2.5\n系统模式: stable\n",
		"'/usr/bin/dockerd' --version 2>&1":                       "Docker version 29.7.2, build 1234567\n",
		"'/usr/bin/redis-server' --version 2>&1":                  "Redis server v=7.2.4 sha=00000000:0 malloc=jemalloc-5.3.0 bits=64\n",
	}
	sc := &swScan{
		proc:    fp.dir,
		unitDir: unitDir,
		run: func(cmd string, _ time.Duration) (string, error) {
			ran = append(ran, cmd)
			if strings.HasPrefix(cmd, "rpm -qf") {
				if strings.Contains(cmd, "/usr/libexec/tomcat/server") {
					return "9.0.62\n", nil
				}
				return "file is not owned by any package\n", nil
			}
			if out, ok := outputs[cmd]; ok {
				return out, nil
			}
			return "", errors.New("exit status 127")
		},
		appUnits: map[string][3]string{"shop-web.service": {"shop", "web", "prod"}},
		unitProps: func(unit string) UnitProps {
			if unit == "redis.service" || unit == "tomcat.service" {
				return UnitProps{Load: "loaded", Active: "inactive", Sub: "dead"}
			}
			return UnitProps{Load: "loaded", Active: "active", Sub: "running"}
		},
		readlink: func(p string) (string, error) {
			if v, ok := fp.links[p]; ok {
				return v, nil
			}
			return "", os.ErrNotExist
		},
		listening: func(pid int) []string {
			return map[int][]string{800: {"*:8000"}, 100: {"*:3306"}, 200: {"*:80"}, 300: {"*:8858"}, 401: {"*:6379"}, 500: {"*:9000"}, 610: {"*:19999"}, 700: {"*:6379", "127.0.0.1:2375"}}[pid]
		},
	}
	rep := sc.scan()

	var got []string
	for _, it := range rep.Items {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%s%s%s|%s|%d|%v|%v|%s", it.Kind, it.Name, it.Version, it.State, it.Via, it.Unit, it.Container, it.Manager, it.PID, it.Listen, it.Inner, it.Path))
	}
	want := []string{
		"mysql|MySQL|8.0.36|running|systemdmysqld.service||100|[*:3306]|[]|/usr/sbin/mysqld",
		"mysql|MySQL|8.4.11|stopped|docker1Panel-mysql-XyZ|1Panel|0|[]|[]|",
		"redis|Redis|8.0.1|running|docker1Panel-redis-AbCd|1Panel|401|[*:6379]|[*:6379]|/usr/local/bin/redis-server",
		"redis|Redis|7.2.4|stopped|systemdredis.service||0|[]|[]|/usr/bin/redis-server",
		"sentinel|Sentinel 控制台|1.8.6|running|process||300|[*:8858]|[]|/opt/sentinel/sentinel-dashboard-1.8.6.jar",
		"nginx|Nginx|1.24.0|running|systemdnginx.service|Ferry|200|[*:80]|[]|/usr/sbin/nginx",
		"tomcat|Tomcat|9.0.62|stopped|systemdtomcat.service||0|[]|[]|/usr/libexec/tomcat/server",
		"docker|Docker|29.7.2|running|systemddocker.service||700|[127.0.0.1:2375]|[]|/usr/bin/dockerd",
		"1panel|1Panel 面板|2.2.5|running|systemd1panel-agent.service||600|[]|[]|/opt/1panel/bin/1panel-agent",
		"1panel|1Panel 面板|2.2.5|running|systemd1panel-core.service||610|[*:19999]|[]|/opt/1panel/bin/1panel-core",
		"|myapp|1.0|running|dockermyapp||500|[*:9000]|[]|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("结果：\n%s\n期望：\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if rep.Docker != "ok" {
		t.Errorf("Docker：%q", rep.Docker)
	}
	if it := rep.Items[0]; it.Since != time.Unix(1700000050, 0).Format(time.RFC3339) {
		t.Errorf("启动时间：%q", it.Since)
	}
	for _, c := range ran {
		if strings.Contains(c, "redis-server' --version") && strings.Contains(c, "/usr/local/bin") {
			t.Errorf("不该在主机上运行容器里的程序：%s", c)
		}
	}
}

func TestSoftwareDockerDown(t *testing.T) {
	sc := &swScan{proc: t.TempDir(), unitDir: t.TempDir(),
		run: func(cmd string, _ time.Duration) (string, error) {
			if cmd == "command -v docker" {
				return "/usr/bin/docker", nil
			}
			if strings.HasPrefix(cmd, "docker ps") {
				return "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n", errors.New("exit status 1")
			}
			return "", errors.New("exit status 1")
		},
		unitProps: func(string) UnitProps { return UnitProps{} },
		readlink:  func(string) (string, error) { return "", os.ErrNotExist },
		listening: func(int) []string { return nil },
	}
	rep := sc.scan()
	if rep.Docker != "Docker 没有运行" || len(rep.Items) != 0 {
		t.Errorf("%+v", rep)
	}
	var _ = proto.SoftwareReport{}
}
