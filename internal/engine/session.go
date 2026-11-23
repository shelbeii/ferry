package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/captain/ferry/internal/agentbin"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/secret"
)

// AgentDir 服务器上存放 agent 的默认目录（root 所有的系统目录，避免被普通用户替换后以 root 执行）；目标可以用 ferryDir 改。
const AgentDir = config.DefaultFerryDir

// conn 到一台服务器的连接及其上已确认的 agent。ferry serve 里会被多个请求复用。
type conn struct {
	c            *remote.Client
	sudo         string
	agent        string
	arch         string
	agentMu      sync.Mutex // 保护已经校验过的 agent 路径和架构
	agentGate    chan struct{}
	opts         remote.Options // 多路上传时用同样的参数（含已取得的密码）再建连接
	label        string         // user@host:port
	sudoMu       sync.Mutex
	sudoPrompted string // 仅单次 CLI 运行的交互式输入
	// ferryDir 服务器上 ferry 组件的目录（agent、运行环境）
	ferryDir string
	// broken 上一次在它上面的操作因连接问题失败：下次复用前先确认还能用
	broken     atomic.Bool
	sftpServer atomic.Pointer[string] // OpenSSH helper discovered on this connection

	// 常驻的 agent（见 agentserve.go）
	srvMu       sync.Mutex
	srv         *agentServer
	srvFailedAt time.Time

	// 「服务器信息」上次读到的 CPU 计数：下次和它比，不用在服务器上等一秒再读
	cpuMu   sync.Mutex
	cpuPrev []string
	cpuAt   time.Time
}

func (c *conn) cpuSample() ([]string, time.Time) {
	c.cpuMu.Lock()
	defer c.cpuMu.Unlock()
	return c.cpuPrev, c.cpuAt
}

func (c *conn) setCPUSample(lines []string) {
	c.cpuMu.Lock()
	defer c.cpuMu.Unlock()
	if len(lines) > 0 {
		c.cpuPrev, c.cpuAt = lines, time.Now()
	}
}

// session 一次操作对一个目标的使用：连接（可能是复用的）+ 本次请求的引擎（输出、配置）。
type session struct {
	*conn
	e            *Engine
	t            *config.Target
	release      func()
	agent        string
	arch         string
	fileIdentity string
}

