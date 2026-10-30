// Package remote：SSH 连接、主机公钥校验、远程执行与 SFTP 上传。不依赖本机的 ssh/scp。
package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/safe"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Options 连接参数。
type Options struct {
	Host         string
	Port         int
	User         string
	IdentityFile string // 空 = ~/.ssh 下的默认私钥
	HostKey      string // 固定指纹 SHA256:...；设置后不看 known_hosts
	AcceptNew    bool   // 未知主机：记录公钥并继续（等同 OpenSSH 的 accept-new）
	Timeout      time.Duration
	OnNewHostKey func(fingerprint string)

	Auth         string                               // "password" 用密码登录；其他 = 私钥（及 ssh-agent）
	Password     func() (string, error)               // Auth=password 时取密码
	Passphrase   func(keyPath string) (string, error) // 私钥有密码时取密码
	OnlyIdentity bool                                 // 只用 IdentityFile，不用默认私钥和 ssh-agent（验证新密钥时）
	// KeyData 私钥的内容（CI 里从环境变量来）：设置后只用它，不用 IdentityFile、默认私钥和 ssh-agent；KeyName 是它的来源（说明、取密码用）
	KeyData []byte
	KeyName string
}

// ErrNoKey 用私钥登录，但这台电脑上一个私钥也没有（没有指定私钥、~/.ssh 下没有默认私钥、没有 ssh-agent）。
var ErrNoKey = errors.New("没有可用的私钥：在目标里设置 identityFile，启动 ssh-agent，或用 ferry keygen 生成并安装密钥")

// UnknownHostError 第一次连接，需要先信任主机公钥。
type UnknownHostError struct {
	Addr        string
	Fingerprint string
	KeyType     string
}

// JSONDetail --json 输出的附加信息。
func (e *UnknownHostError) JSONDetail() map[string]any {
	return map[string]any{"kind": "unknown-host", "addr": e.Addr, "fingerprint": e.Fingerprint}
}

func (e *UnknownHostError) Error() string {
	return fmt.Sprintf("未知主机 %s（%s %s）：先执行 ferry trust <目标> 核对并记录主机公钥", e.Addr, e.KeyType, e.Fingerprint)
}

// Client 一条 SSH 连接（以及按需创建的 SFTP 会话）。
type Client struct {
	ssh  *ssh.Client
	sftp *sftp.Client
	Addr string

	sftpMu sync.Mutex
	dead   atomic.Bool  // 保活失败或已关闭
	rtt    atomic.Int64 // 最近一次量的往返时间（纳秒），见 Latency
	rttAt  atomic.Int64 // 量的时间（UnixNano）；0 = 还没量过
}

// KnownHostsFile ferry 自己记录主机公钥的文件（同时也读 ~/.ssh/known_hosts）。
func KnownHostsFile() string { return appdir.KnownHosts() }

// Dial 建立连接。
func Dial(o Options) (*Client, error) {
	if o.Port == 0 {
		o.Port = 22
	}
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Second
	}
	addr := net.JoinHostPort(o.Host, strconv.Itoa(o.Port))
	auth, err := authMethods(o)
	if err != nil {
		return nil, err
	}
	hk, algos, err := hostKeyCallback(o)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{User: o.User, Auth: auth, HostKeyCallback: hk, HostKeyAlgorithms: algos, Timeout: o.Timeout}
	c, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		var uh *UnknownHostError
		if errors.As(err, &uh) {
			return nil, uh
		}
		// 回调里返回的错误（需要密码、私钥密码不对）原样往上传
		var need interface{ JSONDetail() map[string]any }
		if errors.As(err, &need) {
			return nil, err
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, &AuthFailedError{Addr: o.User + "@" + addr, Err: err}
		}
		return nil, dialError("连接 "+o.User+"@"+addr+" 失败", err)
	}
	cl := &Client{ssh: c, Addr: addr}
	// 保活：长时间等待服务启动时不被中间设备断开；失败说明连接已断，标记后复用前会重连
	safe.Go("保活", func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				cl.dead.Store(true)
				return
			}
		}
	})
	safe.Go("连接监视", func() {
		c.Wait() // 连接断开（对端关闭、网络中断）时返回
		cl.dead.Store(true)
	})
	return cl, nil
}

