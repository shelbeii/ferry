package engine

import (
	"strings"
	"testing"
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
	// 端口转发的 TCP 连接不能当成来源；两个终端取最近有输入的
	if s := by[4000]; s.From != "5.6.7.8" || s.FromPort != 50004 || len(s.TTYs) != 2 || s.Idle != 100 || s.Cmds[0] != "-bash" {
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
