package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
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
	proc   string
	pts    string
	passwd string
	self   int
	now    func() time.Time
	atime  func(string) (int64, bool)
	run    func(cmd string, timeout time.Duration) (string, error)
}

func (a *Agent) who() int {
	p := &whoProbe{
		proc:   "/proc",
		pts:    "/dev/pts",
		passwd: "/etc/passwd",
		self:   os.Getpid(),
		now:    a.Sys.Now,
		atime:  fileAtime,
		run:    a.Sys.Shell,
	}
	return a.result(proto.CodeOK, "", p.collect())
}

const whoAuthLogCmd = `A=""
if command -v journalctl >/dev/null 2>&1; then
  A=$(journalctl -t sshd -t sshd-session --no-pager -o short -n 2000 2>/dev/null | grep 'Accepted ' | tail -n 200)
fi
if [ -z "$A" ]; then
  A=$(tail -n 1000 /var/log/auth.log /var/log/secure 2>/dev/null | grep 'Accepted ' | tail -n 200)
fi
[ -n "$A" ] && printf '%s\n' "$A"`

// collect 输出与 engine.parseSSH 完全兼容的分段行，但免去新开 SSH 通道、重复 sudo 和 ps/awk/grep 子进程。
func (p *whoProbe) collect() []string {
	now := p.now().Unix()
	var (
		psLines   []string
		envLines  []string
		ssLines   []string
		whoLines  []string
		ttyLines  []string
		authLines []string
		keyLines  []string
	)
	eachParallel(4, 4, func(i int) {
		switch i {
		case 0:
			var users []string
			psLines, envLines, users = p.readSSHProcs(now)
			ttyLines = p.readTTYs()
			keyLines = p.readAuthorizedKeys(users)
		case 1:
			if p.run == nil {
				return
			}
			if out, err := p.run("ss -atnp 2>/dev/null", 10*time.Second); err == nil {
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
		case 3:
			if p.run == nil {
				return
			}
			if out, err := p.run(whoAuthLogCmd, 3*time.Second); err == nil {
				for _, line := range splitLines(out) {
					if strings.Contains(line, "Accepted ") {
						authLines = append(authLines, line)
					}
				}
			}
		}
	})

	lines := make([]string, 0, 12+len(psLines)+len(envLines)+len(ssLines)+len(whoLines)+len(ttyLines)+len(authLines)+len(keyLines))
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
	lines = append(lines, "@@auth")
	lines = append(lines, authLines...)
	lines = append(lines, "@@keys")
	lines = append(lines, keyLines...)
	return lines
}

type whoProc struct {
	pid  int
	ppid int
	age  int64
	args string
}

func (p *whoProbe) readSSHProcs(now int64) (psLines, envLines, users []string) {
	btime := readProcBootTime(p.proc)
	ents, err := os.ReadDir(p.proc)
	if err != nil {
		return nil, nil, nil
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
	userSet := map[string]bool{}
	for _, pr := range found {
		if pr == nil {
			continue
		}
		procs[pr.pid] = pr
		kids[pr.ppid] = append(kids[pr.ppid], pr.pid)
		if isSSHDProc(pr.args) {
			sshdRoots = append(sshdRoots, pr.pid)
			if u := sshdSessionUser(pr.args); u != "" {
				userSet[u] = true
			}
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
	for u := range userSet {
		users = append(users, u)
	}
	sort.Strings(users)
	return psLines, envLines, users
}

func sshdSessionUser(args string) string {
	f := strings.Fields(args)
	if len(f) < 2 {
		return ""
	}
	if f[0] != "sshd:" && f[0] != "sshd-session:" {
		return ""
	}
	if u, _, ok := strings.Cut(f[1], "@"); ok && u != "" {
		return u
	}
	if len(f) >= 3 && f[2] == "[priv]" {
		return f[1]
	}
	if len(f) == 2 && !strings.HasPrefix(f[1], "/") && !strings.HasPrefix(f[1], "[") {
		return f[1]
	}
	return ""
}

func (p *whoProbe) readAuthorizedKeys(users []string) []string {
	if len(users) == 0 || p.passwd == "" {
		return nil
	}
	homes := readPasswdHomes(p.passwd, users)
	if len(homes) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, u := range users {
		home := homes[u]
		if home == "" {
			continue
		}
		for _, name := range []string{"authorized_keys", "authorized_keys2"} {
			b, err := readLimitedFile(filepath.Join(home, ".ssh", name), 256<<10)
			if err != nil || len(b) == 0 {
				continue
			}
			for _, rawLine := range strings.Split(string(b), "\n") {
				fp, comment, ok := parseAuthorizedKeyLine(rawLine)
				if !ok || comment == "" {
					continue
				}
				k := u + "\t" + fp
				if seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, fmt.Sprintf("%s\t%s\t%s", u, fp, comment))
			}
		}
	}
	return out
}

func readPasswdHomes(passwdPath string, users []string) map[string]string {
	b, err := os.ReadFile(passwdPath)
	if err != nil {
		return nil
	}
	want := map[string]bool{}
	for _, u := range users {
		want[u] = true
	}
	homes := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 6 {
			continue
		}
		home := parts[5]
		if len(parts) > 7 {
			// 兼容 Windows 单元测试里含盘符冒号的路径（/etc/passwd 最后一列为 shell）
			home = strings.Join(parts[5:len(parts)-1], ":")
		}
		if want[parts[0]] && home != "" {
			homes[parts[0]] = home
		}
	}
	return homes
}

func readLimitedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, maxBytes)
	n, err := f.Read(buf)
	if n > 0 {
		return buf[:n], nil
	}
	return nil, err
}

// parseAuthorizedKeyLine 解析 authorized_keys 单行，返回 OpenSSH SHA256 指纹与末尾备注。
// 支持带选项前缀的行（如 command="...",no-pty ssh-ed25519 AAAA... alice@mac）。
func parseAuthorizedKeyLine(line string) (fingerprint, comment string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		inQuote := false
		for i < len(s) {
			c := s[i]
			if c == '\\' && inQuote && i+1 < len(s) {
				i += 2
				continue
			}
			if c == '"' {
				inQuote = !inQuote
				i++
				continue
			}
			if !inQuote && (c == ' ' || c == '\t') {
				break
			}
			i++
		}
		tok := s[start:i]
		if !isSSHKeyType(tok) {
			continue
		}
		rest := strings.TrimSpace(s[i:])
		if rest == "" {
			return "", "", false
		}
		keyB64, cmt, _ := strings.Cut(rest, " ")
		if tabIdx := strings.IndexByte(keyB64, '\t'); tabIdx >= 0 {
			cmt = strings.TrimSpace(keyB64[tabIdx+1:] + " " + cmt)
			keyB64 = keyB64[:tabIdx]
		}
		blob, err := base64.StdEncoding.DecodeString(keyB64)
		if err != nil || len(blob) < 8 {
			return "", "", false
		}
		sum := sha256.Sum256(blob)
		fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
		cmt = strings.TrimSpace(strings.ReplaceAll(cmt, "\t", " "))
		return fp, cmt, true
	}
	return "", "", false
}

func isSSHKeyType(tok string) bool {
	return strings.HasPrefix(tok, "ssh-") ||
		strings.HasPrefix(tok, "ecdsa-sha2-") ||
		strings.HasPrefix(tok, "sk-ssh-") ||
		strings.HasPrefix(tok, "sk-ecdsa-")
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
