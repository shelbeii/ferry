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
			if strings.HasPrefix(cmd, "ss -tnp") {
				return "ESTAB 0 0 10.0.0.2:22 1.2.3.4:50003 users:((\"sshd\",pid=1200,fd=3))\n" +
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