// connect 取得到目标的连接：有连接池时复用，否则新建（用完关闭）。
func (e *Engine) connect(t *config.Target) (*session, error) {
	if !t.Plain && t.User != "root" && (!t.Sudo || t.SudoAuth == "disabled") {
		return nil, fmt.Errorf("目标 %s 用 %s 登录：agent 需要 root，请设置 sudo: true，并选择服务器支持的 sudo 认证方式", t.Name, t.User)
	}
	s, err := e.connectSSH(t)
	if err != nil || t.Plain {
		return s, err
	}
	if err := s.prepareAgent(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// connectSSH only authenticates the transport. It also reuses connections that
// already have a verified agent, without requiring the agent to open SSH.
func (e *Engine) connectSSH(t *config.Target) (*session, error) {
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
	s := &session{conn: cn, e: e, t: t, release: release}
	if !t.Plain {
		s.readAgent()
	}
	return s, nil
}

func (s *session) readAgent() {
	s.agentMu.Lock()
	s.agent, s.arch = s.conn.agent, s.conn.arch
	s.agentMu.Unlock()
}

func (s *session) prepareAgent() error {
	if s.agent == "" {
		if s.e.Background {
			return ErrNotConnected
		}
		select {
		case s.agentGate <- struct{}{}:
			defer func() { <-s.agentGate }()
		case <-s.e.ctx().Done():
			return ErrCanceled
		}
		s.readAgent()
		if s.agent == "" {
			if err := s.e.ensureAgent(s); err != nil {
				return err
			}
			s.agentMu.Lock()
			s.conn.agent, s.conn.arch = s.agent, s.arch
			s.agentMu.Unlock()
		}
	}
	if s.t.Arch != "" && s.t.Arch != s.arch {
		return fmt.Errorf("目标 %s 配置的 arch 是 %s，服务器实际是 %s", s.t.Name, s.t.Arch, s.arch)
	}
	return nil
}

// dial 新建 SSH 连接。
func (e *Engine) dial(t *config.Target) (*conn, error) {
	sudo := ""
	if t.User != "root" {
		sudo = "sudo -n -u root "
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
	cn := &conn{c: c, sudo: sudo, opts: opts, label: fmt.Sprintf("%s@%s:%d", t.User, t.Host, t.Port), ferryDir: t.FerryDir, agentGate: make(chan struct{}, 1)}
	if cn.ferryDir == "" {
		cn.ferryDir = AgentDir
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
func (e *Engine) ensureAgent(s *session) error {
	// 一次往返：架构、有没有 gzip、ferry 组件目录及其上级是否归 root 且他人不可写（agent 以 root 运行，不能放在别人能替换的地方），
	// 以及这个版本的 agent 在不在、能不能运行（在的话不用再单独问一次）
	paths := map[string]string{}
	sums := map[string]string{}
	sizes := map[string]int64{}
	for _, arch := range []string{"amd64", "arm64"} {
		if b, err := agentbin.Get(arch); err == nil {
			paths[arch] = s.ferryDir + "/ferry-agent-" + b.SHA256[:12]
			sums[arch] = b.SHA256
			sizes[arch] = int64(len(b.Data))
		}
	}
	// 以前完整核对过这个 agent（同一台服务器、同一组件目录、同一个 sha 和大小）：
	// 这次只核对属主、权限和大小，不再读十几 MB 算 sha256。
	trustPath := agentTrustPath(s.t, s.ferryDir)
	trust := readAgentTrust(trustPath)
	trustFlag := 0
	if trust.SHA != "" {
		trustFlag = 1
	}
	prefix := s.sudo
	if s.passwordSudo() {
		prefix = ""
	}
	check := fmt.Sprintf(`uname -m; command -v gzip >/dev/null 2>&1 && echo has-gzip || echo no-gzip; `+
		`unsafe=0; d=%[1]s; while [ -n "$d" ] && [ "$d" != / ]; do if [ -e "$d" ] || [ -L "$d" ]; then u=$(stat -c %%u "$d"); m=$(stat -c %%a "$d"); `+
		`if [ "$u" != 0 ] || [ $((0$m & 022)) -ne 0 ]; then echo "unsafe $d $u $m"; unsafe=1; fi; fi; d=$(dirname "$d"); done; `+
		`case "$(uname -m)" in x86_64|amd64) arch=amd64; a=%[2]s; want=%[3]s; size=%[4]d;; aarch64|arm64) arch=arm64; a=%[5]s; want=%[6]s; size=%[7]d;; *) arch=;; esac; `+
		`if [ "$unsafe" = 0 ] && [ -n "$arch" ]; then `+
		`if [ %[8]d = 1 ] && [ %[9]s = "$want" ] && [ %[10]s = "$arch" ] && [ %[11]d = "$size" ] && [ -f "$a" ] && [ -x "$a" ] && [ ! -L "$a" ] `+
		`&& [ "$(stat -c %%u "$a")" = 0 ] && [ $((0$(stat -c %%a "$a") & 022)) -eq 0 ]; then `+
		`echo @@agent; echo trusted; `+
		`else actual=$(sha256sum < "$a" 2>/dev/null) && [ "${actual%%%% *}" = "$want" ] || exit 0; echo @@agent; `+
		`t0=$(date +%%s%%N); %[12]s"$a" version 2>/dev/null; t1=$(date +%%s%%N); echo; echo "@@took $t0 $t1"; fi; fi`,
		shQuote(s.ferryDir), shQuote(paths["amd64"]), shQuote(sums["amd64"]), sizes["amd64"],
		shQuote(paths["arm64"]), shQuote(sums["arm64"]), sizes["arm64"],
		trustFlag, shQuote(trust.SHA), shQuote(trust.Arch), trust.Size, prefix)
	var out string
	var res remote.RunResult
	var err error
	if s.passwordSudo() {
		out, res, err = s.outputPriv(e.ctx(), "sh -c "+shQuote(check))
	} else {
		var metadata strings.Builder
		res, err = s.c.RunContext(e.ctx(), check, nil, func(line string) { metadata.WriteString(line); metadata.WriteByte('\n') })
		out = metadata.String()
	}
	if err != nil && s.passwordSudo() {
		return err
	}
	if err != nil || res.Code != 0 {
		return fmt.Errorf("读取服务器架构失败：%v %s", err, res.Stderr)
	}
	head, version, _ := strings.Cut(out, "@@agent\n")
	for _, l := range strings.Split(head, "\n") {
		if f := strings.Fields(l); len(f) == 4 && f[0] == "unsafe" {
			return fmt.Errorf("服务器上存放 ferry 组件的目录不安全：%s 的属主 uid 是 %s、权限 %s。agent 以 root 运行，ferryDir（%s）及其上级必须归 root 且其他用户不可写\n"+
				"修复：在服务器上执行 chown root:root %s && chmod go-w %s，或在配置里给这台服务器换一个 ferryDir", f[1], f[2], f[3], s.ferryDir, f[1], f[1])
		}
	}
	lines := strings.Fields(head)
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
	if strings.HasPrefix(strings.TrimSpace(version), "trusted") {
		// 上次完整核对过这个 agent：这次只核对了属主、权限和大小，不再读十几 MB 算 sha256
		return nil
	}
	if version != "" {
		version, took, _ := strings.Cut(version, "@@took ")
		e.warnSlowSudo(s.conn, took)
		if agentVersionOK(version, bin.SHA256) {
			saveAgentTrust(trustPath, agentTrust{SHA: bin.SHA256, Arch: s.arch, Size: int64(len(bin.Data))})
			return nil
		}
	}
	if e.DisableAgentInstall {
		return errors.New("服务器组件需要由用户在 Ferry 中安装或更新，AI 工具不会自动安装")
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
	tmpRemote := fmt.Sprintf("/tmp/ferry-agent-%s-%s", bin.SHA256[:12], filepath.Base(tmpLocal.Name()))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.c.RunContext(ctx, "rm -f -- "+shQuote(tmpRemote+ext), nil, nil)
	}()
	total := int64(len(data))
	err = s.c.UploadContext(e.ctx(), tmpLocal.Name(), tmpRemote+ext, 0o700, func(done, _ int64) {
		e.Rep.Progress("agent", done, total)
	})
	if err != nil {
		return err
	}
	unpack := "cat -- " + shQuote(tmpRemote)
	if hasGzip {
		unpack = "gzip -dc -- " + shQuote(tmpRemote+ext)
	}
	script := fmt.Sprintf("set -e; install -d -m 755 -o root -g root %[1]s; "+
		"tmp=$(mktemp %[1]s/.agent.XXXXXXXX); trap 'rm -f -- \"$tmp\"' EXIT; "+
		"%[3]s > \"$tmp\"; actual=$(sha256sum < \"$tmp\"); "+
		"if [ \"${actual%%%% *}\" != %[4]s ]; then echo 'agent SHA-256 mismatch' >&2; exit 1; fi; "+
		"chmod 755 \"$tmp\"; mv -fT -- \"$tmp\" %[2]s; "+
		"cd %[1]s; ls -t ferry-agent-???????????? 2>/dev/null | tail -n +4 | while IFS= read -r old; do "+
		"if [ \"$old\" != %[5]s ]; then rm -f -- \"$old\"; fi; done || true",
		shQuote(s.ferryDir), shQuote(s.agent), unpack, shQuote(bin.SHA256), shQuote(filepath.Base(s.agent)))
	_, res, err = s.outputPriv(e.ctx(), "sh -c "+shQuote(script))
	if err != nil || res.Code != 0 {
		var policyErr *remote.SudoError
		if err != nil && (s.passwordSudo() || errors.As(err, &policyErr)) {
			return err
		}
		return fmt.Errorf("安装 agent 失败：%v %s", err, strings.TrimSpace(res.Stderr))
	}
	if err := s.agentOK(bin.SHA256); err != nil {
		return err
	}
	saveAgentTrust(trustPath, agentTrust{SHA: bin.SHA256, Arch: s.arch, Size: int64(len(bin.Data))})
	return nil
}

func (s *session) agentCommand(cmd string) string {
	if s.passwordSudo() {
		return shQuote(s.agent) + " " + cmd
	}
	return s.agent + " " + cmd
}

func (s *session) agentOK(sha string) error {
	out, res, err := s.outputPriv(s.e.ctx(), s.agentCommand("version"))
	if err != nil {
		return err
	}
	if res.Code != 0 || !agentVersionOK(out, sha) {
		return errors.New("agent 安装后校验失败")
	}
	return nil
}

// agentVersionOK agent version 的输出是不是这个版本（校验和、协议版本都对上）。
func agentVersionOK(out, sha string) bool {
	var v struct {
		SHA256 string `json:"sha256"`
		Proto  int    `json:"proto"`
	}
	return json.Unmarshal([]byte(strings.TrimSpace(out)), &v) == nil && v.SHA256 == sha && v.Proto == proto.Version
}

// slowSudo 用 sudo 运行 agent 超过这么久就提示：每次操作都要等这么久。
const slowSudo = time.Second

// warnSlowSudo 不是 root 登录、每次要经 sudo 时，sudo 慢（多半是服务器的主机名解析不了）就提示怎么修。
// took 是检查脚本里量的「开始 结束」纳秒数；量不到（date 不支持 %N）时不提示。
func (e *Engine) warnSlowSudo(s *conn, took string) {
	if s.sudo == "" {
		return
	}
	f := strings.Fields(took)
	if len(f) != 2 {
		return
	}
	t0, err0 := strconv.ParseInt(f[0], 10, 64)
	t1, err1 := strconv.ParseInt(f[1], 10, 64)
	if err0 != nil || err1 != nil || t1 < t0 {
		return
	}
	if d := time.Duration(t1 - t0); d >= slowSudo {
		e.Rep.Warn("在 %s 上用 sudo 运行一次要 %.1f 秒，Ferry 的每次操作都要多等这么久。多半是服务器的主机名解析不了："+
			"在服务器上执行 hostname 看主机名，把「127.0.0.1 主机名」加进 /etc/hosts 后再试", s.label, d.Seconds())
	}
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
	onLine := func(line string) {
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
	}
	// 只读的查看交给常驻的 agent：少几次网络往返；用不了时照原来的办法单独运行
	if proto.ServeCmds[cmd] {
		code, ok, err := s.callServe(cmd, req, onLine)
		if ok {
			if err != nil {
				return nil, err
			}
			if result == nil {
				return nil, fmt.Errorf("agent %s 没有返回结果：退出码 %d", cmd, code)
			}
			return result, nil
		}
		result = nil
	}
	res, err := s.runPriv(s.e.ctx(), s.agentCommand(cmd), body, onLine)
	if err != nil {
		var need *secret.NeedError
		if errors.As(err, &need) {
			return nil, err
		}
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
	s.auditLocal(cmd, req, result)
	return result, nil
}

// auditLocal 改动服务器的 agent 命令（服务器上 agent 自己记了审计）：这台电脑上也记一份。
func (s *session) auditLocal(cmd string, req *proto.Request, r *proto.Result) {
	kind, object, detail, ok := proto.AuditOf(cmd, req)
	if !ok {
		return
	}
	var err error
	if r.RC != proto.CodeOK {
		err = errors.New(r.Msg)
	}
	entry := s.auditEntry(kind, object, detail, err)
	if cmd == "swinstall" && req.Install != nil {
		entry.TaskID = req.Install.TaskID
	}
	saveLocalAudit(entry)
}

// changingCmds 改动服务器的 agent 命令：本机取消或连接断开后，agent 在服务器上照常做完（不会停在半路）。
var changingCmds = map[string]string{
	"db-tools-install": "",
	"secretset":        "",
	"deploy":           "（失败时自动回滚）", "rollback": "（失败时切回原版本）", "control": "", "runtime": "", "runjob": "（定时任务在服务器上照常跑完）",
	"swinstall": "", "swcontrol": "", "swremove": "", "portset": "", "fwenable": "（3 分钟内没确认会自动关掉防火墙）", "fwdisable": "",
	"jdkremove": "", "runtimeremove": "",
}

// continuesNote 改动服务器的操作没拿到结果时说明：服务器上会继续做完，到哪里看结果。
func continuesNote(cmd string, err error) error {
	note, ok := changingCmds[cmd]
	if !ok {
		return err
	}
	if cmd == "jdkremove" || cmd == "runtimeremove" {
		return fmt.Errorf("%w。请刷新服务器的运行环境清单或查看审计记录确认卸载结果", err)
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

// discardStage 取消后也尝试清理本次上传和依赖；连接已断时由下次 prepare 回收残留。
func (s *session) discardStage(stage string) {
	if stage == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e := *s.e
	e.Ctx = ctx
	sc := *s
	sc.e = &e
	if err := sc.callData("discard", &proto.Request{Stage: stage}, nil); err != nil {
		s.e.Rep.Warn("上传残留暂未清理，下次连接并准备上传时重试：%v", err)
	}
}

// AgentError agent 返回了非 0 结果码。
type AgentError struct {
	Cmd string
	RC  int
	Msg string
}

func (e *AgentError) Error() string {
	if e.RC == proto.CodeLocked {
		if e.Cmd == "swinstall" {
			return e.Msg
		}
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
