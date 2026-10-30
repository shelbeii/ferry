package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDockerMaps(t *testing.T) {
	got := dockerMaps("0.0.0.0:3306->3306/tcp, :::3306->3306/tcp, 33060/tcp, 127.0.0.1:6380->6379/tcp, 0.0.0.0:8000-8002->9000-9002/tcp, [::1]:53->53/udp, 0.0.0.0:1-500->1-500/tcp")
	var s []string
	for _, m := range got {
		s = append(s, m.host+":"+strconv.Itoa(m.port)+">"+strconv.Itoa(m.ctr)+"/"+m.proto)
	}
	want := "*:3306>3306/tcp|127.0.0.1:6380>6379/tcp|*:8000>9000/tcp|*:8001>9001/tcp|*:8002>9002/tcp|[::1]:53>53/udp"
	if strings.Join(s, "|") != want {
		t.Fatal(strings.Join(s, "|"))
	}
}

const dockerUser = `-N DOCKER-USER
-A DOCKER-USER -i eth0 -p tcp -m conntrack --ctorigdstport 3306 --ctdir ORIGINAL -m comment --comment ferry-guard -j DROP
-A DOCKER-USER -i eth0 -p udp -m conntrack --ctorigdstport 53 --ctdir ORIGINAL -m comment --comment ferry-guard -j DROP
-A DOCKER-USER -s 10.0.0.0/8 -j RETURN
-A DOCKER-USER -j RETURN
`

func TestParseGuardRules(t *testing.T) {
	have := parseGuardRules(dockerUser)
	if len(have) != 2 || !have["3306/tcp"] || !have["53/udp"] {
		t.Fatalf("%v", have)
	}
}

func TestGuardScript(t *testing.T) {
	s := guardScriptText("eth0", []guardEntry{{Port: 3306, Proto: "tcp"}, {Port: 53, Proto: "udp"}})
	for _, w := range []string{
		"for t in iptables ip6tables; do",
		"grep -- '--comment ferry-guard' | sed 's/^-A /-D /'",
		"$t -I DOCKER-USER -i eth0 -p tcp -m conntrack --ctorigdstport 3306 --ctdir ORIGINAL -m comment --comment ferry-guard -j DROP || exit 1",
		"--ctorigdstport 53 ",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("脚本里没有 %q：\n%s", w, s)
		}
	}
	if !strings.Contains(guardUnitText("/usr/local/lib/ferry/docker-guard.sh"), "ExecStart=/bin/sh /usr/local/lib/ferry/docker-guard.sh") {
		t.Error("unit")
	}
}

func TestPortsDockerGuard(t *testing.T) {
	proc := t.TempDir()
	os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	os.WriteFile(filepath.Join(proc, "net", "route"), []byte("Iface\tDestination\tGateway\neth0\t00000000\t0101A8C0\n"), 0o644)
	dir := t.TempDir()
	// 拦过 9999（容器已经没了）；3306 的规则在；8080 记着拦但规则被清掉了
	os.WriteFile(guardFile(dir), []byte(`{"iface":"eth0","ports":[{"port":3306,"proto":"tcp","container":"mysql"},{"port":8080,"proto":"tcp","container":"web"},{"port":9999,"proto":"tcp","container":"old"}]}`), 0o600)
	rules := dockerUser + "-A DOCKER-USER -i eth0 -p tcp -m conntrack --ctorigdstport 9999 --ctdir ORIGINAL -m comment --comment ferry-guard -j DROP\n"
	sc := &fwScan{proc: proc, ferryDir: dir, run: fakeRun(map[string]string{
		"firewall-cmd": "none", "command -v ufw": "none", "iptables -S INPUT": "", "nft list ruleset": "",
		"iptables -S DOCKER-USER": rules,
		"docker ps": "aaaa\tmysql\t0.0.0.0:3306->3306/tcp\tcom.docker.compose.project=shop,com.docker.compose.service=db,com.docker.compose.project.working_dir=/opt/shop,com.docker.compose.project.config_files=/opt/shop/docker-compose.yml,/opt/shop/override.yml\n" +
			"bbbb\t1Panel-redis-abcd\t0.0.0.0:6379->6379/tcp\tcreatedBy=1Panel\n" +
			"cccc\tweb\t0.0.0.0:8080->80/tcp\t\n",
	})}
	rep, _ := sc.report()
	if g := rep.DockerGuard; g == nil || !g.OK || g.Iface != "eth0" {
		t.Fatalf("%+v", rep.DockerGuard)
	}
	got := map[int]string{}
	for _, p := range rep.Ports {
		got[p.Port] = strings.Join([]string{p.Container, p.Manager, p.ComposeFile, p.ComposeDir, p.ComposeService, strconv.Itoa(p.CtrPort), map[bool]string{true: "guard"}[p.Guard]}, "|")
	}
	want := map[int]string{
		3306: "mysql|compose|/opt/shop/docker-compose.yml|/opt/shop|db|3306|guard",
		6379: "1Panel-redis-abcd|1Panel||||6379|",
		8080: "web|||||80|",
		9999: "old|||||0|guard",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%d：%s", k, got[k])
		}
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "8080/tcp 现在没有拦着") {
		t.Errorf("%q", rep.Notes)
	}

	// 没有 DOCKER-USER：拦不了
	sc.run = fakeRun(map[string]string{"firewall-cmd": "none", "command -v ufw": "none", "docker ps": "aaaa\tmysql\t0.0.0.0:3306->3306/tcp\t\n"})
	rep, _ = sc.report()
	if g := rep.DockerGuard; g == nil || g.OK || !strings.Contains(g.Why, "DOCKER-USER") {
		t.Fatalf("%+v", rep.DockerGuard)
	}
	// 没有 Docker 端口、也没拦过：不看
	sc.ferryDir = t.TempDir()
	sc.run = fakeRun(map[string]string{"firewall-cmd": "none", "command -v ufw": "none"})
	if rep, _ = sc.report(); rep.DockerGuard != nil {
		t.Fatalf("%+v", rep.DockerGuard)
	}
}
