package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRun 按命令里的关键字返回输出；没有对应的返回错误。
func fakeRun(out map[string]string) func(string, time.Duration) (string, error) {
	return func(cmd string, _ time.Duration) (string, error) {
		best := ""
		for k := range out {
			if strings.Contains(cmd, k) && len(k) > len(best) {
				best = k
			}
		}
		if best == "" {
			return "", errors.New("exit 1")
		}
		return out[best], nil
	}
}

const zonePublic = `public (active)
  target: default
  icmp-block-inversion: no
  interfaces: eth0
  sources:
  services: dhcpv6-client http ssh
  ports: 8080/tcp 9000-9100/tcp 5353/udp
  protocols:
  forward: yes
  masquerade: no
  forward-ports:
  source-ports:
  icmp-blocks:
  rich rules:
	rule family="ipv4" source address="10.0.0.0/8" port port="3306" protocol="tcp" accept
	rule family="ipv4" port port="6379" protocol="tcp" reject
`

func TestFirewalld(t *testing.T) {
	root := t.TempDir()
	svc := filepath.Join(root, "usr", "lib", "firewalld", "services")
	os.MkdirAll(svc, 0o755)
	os.WriteFile(filepath.Join(svc, "ssh.xml"), []byte(`<?xml version="1.0"?><service><short>SSH</short><port protocol="tcp" port="22"/></service>`), 0o644)
	os.WriteFile(filepath.Join(svc, "http.xml"), []byte(`<service><port protocol="tcp" port="80"/></service>`), 0o644)
	os.WriteFile(filepath.Join(svc, "dhcpv6-client.xml"), []byte(`<service><port protocol="udp" port="546"/></service>`), 0o644)
	proc := t.TempDir()
	os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	os.WriteFile(filepath.Join(proc, "net", "route"), []byte("Iface\tDestination\tGateway\neth0\t00000000\t0101A8C0\n"), 0o644)
	sc := &fwScan{proc: proc, root: root, run: fakeRun(map[string]string{
		"firewall-cmd --state":                 "running\n",
		"--get-zone-of-interface=eth0":         "public\n",
		"--zone=public --list-all":             zonePublic,
		"--permanent --zone=public --list-all": strings.Replace(zonePublic, " 8080/tcp", "", 1),
		"command -v docker":                    "",
	})}
	st := sc.firewall()
	if st.Kind != "firewalld" || !st.Active || !st.Manage || st.Zone != "public" {
		t.Fatalf("%+v", st.Firewall)
	}
	cases := []struct {
		port  int
		pr    string
		state string
		kind  string
	}{
		{22, "tcp", "open", "service"},
		{80, "tcp", "open", "service"},
		{8080, "tcp", "open", "port"},
		{9050, "tcp", "open", "range"},
		{5353, "udp", "open", "port"},
		{5353, "tcp", "blocked", ""},
		{3306, "tcp", "partial", "rich"},
		{6379, "tcp", "blocked", "rich"},
		{8081, "tcp", "blocked", ""},
	}
	for _, c := range cases {
		state, r := st.decide(c.port, c.pr)
		kind := ""
		if r != nil {
			kind = r.kind
		}
		if state != c.state || kind != c.kind {
			t.Errorf("%d/%s：%s %s", c.port, c.pr, state, kind)
		}
	}
	if _, r := st.decide(8080, "tcp"); !r.temp {
		t.Error("8080 只在运行时放行，应标为临时")
	}
	if _, r := st.decide(22, "tcp"); r.temp || r.label != "服务 ssh（22/tcp）" {
		t.Errorf("%+v", r)
	}
	if !contains(st.Rules, "3306/tcp 只允许 10.0.0.0/8") || !contains(st.Rules, "范围 9000-9100/tcp") {
		t.Errorf("%q", st.Rules)
	}

	// 关闭服务放行的端口：去掉服务；关闭单个端口：去掉端口
	_, r := st.decide(80, "tcp")
	if got := strings.Join(closeCmds(st, r), ";"); got != "firewall-cmd --zone=public --remove-service=http;firewall-cmd --permanent --zone=public --remove-service=http" {
		t.Error(got)
	}
	_, r = st.decide(8080, "tcp")
	if got := closeCmds(st, r)[1]; got != "firewall-cmd --permanent --zone=public --remove-port=8080/tcp" {
		t.Error(got)
	}
	if got := openCmds(st, 8443, "tcp")[0]; got != "firewall-cmd --zone=public --add-port=8443/tcp" {
		t.Error(got)
	}

	// 区域放行全部：不能改
	sc.run = fakeRun(map[string]string{"firewall-cmd --state": "running", "--get-zone-of-interface": "trusted", "--list-all": "trusted (active)\n  target: ACCEPT\n"})
	st = sc.firewall()
	if st.Manage || st.Why == "" {
		t.Fatalf("%+v", st.Firewall)
	}
	if s, _ := st.decide(1234, "tcp"); s != "open" {
		t.Error(s)
	}
}

