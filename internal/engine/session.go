package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/captain/ferry/internal/agentbin"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
)

// AgentDir 服务器上存放 agent 的默认目录（root 所有的系统目录，避免被普通用户替换后以 root 执行）；目标可以用 ferryDir 改。
const AgentDir = config.DefaultFerryDir

// conn 到一台服务器的连接及其上已确认的 agent。ferry serve 里会被多个请求复用。
type conn struct {
	c     *remote.Client
	sudo  string
	agent string
	arch  string
	opts  remote.Options // 多路上传时用同样的参数（含已取得的密码）再建连接
	label string         // user@host:port
	// ferryDir 服务器上 ferry 组件的目录（agent、运行环境）
	ferryDir string
	// broken 上一次在它上面的操作因连接问题失败：下次复用前先确认还能用
	broken atomic.Bool
}

// session 一次操作对一个目标的使用：连接（可能是复用的）+ 本次请求的引擎（输出、配置）。
type session struct {
	*conn
	e       *Engine
	t       *config.Target
	release func()
}

// connect 取得到目标的连接：有连接池时复用，否则新建（用完关闭）。
func (e *Engine) connect(t *config.Target) (*session, error) {
	if !t.Plain && t.User != "root" && !t.Sudo {
		return nil, fmt.Errorf("目标 %s 用 %s 登录：agent 需要 root，请设置 sudo: true（并允许免密 sudo）", t.Name, t.User)
	}
	var cn *conn
	var release func()
	var err error
	if e.Pool != nil {
		cn, release, err = e.Pool.Acquire(e, t)
	} else {
		e.Rep.Step("连接 %s（%s@%s:%d）", t.Name, t.User, t.Host, t.Port)
		cn, err = e.dial(t)
		if err == nil {
			release = func() { cn.c.Close() }
		}
	}
	if err != nil {
		return nil, err
	}
	if t.Arch != "" && t.Arch != cn.arch {
		release()
		return nil, fmt.Errorf("目标 %s 配置的 arch 是 %s，服务器实际是 %s", t.Name, t.Arch, cn.arch)
	}
	return &session{conn: cn, e: e, t: t, release: release}, nil
}

// dial 新建连接并确保服务器上有匹配版本的 agent。
func (e *Engine) dial(t *config.Target) (*conn, error) {
	sudo := ""
	if t.User != "root" {
		sudo = "sudo -n "
	}
	cred := e.credentials(t)
	opts := e.dialOptions(t, cred)
	c, err := remote.Dial(opts)
	if err != nil {
		return nil, cred.translate(err)
	}
	cred.offerRemember()
	if cred.missingKey != "" {
		e.Rep.Warn("这台电脑上没有私钥 %s，用默认私钥或 ssh-agent 登录了；可以在本机设置里改成这台电脑上的私钥", cred.missingKey)
	}
	cn := &conn{c: c, sudo: sudo, opts: opts, label: fmt.Sprintf("%s@%s:%d", t.User, t.Host, t.Port), ferryDir: t.FerryDir}
	if cn.ferryDir == "" {
		cn.ferryDir = AgentDir
	}
	if t.Plain {
		return cn, nil // 只登录（终端、文件），不用 agent
	}
	if err := e.ensureAgent(cn); err != nil {
		c.Close()
		return nil, err
	}
	return cn, nil
}

func identity(f *config.File, t *config.Target) string {
	if t.IdentityFile == "" {
		return ""
	}
	return f.LocalPath(t.IdentityFile)
}

func (s *session) close() { s.release() }

// reconnect 连接断了：换一条新连接（连接池里的旧连接作废），本次操作接着用 s。
func (s *session) reconnect() error {
	s.broken.Store(true)
	s.c.Close()
	s.release()
	s.release = func() {}
	ns, err := s.e.connect(s.t)
	if err != nil {
		return err
	}
	s.conn, s.release = ns.conn, ns.release
	return nil
}

// gzipped 各架构 agent 压缩后的内容（同一个进程里只压缩一次）。
var gzipped sync.Map

