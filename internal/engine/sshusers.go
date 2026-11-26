package engine

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
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

	// Origin 连接归属分类：
	//   "self"        当前查询所用的本机 Ferry 连接
	//   "ferry-pool"  本机 Ferry 连接池里的其他连接（其他项目、独立连接、或并发溢出扩展连接）
	//   "local-proc"  本机上的其他进程（如 ssh、Xshell、GoLand、另一实例 Ferry MCP/CLI）
	//   "same-ip"     同出口 IP 的其他设备（确认无 NAT 端口转换且本机未占用该源端口）
	//   "nat-same-ip" 同出口 IP，但经过 NAT/代理端口转换或本机 TCP 表不可用，无法仅靠端口确认是否本机
	//   "remote"      其他 IP 来源
	Origin string `json:"origin,omitempty"`
	// OriginLabel 面向用户的简短归属标签（如「当前连接」「本机 Ferry（独立连接：xxx）」「本机 · ssh.exe」「同 IP 其他设备」）
	OriginLabel string `json:"originLabel,omitempty"`
	// OriginDetail 归属详情说明（用于悬停提示或展开查看）
	OriginDetail string `json:"originDetail,omitempty"`
	// LocalPID 匹配到本机进程时的本地进程 PID
	LocalPID int `json:"localPid,omitempty"`
	// LocalProcess 匹配到本机进程时的进程名
	LocalProcess string `json:"localProcess,omitempty"`

	// AuthMethod 服务端日志识别出的认证方式（publickey | password | keyboard-interactive）
	AuthMethod string `json:"authMethod,omitempty"`
	// AuthKey 公钥登录时的密钥指纹（如 SHA256:...）
	AuthKey string `json:"authKey,omitempty"`
	// AuthKeyComment 从目标用户 authorized_keys 反查到的公钥备注（如 zhangsan@MacBook）
	AuthKeyComment string `json:"authKeyComment,omitempty"`
	// Forwards 该连接当前活跃的端口转发目标（如 127.0.0.1:3306）
	Forwards []string `json:"forwards,omitempty"`

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
	// SelfFrom 当前 Ferry 连接在服务器侧看到的客户端 IP（用于区分同 IP 连接）
	SelfFrom string `json:"selfFrom,omitempty"`
	// NATPortTranslated 客户端本机源端口与服务器看到的源端口不一致（说明经过了端口转换 NAT / 代理）
	NATPortTranslated bool `json:"natPortTranslated,omitempty"`
}

// localTCPLookup 可在单元测试里替换本机 TCP 进程表查询函数。
var localTCPLookup = lookupLocalTCP

