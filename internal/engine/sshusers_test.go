package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

func TestParseSSH(t *testing.T) {
	out := `priv root
@@self 2105
@@now 1000000
@@ps
    1     0 9000 ?        /usr/lib/systemd/systemd --system
  900     1 8000 ?        /usr/sbin/sshd -D
 1200   900  600 ?        sshd: root@pts/0
 1201  1200  600 pts/0    -bash
 1202  1201   30 pts/0    top
 1300   900  300 ?        sshd: alice [priv]
 1301  1300  300 ?        sshd: alice@notty
 1302  1301  300 ?        /usr/libexec/openssh/sftp-server
 2000   900   20 ?        sshd: root@notty
 2100  2000    1 ?        sh -c if [ "$(id -u)" = 0 ]; then echo "priv root"; sh -s; fi
 2105  2100    1 ?        sh -s
 2001  2000   10 ?        sudo -n /usr/local/lib/ferry/ferry-agent-0123456789ab status
 3000   900    5 ?        sshd: root [priv]
 3001  3000    5 ?        sshd: root [net]
 4000   900  100 ?        sshd: bob@pts/1,pts/2
 4001  4000  100 pts/1    -bash
@@env
/proc/1201/environ:SSH_CONNECTION=10.0.0.9 50001 10.0.0.2 22
/proc/1302/environ:SSH_CONNECTION=::ffff:192.168.1.7 50002 10.0.0.2 22
@@ss
ESTAB 0 0 10.0.0.2:41000 127.0.0.1:3306 users:(("sshd",pid=4000,fd=9))
ESTAB 0 0 10.0.0.2:22 1.2.3.4:50003 users:(("sshd",pid=1200,fd=3))
ESTAB 0 0 [::ffff:10.0.0.2]:22 [::ffff:5.6.7.8]:50004 users:(("sshd",pid=4000,fd=3))
ESTAB 0 0 10.0.0.2:22 9.9.9.9:50005 users:(("sshd",pid=2000,fd=3))
@@who
root     pts/0        2026-10-23 10:00 (1.2.3.4)
bob      pts/1        2026-10-23 10:08 (5.6.7.8)
@@tty
/dev/pts/0 999990
/dev/pts/1 999000
/dev/pts/2 999900
`
	rep, err := parseSSH(strings.Split(out, "\n"), "root", 22)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Priv != "root" || rep.Pending != 1 || len(rep.Sessions) != 4 {
		t.Fatalf("%+v", rep)
	}
	by := map[int]SSHSession{}
	for _, s := range rep.Sessions {
		by[s.PID] = s
	}
	if s := rep.Sessions[0]; !s.Self || s.PID != 2000 || s.Kind != "ferry" || s.Kick || s.From != "9.9.9.9" || len(s.Cmds) != 1 || !strings.Contains(s.Cmds[0], "ferry-agent") {
		t.Errorf("本机 Ferry 的连接应在最前、不能断开：%+v", s)
	}
	if s := by[1200]; s.User != "root" || s.Kind != "terminal" || s.From != "1.2.3.4" || s.FromPort != 50003 || s.Idle != 10 || s.Since != 1000000-600 ||
		len(s.Cmds) != 1 || s.Cmds[0] != "top" || !s.Kick {
		t.Errorf("root 的终端：%+v", s)
	}
	if s := by[1300]; s.User != "alice" || s.Kind != "sftp" || s.From != "192.168.1.7" || len(s.pids) != 2 || s.TTYs != nil || s.Idle != -1 {
		t.Errorf("alice 传文件：%+v", s)
	}
	// 端口转发的 TCP 连接不能当成来源；两个终端取最近有输入的；顺带记下端口转发目标
	if s := by[4000]; s.From != "5.6.7.8" || s.FromPort != 50004 || len(s.TTYs) != 2 || s.Idle != 100 || s.Cmds[0] != "-bash" || !slices.Equal(s.Forwards, []string{"127.0.0.1:3306"}) {
		t.Errorf("bob：%+v", s)
	}
	// 新连上的在前（本机 Ferry 的除外）
	if rep.Sessions[1].PID != 4000 || rep.Sessions[3].PID != 1200 {
		t.Errorf("顺序：%v %v", rep.Sessions[1].PID, rep.Sessions[3].PID)
	}
}

