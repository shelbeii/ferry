package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// UnitProps systemctl show 的几个属性。systemd 219 没有 --value，也没有 NRestarts。
type UnitProps struct {
	Load   string // loaded / not-found
	Active string // active / activating / deactivating / inactive / failed
	Sub    string // running / auto-restart / dead / exited ...
	PID    int
	Since  string
}

// System 对操作系统的全部依赖。真实实现见 realSystem；测试用假实现。
type System interface {
	Systemctl(args ...string) error
	UnitProps(unit string) UnitProps
	Journal(unit string, n int, since time.Time) []string
	LookupUser(name string) (uid, gid int, err error)
	// AccessAs 以 user 的身份测试 path 有没有 flag（'r' / 'w' / 'x'）权限
	AccessAs(user, path string, flag byte) (bool, error)
	// Ownership 文件的属主、组与权限（说明访问不了的原因）
	Ownership(path string) (Ownership, error)
	// UserGroups 用户的 uid 与所属的全部组
	UserGroups(name string) (uid int, gids []int, err error)
	// Chmod 修改权限（自动修正权限时）
	Chmod(path string, mode os.FileMode) error
	// SetFACL 用 ACL 给 user 加上 flag 权限；不支持 ACL（没有 setfacl 或文件系统不支持）时 supported 为 false
	SetFACL(user, path string, flag byte) (supported bool, err error)
	// Listening 进程 pid 及其子进程正在监听的 TCP 地址（127.0.0.1:9201、*:8080 …，按端口排序）
	Listening(pid int) []string
	// ProcArgs 进程 pid 及其子进程的命令行参数（识别附加端口的用途）
	ProcArgs(pid int) [][]string
	// NginxDump nginx -T 的输出（完整配置）；没有 nginx 时返回 ErrNoNginx
	NginxDump() (string, error)
	// JavaVersion 运行 <java> -version 的输出
	JavaVersion(java string) (string, error)
	Lchown(path string, uid, gid int) error
	Shell(cmd string, timeout time.Duration) (string, error)
	// Pipe 执行 sh -c cmd（以 root）：env 追加到环境变量里，stdin、stdout 接上，stderr 最后一段放进错误里；超时结束整组进程
	Pipe(cmd string, env []string, stdin io.Reader, stdout io.Writer, timeout time.Duration) error
	// RunHook 执行部署钩子：以 h 里的用户身份、在 h.Dir 里执行 sh -c，每输出一行调用一次 line；超时结束整组进程
	RunHook(h HookExec, line func(string)) error
	Probe(h proto.Health) error
	SwitchLink(link, target string) error
	ReadLink(link string) (string, error)
	RemoveLink(link string) error
	Lock(path string, wait time.Duration) (func(), error)
	UnitDir() string
	Now() time.Time
	Sleep(d time.Duration)
}

// Ownership 文件的属主、组与权限。
type Ownership struct {
	UID, GID    int
	User, Group string // 名字；查不到时为数字
	Perm        os.FileMode
}

// ErrLocked 另一个操作持有锁。
var ErrLocked = errors.New("另一个部署/回滚/启停正在进行")

type realSystem struct{}

// NewSystem 真实的 Linux 系统。
func NewSystem() System { return realSystem{} }

func (realSystem) Systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s：%v %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (realSystem) AccessAs(name, path string, flag byte) (bool, error) {
	runuser, err := exec.LookPath("runuser")
	if err != nil {
		return false, errors.New("服务器上没有 runuser 命令")
	}
	test, err := exec.LookPath("test")
	if err != nil {
		test = "test"
	}
	var stderr bytes.Buffer
	cmd := exec.Command(runuser, "-u", name, "--", test, "-"+string(flag), path)
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && strings.TrimSpace(stderr.String()) == "" {
		return false, nil
	}
	return false, fmt.Errorf("runuser：%v %s", err, strings.TrimSpace(stderr.String()))
}

func (realSystem) Ownership(path string) (Ownership, error) { return ownershipOf(path) }

func (realSystem) Chmod(path string, mode os.FileMode) error { return os.Chmod(path, mode) }