// sshScript 列出 sshd 的进程、它们的子进程、连接的对方地址、认证日志与公钥备注。分段输出，由 parseSSH 解析。
// 对方地址：root 时从 ss 看 sshd 的 TCP 连接；再从子进程的环境变量 SSH_CONNECTION 看（自己用户的能读）；终端的还有 who。
const sshScript = `echo "@@self $$"
echo "@@now $(date +%s)"
P=$(ps -ww -eo pid=,ppid=,etimes=,tty=,args= 2>/dev/null) || P=$(ps -ww -eo pid=,ppid=,etime=,tty=,args= 2>/dev/null)
echo "@@ps"
# 只发回 sshd 的进程和它们的子孙进程：服务器上进程很多时（上万个没回收的僵尸进程），全部发回来有好几 MB
echo "$P" | awk '{ id[NR] = $1; pp[NR] = $2; l[NR] = $0; if ($5 ~ /^sshd/) k[$1] = 1 }
END { do { c = 0; for (i = 1; i <= NR; i++) if (!(id[i] in k) && (pp[i] in k)) { k[id[i]] = 1; c = 1 } } while (c)
  for (i = 1; i <= NR; i++) if (id[i] in k) print l[i] }' 2>/dev/null || echo "$P"
K=$(echo "$P" | awk '{ n[NR] = $1; p[NR] = $2; a[NR] = $5; if ($5 ~ /^sshd/) s[$1] = 1 } END { for (i = 1; i <= NR; i++) if ((p[i] in s) && a[i] !~ /^sshd/) print "/proc/" n[i] "/environ" }')
echo "@@env"
[ -n "$K" ] && grep -a -s -o -m1 -H 'SSH_CONNECTION=[0-9A-Fa-f.:% ]*' $K
echo "@@ss"
[ "$(id -u)" = 0 ] && ss -atnp 2>/dev/null | grep sshd
echo "@@who"
who 2>/dev/null
echo "@@tty"
stat -c '%n %X' /dev/pts/[0-9]* 2>/dev/null
echo "@@auth"
if [ "$(id -u)" = 0 ]; then
  A=""
  if command -v journalctl >/dev/null 2>&1; then
    A=$(journalctl -t sshd -t sshd-session --no-pager -o short -n 2000 2>/dev/null | grep 'Accepted ' | tail -n 200)
  fi
  if [ -z "$A" ]; then
    A=$(tail -n 1000 /var/log/auth.log /var/log/secure 2>/dev/null | grep 'Accepted ' | tail -n 200)
  fi
  [ -n "$A" ] && printf '%s\n' "$A"
fi
echo "@@keylines"
USERS=$(echo "$P" | awk '$5 ~ /^sshd/ { if ($6 ~ /@/) { u=$6; sub(/@.*/, "", u); seen[u]=1 } else if ($7 == "[priv]" || (NF == 6 && $6 !~ /^[/[]/)) { seen[$6]=1 } } END { for (x in seen) print x }')
for u in $USERS; do
  h=$(awk -F: -v u="$u" '$1==u {print $6; exit}' /etc/passwd 2>/dev/null)
  [ -n "$h" ] || continue
  for kf in "$h/.ssh/authorized_keys" "$h/.ssh/authorized_keys2"; do
    [ -r "$kf" ] && head -n 200 "$kf" 2>/dev/null | awk -v u="$u" 'NF && $1 !~ /^#/ { printf "%s\t%s\n", u, $0 }'
  done
done
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
	s, err := e.connectSSH(t)
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
	// 连接上已经准备好常驻 agent 时，直接走常驻 agent 原生采集：
	// 免去新开 SSH 通道、ps/awk/grep 子进程，以及 sudoAuth=password 目标每次重新提权的开销。
	if !s.t.Plain && s.agent != "" {
		var lines []string
		err := s.callData("who", &proto.Request{}, &lines)
		if err == nil {
			priv := "sudo"
			if s.t.User == "root" {
				priv = "root"
			}
			lines = append([]string{"priv " + priv}, lines...)
			rep, descToTop, perr := parseSSHInternal(lines, s.t.User, keepPorts(s)[0])
			if perr != nil {
				return nil, perr
			}
			enrichSSHReport(s, rep, descToTop)
			return rep, nil
		}
		if errors.Is(err, ErrCanceled) || s.e.ctx().Err() != nil {
			return nil, err
		}
	}
	var lines []string
	res, err := s.runPrivilegeProbe(s.e.ctx(), sshScript, func(l string) { lines = append(lines, l) })
	if err != nil {
		return nil, err
	}
	if res.Code != 0 && len(lines) == 0 {
		return nil, fmt.Errorf("查看 SSH 连接失败：%s", strings.TrimSpace(res.Stderr))
	}
	rep, descToTop, perr := parseSSHInternal(lines, s.t.User, keepPorts(s)[0])
	if perr != nil {
		return nil, perr
	}
	enrichSSHReport(s, rep, descToTop)
	return rep, nil
}

// enrichSSHReport 在客户端侧补全每条 SSH 连接的归属信息：
// 1) 当前 Ferry 连接；
// 2) 同进程连接池中的其他 Ferry 连接（含独立连接、其他项目、并发溢出扩展连接；优先按常驻 agent 远端 PID 匹配，跨 NAT 也准确）；
// 3) 同 IP 且无 NAT 端口转换时，反查本机 TCP 表定位到本机具体进程名（如 Xshell、GoLand、ssh、另一 Ferry MCP 实例），或确认是「同 IP 其他设备」。
func enrichSSHReport(s *session, rep *SSHReport, descToTop map[int]int) {
	if s == nil || rep == nil {
		return
	}
	remoteIP, remotePort := "", 0
	selfLocalPort := 0
	hostFP := ""
	var selfExtraPorts []int
	if s.c != nil {
		remoteIP, remotePort = s.c.RemoteAddr()
		selfLocalPort = s.c.LocalPort()
		hostFP = s.c.HostFingerprint
		selfExtraPorts = s.c.ExtraLocalPorts()
	}
	var poolSnaps []poolConnSnap
	if s.e != nil && s.e.Pool != nil {
		poolSnaps = s.e.Pool.snapshotForServer(hostFP, remoteIP, remotePort)
	}
	enrichSSHReportWith(enrichInput{
		CurrentConn:    s.conn,
		CurrentRef:     currentSessionRefLabel(s),
		SelfLocalPort:  selfLocalPort,
		SelfExtraPorts: selfExtraPorts,
		RemoteIP:       remoteIP,
		RemotePort:     remotePort,
		PoolSnaps:      poolSnaps,
		DescToTop:      descToTop,
		LocalPID:       os.Getpid(),
		LocalProcName:  selfProcessName(),
		LookupTCP:      localTCPLookup,
	}, rep)
}

type enrichInput struct {
	CurrentConn    *conn
	CurrentRef     string
	SelfLocalPort  int
	SelfExtraPorts []int
	RemoteIP       string
	RemotePort     int
	PoolSnaps      []poolConnSnap
	DescToTop      map[int]int
	LocalPID       int
	LocalProcName  string
	LookupTCP      func(remoteIP string, remotePort int) (map[int]localTCPProc, bool)
}

func enrichSSHReportWith(in enrichInput, rep *SSHReport) {
	if rep == nil {
		return
	}
	var selfSess *SSHSession
	for i := range rep.Sessions {
		if rep.Sessions[i].Self {
			selfSess = &rep.Sessions[i]
			break
		}
	}
	if selfSess != nil && selfSess.From != "" {
		rep.SelfFrom = selfSess.From
	}
	natTranslated := in.SelfLocalPort > 0 && selfSess != nil && selfSess.FromPort > 0 && in.SelfLocalPort != selfSess.FromPort
	rep.NATPortTranslated = natTranslated

	// 1. 先标记当前连接与同进程 Ferry 连接池中的所有连接
	for i := range rep.Sessions {
		ss := &rep.Sessions[i]
		if ss.Self {
			ss.Origin = "self"
			ss.OriginLabel = "当前连接"
			ref := in.CurrentRef
			if ref != "" {
				ss.OriginDetail = fmt.Sprintf("本机 Ferry 当前页面正在使用的连接（%s，本机 PID %d）", ref, in.LocalPID)
			} else {
				ss.OriginDetail = fmt.Sprintf("本机 Ferry 当前页面正在使用的连接（本机 PID %d）", in.LocalPID)
			}
			ss.LocalPID = in.LocalPID
			ss.LocalProcess = in.LocalProcName
			continue
		}

		sameIP := rep.SelfFrom != "" && ss.From != "" && ss.From == rep.SelfFrom

		// 当前连接的并发溢出扩展连接（c.extra）
		if !natTranslated && sameIP && ss.FromPort > 0 && slices.Contains(in.SelfExtraPorts, ss.FromPort) {
			ss.Origin = "ferry-pool"
			ss.OriginLabel = "本机 Ferry（并发扩展）"
			if in.CurrentRef != "" {
				ss.OriginDetail = fmt.Sprintf("当前连接（%s）因会话数达到服务器上限自动扩展的并发连接（本机 PID %d）", in.CurrentRef, in.LocalPID)
			} else {
				ss.OriginDetail = fmt.Sprintf("本机 Ferry 因会话数达到上限自动扩展的并发连接（本机 PID %d）", in.LocalPID)
			}
			ss.LocalPID = in.LocalPID
			ss.LocalProcess = in.LocalProcName
			continue
		}

		// 连接池里的其他条目（如左侧独立连接、其他项目目标）
		if snap, extra, matched := matchPoolSnap(ss, in.CurrentConn, in.PoolSnaps, in.DescToTop, sameIP, natTranslated); matched {
			ss.Origin = "ferry-pool"
			refShort, refFull := formatPoolSnapRefs(snap)
			if extra {
				if refShort != "" {
					ss.OriginLabel = fmt.Sprintf("本机 Ferry（%s · 扩展）", refShort)
				} else {
					ss.OriginLabel = "本机 Ferry（并发扩展）"
				}
			} else if refShort != "" {
				ss.OriginLabel = fmt.Sprintf("本机 Ferry（%s）", refShort)
			} else {
				ss.OriginLabel = "本机 Ferry（其他连接）"
			}
			detail := fmt.Sprintf("同一个 Ferry 进程维持的另一条 SSH 连接（%s，本机 PID %d）", refFull, in.LocalPID)
			if extra {
				detail += " · 并发溢出扩展连接"
			}
			ss.OriginDetail = detail
			ss.LocalPID = in.LocalPID
			ss.LocalProcess = in.LocalProcName
		}
	}

	// 2. 对剩余「与当前连接同 IP」的会话，若未经过端口转换 NAT，则查本机 TCP 进程表
	needLocalLookup := false
	if !natTranslated && rep.SelfFrom != "" && in.RemoteIP != "" && in.RemotePort > 0 && in.LookupTCP != nil {
		for i := range rep.Sessions {
			ss := &rep.Sessions[i]
			if ss.Origin == "" && ss.From == rep.SelfFrom && ss.FromPort > 0 {
				needLocalLookup = true
				break
			}
		}
	}
	var localMap map[int]localTCPProc
	localLookupOK := false
	if needLocalLookup {
		localMap, localLookupOK = in.LookupTCP(in.RemoteIP, in.RemotePort)
	}

	// 3. 给所有剩余会话分配最终归属
	for i := range rep.Sessions {
		ss := &rep.Sessions[i]
		if ss.Origin != "" || ss.From == "" {
			continue
		}
		if rep.SelfFrom != "" && ss.From == rep.SelfFrom {
			if !natTranslated && localLookupOK && ss.FromPort > 0 {
				if proc, ok := localMap[ss.FromPort]; ok {
					ss.Origin = "local-proc"
					ss.LocalPID = proc.PID
					ss.LocalProcess = proc.Name
					ss.OriginLabel, ss.OriginDetail = formatLocalProcOrigin(proc, ss.FromPort)
					continue
				}
				ss.Origin = "same-ip"
				ss.OriginLabel = "同 IP 其他设备"
				ss.OriginDetail = fmt.Sprintf("来源 IP 与本机相同（%s），但本机未占用源端口 %d，通常来自同一局域网 NAT、VPN 或出口网关下的其他设备", ss.From, ss.FromPort)
				continue
			}
			ss.Origin = "nat-same-ip"
			ss.OriginLabel = "同出口 IP"
			if natTranslated {
				ss.OriginDetail = fmt.Sprintf("与当前连接来源 IP 相同（%s），但链路经过了 NAT / 代理端口转换，无法仅凭源端口确认是否为本机其他软件，可结合认证密钥或终端内容判断", ss.From)
			} else {
				ss.OriginDetail = fmt.Sprintf("与当前连接来源 IP 相同（%s），本机进程表暂不可用，无法进一步区分是本机其他软件还是同网关其他设备", ss.From)
			}
			continue
		}
		ss.Origin = "remote"
		ss.OriginLabel = "其他 IP"
		if rep.SelfFrom != "" {
			ss.OriginDetail = fmt.Sprintf("来自外部地址 %s（与本机来源 %s 不同）", ss.From, rep.SelfFrom)
		} else {
			ss.OriginDetail = fmt.Sprintf("来自地址 %s", ss.From)
		}
	}
}

func matchPoolSnap(ss *SSHSession, cur *conn, snaps []poolConnSnap, descToTop map[int]int, sameIP, natTranslated bool) (poolConnSnap, bool, bool) {
	for _, snap := range snaps {
		if snap.Conn != nil && snap.Conn == cur {
			continue
		}
		// 优先用常驻 agent 在服务器上的 PID 匹配：即使经过 NAT/PAT 端口改写也能 100% 精准命中
		if snap.RemoteAgentPID > 0 && descToTop[snap.RemoteAgentPID] == ss.PID {
			return snap, false, true
		}
	}
	if natTranslated || !sameIP || ss.FromPort <= 0 {
		return poolConnSnap{}, false, false
	}
	for _, snap := range snaps {
		if snap.Conn != nil && snap.Conn == cur {
			continue
		}
		if snap.PrimaryPort > 0 && snap.PrimaryPort == ss.FromPort {
			return snap, false, true
		}
		if slices.Contains(snap.ExtraPorts, ss.FromPort) {
			return snap, true, true
		}
	}
	return poolConnSnap{}, false, false
}

func currentSessionRefLabel(s *session) string {
	if s == nil || s.t == nil {
		return ""
	}
	if s.e != nil && s.e.File != nil {
		if s.e.File.Kind == config.KindConnections {
			name := s.t.Title
			if name == "" {
				name = s.t.Name
			}
			return "独立连接：" + name
		}
		if s.e.File.Project != "" {
			return s.e.File.Project + " / " + s.t.Name
		}
	}
	return s.t.Name
}

func formatPoolSnapRefs(snap poolConnSnap) (short, full string) {
	if len(snap.Refs) == 0 {
		if snap.Label != "" {
			return snap.Label, snap.Label
		}
		return "", "连接池连接"
	}
	var shorts []string
	var fulls []string
	for _, r := range snap.Refs {
		if r.Kind == config.KindConnections {
			name := r.Title
			if name == "" {
				name = r.Target
			}
			shorts = append(shorts, "独立连接："+name)
			fulls = append(fulls, "独立连接 "+name)
		} else if r.Project != "" {
			shorts = append(shorts, "项目："+r.Project)
			fulls = append(fulls, fmt.Sprintf("项目 %s / %s", r.Project, r.Target))
		} else if r.Target != "" {
			shorts = append(shorts, r.Target)
			fulls = append(fulls, r.Target)
		}
	}
	if len(shorts) == 0 {
		return snap.Label, snap.Label
	}
	short = shorts[0]
	if len(shorts) > 1 {
		short += fmt.Sprintf(" 等 %d 处", len(shorts))
	}
	return short, strings.Join(fulls, "、")
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
		if _, err := s.runPrivilegeProbe(e.ctx(), kickScript(kill), nil); err != nil {
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

type sshAuthInfo struct {
	user     string
	from     string
	fromPort int
	method   string
	key      string
}

type sshAuthTuple struct {
	user     string
	from     string
	fromPort int
}

type sshUserKey struct {
	user string
	fp   string
}

var (
	// sshd: alice@pts/0,pts/1、sshd-session: alice@notty、sshd: alice@internal-sftp，以及 ssh -N 纯端口转发时的 sshd-session: alice
	reSSHUser = regexp.MustCompile(`^sshd(?:-session)?: ([^\s/\[\]@]+)(?:@(\S+))?$`)
	reSSHPriv = regexp.MustCompile(`^sshd(?:-session)?: (\S+) \[priv\]$`)
	reSSHPre  = regexp.MustCompile(`^sshd(?:-session|-auth)?: .*\[(?:net|preauth)\]$`)
	reSSHPeer = regexp.MustCompile(`\s\S+:(\d+)\s+(\S+):(\d+|\*)\s+users:\(\((.*)\)\)`)
	reSSHPid  = regexp.MustCompile(`pid=(\d+)`)
	reWhoHost = regexp.MustCompile(`\(([^)]*)\)\s*$`)
	reSSHAuth = regexp.MustCompile(`sshd(?:-session|-auth)?\[(\d+)\]:\s+Accepted\s+(\S+)\s+for\s+(\S+)\s+from\s+(\S+)\s+port\s+(\d+)\s+ssh2(?::\s+(.*))?$`)
)

type ssSocket struct {
	state    string // "LISTEN" | "ESTAB"
	local    int
	peerIP   string
	peerPort string
	pids     []int
}

// sshPort Ferry 连这台服务器用的端口（ss 里认哪个是客户端连进来的）。
func parseSSH(lines []string, me string, sshPort int) (*SSHReport, error) {
	rep, _, err := parseSSHInternal(lines, me, sshPort)
	return rep, err
}

func parseSSHInternal(lines []string, me string, sshPort int) (*SSHReport, map[int]int, error) {
	rep := &SSHReport{Priv: "none", Me: me, Sessions: []SSHSession{}}
	var self int
	var now int64
	sec := ""
	procs := map[int]*psProc{}
	kids := map[int][]int{}
	env := map[int][2]string{} // pid → 对方 IP、端口
	envServerPorts := map[int]bool{}
	var ssSockets []ssSocket
	whoHost := map[string]string{}
	ttyAt := map[string]int64{}
	authByPID := map[int]sshAuthInfo{}
	authByTuple := map[sshAuthTuple]sshAuthInfo{}
	keyComments := map[sshUserKey]string{}

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
				if len(f) >= 4 {
					if sp := atoi(f[3]); sp > 0 {
						envServerPorts[sp] = true
					}
				}
			}
		case "ss":
			trimmed := strings.TrimSpace(l)
			state := ""
			switch {
			case strings.HasPrefix(trimmed, "ESTAB"):
				state = "ESTAB"
			case strings.HasPrefix(trimmed, "LISTEN"):
				state = "LISTEN"
			default:
				continue
			}
			m := reSSHPeer.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			var spids []int
			for _, pm := range reSSHPid.FindAllStringSubmatch(m[4], -1) {
				if pid, err := strconv.Atoi(pm[1]); err == nil {
					spids = append(spids, pid)
				}
			}
			if len(spids) == 0 {
				continue
			}
			ssSockets = append(ssSockets, ssSocket{
				state:    state,
				local:    atoi(m[1]),
				peerIP:   cleanIP(m[2]),
				peerPort: m[3],
				pids:     spids,
			})
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
		case "auth":
			m := reSSHAuth.FindStringSubmatch(strings.TrimSpace(l))
			if m == nil {
				continue
			}
			pid, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			method := normalizeAuthMethod(m[2])
			user := m[3]
			from := cleanIP(m[4])
			fromPort := atoi(m[5])
			key := extractAuthFingerprint(m[6])
			info := sshAuthInfo{user: user, from: from, fromPort: fromPort, method: method, key: key}
			authByPID[pid] = info
			if from != "" && fromPort > 0 {
				authByTuple[sshAuthTuple{user: user, from: from, fromPort: fromPort}] = info
			}
		case "keys":
			parts := strings.SplitN(l, "\t", 3)
			if len(parts) == 3 && parts[0] != "" && parts[1] != "" && strings.TrimSpace(parts[2]) != "" {
				keyComments[sshUserKey{user: parts[0], fp: parts[1]}] = strings.TrimSpace(parts[2])
			}
		case "keylines":
			user, rawLine, ok := strings.Cut(l, "\t")
			if !ok || user == "" {
				continue
			}
			if fp, cmt, ok := parseAuthorizedKeyLine(rawLine); ok && cmt != "" {
				keyComments[sshUserKey{user: user, fp: fp}] = cmt
			}
		}
	}
	if len(procs) == 0 {
		return nil, nil, errors.New("查看 SSH 连接失败：服务器上的 ps 不支持（需要 procps）")
	}
	if now == 0 {
		now = time.Now().Unix()
	}
	rep.Now = now

	// 解析服务器真实的 sshd 监听端口（经 Docker/NAT/路由器端口映射时，服务器内部端口可能与客户端配置的 sshPort 不同）
	sessionPIDs := map[int]bool{}
	for _, p := range procs {
		if reSSHUser.MatchString(p.args) || reSSHPriv.MatchString(p.args) || reSSHPre.MatchString(p.args) {
			sessionPIDs[p.pid] = true
		}
	}
	realSSHPorts := map[int]bool{}
	for sp := range envServerPorts {
		realSSHPorts[sp] = true
	}
	for _, sk := range ssSockets {
		if sk.state != "LISTEN" || sk.local <= 0 {
			continue
		}
		// 只采纳主 sshd 监听进程（排除会话进程通过 -R 反向转发打开的监听端口）
		isMaster := false
		for _, pid := range sk.pids {
			if !sessionPIDs[pid] {
				isMaster = true
				break
			}
		}
		if isMaster {
			realSSHPorts[sk.local] = true
		}
	}
	if len(realSSHPorts) == 0 && sshPort > 0 {
		realSSHPorts[sshPort] = true
	}

	peer := map[int][2]string{}
	peerOnSSH := map[int]bool{}
	forwards := map[int][]string{}
	for _, sk := range ssSockets {
		if sk.state != "ESTAB" {
			continue
		}
		onSSH := realSSHPorts[sk.local]
		for _, pid := range sk.pids {
			if _, ok := peer[pid]; !ok || (onSSH && !peerOnSSH[pid]) {
				peer[pid] = [2]string{sk.peerIP, sk.peerPort}
				peerOnSSH[pid] = onSSH
			}
			if !onSSH && sk.peerIP != "" && sk.peerPort != "" && sk.peerPort != "*" {
				dst := sk.peerIP + ":" + sk.peerPort
				if !slices.Contains(forwards[pid], dst) {
					forwards[pid] = append(forwards[pid], dst)
				}
			}
		}
	}

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
	descToTop := map[int]int{}
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
		walk(top.pid, procs, kids, func(d *psProc) {
			descToTop[d.pid] = top.pid
		})

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
		// 端口转发目标（ss 中非 SSH 监听端口的 ESTAB 连接，且排除会话自身客户端地址）
		selfPeer := ""
		if ss.From != "" && ss.FromPort > 0 {
			selfPeer = fmt.Sprintf("%s:%d", ss.From, ss.FromPort)
		}
		fwSet := map[string]bool{}
		for _, pid := range ss.pids {
			for _, dst := range forwards[pid] {
				if dst != selfPeer {
					fwSet[dst] = true
				}
			}
		}
		if len(fwSet) > 0 {
			for dst := range fwSet {
				ss.Forwards = append(ss.Forwards, dst)
			}
			sort.Strings(ss.Forwards)
			if len(ss.Forwards) > 4 {
				ss.Forwards = ss.Forwards[:4]
			}
		}

		ferry, exec := false, false
		for _, c := range kids[p.pid] {
			cp := procs[c]
			if a, ok := env[c]; ok && ss.From == "" {
				ss.From, ss.FromPort = cleanIP(a[0]), atoi(a[1])
			}
			if strings.HasPrefix(cp.args, "sshd") || ancestors[c] {
				continue // 查看这个列表的脚本本身不算
			}
			exec = true
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

		// 匹配认证日志与公钥备注：优先按 sshd PID 匹配（校验 user/from 防 PID 复用误配），再按 (user, from, fromPort) 兜底
		if ai, ok := matchAuthInfo(&ss, authByPID, authByTuple); ok {
			ss.AuthMethod = ai.method
			ss.AuthKey = ai.key
			if ai.key != "" {
				ss.AuthKeyComment = keyComments[sshUserKey{user: ss.User, fp: ai.key}]
			}
		}

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
	return rep, descToTop, nil
}

func matchAuthInfo(ss *SSHSession, byPID map[int]sshAuthInfo, byTuple map[sshAuthTuple]sshAuthInfo) (sshAuthInfo, bool) {
	for _, pid := range ss.pids {
		if ai, ok := byPID[pid]; ok && ai.user == ss.User && (ss.From == "" || ai.from == ss.From) && (ss.FromPort == 0 || ai.fromPort == 0 || ai.fromPort == ss.FromPort) {
			return ai, true
		}
	}
	if ss.From != "" && ss.FromPort > 0 {
		if ai, ok := byTuple[sshAuthTuple{user: ss.User, from: ss.From, fromPort: ss.FromPort}]; ok {
			return ai, true
		}
	}
	return sshAuthInfo{}, false
}

func normalizeAuthMethod(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if strings.HasPrefix(raw, "keyboard-interactive") {
		return "keyboard-interactive"
	}
	return raw
}

func extractAuthFingerprint(rest string) string {
	for _, tok := range strings.Fields(rest) {
		if strings.HasPrefix(tok, "SHA256:") || strings.HasPrefix(tok, "MD5:") {
			return tok
		}
	}
	return ""
}

// parseAuthorizedKeyLine 解析 authorized_keys 单行，返回 OpenSSH SHA256 指纹与末尾备注。
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
		if !strings.HasPrefix(tok, "ssh-") && !strings.HasPrefix(tok, "ecdsa-sha2-") &&
			!strings.HasPrefix(tok, "sk-ssh-") && !strings.HasPrefix(tok, "sk-ecdsa-") {
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
		return fp, strings.TrimSpace(strings.ReplaceAll(cmt, "\t", " ")), true
	}
	return "", "", false
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