// OpenSSH 9.8 起每个连接是 sshd-session；不是 root、不能 sudo 时只能断开自己用户的连接，来源从环境变量、who 看
func TestParseSSHNoPriv(t *testing.T) {
	out := `priv none
@@self 70
@@now 500
@@ps
   10     1 400 ?        sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups
   20    10 300 ?        sshd-session: deploy [priv]
   21    20 300 ?        sshd-session: deploy@pts/0
   22    21 300 pts/0    -bash
   30    10 200 ?        sshd-session: carol [priv]
   31    30 200 ?        sshd-session: carol@pts/1
   32    31 200 pts/1    -bash
   40    10 9 ?          sshd-session: deploy [priv]
   41    40 9 ?          sshd-session: deploy@notty
   60    41 9 ?          bash -c x
   70    60 9 ?          sh -s
@@env
/proc/22/environ:SSH_CONNECTION=10.1.1.1 40000 10.1.1.2 22
@@ss
@@who
deploy   pts/0        2026-10-23 10:00 (10.1.1.1)
carol    pts/1        2026-10-23 10:01 (10.1.1.3)
@@tty
`
	rep, err := parseSSH(strings.Split(out, "\n"), "deploy", 22)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sessions) != 3 || len(rep.Notes) != 1 {
		t.Fatalf("%+v", rep)
	}
	by := map[int]SSHSession{}
	for _, s := range rep.Sessions {
		by[s.PID] = s
	}
	if s := by[20]; !s.Kick || s.From != "10.1.1.1" || s.Idle != -1 || s.Kind != "terminal" {
		t.Errorf("自己的终端：%+v", s)
	}
	if s := by[30]; s.Kick || s.From != "10.1.1.3" {
		t.Errorf("别人的连接不能断开，来源看 who：%+v", s)
	}
	if s := by[40]; !s.Self || s.Kick || s.Kind != "ferry" || len(s.Cmds) != 0 {
		t.Errorf("本机 Ferry：%+v", s)
	}
}

func TestPsAge(t *testing.T) {
	for in, want := range map[string]int64{"42": 42, "01:02": 62, "01:00:00": 3600, "2-00:00:01": 172801} {
		if got := psAge(in); got != want {
			t.Errorf("%s → %d，应为 %d", in, got, want)
		}
	}
}

func TestKickScript(t *testing.T) {
	s := kickScript([]int{12, 34})
	if !strings.Contains(s, `S="12 34"`) || !strings.Contains(s, "kill -KILL") {
		t.Error(s)
	}
}