func (realSystem) SetFACL(user, path string, flag byte) (bool, error) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		return false, nil
	}
	// -m 会整条替换这个用户的 ACL 项：先读出已有的权限再合并（先加 r 再加 x，不能变成只有 x）
	perms := string(flag)
	if cur, err := exec.Command("getfacl", "-p", "--omit-header", path).Output(); err == nil {
		for _, l := range strings.Split(string(cur), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "user:"+user+":"); ok {
				v, _, _ = strings.Cut(v, "\t") // 去掉 #effective 注释
				for _, c := range strings.TrimSpace(v) {
					if c != '-' && !strings.ContainsRune(perms, c) {
						perms += string(c)
					}
				}
			}
		}
	}
	out, err := exec.Command("setfacl", "-m", fmt.Sprintf("u:%s:%s", user, perms), path).CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "not supported") {
			return false, nil
		}
		return true, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return true, nil
}

func (realSystem) JavaVersion(java string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, java, "-version").CombinedOutput()
	return string(out), err
}

func (realSystem) UserGroups(name string) (int, []int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	ids, err := u.GroupIds()
	if err != nil {
		return 0, nil, err
	}
	var gids []int
	for _, g := range ids {
		if n, err := strconv.Atoi(g); err == nil {
			gids = append(gids, n)
		}
	}
	return uid, gids, nil
}

func (realSystem) UnitProps(unit string) UnitProps {
	out, _ := exec.Command("systemctl", "show", unit,
		"-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "MainPID", "-p", "ActiveEnterTimestamp").Output()
	return parseProps(string(out))
}

func parseProps(s string) UnitProps {
	var p UnitProps
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			p.Load = v
		case "ActiveState":
			p.Active = v
		case "SubState":
			p.Sub = v
		case "MainPID":
			p.PID, _ = strconv.Atoi(v)
		case "ActiveEnterTimestamp":
			p.Since = v
		}
	}
	return p
}

func (realSystem) Journal(unit string, n int, since time.Time) []string {
	out, _ := exec.Command("journalctl", journalArgs(unit, n, since)...).Output()
	return keepLast(splitLines(string(out)), n)
}

// journalArgs 取服务最后 n 条输出的参数。老版本的 systemd（如 219）同时给 --since 和 -n 时
// 从 --since 往后取 n 条，不是最后 n 条：有 since 时不给 -n，读完再留最后 n 条（since 都是不久前，不会太多）。
func journalArgs(unit string, n int, since time.Time) []string {
	args := []string{"-u", unit, "--no-pager", "-o", "cat"}
	if since.IsZero() {
		return append(args, "-n", strconv.Itoa(n))
	}
	return append(args, "--since", since.Format("2006-01-02 15:04:05"))
}

func keepLast(lines []string, n int) []string {
	if n >= 0 && len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

func (realSystem) LookupUser(name string) (int, int, error) {
	if u, err := user.Lookup(name); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		return uid, gid, nil
	}
	// 静态编译的 os/user 只读 /etc/passwd；LDAP 等账号用 getent
	out, err := exec.Command("getent", "passwd", name).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("用户 %s 不存在", name)
	}
	f := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(f) < 4 {
		return 0, 0, fmt.Errorf("用户 %s 信息无法解析", name)
	}
	uid, err1 := strconv.Atoi(f[2])
	gid, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("用户 %s 信息无法解析", name)
	}
	return uid, gid, nil
}

func (realSystem) Lchown(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }

func (realSystem) Shell(cmd string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c", cmd).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("超时（%s）", timeout)
	}
	return string(out), err
}

func (realSystem) Pipe(cmd string, env []string, stdin io.Reader, stdout io.Writer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Env = append(os.Environ(), env...)
	c.Stdin, c.Stdout = stdin, stdout
	var stderr tailBuffer
	c.Stderr = &stderr
	setPgid(c)
	err := c.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("超时（%s）", timeout)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%v：%s", err, msg)
		}
	}
	return err
}

// tailBuffer 只留最后 4KB（命令的错误输出）。
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

