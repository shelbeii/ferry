package engine

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 服务器上连着的 SSH：谁（登录用户）、从哪里（对方 IP）、什么时候连上、在干什么（终端、传文件、执行命令），可以断开。
// 不用 agent，直接在登录用户下执行脚本（能 sudo 就用 root），「新建连接」的服务器也能用。
//
// sshd 为每个连接开的进程：
//
//	sshd: alice [priv]        root 所有（OpenSSH 9.8 起叫 sshd-session）
//	└ sshd: alice@pts/0,pts/1 登录用户所有，标题里是这个连接的终端（没有终端是 notty）
//	  └ -bash、sftp-server、执行的命令……
//
// root 登录时只有一个 sshd: root@pts/0。结束最上面那个 sshd 进程就断开了这个连接。

// SSHSession 服务器上一个已登录的 SSH 连接。
type SSHSession struct {
	// PID 这个连接最上面的 sshd 进程（断开时用它指定）
	PID  int    `json:"pid"`
	User string `json:"user"`
	// From 对方的 IP；看不到时为空（登录用户不是 root、不能 sudo，又不是自己的连接）
	From     string `json:"from,omitempty"`
	FromPort int    `json:"fromPort,omitempty"`
	// TTYs 这个连接开的终端（pts/0……）；传文件、执行命令、端口转发没有终端
	TTYs []string `json:"ttys,omitempty"`
	// Since 连上的时间（Unix 秒）
	Since int64 `json:"since"`
	// Idle 终端多久没有输入（秒）；没有终端时为 -1
	Idle int64 `json:"idle"`
	// Cmds 正在运行的程序（shell 里在跑的命令）
	Cmds []string `json:"cmds,omitempty"`
	// Kind terminal（终端）| sftp（传文件）| ferry（Ferry 在操作）| exec（执行命令）| idle（没有在运行的程序：空闲或端口转发）
	Kind string `json:"kind"`
	// Self 本机 Ferry 正在用的连接（查看这个列表也是经它）：不能在这里断开
	Self bool `json:"self,omitempty"`
	// Kick 登录用户能不能断开它（root、能 sudo 时都能；否则只能断开自己用户的连接）
	Kick bool `json:"kick"`

	pids []int // 这个连接的 sshd 进程（断开时一起结束）
}