func gzipAgent(arch string, data []byte) []byte {
	if v, ok := gzipped.Load(arch); ok {
		return v.([]byte)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(data)
	zw.Close()
	gzipped.Store(arch, buf.Bytes())
	return buf.Bytes()
}

// ensureAgent 确认服务器上有这个版本的 agent，没有就上传。agent 内容不随应用版本变化（只在 agent 代码改了时才重新上传），
// 上传压缩后的文件（约为原来的 40%，经 VPN 等慢速网络时差别明显），在服务器上解压。
func (e *Engine) ensureAgent(s *conn) error {
	// 一次往返：架构、有没有 gzip、ferry 组件目录及其上级是否归 root 且他人不可写（agent 以 root 运行，不能放在别人能替换的地方）
	check := fmt.Sprintf(`uname -m; command -v gzip >/dev/null 2>&1 && echo has-gzip || echo no-gzip; `+
		`d=%s; while [ -n "$d" ] && [ "$d" != / ]; do if [ -e "$d" ]; then u=$(stat -c %%u "$d"); m=$(stat -c %%a "$d"); `+
		`if [ "$u" != 0 ] || [ $((0$m & 022)) -ne 0 ]; then echo "unsafe $d $u $m"; fi; fi; d=$(dirname "$d"); done`, shQuote(s.ferryDir))
	out, res, err := s.c.Output(check)
	if err != nil || res.Code != 0 {
		return fmt.Errorf("读取服务器架构失败：%v %s", err, res.Stderr)
	}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 4 && f[0] == "unsafe" {
			return fmt.Errorf("服务器上存放 ferry 组件的目录不安全：%s 的属主 uid 是 %s、权限 %s。agent 以 root 运行，ferryDir（%s）及其上级必须归 root 且其他用户不可写\n"+
				"修复：在服务器上执行 chown root:root %s && chmod go-w %s，或在配置里给这台服务器换一个 ferryDir", f[1], f[2], f[3], s.ferryDir, f[1], f[1])
		}
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return errors.New("读取服务器架构失败：没有输出")
	}
	switch m := lines[0]; m {
	case "x86_64", "amd64":
		s.arch = "amd64"
	case "aarch64", "arm64":
		s.arch = "arm64"
	default:
		return fmt.Errorf("不支持的服务器架构 %s", m)
	}
	hasGzip := len(lines) > 1 && lines[1] == "has-gzip"
	bin, err := agentbin.Get(s.arch)
	if err != nil {
		return err
	}
	s.agent = s.ferryDir + "/ferry-agent-" + bin.SHA256[:12]
	if s.agentOK(bin.SHA256) {
		return nil
	}
	data, ext := bin.Data, ""
	if hasGzip {
		data, ext = gzipAgent(s.arch, bin.Data), ".gz"
	}
	e.Rep.Info("首次连接这个版本：上传 ferry 的服务器端组件 agent（linux/%s，%s%s），之后不再重复上传", s.arch, humanBytes(int64(len(data))),
		map[bool]string{true: "，已压缩", false: ""}[hasGzip])
	tmpLocal, err := os.CreateTemp(appdir.TempDir(), "agent-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmpLocal.Name())
	if _, err := tmpLocal.Write(data); err != nil {
		tmpLocal.Close()
		return err
	}
	tmpLocal.Close()
	tmpRemote := fmt.Sprintf("/tmp/ferry-agent-%s-%d", bin.SHA256[:12], os.Getpid())
	total := int64(len(data))
	err = s.c.UploadContext(e.ctx(), tmpLocal.Name(), tmpRemote+ext, 0o700, func(done, _ int64) {
		e.Rep.Progress("agent", done, total)
	})
	if err != nil {
		s.c.Output("rm -f " + tmpRemote + ext)
		return err
	}
	unpack := ""
	if hasGzip {
		unpack = fmt.Sprintf("gzip -dc %[1]s.gz > %[1]s; rm -f %[1]s.gz; ", tmpRemote)
	}
	script := fmt.Sprintf("set -e; %[4]sinstall -d -m 755 -o root -g root %[1]s; install -m 755 -o root -g root %[2]s %[3]s; rm -f %[2]s; "+
		"cd %[1]s && ls -t ferry-agent-* 2>/dev/null | tail -n +4 | xargs -r rm -f",
		shQuote(s.ferryDir), tmpRemote, s.agent, unpack)
	if _, res, err := s.c.Output(s.sudo + "sh -c " + shQuote(script)); err != nil || res.Code != 0 {
		s.c.Output("rm -f " + tmpRemote + " " + tmpRemote + ".gz")
		return fmt.Errorf("安装 agent 失败：%v %s", err, strings.TrimSpace(res.Stderr))
	}
	if !s.agentOK(bin.SHA256) {
		return errors.New("agent 安装后校验失败")
	}
	return nil
}

func (s *conn) agentOK(sha string) bool {
	out, res, err := s.c.Output(s.sudo + s.agent + " version")
	if err != nil || res.Code != 0 {
		return false
	}
	var v struct {
		SHA256 string `json:"sha256"`
		Proto  int    `json:"proto"`
	}
	return json.Unmarshal([]byte(out), &v) == nil && v.SHA256 == sha && v.Proto == proto.Version
}

// call 调用 agent 命令：事件实时交给 Reporter，返回最后的结果。
func (s *session) call(cmd string, req *proto.Request) (*proto.Result, error) {
	req.Proto = proto.Version
	req.Root = s.t.Root
	req.FerryDir = s.ferryDir
	req.Project = s.e.File.Project
	req.Target = s.t.Name
	req.Actor = s.e.actor()
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var result *proto.Result
	res, err := s.c.RunContext(s.e.ctx(), s.sudo+s.agent+" "+cmd, body, func(line string) {
		var head struct {
			T string `json:"t"`
		}
		if json.Unmarshal([]byte(line), &head) != nil {
			s.e.Rep.Warn("agent：%s", line)
			return
		}
		switch head.T {
		case "ev":
			var ev proto.Event
			if json.Unmarshal([]byte(line), &ev) == nil {
				s.e.Rep.Event(s.t.Name, ev)
			}
		case "result":
			var r proto.Result
			if json.Unmarshal([]byte(line), &r) == nil {
				result = &r
			}
		}
	})
	if err != nil {
		if !errors.Is(err, remote.ErrCanceled) {
			s.broken.Store(true)
		}
		return nil, continuesNote(cmd, err)
	}
	if result == nil {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("退出码 %d", res.Code)
		}
		return nil, continuesNote(cmd, fmt.Errorf("agent %s 没有返回结果：%s", cmd, msg))
	}
	return result, nil
}