func (realSystem) Probe(h proto.Health) error {
	if h.TCP != "" {
		c, err := net.DialTimeout("tcp", h.TCP, 2*time.Second)
		if err != nil {
			return fmt.Errorf("%s 未监听", h.TCP)
		}
		c.Close()
	}
	if h.HTTP != "" {
		client := &http.Client{
			Timeout: 5 * time.Second,
			// 健康检查只访问本机，自签名证书很常见
			Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		req, err := http.NewRequest(http.MethodGet, h.HTTP, nil)
		if err != nil {
			return err
		}
		if h.Host != "" {
			req.Host = h.Host
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%s：%v", h.HTTP, shortErr(err))
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if !statusOK(resp.StatusCode, h.Status) {
			return fmt.Errorf("%s 返回 %d", h.HTTP, resp.StatusCode)
		}
		if h.Contains != "" && !bytes.Contains(body, []byte(h.Contains)) {
			return fmt.Errorf("%s 响应中没有 %q", h.HTTP, h.Contains)
		}
	}
	return nil
}

func statusOK(code int, want []int) bool {
	if len(want) == 0 {
		return code >= 200 && code < 400
	}
	for _, w := range want {
		if w == code {
			return true
		}
	}
	return false
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}

// SwitchLink 原子替换符号链接：先建临时链接，再 rename 覆盖。link 是真实目录或文件时拒绝。
func (realSystem) SwitchLink(link, target string) error {
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s 已存在且不是符号链接，请先移走", link)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", link, time.Now().UnixNano())
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (realSystem) ReadLink(link string) (string, error) { return os.Readlink(link) }

// RemoveLink 删除符号链接（不是链接时不动）。
func (realSystem) RemoveLink(link string) error {
	fi, err := os.Lstat(link)
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s 不是符号链接", link)
	}
	return os.Remove(link)
}

func (realSystem) Lock(path string, wait time.Duration) (func(), error) { return flockFile(path, wait) }

func (realSystem) UnitDir() string {
	if d := os.Getenv("FERRY_SYSTEMD_UNIT_DIR"); d != "" {
		return d
	}
	return "/etc/systemd/system"
}

func (realSystem) Now() time.Time        { return time.Now() }
func (realSystem) Sleep(d time.Duration) { time.Sleep(d) }

func splitLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// tailFile 读文件最后 n 行（最多读末尾 256KB）。
func tailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const max = 256 << 10
	off := int64(0)
	if st.Size() > max {
		off = st.Size() - max
	}
	buf, err := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	if err != nil {
		return nil, err
	}
	lines := splitLines(string(buf))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // 第一行可能不完整
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// isStackLine Java 堆栈行，诊断输出时去掉。
func isStackLine(s string) bool {
	t := strings.TrimLeft(s, " \t")
	if len(t) == len(s) {
		return false
	}
	return strings.HasPrefix(t, "at ") || (strings.HasPrefix(t, "... ") && (strings.HasSuffix(t, "more") || strings.Contains(t, "common frames")))
}

// sameFile 内容是否一致（目标不存在视为不同）。
func sameFile(path string, data []byte) bool {
	old, err := os.ReadFile(path)
	return err == nil && bytes.Equal(old, data)
}

// writeFileAtomic 写临时文件再改名。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := fmt.Sprintf("%s.tmp-%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// moveFile 改名，跨分区时复制。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// moveTree 移动文件或目录：同一分区内改名；跨分区时复制（保留权限、属主、修改时间）后删除原来的。
func moveTree(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		os.RemoveAll(dst)
		return err
	}
	return os.RemoveAll(src)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			if err := os.Mkdir(to, fi.Mode().Perm()); err != nil && !os.IsExist(err) {
				return err
			}
		case fi.Mode()&os.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(t, to); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm())
			if err != nil {
				in.Close()
				return err
			}
			_, err = io.Copy(out, in)
			in.Close()
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return nil // 设备文件等：不复制
		}
		if uid, gid, ok := fileOwner(fi); ok {
			os.Lchown(to, uid, gid)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			os.Chmod(to, fi.Mode().Perm())
			os.Chtimes(to, fi.ModTime(), fi.ModTime())
		}
		return nil
	})
}

// mergeTree 把 src 里的东西并进 dst：dst 里没有的移过去，两边都是目录的往下合并，重名的文件留在 src。返回留下的个数。
func mergeTree(src, dst string) (int, error) {
	if !exists(dst) {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		return 0, moveTree(src, dst)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return 0, err
	}
	left := 0
	for _, e := range ents {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		switch {
		case !exists(d):
			if err := moveTree(s, d); err != nil {
				return left, err
			}
		case realDir(s) && realDir(d):
			n, err := mergeTree(s, d)
			left += n
			if err != nil {
				return left, err
			}
		default:
			left++
		}
	}
	os.Remove(src) // 空了才删得掉
	return left, nil
}