const ufwStatus = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
8443/tcp                   DENY IN     Anywhere
8443                       ALLOW IN    Anywhere
80,443/tcp (Nginx Full)    ALLOW IN    Anywhere
3306                       ALLOW IN    10.0.0.0/8
OpenSSH                    LIMIT IN    Anywhere
9000:9100/tcp              ALLOW IN    Anywhere
8000,8001/tcp              ALLOW IN    Anywhere
192.168.1.5 7000/tcp       ALLOW IN    Anywhere
Anywhere                   ALLOW IN    172.16.0.9
8080/tcp                   ALLOW OUT   Anywhere
22/tcp (v6)                ALLOW IN    Anywhere (v6)
8443 (v6)                  ALLOW IN    Anywhere (v6)
`

func TestUFW(t *testing.T) {
	sc := &fwScan{proc: t.TempDir(), run: fakeRun(map[string]string{
		"firewall-cmd": "none",
		"ufw status":   ufwStatus,
		"ufw app info 'OpenSSH'": `Profile: OpenSSH
Title: Secure shell server, an rshd replacement
Description: OpenSSH is a free implementation of the Secure Shell protocol.

Port:
  22/tcp
`,
	})}
	st := sc.firewall()
	if st.Kind != "ufw" || !st.Active || !st.Manage {
		t.Fatalf("%+v", st.Firewall)
	}
	cases := []struct {
		port  int
		pr    string
		state string
		kind  string
	}{
		{22, "tcp", "open", "port"},
		{8443, "tcp", "blocked", "port"}, // 先匹配的拦截规则生效
		{8443, "udp", "open", "port"},
		{443, "tcp", "open", "app"},
		{3306, "tcp", "partial", "port"},
		{9050, "tcp", "open", "range"},
		{8001, "tcp", "open", "list"},
		{7000, "tcp", "open", "complex"},
		{8080, "tcp", "blocked", ""}, // OUT 不算
		{5000, "tcp", "blocked", ""},
	}
	for _, c := range cases {
		state, r := st.decide(c.port, c.pr)
		kind := ""
		if r != nil {
			kind = r.kind
		}
		if state != c.state || kind != c.kind {
			t.Errorf("%d/%s：%s %s", c.port, c.pr, state, kind)
		}
	}
	if !contains(st.Rules, "应用 Nginx Full（80/tcp 443/tcp）") || !contains(st.Rules, "应用 OpenSSH（22/tcp）") || !contains(st.Rules, "3306 只允许 10.0.0.0/8") {
		t.Errorf("%q", st.Rules)
	}
	_, r := st.decide(443, "tcp")
	if got := closeCmds(st, r)[0]; got != "ufw --force delete allow 'Nginx Full'" {
		t.Error(got)
	}
	_, r = st.decide(8443, "udp")
	if got := closeCmds(st, r)[0]; got != "ufw --force delete allow '8443'" {
		t.Error(got)
	}
	if got := openCmds(st, 5000, "tcp")[0]; got != "ufw allow 5000/tcp" {
		t.Error(got)
	}

	// 没启用
	sc.run = fakeRun(map[string]string{"firewall-cmd": "none", "ufw status": "Status: inactive\n"})
	st = sc.firewall()
	if st.Kind != "ufw" || st.Active || st.Manage || !strings.Contains(st.Why, "没有启用") {
		t.Fatalf("%+v", st.Firewall)
	}
	if s, _ := st.decide(22, "tcp"); s != "" {
		t.Error(s)
	}
	// 默认放行
	sc.run = fakeRun(map[string]string{"firewall-cmd": "none", "ufw status": "Status: active\nDefault: allow (incoming), allow (outgoing)\n"})
	if st = sc.firewall(); st.Manage || st.deflt != "open" {
		t.Fatalf("%+v", st)
	}
}

func TestNoFirewall(t *testing.T) {
	sc := &fwScan{proc: t.TempDir(), run: fakeRun(map[string]string{
		"firewall-cmd":     "not running",
		"command -v ufw":   "none",
		"iptables -S":      "-P INPUT ACCEPT\n-A INPUT -p tcp -m multiport --dports 22 -j f2b-sshd\n",
		"nft list ruleset": "",
	})}
	st := sc.firewall()
	if st.Kind != "firewalld" || st.Active || st.Manage || !strings.Contains(st.Why, "没有运行") {
		t.Fatalf("%+v", st.Firewall)
	}
	sc.run = fakeRun(map[string]string{"firewall-cmd": "none", "command -v ufw": "none", "iptables -S": "-P INPUT DROP\n"})
	if st = sc.firewall(); st.Kind != "iptables" || !st.Active || st.Manage {
		t.Fatalf("%+v", st.Firewall)
	}
	if !iptablesFiltering("-P INPUT ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n") || iptablesFiltering("-A INPUT -s 1.2.3.4/32 -j DROP\n") {
		t.Error("iptablesFiltering")
	}
	if !nftFiltering("table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n") {
		t.Error("nftFiltering")
	}
}

func TestPortsReport(t *testing.T) {
	proc := t.TempDir()
	os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:23E1 00000000:0000 0A 00000000:00000000 00:00000000 00000000   995        0 1111 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000   995        0 2222 1 0000000000000000 100 0 0 10 0
   2: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3333 1 0000000000000000 100 0 0 10 0
`), 0o644)
	os.WriteFile(filepath.Join(proc, "net", "tcp6"), []byte(`  sl  local_address                         remote_address                        st
   0: 00000000000000000000000001000000:23E1 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   995        0 4444 1 0000000000000000 100 0 0 10 0
`), 0o644)
	sc := &fwScan{proc: proc, keep: []int{22}, run: fakeRun(map[string]string{
		"firewall-cmd": "none", "command -v ufw": "none", "iptables -S": "", "nft list ruleset": "",
		"docker ps": "0123456789abcdef\tmysql\t0.0.0.0:3306->3306/tcp, :::3306->3306/tcp, 33060/tcp\nfedcba9876543210\tdns\t127.0.0.1:53->53/udp\n",
	})}
	rep, _ := sc.report()
	var got []string
	for _, p := range rep.Ports {
		s := strings.Join(p.Listen, ",") + ":" + p.Proto + "/" + p.Container
		if p.SSH {
			s += " ssh"
		}
		got = append(got, s)
	}
	want := "*:tcp/ ssh|127.0.0.1:udp/dns|*:tcp/mysql|*:tcp/|127.0.0.1,[::1]:tcp/"
	if strings.Join(got, "|") != want {
		t.Fatalf("%s", strings.Join(got, "|"))
	}
	if rep.Ports[1].Port != 53 || !rep.Ports[2].Docker || rep.Firewall.Kind != "" || rep.Ports[0].Firewall != "" {
		t.Fatalf("%+v", rep)
	}
}