// SSHReport 服务器上连着的 SSH。
type SSHReport struct {
	// Priv 查看用的权限：root | sudo | none（登录用户不是 root，也不能免密 sudo）
	Priv string `json:"priv"`
	// Me 登录用户
	Me       string       `json:"me"`
	Sessions []SSHSession `json:"sessions"`
	// Pending 正在登录、还没通过验证的连接数（很多时多半有人在试密码）
	Pending int      `json:"pending,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	// Now 服务器上的当前时间（Unix 秒）：算连了多久用它，不受本机时钟影响
	Now int64 `json:"now"`
}

// sshScript 列出 sshd 的进程、它们的子进程、连接的对方地址。分段输出，由 parseSSH 解析。
// 对方地址：root 时从 ss 看 sshd 的 TCP 连接；再从子进程的环境变量 SSH_CONNECTION 看（自己用户的能读）；终端的还有 who。
const sshScript = `echo "@@self $$"
echo "@@now $(date +%s)"
P=$(ps -ww -eo pid=,ppid=,etimes=,tty=,args= 2>/dev/null) || P=$(ps -ww -eo pid=,ppid=,etime=,tty=,args= 2>/dev/null)
echo "@@ps"
echo "$P"
K=$(echo "$P" | awk '{ n[NR] = $1; p[NR] = $2; a[NR] = $5; if ($5 ~ /^sshd/) s[$1] = 1 } END { for (i = 1; i <= NR; i++) if ((p[i] in s) && a[i] !~ /^sshd/) print "/proc/" n[i] "/environ" }')
echo "@@env"
[ -n "$K" ] && grep -a -s -o -m1 -H 'SSH_CONNECTION=[0-9A-Fa-f.:% ]*' $K
echo "@@ss"
[ "$(id -u)" = 0 ] && ss -tnp 2>/dev/null | grep sshd
echo "@@who"
who 2>/dev/null
echo "@@tty"
stat -c '%n %X' /dev/pts/[0-9]* 2>/dev/null
true
`

// SSHSessions 服务器上已登录的 SSH 连接。
func (e *Engine) SSHSessions(target string) (*SSHReport, error) {
	var rep *SSHReport
	err := e.withConn(target, func(s *session) error {
		var err error
		rep, err = listSSH(s)
		return err
	})
	return rep, err
}

// withConn 用目标的连接（不需要应用）。
func (e *Engine) withConn(target string, fn func(s *session) error) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	s, err := e.connect(t)
	if err != nil {
		return err
	}
	defer s.close()
	err = fn(s)
	if err != nil && !s.c.Alive() {
		s.broken.Store(true)
	}
	return err
}

func listSSH(s *session) (*SSHReport, error) {
	var lines []string
	res, err := s.c.Run(privWrap, []byte(sshScript), func(l string) { lines = append(lines, l) })
	if err != nil {
		return nil, err
	}
	if res.Code != 0 && len(lines) == 0 {
		return nil, fmt.Errorf("查看 SSH 连接失败：%s", strings.TrimSpace(res.Stderr))
	}
	return parseSSH(lines, s.t.User, keepPorts(s)[0])
}

// SSHKick 断开这些连接（pids 为 SSHSession.PID），返回断开后的情况；断不开的报错（同时返回现在的情况）。
func (e *Engine) SSHKick(target string, pids []int) (*SSHReport, error) {
	if len(pids) == 0 {
		return nil, errors.New("要指定断开哪个连接（PID）")
	}
	var rep *SSHReport
	err := e.withConn(target, func(s *session) error {
		cur, err := listSSH(s)
		if err != nil {
			return err
		}
		byPID := map[int]*SSHSession{}
		for i := range cur.Sessions {
			byPID[cur.Sessions[i].PID] = &cur.Sessions[i]
		}
		var kill []int
		var what []string
		for _, p := range pids {
			ss := byPID[p]
			switch {
			case ss == nil:
				continue // 已经断开了
			case ss.Self:
				return fmt.Errorf("PID %d 是本机 Ferry 正在用的连接，不能在这里断开", p)
			case !ss.Kick:
				return fmt.Errorf("登录用户 %s 不是 root，也不能免密 sudo：只能断开 %s 自己的连接，%s 的连接（PID %d）断不开", cur.Me, cur.Me, ss.User, p)
			}
			kill = append(kill, ss.pids...)
			what = append(what, sessionDesc(ss))
		}
		if len(kill) == 0 {
			rep = cur
			return nil
		}
		if s.t.NeedConfirm() && !e.Yes {
			if !e.Rep.Confirm(fmt.Sprintf("断开 %s 上的 %s？里面正在运行的命令会被结束", s.t.Name, strings.Join(what, "、"))) {
				return ErrCanceled
			}
		}
		if _, err := s.c.Run(privWrap, []byte(kickScript(kill)), nil); err != nil {
			return err
		}
		if rep, err = listSSH(s); err != nil {
			return err
		}
		var left []string
		for _, p := range pids {
			for _, x := range rep.Sessions {
				if x.PID == p && byPID[p] != nil {
					left = append(left, sessionDesc(&x))
				}
			}
		}
		if len(left) > 0 {
			return fmt.Errorf("没有断开：%s", strings.Join(left, "、"))
		}
		return nil
	})
	return rep, err
}

func sessionDesc(s *SSHSession) string {
	d := s.User
	if s.From != "" {
		d += "（来自 " + s.From + "）"
	}
	return fmt.Sprintf("%s 的连接 PID %d", d, s.PID)
}

// kickScript 先 TERM，2 秒内没退出的 KILL。每次都确认还是 sshd 进程（PID 可能已经给了别的进程）。
func kickScript(pids []int) string {
	ps := make([]string, len(pids))
	for i, p := range pids {
		ps[i] = strconv.Itoa(p)
	}
	return fmt.Sprintf(`S="%s"
alive() { case "$(ps -o args= -p $1 2>/dev/null)" in sshd*) return 0;; esac; return 1; }
for p in $S; do alive $p && kill -TERM $p 2>/dev/null; done
i=0; left=$S
while [ $i -lt 8 ] && [ -n "$left" ]; do
  sleep 0.25 2>/dev/null || sleep 1
  l=; for p in $left; do alive $p && l="$l $p"; done; left=$l; i=$((i+1))