// changingCmds 改动服务器的 agent 命令：本机取消或连接断开后，agent 在服务器上照常做完（不会停在半路）。
var changingCmds = map[string]string{
	"deploy": "（失败时自动回滚）", "rollback": "（失败时切回原版本）", "control": "", "runtime": "",
	"swinstall": "", "swcontrol": "", "swremove": "", "portset": "", "fwenable": "（3 分钟内没确认会自动关掉防火墙）", "fwdisable": "",
}

// continuesNote 改动服务器的操作没拿到结果时说明：服务器上会继续做完，到哪里看结果。
func continuesNote(cmd string, err error) error {
	note, ok := changingCmds[cmd]
	if !ok {
		return err
	}
	return fmt.Errorf("%w。服务器上这次操作会继续做完%s，稍后看状态和部署历史确认结果", err, note)
}

// callData 调用并把结果数据解到 out；结果码非 0 视为错误。
func (s *session) callData(cmd string, req *proto.Request, out any) error {
	r, err := s.call(cmd, req)
	if err != nil {
		return err
	}
	if r.RC != proto.CodeOK {
		return &AgentError{Cmd: cmd, RC: r.RC, Msg: r.Msg}
	}
	if out != nil && len(r.Data) > 0 {
		return json.Unmarshal(r.Data, out)
	}
	return nil
}

// AgentError agent 返回了非 0 结果码。
type AgentError struct {
	Cmd string
	RC  int
	Msg string
}

func (e *AgentError) Error() string {
	if e.RC == proto.CodeLocked {
		return "另一个部署/回滚/启停正在进行，稍后再试"
	}
	return fmt.Sprintf("%s 失败：%s", e.Cmd, e.Msg)
}

// actor 记在部署记录里的操作人。环境变量 FERRY_VIA 是经谁发起的（编程软件经 MCP 请求、桌面版代为执行时），附在后面。
func (e *Engine) actor() string {
	a := actor()
	if n := e.personName(); n != "" {
		a = n + "（" + a + "）"
	}
	via := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(e.getenv("FERRY_VIA")))
	if via == "" {
		return a
	}
	if r := []rune(via); len(r) > 60 {
		via = string(r[:60])
	}
	return a + "（经 " + via + "）"
}

// personName 操作人的名字：环境变量 FERRY_ACTOR、设置里填的名字，CI 里是 GitHub / GitLab 触发的人。
func (e *Engine) personName() string {
	clean := func(s string) string {
		s = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, strings.TrimSpace(s))
		if r := []rune(s); len(r) > 40 {
			s = string(r[:40])
		}
		return s
	}
	if v := clean(e.getenv("FERRY_ACTOR")); v != "" {
		return v
	}
	if v := clean(e.Settings.Team.Name); v != "" {
		return v
	}
	if v := clean(e.getenv("GITHUB_ACTOR")); v != "" {
		return "github:" + v
	}
	if v := clean(e.getenv("GITLAB_USER_LOGIN")); v != "" {
		return "gitlab:" + v
	}
	return ""
}

// checkDeployer 目标写了 deployers 时：只有名单里的人能部署、回滚、启停。
func (e *Engine) checkDeployer(t *config.Target, what string) error {
	if len(t.Deployers) == 0 {
		return nil
	}
	me := actor()
	user, _, _ := strings.Cut(me, "@")
	name := e.personName()
	for _, d := range t.Deployers {
		d = strings.TrimSpace(d)
		if d != "" && (strings.EqualFold(d, name) || strings.EqualFold(d, me) || strings.EqualFold(d, user)) {
			return nil
		}
	}
	who := me
	if name != "" {
		who = name + "（" + me + "）"
	} else {
		who += "，设置 → 部署里还没填你的名字"
	}
	return fmt.Errorf("%s 只允许 %s %s（ferry.yaml 的 deployers）；你是 %s", t.Name, strings.Join(t.Deployers, "、"), what, who)
}

func actor() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
		if i := strings.LastIndexAny(name, `\/`); i >= 0 {
			name = name[i+1:]
		}
	}
	host, _ := os.Hostname()
	return name + "@" + host
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