// Alive 连接是否还可用（已知断开时为 false）。
func (c *Client) Alive() bool { return !c.dead.Load() }

// ListenRemote 在服务器上监听 addr（如 127.0.0.1:0，端口由服务器分配），连进来的连接经这条 SSH 转给本机。
// 服务器的 sshd 不允许端口转发（AllowTcpForwarding no）时报错。
func (c *Client) ListenRemote(addr string) (net.Listener, error) {
	return c.ssh.Listen("tcp", addr)
}

// ResetSFTP 丢掉缓存的 SFTP 会话（它断了而 SSH 连接还在时），下次用时重新打开。
func (c *Client) ResetSFTP() {
	c.sftpMu.Lock()
	defer c.sftpMu.Unlock()
	if c.sftp != nil {
		c.sftp.Close()
		c.sftp = nil
	}
}

// Ping 发一次保活请求确认连接可用（VPN 重连后旧连接常常已失效，但要发包才知道）。
func (c *Client) Ping(timeout time.Duration) error {
	if c.dead.Load() {
		return errors.New("连接已断开")
	}
	done := make(chan error, 1)
	safe.Go("确认连接", func() {
		_, _, err := c.ssh.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	})
	select {
	case err := <-done:
		if err != nil {
			c.dead.Store(true)
		}
		return err
	case <-time.After(timeout):
		c.dead.Store(true)
		return errors.New("连接无响应")
	}
}

// Latency 到服务器的往返时间：发一个 SSH 保活请求，量一来一回（和打字回显经过同一条连接、同样的路）。
// 超过 timeout 时返回 ErrSlow；网络慢不等于断了，不把连接标成断开。最近一次的结果记下来，maxAge 内不重新量。
func (c *Client) Latency(timeout, maxAge time.Duration) (time.Duration, error) {
	if c.dead.Load() {
		return 0, errors.New("连接已断开")
	}
	if at := c.rttAt.Load(); maxAge > 0 && at != 0 && time.Since(time.Unix(0, at)) < maxAge {
		return time.Duration(c.rtt.Load()), nil
	}
	// 连着量两次取小的：服务器没有打开终端时（非交互连接）会把小包攒一下再发，
	// 刚做完别的操作时第一次常常多出一个来回，第二次才是真正的往返时间
	type res struct {
		d   time.Duration
		err error
	}
	done := make(chan res, 1)
	safe.Go("测延迟", func() {
		best := time.Duration(0)
		for range 2 {
			start := time.Now()
			if _, _, err := c.ssh.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				done <- res{err: err}
				return
			}
			if d := time.Since(start); best == 0 || d < best {
				best = d
			}
		}
		done <- res{d: best}
	})
	select {
	case r := <-done:
		if r.err != nil {
			c.dead.Store(true)
			return 0, r.err
		}
		c.rtt.Store(int64(r.d))
		c.rttAt.Store(time.Now().UnixNano())
		return r.d, nil
	case <-time.After(timeout):
		return 0, ErrSlow
	}
}

// ErrSlow 量往返时间时超时（网络很慢或卡住了）。
var ErrSlow = errors.New("超时")

// Close 关闭连接。
func (c *Client) Close() error {
	c.dead.Store(true)
	c.sftpMu.Lock()
	if c.sftp != nil {
		c.sftp.Close()
	}
	c.sftpMu.Unlock()
	return c.ssh.Close()
}