func TestParseSSHAuthAndKeyComments(t *testing.T) {
	fpEd, cmtEd, ok := parseAuthorizedKeyLine("ssh-ed25519 dGVzdC1lZDI1NTE5LWtleS1ibG9i iron@workstation")
	if !ok || cmtEd != "iron@workstation" {
		t.Fatalf("parseAuthorizedKeyLine: ok=%v fp=%q cmt=%q", ok, fpEd, cmtEd)
	}
	fpRsa, cmtRsa, ok := parseAuthorizedKeyLine(`no-pty,command="/bin/true" ssh-rsa dGVzdC1yc2Eta2V5LWJsb2I= alice@MacBook`)
	if !ok || cmtRsa != "alice@MacBook" {
		t.Fatalf("parseAuthorizedKeyLine opts: ok=%v fp=%q cmt=%q", ok, fpRsa, cmtRsa)
	}

	out := `priv root
@@self 2101
@@now 2000
@@ps
  900     1 9000 ?        /usr/sbin/sshd -D
 2100   900  500 ?        sshd: root@notty
 2101  2100  500 ?        /usr/local/lib/ferry/ferry-agent-abc serve
 2200   900  400 ?        sshd-session: root [priv]
 2201  2200  400 ?        sshd-session: root@pts/0
 2202  2201  400 pts/0    -bash
 2300   900  300 ?        sshd: bob [priv]
 2301  2300  300 ?        sshd: bob@pts/1
 2302  2301  300 pts/1    -bash
 2400   900  200 ?        sshd: carol [priv]
 2401  2400  200 ?        sshd: carol@notty
@@env
@@ss
ESTAB 0 0 172.22.18.28:22 172.21.106.3:58338 users:(("sshd",pid=2100,fd=3))
ESTAB 0 0 172.22.18.28:22 172.21.106.3:55763 users:(("sshd-session",pid=2200,fd=3))
ESTAB 0 0 172.22.18.28:22 10.0.0.9:51111 users:(("sshd",pid=2300,fd=3))
ESTAB 0 0 172.22.18.28:22 10.0.0.10:52222 users:(("sshd",pid=2400,fd=3))
@@who
@@tty
@@auth
Nov 25 14:00:01 host sshd[2100]: Accepted publickey for root from 172.21.106.3 port 58338 ssh2: ED25519 ` + fpEd + `
Nov 25 14:05:01 host sshd-auth[99999]: Accepted publickey for root from ::ffff:172.21.106.3 port 55763 ssh2: RSA ` + fpRsa + `
Nov 25 14:10:01 host sshd[2300]: Accepted password for bob from 10.0.0.9 port 51111 ssh2
Nov 25 14:12:01 host sshd[2400]: Accepted keyboard-interactive/pam for carol from 10.0.0.10 port 52222 ssh2
@@keys
root	` + fpEd + `	iron@workstation
@@keylines
root	no-pty,command="/bin/true" ssh-rsa dGVzdC1yc2Eta2V5LWJsb2I= alice@MacBook
`
	rep, descToTop, err := parseSSHInternal(strings.Split(out, "\n"), "root", 22)
	if err != nil {
		t.Fatal(err)
	}
	if descToTop[2101] != 2100 || descToTop[2202] != 2200 {
		t.Errorf("descToTop 映射不对：%v", descToTop)
	}
	by := map[int]SSHSession{}
	for _, s := range rep.Sessions {
		by[s.PID] = s
	}
	if s := by[2100]; s.AuthMethod != "publickey" || s.AuthKey != fpEd || s.AuthKeyComment != "iron@workstation" {
		t.Errorf("PID 匹配公钥备注失败：%+v", s)
	}
	// OpenSSH 9.8+ sshd-auth 临时子进程 PID 不同，按 (user, from, fromPort) 兜底匹配
	if s := by[2200]; s.AuthMethod != "publickey" || s.AuthKey != fpRsa || s.AuthKeyComment != "alice@MacBook" {
		t.Errorf("四元组兜底匹配公钥备注失败：%+v", s)
	}
	if s := by[2300]; s.AuthMethod != "password" || s.AuthKey != "" {
		t.Errorf("密码认证解析失败：%+v", s)
	}
	if s := by[2400]; s.AuthMethod != "keyboard-interactive" {
		t.Errorf("keyboard-interactive 规范化失败：%+v", s)
	}
}