done
for p in $left; do alive $p && kill -KILL $p 2>/dev/null; done
[ -n "$left" ] && sleep 0.5 2>/dev/null
true
`, strings.Join(ps, " "))
}

// ---------------------------------------------------------------- 解析

type psProc struct {
	pid, ppid int
	age       int64 // 运行了多少秒
	tty       string
	args      string
}

var (
	// sshd: alice@pts/0,pts/1、sshd-session: alice@notty、sshd: alice@internal-sftp
	reSSHUser = regexp.MustCompile(`^sshd(?:-session)?: (\S+)@(\S+)$`)
	reSSHPriv = regexp.MustCompile(`^sshd(?:-session)?: (\S+) \[priv\]$`)
	reSSHPre  = regexp.MustCompile(`^sshd(?:-session|-auth)?: .*\[(?:net|preauth)\]$`)
	reSSHPeer = regexp.MustCompile(`\s\S+:(\d+)\s+(\S+):(\d+)\s+users:\(\((.*)\)\)`)
	reSSHPid  = regexp.MustCompile(`pid=(\d+)`)
	reWhoHost = regexp.MustCompile(`\(([^)]*)\)\s*$`)
)

// sshPort Ferry 连这台服务器用的端口（ss 里认哪个是客户端连进来的）。
func parseSSH(lines []string, me string, sshPort int) (*SSHReport, error) {
	rep := &SSHReport{Priv: "none", Me: me, Sessions: []SSHSession{}}
	var self int
	var now int64
	sec := ""
	procs := map[int]*psProc{}
	kids := map[int][]int{}
	env := map[int][2]string{}  // pid → 对方 IP、端口
	peer := map[int][2]string{} // sshd 的 pid → 对方 IP、端口（ss）
	peerOnSSH := map[int]bool{}
	whoHost := map[string]string{}
	ttyAt := map[string]int64{}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "priv "):
			rep.Priv = strings.TrimSpace(l[5:])
			continue
		case strings.HasPrefix(l, "@@self "):
			self, _ = strconv.Atoi(strings.TrimSpace(l[7:]))
			continue
		case strings.HasPrefix(l, "@@now "):
			now, _ = strconv.ParseInt(strings.TrimSpace(l[6:]), 10, 64)
			continue
		case strings.HasPrefix(l, "@@"):
			sec = strings.TrimSpace(l[2:])
			continue
		}
		switch sec {
		case "ps":
			f := strings.Fields(l)
			if len(f) < 5 {
				continue
			}
			pid, err1 := strconv.Atoi(f[0])
			ppid, err2 := strconv.Atoi(f[1])
			if err1 != nil || err2 != nil {
				continue
			}
			p := &psProc{pid: pid, ppid: ppid, age: psAge(f[2]), tty: f[3], args: strings.Join(f[4:], " ")}
			procs[pid] = p
			kids[ppid] = append(kids[ppid], pid)
		case "env":
			// /proc/123/environ:SSH_CONNECTION=1.2.3.4 50000 10.0.0.2 22
			file, v, ok := strings.Cut(l, ":SSH_CONNECTION=")
			if !ok {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(file, "/proc/"), "/environ"))
			if f := strings.Fields(v); err == nil && len(f) >= 2 {
				env[pid] = [2]string{f[0], f[1]}
			}
		case "ss":
			m := reSSHPeer.FindStringSubmatch(l)
			if m == nil || !strings.HasPrefix(strings.TrimSpace(l), "ESTAB") {
				continue
			}
			// sshd 的其他 TCP 连接是端口转发：只认本地端口是 SSH 端口的（认不出 SSH 端口时取第一个）
			local := atoi(m[1])
			for _, pm := range reSSHPid.FindAllStringSubmatch(m[4], -1) {
				pid, err := strconv.Atoi(pm[1])
				if err != nil {
					continue
				}
				if _, ok := peer[pid]; !ok || (local == sshPort && !peerOnSSH[pid]) {
					peer[pid] = [2]string{cleanIP(m[2]), m[3]}
					peerOnSSH[pid] = local == sshPort
				}
			}
		case "who":
			f := strings.Fields(l)
			if m := reWhoHost.FindStringSubmatch(l); m != nil && len(f) >= 2 && m[1] != "" && !strings.HasPrefix(m[1], ":") {
				whoHost[f[1]] = m[1]
			}
		case "tty":
			f := strings.Fields(l)
			if len(f) == 2 {
				if t, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					ttyAt[strings.TrimPrefix(f[0], "/dev/")] = t
				}
			}
		}
	}
	if len(procs) == 0 {
		return nil, errors.New("查看 SSH 连接失败：服务器上的 ps 不支持（需要 procps）")
	}
	if now == 0 {
		now = time.Now().Unix()
	}
	rep.Now = now

	// 本机 Ferry 的连接：这个脚本的上级进程
	ancestors := map[int]bool{}
	for p, n := self, 0; p > 1 && n < 64; n++ {
		ancestors[p] = true
		pp := procs[p]
		if pp == nil {
			break
		}
		p = pp.ppid
	}

	seen := map[int]bool{}
	for _, p := range procs {
		if reSSHPre.MatchString(p.args) {
			rep.Pending++
		}
		m := reSSHUser.FindStringSubmatch(p.args)
		if m == nil {
			continue
		}
		top := p
		if pp := procs[p.ppid]; pp != nil && reSSHPriv.MatchString(pp.args) {
			top = pp
		}
		if seen[top.pid] {
			continue
		}
		seen[top.pid] = true
		ss := SSHSession{PID: top.pid, User: m[1], Since: now - top.age, Idle: -1, Kind: "idle", pids: []int{top.pid}}
		if top != p {
			ss.pids = append(ss.pids, p.pid)
		}
		sftp := m[2] == "internal-sftp"
		for _, t := range strings.Split(m[2], ",") {
			if strings.HasPrefix(t, "pts/") || strings.HasPrefix(t, "tty") {
				ss.TTYs = append(ss.TTYs, t)
				if at, ok := ttyAt[t]; ok {
					if idle := max(now-at, 0); ss.Idle < 0 || idle < ss.Idle {
						ss.Idle = idle
					}
				}
			}
		}
		// 对方地址：ss（root）> 子进程的 SSH_CONNECTION > who（终端）
		if a, ok := peer[top.pid]; ok {
			ss.From, ss.FromPort = a[0], atoi(a[1])
		} else if a, ok := peer[p.pid]; ok {
			ss.From, ss.FromPort = a[0], atoi(a[1])
		}
		ferry, exec := false, false
		for _, c := range kids[p.pid] {
			cp := procs[c]
			if strings.HasPrefix(cp.args, "sshd") || ancestors[c] {
				continue // 查看这个列表的脚本本身不算
			}
			exec = true
			if a, ok := env[c]; ok && ss.From == "" {
				ss.From, ss.FromPort = cleanIP(a[0]), atoi(a[1])
			}
			ss.Cmds = append(ss.Cmds, running(cp, procs, kids)...)
			walk(c, procs, kids, func(d *psProc) {
				switch {
				case strings.Contains(d.args, "ferry-agent-"):
					ferry = true
				case strings.Contains(d.args, "sftp-server"):
					sftp = true
				}
			})
		}
		if ss.From == "" {
			for _, t := range ss.TTYs {
				if h := whoHost[t]; h != "" {
					ss.From = h
					break
				}
			}
		}
		switch {
		case len(ss.TTYs) > 0:
			ss.Kind = "terminal"
		case ferry:
			ss.Kind = "ferry"
		case sftp:
			ss.Kind = "sftp"
		case exec:
			ss.Kind = "exec"
		}
		ss.Self = ancestors[top.pid] || ancestors[p.pid]
		if ss.Self {
			ss.Kind = "ferry" // Ferry 的连接上还开着传文件（SFTP）、终端等
		}
		if len(ss.Cmds) > 4 {
			ss.Cmds = append(ss.Cmds[:4], fmt.Sprintf("等 %d 个", len(ss.Cmds)))
		}
		ss.Kick = !ss.Self && (rep.Priv != "none" || ss.User == me)
		rep.Sessions = append(rep.Sessions, ss)
	}
	// 本机 Ferry 的在最前，其余新连上的在前
	sort.SliceStable(rep.Sessions, func(i, j int) bool {
		a, b := rep.Sessions[i], rep.Sessions[j]
		if a.Self != b.Self {
			return a.Self
		}
		if a.Since != b.Since {
			return a.Since > b.Since
		}
		return a.PID < b.PID
	})
	if rep.Priv == "none" {
		rep.Notes = append(rep.Notes, fmt.Sprintf("登录用户 %s 不是 root，也不能免密 sudo：别的用户的连接可能看不到来源 IP，也只能断开 %s 自己的连接", me, me))
	}
	return rep, nil
}

// running 一个子进程在干什么：是登录 shell 而且里面在跑命令时，列出那些命令。
func running(p *psProc, procs map[int]*psProc, kids map[int][]int) []string {
	if isShell(p.args) {
		var out []string
		for _, c := range kids[p.pid] {
			out = append(out, shortCmd(procs[c].args))
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{shortCmd(p.args)}
}

func isShell(args string) bool {
	f := strings.Fields(args)
	if len(f) == 0 {
		return false
	}
	name := strings.TrimPrefix(f[0], "-")
	name = name[strings.LastIndexByte(name, '/')+1:]
	switch name {
	case "bash", "sh", "zsh", "ksh", "fish", "dash", "ash", "tcsh", "csh":
		return len(f) == 1 || strings.HasPrefix(f[0], "-") || f[1] == "-l" || f[1] == "--login"
	}
	return false
}

func shortCmd(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

func walk(pid int, procs map[int]*psProc, kids map[int][]int, fn func(*psProc)) {
	var rec func(int, int)
	rec = func(p, depth int) {
		if depth > 32 || procs[p] == nil {
			return
		}
		fn(procs[p])
		for _, c := range kids[p] {
			rec(c, depth+1)
		}
	}
	rec(pid, 0)
}

// psAge etimes（秒）或 etime（[[dd-]hh:]mm:ss）。
func psAge(s string) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	var days int64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseInt(d, 10, 64)
		s = rest
	}
	var t int64
	for _, part := range strings.Split(s, ":") {
		n, _ := strconv.ParseInt(part, 10, 64)
		t = t*60 + n
	}
	return days*86400 + t
}

// cleanIP 去掉 [ ]、IPv4 映射的 ::ffff: 前缀、网卡后缀 %eth0。
func cleanIP(s string) string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	s = strings.TrimPrefix(s, "::ffff:")
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	return s
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