func authMethods(o Options) ([]ssh.AuthMethod, error) {
	if o.Auth == "password" {
		if o.Password == nil {
			return nil, errors.New("没有提供密码")
		}
		// 密码只取一次：password 与 keyboard-interactive 共用
		var cached *string
		get := func() (string, error) {
			if cached != nil {
				return *cached, nil
			}
			p, err := o.Password()
			if err != nil {
				return "", err
			}
			cached = &p
			return p, nil
		}
		return []ssh.AuthMethod{
			ssh.PasswordCallback(get),
			// 不少服务器只开了 keyboard-interactive（PAM）：对所有不回显的提问回答密码
			ssh.KeyboardInteractive(func(_, _ string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					if echos[i] {
						continue
					}
					p, err := get()
					if err != nil {
						return nil, err
					}
					answers[i] = p
				}
				return answers, nil
			}),
		}, nil
	}
	if len(o.KeyData) > 0 {
		s, err := parseKey(o.KeyData, o.KeyName, o.Passphrase)
		if err != nil {
			return nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(s)}, nil
	}
	var signers []ssh.Signer
	var files []string
	if o.IdentityFile != "" {
		files = []string{o.IdentityFile}
	} else if home, err := os.UserHomeDir(); err == nil && !o.OnlyIdentity {
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			p := filepath.Join(home, ".ssh", n)
			if _, err := os.Stat(p); err == nil {
				files = append(files, p)
			}
		}
	}
	for _, f := range files {
		s, err := LoadKey(f, o.Passphrase)
		if err != nil {
			if o.IdentityFile != "" {
				return nil, err
			}
			continue
		}
		signers = append(signers, s)
	}
	var methods []ssh.AuthMethod
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	if !o.OnlyIdentity {
		if ag := sshAgent(); ag != nil {
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}
	if len(methods) == 0 {
		return nil, ErrNoKey
	}
	return methods, nil
}

// LoadKey 读取私钥；有密码时调用 passphrase 取密码（nil 则报错）。
func LoadKey(p string, passphrase func(keyPath string) (string, error)) (ssh.Signer, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取私钥 %s：%w", p, err)
	}
	return parseKey(data, p, passphrase)
}

// parseKey 解析私钥；有密码时调用 passphrase(name) 取密码。
func parseKey(data []byte, p string, passphrase func(keyPath string) (string, error)) (ssh.Signer, error) {
	s, err := ssh.ParsePrivateKey(data)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if passphrase == nil {
			return nil, fmt.Errorf("私钥 %s 有密码", p)
		}
		pass, perr := passphrase(p)
		if perr != nil {
			return nil, perr
		}
		s, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(pass))
		if errors.Is(err, x509.IncorrectPasswordError) {
			return nil, &PassphraseRejectedError{Key: p}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("解析私钥 %s：%w", p, err)
	}
	return s, nil
}

// PassphraseRejectedError 私钥密码不正确。
type PassphraseRejectedError struct{ Key string }

func (e *PassphraseRejectedError) Error() string { return "私钥 " + e.Key + " 的密码不正确" }

// AuthFailedError 服务器拒绝了登录（密码或私钥不对）。
type AuthFailedError struct {
	Addr string
	Err  error
}

func (e *AuthFailedError) Error() string { return "登录 " + e.Addr + " 被拒绝：" + e.Err.Error() }
func (e *AuthFailedError) Unwrap() error { return e.Err }

// sshAgent Unix 用 SSH_AUTH_SOCK，Windows 用 OpenSSH 的命名管道。
func sshAgent() agent.ExtendedAgent {
	var conn io.ReadWriter
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" && runtime.GOOS != "windows" {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return nil
		}
		conn = c
	} else if runtime.GOOS == "windows" {
		f, err := os.OpenFile(`\\.\pipe\openssh-ssh-agent`, os.O_RDWR, 0)
		if err != nil {
			return nil
		}
		conn = f
	}
	if conn == nil {
		return nil
	}
	return agent.NewClient(conn)
}

// knownHostsDB ferry 自己的 known_hosts 加上 ~/.ssh/known_hosts。
func knownHostsDB() (ssh.HostKeyCallback, string, error) {
	own := KnownHostsFile()
	if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
		return nil, own, err
	}
	if f, err := os.OpenFile(own, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close()
	}
	files := []string{own}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".ssh", "known_hosts")
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	cb, err := knownhosts.New(files...)
	if err != nil {
		return nil, own, fmt.Errorf("读取 known_hosts：%w", err)
	}
	return cb, own, nil
}