func TestEnrichSSHReportAttribution(t *testing.T) {
	curConn := &conn{label: "root@172.22.18.28:22"}
	otherConn := &conn{label: "root@172.22.18.28:22"}

	// 场景 1：无 NAT 端口转换（直连内网）——精准区分当前连接、独立连接、并发扩展、本机 IDE、本机 MCP、同 IP 其他设备、外部 IP
	rep := &SSHReport{
		Priv: "root",
		Me:   "root",
		Now:  2000,
		Sessions: []SSHSession{
			{PID: 100, User: "root", From: "172.21.106.3", FromPort: 58338, Kind: "ferry", Self: true, Kick: false},
			{PID: 200, User: "root", From: "172.21.106.3", FromPort: 59319, Kind: "ferry", Kick: true},
			{PID: 250, User: "root", From: "172.21.106.3", FromPort: 59400, Kind: "sftp", Kick: true},
			{PID: 300, User: "root", From: "172.21.106.3", FromPort: 55763, Kind: "terminal", Kick: true},
			{PID: 350, User: "root", From: "172.21.106.3", FromPort: 55800, Kind: "ferry", Kick: true},
			{PID: 400, User: "root", From: "172.21.106.3", FromPort: 60111, Kind: "terminal", Kick: true},
			{PID: 500, User: "root", From: "10.8.0.99", FromPort: 44000, Kind: "terminal", Kick: true},
		},
	}
	enrichSSHReportWith(enrichInput{
		CurrentConn:    curConn,
		CurrentRef:     "gztunnel / 172.22.18.28-集群版",
		SelfLocalPort:  58338,
		SelfExtraPorts: []int{59400},
		RemoteIP:       "172.22.18.28",
		RemotePort:     22,
		PoolSnaps: []poolConnSnap{
			{
				Conn:        otherConn,
				PrimaryPort: 59319,
				Refs:        []poolRefMeta{{Kind: config.KindConnections, Target: "172.22.18.28", Title: "172.22.18.28"}},
			},
		},
		DescToTop:     map[int]int{},
		LocalPID:      1234,
		LocalProcName: "ferry.exe",
		LookupTCP: func(ip string, port int) (map[int]localTCPProc, bool) {
			if ip != "172.22.18.28" || port != 22 {
				t.Errorf("unexpected lookup target %s:%d", ip, port)
			}
			return map[int]localTCPProc{
				55763: {PID: 7788, Name: "goland64.exe"},
				55800: {PID: 9900, Name: "ferry.exe", CmdLine: "ferry.exe mcp"},
			}, true
		},
	}, rep)

	if rep.SelfFrom != "172.21.106.3" || rep.NATPortTranslated {
		t.Fatalf("SelfFrom/NAT 错误：%+v", rep)
	}
	by := map[int]SSHSession{}
	for _, s := range rep.Sessions {
		by[s.PID] = s
	}
	if s := by[100]; s.Origin != "self" || s.OriginLabel != "当前连接" || s.LocalPID != 1234 {
		t.Errorf("self: %+v", s)
	}
	if s := by[200]; s.Origin != "ferry-pool" || !strings.Contains(s.OriginLabel, "独立连接：172.22.18.28") {
		t.Errorf("ferry-pool standalone: %+v", s)
	}
	if s := by[250]; s.Origin != "ferry-pool" || !strings.Contains(s.OriginLabel, "并发扩展") {
		t.Errorf("ferry-pool extra: %+v", s)
	}
	if s := by[300]; s.Origin != "local-proc" || s.OriginLabel != "本机 · goland64.exe" || s.LocalPID != 7788 {
		t.Errorf("local-proc ide: %+v", s)
	}
	if s := by[350]; s.Origin != "local-proc" || s.OriginLabel != "本机 · Ferry (MCP)" || s.LocalPID != 9900 {
		t.Errorf("local-proc mcp: %+v", s)
	}
	if s := by[400]; s.Origin != "same-ip" || s.OriginLabel != "同 IP 其他设备" {
		t.Errorf("same-ip other device: %+v", s)
	}
	if s := by[500]; s.Origin != "remote" || s.OriginLabel != "其他 IP" {
		t.Errorf("remote: %+v", s)
	}

	// 场景 2：NAT 改写源端口 —— 连接池里带常驻 agent PID 的连接依然能准确命中，其余同 IP 标为 nat-same-ip（不误判为非本机）
	repNAT := &SSHReport{
		Priv: "root",
		Me:   "root",
		Now:  2000,
		Sessions: []SSHSession{
			{PID: 100, User: "root", From: "172.17.0.1", FromPort: 41000, Kind: "ferry", Self: true, Kick: false},
			{PID: 200, User: "root", From: "172.17.0.1", FromPort: 42000, Kind: "ferry", Kick: true},
			{PID: 300, User: "root", From: "172.17.0.1", FromPort: 43000, Kind: "terminal", Kick: true},
		},
	}
	lookupCalled := false
	enrichSSHReportWith(enrichInput{
		CurrentConn:   curConn,
		SelfLocalPort: 51000, // 与服务端看到的 41000 不同 → 判定经过 NAT 端口转换
		RemoteIP:      "127.0.0.1",
		RemotePort:    2223,
		PoolSnaps: []poolConnSnap{
			{
				Conn:           otherConn,
				PrimaryPort:    52000,
				RemoteAgentPID: 205, // 远端 agent PID 是 PID 200 的子进程
				Refs:           []poolRefMeta{{Project: "demo", Target: "prod"}},
			},
		},
		DescToTop:     map[int]int{205: 200},
		LocalPID:      1234,
		LocalProcName: "ferry",
		LookupTCP: func(string, int) (map[int]localTCPProc, bool) {
			lookupCalled = true
			return nil, true
		},
	}, repNAT)

	if !repNAT.NATPortTranslated {
		t.Fatal("应该检测到 NATPortTranslated=true")
	}
	if lookupCalled {
		t.Error("经 NAT 端口转换后不应再做无意义的本机端口反查")
	}
	byNAT := map[int]SSHSession{}
	for _, s := range repNAT.Sessions {
		byNAT[s.PID] = s
	}
	if s := byNAT[200]; s.Origin != "ferry-pool" || !strings.Contains(s.OriginLabel, "项目：demo") {
		t.Errorf("NAT 下应通过 RemoteAgentPID 精确匹配连接池：%+v", s)
	}
	if s := byNAT[300]; s.Origin != "nat-same-ip" || s.OriginLabel != "同出口 IP" {
		t.Errorf("NAT 下其余同 IP 连接应标为 nat-same-ip：%+v", s)
	}
}

