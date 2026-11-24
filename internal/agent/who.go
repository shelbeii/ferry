package agent

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type whoProbe struct {
	proc  string
	pts   string
	self  int
	now   func() time.Time
	atime func(string) (int64, bool)
	run   func(cmd string, timeout time.Duration) (string, error)
}

func (a *Agent) who() int {
	p := &whoProbe{
		proc:  "/proc",
		pts:   "/dev/pts",
		self:  os.Getpid(),
		now:   a.Sys.Now,
		atime: fileAtime,
		run:   a.Sys.Shell,
	}
	return a.result(proto.CodeOK, "", p.collect())
}

// collect 输出与 engine.parseSSH 完全兼容的分段行，但免去新开 SSH 通道、重复 sudo 和 ps/awk/grep 子进程。
func (p *whoProbe) collect() []string {
	now := p.now().Unix()
	var (
		psLines  []string
		envLines []string
		ssLines  []string
		whoLines []string
		ttyLines []string
	)
	eachParallel(3, 3, func(i int) {
		switch i {
		case 0:
			psLines, envLines = p.readSSHProcs(now)
			ttyLines = p.readTTYs()
		case 1:
			if p.run == nil {
				return
			}
			if out, err := p.run("ss -tnp 2>/dev/null", 10*time.Second); err == nil {
				for _, line := range splitLines(out) {
					if strings.Contains(line, "sshd") {
						ssLines = append(ssLines, line)
					}
				}
			}
		case 2:
			if p.run == nil {
				return
			}
			if out, err := p.run("who 2>/dev/null", 5*time.Second); err == nil {
				whoLines = splitLines(out)
			}
		}
	})

	lines := make([]string, 0, 8+len(psLines)+len(envLines)+len(ssLines)+len(whoLines)+len(ttyLines))
	lines = append(lines,
		fmt.Sprintf("@@self %d", p.self),
		fmt.Sprintf("@@now %d", now),
		"@@ps",
	)
	lines = append(lines, psLines...)
	lines = append(lines, "@@env")
	lines = append(lines, envLines...)
	lines = append(lines, "@@ss")
	lines = append(lines, ssLines...)
	lines = append(lines, "@@who")
	lines = append(lines, whoLines...)
	lines = append(lines, "@@tty")
	lines = append(lines, ttyLines...)
	return lines
}

type whoProc struct {
	pid  int
	ppid int
	age  int64
	args string
}

func (p *whoProbe) readSSHProcs(now int64) (psLines, envLines []string) {
	btime := readProcBootTime(p.proc)
	ents, err := os.ReadDir(p.proc)
	if err != nil {
		return nil, nil
	}
	var pids []int
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	found := make([]*whoProc, len(pids))
	eachParallel(len(pids), 4, func(i int) {
		pid := pids[i]
		dir := filepath.Join(p.proc, strconv.Itoa(pid))
		cmd, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || len(cmd) == 0 {
			return
		}
		args := strings.TrimSpace(string(bytes.ReplaceAll(bytes.TrimRight(cmd, "\x00"), []byte{0}, []byte{' '})))
		if args == "" {
			return
		}
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			return
		}
		f := statFields(string(stat))
		if len(f) < 20 {
			return
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			return
		}
		var age int64
		if ticks, err := strconv.ParseInt(f[19], 10, 64); err == nil && btime > 0 {
			age = max(now-(btime+ticks/100), 0)
		}
		found[i] = &whoProc{pid: pid, ppid: ppid, age: age, args: args}
	})

	procs := map[int]*whoProc{}
	kids := map[int][]int{}
	var sshdRoots []int
	for _, pr := range found {
		if pr == nil {
			continue
		}
		procs[pr.pid] = pr
		kids[pr.ppid] = append(kids[pr.ppid], pr.pid)
		if isSSHDProc(pr.args) {
			sshdRoots = append(sshdRoots, pr.pid)
		}
	}

	// 只保留 sshd 及其全部子孙进程（与 sshScript 的 awk 过滤等价），顺带确保 self 的祖先链在列
	keep := map[int]bool{}
	var queue []int
	for _, pid := range sshdRoots {
		if !keep[pid] {
			keep[pid] = true
			queue = append(queue, pid)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range kids[cur] {
			if !keep[c] {
				keep[c] = true
				queue = append(queue, c)
			}
		}
	}
	if p.self > 0 {
		for cur, n := p.self, 0; cur > 1 && n < 64; n++ {
			pr := procs[cur]
			if pr == nil {
				break
			}
			keep[cur] = true
			cur = pr.ppid
		}
	}

	keptPIDs := make([]int, 0, len(keep))
	for pid := range keep {
		keptPIDs = append(keptPIDs, pid)
	}
	sort.Ints(keptPIDs)
	for _, pid := range keptPIDs {
		pr := procs[pid]
		psLines = append(psLines, fmt.Sprintf("%d %d %d ? %s", pr.pid, pr.ppid, pr.age, pr.args))
		if parent := procs[pr.ppid]; parent != nil && isSSHDProc(parent.args) && !isSSHDProc(pr.args) {
			if val := readSSHConnectionEnv(filepath.Join(p.proc, strconv.Itoa(pr.pid), "environ")); val != "" {
				envLines = append(envLines, fmt.Sprintf("/proc/%d/environ:SSH_CONNECTION=%s", pr.pid, val))
			}
		}
	}
	return psLines, envLines
}

func isSSHDProc(args string) bool {
	if strings.HasPrefix(args, "sshd") {
		return true
	}
	// 有些系统 ps args 的第 0 段是 /usr/sbin/sshd
	f := strings.Fields(args)
	return len(f) > 0 && strings.HasSuffix(f[0], "/sshd")
}

func readProcBootTime(proc string) int64 {
	b, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			bt, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return bt
		}
	}
	return 0
}

func readSSHConnectionEnv(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, entry := range bytes.Split(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(entry, []byte("SSH_CONNECTION=")); ok {
			return string(v)
		}
	}
	return ""
}

func (p *whoProbe) readTTYs() []string {
	if p.pts == "" || p.atime == nil {
		return nil
	}
	ents, err := os.ReadDir(p.pts)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if at, ok := p.atime(filepath.Join(p.pts, e.Name())); ok {
			out = append(out, fmt.Sprintf("/dev/pts/%s %d", e.Name(), at))
		}
	}
	sort.Strings(out)
	return out
}