// hostKeyCallback 主机公钥校验，以及协商用的主机公钥算法（已记录过就只用记录的类型）。
func hostKeyCallback(o Options) (ssh.HostKeyCallback, []string, error) {
	if o.HostKey != "" {
		return func(host string, _ net.Addr, key ssh.PublicKey) error {
			if fp := ssh.FingerprintSHA256(key); fp != o.HostKey {
				return fmt.Errorf("主机 %s 的公钥指纹是 %s，与配置的 hostKey %s 不一致：可能遭到中间人攻击，已中止", host, fp, o.HostKey)
			}
			return nil
		}, DefaultHostKeyAlgorithms, nil
	}
	cb, own, err := knownHostsDB()
	if err != nil {
		return nil, nil, err
	}
	algos := knownAlgorithms(cb, o.Host, o.Port)
	if len(algos) == 0 {
		algos = DefaultHostKeyAlgorithms
	}
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(host, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			fp := ssh.FingerprintSHA256(key)
			if !o.AcceptNew {
				return &UnknownHostError{Addr: host, Fingerprint: fp, KeyType: key.Type()}
			}
			line := knownhosts.Line([]string{knownhosts.Normalize(host)}, key)
			f, ferr := os.OpenFile(own, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
			if ferr != nil {
				return ferr
			}
			defer f.Close()
			if _, ferr := f.WriteString(line + "\n"); ferr != nil {
				return ferr
			}
			if o.OnNewHostKey != nil {
				o.OnNewHostKey(fp)
			}
			return nil
		}
		if errors.As(err, &ke) {
			return fmt.Errorf("主机 %s 的公钥与已记录的不一致（%s）：可能遭到中间人攻击，已中止。确认服务器确实重装过后，从 %s 删除旧记录",
				host, ssh.FingerprintSHA256(key), ke.Want[0].Filename)
		}
		return err
	}, algos, nil
}

// ScanHostKey 只握手取主机公钥（用于 ferry trust）。
func ScanHostKey(host string, port int) (ssh.PublicKey, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var got ssh.PublicKey
	// 与实际连接用同一算法顺序：已记录过就取记录的类型，否则 ED25519 优先
	db, _, _ := knownHostsDB()
	algos := knownAlgorithms(db, host, port)
	if len(algos) == 0 {
		algos = DefaultHostKeyAlgorithms
	}
	cfg := &ssh.ClientConfig{
		User: "ferry-scan", Timeout: 15 * time.Second, HostKeyAlgorithms: algos,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got = key
			return errors.New("scan")
		},
	}
	_, err := ssh.Dial("tcp", addr, cfg)
	if got != nil {
		return got, nil
	}
	return nil, dialError("连接 "+addr, err)
}

// dialError 连不上服务器的错误：认得出原因时先用直白的话说明，原始错误放在括号里。
func dialError(what string, err error) error {
	if h := dialHint(err); h != "" {
		return fmt.Errorf("%s：%s（%w）", what, h, err)
	}
	return fmt.Errorf("%s：%w", what, err)
}

// dialHint 连不上服务器的原因（直白的说法）；认不出来时返回空。
func dialHint(err error) string {
	var dns *net.DNSError
	var ne net.Error
	msg := strings.ToLower(err.Error())
	switch {
	case errors.As(err, &dns):
		return "找不到这个地址，确认地址有没有写错。"
	case strings.Contains(msg, "refused"):
		return "服务器拒绝连接，这个端口上没有 SSH 服务。确认 SSH 端口对不对（不一定是 22），以及防火墙有没有放行。"
	case errors.As(err, &ne) && ne.Timeout(), strings.Contains(msg, "timed out"), strings.Contains(msg, "timeout"):
		return "连接超时，确认地址、端口对不对，以及防火墙、安全组有没有放行这个端口。"
	case strings.Contains(msg, "no route") || strings.Contains(msg, "unreachable"):
		return "网络不通，确认地址对不对、本机能不能访问这个网络（是否要连 VPN）。"
	}
	return ""
}

// TrustHostKey 记录主机公钥到数据目录下的 known_hosts。
func TrustHostKey(host string, port int, key ssh.PublicKey) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	f, err := os.OpenFile(KnownHostsFile(), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(knownhosts.Line([]string{knownhosts.Normalize(addr)}, key) + "\n")
	return err
}