// TestParseSSHPortMappedAndNoShellTunnel 验证：
// 1) 客户端连外部映射端口（如 2223）而容器/服务器内部 sshd 监听 22 时，普通连接不会把自己的来源误报成端口转发；
// 2) ssh -N -L 纯端口转发（进程名为 sshd-session: deployer，没有 @notty）能被识别并准确提取转发目标。
func TestParseSSHPortMappedAndNoShellTunnel(t *testing.T) {
	out := `priv sudo
@@self 1002
@@now 5000
@@ps
   10     1 4000 ?        sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups
 1000    10   60 ?        sshd-session: deployer [priv]
 1001  1000   60 ?        sshd-session: deployer@notty
 1002  1001   60 ?        /usr/local/lib/ferry/ferry-agent-abc serve
 2000    10   30 ?        sshd-session: deployer [priv]
 2001  2000   30 ?        sshd-session: deployer
@@env
@@ss
LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=10,fd=7))
LISTEN 0 128 [::]:22 [::]:* users:(("sshd",pid=10,fd=8))
ESTAB 0 0 172.17.0.3:22 172.17.0.1:46666 users:(("sshd-session",pid=1000,fd=4))
ESTAB 0 0 172.17.0.3:22 172.17.0.1:46630 users:(("sshd-session",pid=2000,fd=4))
ESTAB 0 0 127.0.0.1:51234 127.0.0.1:22 users:(("sshd-session",pid=2001,fd=9))
@@who
@@tty
`
	// 注意：客户端配置的外部端口是 2223，但服务器内部实际 sshd 监听 22
	rep, err := parseSSH(strings.Split(out, "\n"), "deployer", 2223)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sessions) != 2 {
		t.Fatalf("应识别出 2 条会话（含 ssh -N 纯隧道），实际：%d (%+v)", len(rep.Sessions), rep.Sessions)
	}
	by := map[int]SSHSession{}
	for _, s := range rep.Sessions {
		by[s.PID] = s
	}
	if s := by[1000]; !s.Self || s.From != "172.17.0.1" || s.FromPort != 46666 || len(s.Forwards) != 0 {
		t.Errorf("普通映射端口会话不应有误报 forwards：%+v", s)
	}
	if s := by[2000]; s.User != "deployer" || s.From != "172.17.0.1" || s.FromPort != 46630 || s.Kind != "idle" || !slices.Equal(s.Forwards, []string{"127.0.0.1:22"}) {
		t.Errorf("ssh -N -L 纯端口转发会话解析失败：%+v", s)
	}
}
