package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWhoProbeCollect(t *testing.T) {
	proc := t.TempDir()
	pts := t.TempDir()
	os.WriteFile(filepath.Join(proc, "stat"), []byte("btime 1000\n"), 0o644)
	addProc := func(pid, ppid int, startTicks int64, cmd, env string) {
		dir := filepath.Join(proc, fmt.Sprint(pid))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.ReplaceAll(cmd, " ", "\x00")+"\x00"), 0o644)
		// stat 第 22 列（comm 之后第 20 列，即下标 19）是进程启动 ticks
		fields := make([]string, 20)
		fields[0] = "S"
		fields[1] = fmt.Sprint(ppid)
		for i := 2; i < 19; i++ {
			fields[i] = "0"
		}
		fields[19] = fmt.Sprint(startTicks)
		os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (proc) %s\n", pid, strings.Join(fields, " "))), 0o644)
		if env != "" {
			os.WriteFile(filepath.Join(dir, "environ"), []byte(env), 0o644)
		}
	}
	// 无关进程（不应出现在 @@ps 里）
	addProc(50, 1, 100, "/usr/bin/python3 worker.py", "")
	// sshd 与两个会话
	addProc(900, 1, 1000, "/usr/sbin/sshd -D", "")
	addProc(1200, 900, 40000, "sshd: root@pts/0", "")
	addProc(1201, 1200, 40000, "-bash", "HOME=/root\x00SSH_CONNECTION=1.2.3.4 50001 10.0.0.2 22\x00")
	addProc(1202, 1201, 90000, "top", "")
	addProc(2000, 900, 95000, "sshd: root@notty", "")
	addProc(2001, 2000, 95000, "/usr/local/lib/ferry/ferry-agent-abc serve", "")

	os.WriteFile(filepath.Join(pts, "0"), nil, 0o644)
	os.WriteFile(filepath.Join(pts, "ptmx"), nil, 0o644)

	p := &whoProbe{
		proc: proc,
		pts:  pts,
		self: 2001,
		now:  func() time.Time { return time.Unix(2000, 0) },
		atime: func(path string) (int64, bool) {
			if strings.HasSuffix(path, "0") {
				return 1990, true
			}
			return 0, false
		},
		run: func(cmd string, _ time.Duration) (string, error) {
			if strings.HasPrefix(cmd, "ss -") {
				return "LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:((\"sshd\",pid=900,fd=3))\n" +
					"ESTAB 0 0 10.0.0.2:22 1.2.3.4:50003 users:((\"sshd\",pid=1200,fd=3))\n" +
					"ESTAB 0 0 10.0.0.2:80 9.9.9.9:12345 users:((\"nginx\",pid=88,fd=6))\n", nil
			}
			return "root pts/0 2026-11-24 10:00 (1.2.3.4)\n", nil
		},
	}

	got := strings.Join(p.collect(), "\n")
	for _, want := range []string{
		"@@self 2001",
		"@@now 2000",
		"1200 900 600 ? sshd: root@pts/0",
		"1202 1201 100 ? top",
		"2001 2000 50 ? /usr/local/lib/ferry/ferry-agent-abc serve",
		"/proc/1201/environ:SSH_CONNECTION=1.2.3.4 50001 10.0.0.2 22",
		`ESTAB 0 0 10.0.0.2:22 1.2.3.4:50003 users:(("sshd",pid=1200,fd=3))`,
		"/dev/pts/0 1990",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q，实际输出：\n%s", want, got)
		}
	}
	if strings.Contains(got, "worker.py") || strings.Contains(got, "nginx") || strings.Contains(got, "ptmx") {
		t.Errorf("不应包含无关进程、非 sshd 连接或 ptmx：\n%s", got)
	}
}

func TestWhoProbeCollectAuthAndKeys(t *testing.T) {
	proc := t.TempDir()
	pts := t.TempDir()
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	// 用一个已知 base64 blob 验证 SHA256 指纹与备注解析（含选项前缀）
	// base64("test-ed25519-key-blob") = dGVzdC1lZDI1NTE5LWtleS1ibG9i
	authKeys := strings.Join([]string{
		"# comment",
		"ssh-ed25519 dGVzdC1lZDI1NTE5LWtleS1ibG9i zhangsan@MacBook",
		`command="/bin/echo hi",no-pty ssh-rsa dGVzdC1yc2Eta2V5LWJsb2I= deploy-bot@ci`,
	}, "\n") + "\n"
	os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(authKeys), 0o600)
	passwdFile := filepath.Join(t.TempDir(), "passwd")
	os.WriteFile(passwdFile, []byte("root:x:0:0:root:"+home+":/bin/bash\n"), 0o644)

	os.WriteFile(filepath.Join(proc, "stat"), []byte("btime 1000\n"), 0o644)
	addProc := func(pid, ppid int, cmd string) {
		dir := filepath.Join(proc, fmt.Sprint(pid))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.ReplaceAll(cmd, " ", "\x00")+"\x00"), 0o644)
		fields := make([]string, 20)
		fields[0] = "S"
		fields[1] = fmt.Sprint(ppid)
		for i := 2; i < 20; i++ {
			fields[i] = "0"
		}
		os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (proc) %s\n", pid, strings.Join(fields, " "))), 0o644)
	}
	addProc(900, 1, "/usr/sbin/sshd -D")
	addProc(1200, 900, "sshd: root@pts/0")
	addProc(1201, 1200, "-bash")

	fp1, cmt1, ok1 := parseAuthorizedKeyLine("ssh-ed25519 dGVzdC1lZDI1NTE5LWtleS1ibG9i zhangsan@MacBook")
	if !ok1 || cmt1 != "zhangsan@MacBook" || !strings.HasPrefix(fp1, "SHA256:") {
		t.Fatalf("parseAuthorizedKeyLine 失败：ok=%v fp=%q cmt=%q", ok1, fp1, cmt1)
	}
	fp2, cmt2, ok2 := parseAuthorizedKeyLine(`command="/bin/echo hi",no-pty ssh-rsa dGVzdC1yc2Eta2V5LWJsb2I= deploy-bot@ci`)
	if !ok2 || cmt2 != "deploy-bot@ci" || !strings.HasPrefix(fp2, "SHA256:") {
		t.Fatalf("带选项的 authorized_keys 解析失败：ok=%v fp=%q cmt=%q", ok2, fp2, cmt2)
	}

	p := &whoProbe{
		proc:   proc,
		pts:    pts,
		passwd: passwdFile,
		self:   1201,
		now:    func() time.Time { return time.Unix(2000, 0) },
		run: func(cmd string, _ time.Duration) (string, error) {
			if strings.Contains(cmd, "journalctl") {
				return fmt.Sprintf("Nov 25 15:00:01 host sshd[1200]: Accepted publickey for root from 172.21.106.3 port 58338 ssh2: ED25519 %s\n", fp1), nil
			}
			return "", nil
		},
	}

	got := strings.Join(p.collect(), "\n")
	for _, want := range []string{
		"@@auth",
		"Accepted publickey for root from 172.21.106.3 port 58338 ssh2: ED25519 " + fp1,
		"@@keys",
		fmt.Sprintf("root\t%s\tzhangsan@MacBook", fp1),
		fmt.Sprintf("root\t%s\tdeploy-bot@ci", fp2),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q，实际输出：\n%s", want, got)
		}
	}
}