// ---------------------------------------------------------------- 执行

// RunResult 远程命令结果。
type RunResult struct {
	Code   int
	Stderr string
}

// Run 执行命令；stdout 逐行回调，stderr 收集。退出码非 0 不算 error（看 Code）。
func (c *Client) Run(cmd string, stdin []byte, onLine func(string)) (RunResult, error) {
	return c.RunContext(context.Background(), cmd, stdin, onLine)
}

// RunContext 同 Run；ctx 取消时关闭这个通道（不影响同一连接上的其他操作）。
func (c *Client) RunContext(ctx context.Context, cmd string, stdin []byte, onLine func(string)) (RunResult, error) {
	s, err := c.ssh.NewSession()
	if err != nil {
		return RunResult{}, err
	}
	stopWatch := context.AfterFunc(ctx, func() { s.Close() })
	defer stopWatch()
	defer s.Close()
	if stdin != nil {
		s.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	s.Stderr = &limitedBuffer{buf: &stderr, max: 64 << 10}
	out, err := s.StdoutPipe()
	if err != nil {
		return RunResult{}, err
	}
	if err := s.Start(cmd); err != nil {
		return RunResult{}, err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if onLine != nil {
			onLine(sc.Text())
		}
	}
	err = s.Wait()
	res := RunResult{Stderr: stderr.String()}
	if ctx.Err() != nil {
		return res, ErrCanceled
	}
	var ee *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Code = ee.ExitStatus()
	default:
		var missing *ssh.ExitMissingError
		if errors.As(err, &missing) {
			return res, errors.New("连接中断，远程命令的结果未知：用 ferry status 查看服务器状态")
		}
		return res, err
	}
	return res, nil
}

// Output 执行命令并返回 stdout 全文。
func (c *Client) Output(cmd string) (string, RunResult, error) {
	var b strings.Builder
	res, err := c.Run(cmd, nil, func(l string) { b.WriteString(l); b.WriteByte('\n') })
	return b.String(), res, err
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// ---------------------------------------------------------------- SFTP

func (c *Client) sftpClient() (*sftp.Client, error) {
	c.sftpMu.Lock() // 连接被多个请求复用时可能同时调用
	defer c.sftpMu.Unlock()
	if c.sftp != nil {
		return c.sftp, nil
	}
	s, err := sftp.NewClient(c.ssh, sftp.UseConcurrentWrites(true), sftp.MaxConcurrentRequestsPerFile(64))
	if err != nil {
		return nil, fmt.Errorf("打开 SFTP：%w", err)
	}
	c.sftp = s
	return s, nil
}

// Progress 上传进度回调：本文件已传字节、本文件总字节。
type Progress func(done, total int64)

// Upload 上传文件到 remotePath（先写 .part 再改名）。
func (c *Client) Upload(local, remotePath string, mode os.FileMode, progress Progress) error {
	return c.UploadContext(context.Background(), local, remotePath, mode, progress)
}

// UploadContext 同 Upload，ctx 取消时中止。
func (c *Client) UploadContext(ctx context.Context, local, remotePath string, mode os.FileMode, progress Progress) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	tmp := remotePath + ".part"
	dst, err := s.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("创建远程文件 %s：%w", tmp, err)
	}
	r := &countingReader{ctx: ctx, r: src, size: st.Size(), progress: progress}
	if _, err := dst.ReadFrom(r); err != nil {
		dst.Close()
		s.Remove(tmp)
		return fmt.Errorf("上传 %s：%w", filepath.Base(local), err)
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if err := s.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := s.PosixRename(tmp, remotePath); err != nil {
		return fmt.Errorf("改名 %s：%w", remotePath, err)
	}
	return nil
}

// MkdirAll 远程创建目录。
func (c *Client) MkdirAll(dir string) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	return s.MkdirAll(dir)
}

// Join 远程路径拼接。
func Join(elem ...string) string { return path.Join(elem...) }

type countingReader struct {
	ctx      context.Context
	r        io.Reader
	size     int64
	done     atomic.Int64
	progress Progress
	last     time.Time
}