// within 判断 p 是否在 dir 下（都已 Clean）。
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// ---------------------------------------------------------------- 监听端口（读 /proc）

func (realSystem) Listening(pid int) []string { return listeningFromProc("/proc", pid) }

// listeningFromProc 找出 pid 及其子孙进程的 socket，再到 net/tcp、tcp6 里查哪些处于监听状态。
func listeningFromProc(proc string, pid int) []string {
	if pid <= 0 {
		return nil
	}
	pids := procTree(proc, pid)
	inodes := map[string]bool{}
	for p := range pids {
		fds, _ := os.ReadDir(filepath.Join(proc, strconv.Itoa(p), "fd"))
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(proc, strconv.Itoa(p), "fd", fd.Name()))
			if err == nil && strings.HasPrefix(link, "socket:[") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
			}
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	var tables [][]byte
	for _, name := range []string{"tcp", "tcp6"} {
		if b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "net", name)); err == nil {
			tables = append(tables, b)
		}
	}
	return listenAddrs(tables, inodes)
}

func (realSystem) ProcArgs(pid int) [][]string {
	if pid <= 0 {
		return nil
	}
	var out [][]string
	for p := range procTree("/proc", pid) {
		b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(p), "cmdline"))
		if err == nil && len(b) > 0 {
			out = append(out, strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"))
		}
	}
	return out
}

// procTree 进程 pid 及其子孙进程（包装脚本、fork 出的工作进程）。
func procTree(proc string, pid int) map[int]bool {
	pids := map[int]bool{pid: true}
	// 子孙进程：按父进程号一层层找（包装脚本、fork 出的工作进程）
	parent := map[int]int{}
	ents, _ := os.ReadDir(proc)
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(proc, e.Name(), "stat"))
		if err != nil {
			continue
		}
		// stat：pid (comm) state ppid …；comm 里可能有空格和括号，取最后一个 ) 之后
		rest := string(b)
		if i := strings.LastIndexByte(rest, ')'); i >= 0 {
			rest = rest[i+1:]
		}
		if f := strings.Fields(rest); len(f) >= 2 {
			if pp, err := strconv.Atoi(f[1]); err == nil {
				parent[n] = pp
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for c, pp := range parent {
			if pids[pp] && !pids[c] {
				pids[c] = true
				changed = true
			}
		}
	}
	return pids
}

// listenAddrs 从 /proc/net/tcp、tcp6 的内容里找出属于这些 socket 的监听地址。
// 同一个端口监听了全部地址（0.0.0.0 / ::）时只写 *:端口，否则列出各个地址；按端口排序。
func listenAddrs(tables [][]byte, inodes map[string]bool) []string {
	hosts := map[int]map[string]bool{}
	for _, b := range tables {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" || !inodes[f[9]] { // 0A = LISTEN
				continue
			}
			hostHex, portHex, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil {
				continue
			}
			if hosts[int(port)] == nil {
				hosts[int(port)] = map[string]bool{}
			}
			hosts[int(port)][hexHost(hostHex)] = true
		}
	}
	ports := make([]int, 0, len(hosts))
	for p := range hosts {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	var out []string
	for _, p := range ports {
		if hosts[p]["*"] {
			out = append(out, "*:"+strconv.Itoa(p))
			continue
		}
		hs := make([]string, 0, len(hosts[p]))
		for h := range hosts[p] {
			hs = append(hs, h)
		}
		sort.Strings(hs)
		for _, h := range hs {
			out = append(out, h+":"+strconv.Itoa(p))
		}
	}
	return out
}

// hexHost /proc/net/tcp 里的地址（按 32 位小端存放）→ 127.0.0.1、::1；全 0 写成 *。
func hexHost(h string) string {
	raw, err := hex.DecodeString(h)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return h
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		ip[i], ip[i+1], ip[i+2], ip[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	if ip.IsUnspecified() {
		return "*"
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return "[" + ip.String() + "]"
}