func (c *countingReader) Size() int64 { return c.size }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.ctx != nil && c.ctx.Err() != nil {
		return 0, ErrCanceled
	}
	n, err := c.r.Read(p)
	if n > 0 {
		d := c.done.Add(int64(n))
		if c.progress != nil && (time.Since(c.last) > 200*time.Millisecond || d == c.size) {
			c.last = time.Now()
			c.progress(d, c.size)
		}
	}
	return n, err
}

// Stream 执行命令，输出原样写到 out，直到命令结束或 stop 关闭。
// pty 为 true 时申请伪终端：断开时远端进程收到 SIGHUP 退出（用于 journalctl -f、tail -F）。
func (c *Client) Stream(cmd string, out io.Writer, pty bool, stop <-chan struct{}) error {
	s, err := c.ssh.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	if pty {
		if err := s.RequestPty("dumb", 40, 200, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
			return err
		}
	}
	s.Stdout = out
	var stderr bytes.Buffer
	s.Stderr = &limitedBuffer{buf: &stderr, max: 16 << 10}
	if err := s.Start(cmd); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("退出码 %d：%s", ee.ExitStatus(), strings.TrimSpace(stderr.String()))
		}
		return err
	case <-stop:
		s.Close()
		return nil
	}
}

// ShellOptions 交互式终端的参数。
type ShellOptions struct {
	Term       string // 终端类型，如 xterm-256color
	Cols, Rows int
	Stdin      io.Reader     // 键盘输入；关闭（返回 EOF）后不再读
	Stdout     io.Writer     // 终端输出（伪终端里 stderr 也在这里）
	Resize     <-chan [2]int // 窗口大小变化：列、行
	OnStart    func()        // shell 已启动
}

// Shell 打开登录用户的交互式 shell（申请伪终端），直到 shell 退出、连接断开或 ctx 取消。返回 shell 的退出码。
func (c *Client) Shell(ctx context.Context, o ShellOptions) (int, error) {
	s, err := c.ssh.NewSession()
	if err != nil {
		return -1, err
	}
	defer s.Close()
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := s.RequestPty(o.Term, max(o.Rows, 1), max(o.Cols, 1), modes); err != nil {
		return -1, fmt.Errorf("申请终端失败：%w", err)
	}
	// 输入自己转发：交给 s.Stdin 的话，Wait 要等输入结束才返回（shell 已经退出也一样）
	in, err := s.StdinPipe()
	if err != nil {
		return -1, err
	}
	s.Stdout = o.Stdout
	s.Stderr = o.Stdout
	if err := s.Shell(); err != nil {
		return -1, fmt.Errorf("启动 shell 失败：%w", err)
	}
	safe.Go("终端输入", func() {
		io.Copy(in, o.Stdin)
		in.Close()
	})
	if o.OnStart != nil {
		o.OnStart()
	}
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	for {
		select {
		case err := <-done:
			var ee *ssh.ExitError
			var missing *ssh.ExitMissingError
			switch {
			case err == nil:
				return 0, nil
			case errors.As(err, &ee):
				return ee.ExitStatus(), nil
			case errors.As(err, &missing):
				return -1, errors.New("连接断开了")
			}
			return -1, err
		case sz := <-o.Resize:
			s.WindowChange(max(sz[1], 1), max(sz[0], 1))
		case <-ctx.Done():
			s.Close()
			return -1, ErrCanceled
		}
	}
}

// HostKeyState 主机公钥与本机记录的关系：known（已记录且一致）、unknown（未记录）、mismatch（与记录不一致）。
func HostKeyState(host string, port int, key ssh.PublicKey) string {
	cb, _, err := knownHostsDB()
	if err != nil {
		return "unknown"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	err = cb(addr, &net.TCPAddr{IP: net.ParseIP(host), Port: port}, key)
	var ke *knownhosts.KeyError
	switch {
	case err == nil:
		return "known"
	case errors.As(err, &ke) && len(ke.Want) > 0:
		return "mismatch"
	}
	return "unknown"
}

// ErrCanceled 操作被取消（本机这一侧）。
var ErrCanceled = errors.New("已取消")
