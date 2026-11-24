package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/agentbin"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/dbnative"
	"github.com/captain/ferry/internal/swpkg"
)

const (
	mysqlShellModernVersion = "8.4.10"
	mysqlShellLegacyVersion = "8.0.42"
)

var reGlibcVersion = regexp.MustCompile(`(\d+)\.(\d+)\s*$`)

// parseGlibcVersion 从 `ldd --version` 的第一行提取 glibc 主次版本号。
func parseGlibcVersion(line string) (major, minor int, ok bool) {
	m := reGlibcVersion.FindStringSubmatch(strings.TrimSpace(line))
	if len(m) != 3 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(m[1])
	min, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// mysqlShellPackage 根据目标服务器的 CPU 架构和 glibc 版本选择 MySQL Shell 官方通用压缩包及候选下载地址。
func mysqlShellPackage(rawArch, lddLine, mysqlMirror string) (file string, urls []string, err error) {
	var arch string
	switch strings.ToLower(strings.TrimSpace(rawArch)) {
	case "x86_64", "amd64":
		arch = "x86-64bit"
	case "aarch64", "arm64":
		arch = "arm-64bit"
	default:
		return "", nil, fmt.Errorf("MySQL Shell 官方通用包暂不支持架构 %q", strings.TrimSpace(rawArch))
	}
	ver, glibc := mysqlShellModernVersion, "glibc2.28"
	if maj, min, ok := parseGlibcVersion(lddLine); ok {
		if maj < 2 || (maj == 2 && min < 17) {
			return "", nil, fmt.Errorf("执行服务器 glibc 版本过旧（%d.%d），无法运行 MySQL Shell 官方通用包", maj, min)
		}
		if maj == 2 && min < 28 {
			ver, glibc = mysqlShellLegacyVersion, "glibc2.17"
		}
	}
	file = fmt.Sprintf("mysql-shell-%s-linux-%s-%s.tar.gz", ver, glibc, arch)
	official := []string{
		"https://cdn.mysql.com/Downloads/MySQL-Shell/" + file,
		"https://cdn.mysql.com/archives/mysql-shell/" + file,
	}
	src := swpkg.Sources{MySQL: mysqlMirror}
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	if mysqlMirror != "" {
		for _, u := range official {
			add(src.Rewrite(u))
		}
	}
	for _, u := range official {
		add(u)
	}
	return file, urls, nil
}

// installMySQLShellBinary 路线 A：在本机下载（并缓存）MySQL 官方通用二进制包，上传解压到执行服务器的 Ferry 托管目录。
// 无需依赖 openEuler/CentOS 等发行版软件源，服务器完全断网也可安装。
func (e *Engine) installMySQLShellBinary(target string) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	s, err := e.connectSSH(t)
	if err != nil {
		return err
	}
	defer s.close()

	ctx := e.ctx()
	e.Rep.Step("检查执行服务器架构与系统库版本")
	var probeLines []string
	res, err := s.c.RunContext(ctx, "uname -m && (ldd --version 2>&1 | head -n 1 || true)", nil, func(line string) {
		probeLines = append(probeLines, line)
	})
	if err != nil || res.Code != 0 || len(probeLines) == 0 {
		return fmt.Errorf("无法获取执行服务器的架构信息：%v %s", err, strings.TrimSpace(res.Stderr))
	}
	rawArch := strings.TrimSpace(probeLines[0])
	lddLine := ""
	if len(probeLines) > 1 {
		lddLine = strings.TrimSpace(probeLines[1])
	}
	file, urls, err := mysqlShellPackage(rawArch, lddLine, e.Settings.Runtime.MySQLSource)
	if err != nil {
		return err
	}

	cacheDir := appdir.SubDir("dbtools")
	localPath := filepath.Join(cacheDir, file)
	if cachedGzipOK(localPath) {
		e.Rep.Step("复用本机已缓存的 %s", file)
	} else {
		e.Rep.Step("在本机下载 %s（支持复用 MySQL 镜像源）", file)
		if err := e.downloadDBTool(ctx, urls, localPath, file); err != nil {
			return err
		}
	}

	var nonce [6]byte
	_, _ = rand.Read(nonce[:])
	remoteTmp := fmt.Sprintf("/tmp/ferry-mysqlsh-%s.tar.gz", hex.EncodeToString(nonce[:]))
	e.Rep.Step("上传 %s 到执行服务器", file)
	if err := s.c.Upload(localPath, remoteTmp, 0o600, func(done, total int64) {
		e.Rep.Progress("上传 "+file, done, total)
	}); err != nil {
		return fmt.Errorf("上传 MySQL Shell 到执行服务器失败：%w", err)
	}

	usePriv := t.User == "root" || s.allowsSudo()
	installDir := dbnative.ManagedToolsRoot + "/mysql-shell"
	if !usePriv {
		installDir = `$HOME/.ferry/tools/mysql-shell`
	}
	script := fmt.Sprintf(`set -eu
tmp=%s
dest="%s"
parent=$(dirname "$dest")
staging="${dest}.new.$$"
cleanup() { rm -rf -- "$tmp" "$staging"; }
trap cleanup EXIT HUP INT TERM
mkdir -p "$parent" "$staging"
tar -xzf "$tmp" --strip-components=1 -C "$staging"
if [ ! -x "$staging/bin/mysqlsh" ]; then
  echo "压缩包中未找到 bin/mysqlsh" >&2
  exit 1
fi
"$staging/bin/mysqlsh" --version
rm -rf -- "$dest"
mv "$staging" "$dest"
`, shQuote(remoteTmp), installDir)

	e.Rep.Step("在执行服务器解压并校验 MySQL Shell")
	var outLines []string
	onLine := func(line string) {
		outLines = append(outLines, line)
		e.Rep.Info("%s", line)
	}
	if usePriv {
		res, err = s.runPriv(ctx, "sh -ec "+shQuote(script), nil, onLine)
	} else {
		res, err = s.c.RunContext(ctx, "sh -ec "+shQuote(script), nil, onLine)
	}
	if err != nil {
		return fmt.Errorf("安装 MySQL Shell 失败：%w", err)
	}
	if res.Code != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" && len(outLines) > 0 {
			detail = strings.Join(outLines[max(0, len(outLines)-5):], "\n")
		}
		return fmt.Errorf("在执行服务器解压或校验 MySQL Shell 失败：%s", detail)
	}
	e.Rep.Info("MySQL Shell 已安装到 %s", installDir)
	return nil
}

func cachedGzipOK(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || st.Size() < 1<<20 {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [2]byte
	_, err = io.ReadFull(f, magic[:])
	return err == nil && magic[0] == 0x1f && magic[1] == 0x8b
}

func (e *Engine) downloadDBTool(ctx context.Context, urls []string, dst, label string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	var errs []string
	for _, u := range urls {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := fetchGzipTo(ctx, u, dst, func(done, total int64) {
			e.Rep.Progress("下载 "+label, done, total)
		})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		errs = append(errs, fmt.Sprintf("%s：%v", u, err))
	}
	return fmt.Errorf("下载 %s 失败（可在「设置 → 运行环境」配置 MySQL 镜像源）：%s", label, strings.Join(errs, "；"))
}

func fetchGzipTo(ctx context.Context, u, dst string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ferry")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	part := dst + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	var done int64
	last := time.Time{}
	buf := make([]byte, 256<<10)
	var first2 [2]byte
	haveHeader := 0
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if haveHeader < 2 {
				copied := copy(first2[haveHeader:], buf[:n])
				haveHeader += copied
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(part)
				return werr
			}
			done += int64(n)
			if progress != nil && time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(part)
			return rerr
		}
	}
	if progress != nil {
		progress(done, total)
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	if haveHeader < 2 || first2[0] != 0x1f || first2[1] != 0x8b {
		os.Remove(part)
		return errors.New("下载到的不是有效的 tar.gz 压缩包")
	}
	_ = os.Remove(dst)
	return os.Rename(part, dst)
}

// prepareUserAgent 在普通 SSH 用户（无 root/sudo）的家目录下准备对应架构的 ferry-agent，
// 仅用于以当前用户权限运行服务器端逻辑迁移（不提权、不改系统目录）。
func (e *Engine) prepareUserAgent(s *session) (string, error) {
	ctx := e.ctx()
	var lines []string
	res, err := s.c.RunContext(ctx, "uname -m && printf '%s\n' \"$HOME\"", nil, func(line string) {
		lines = append(lines, strings.TrimSpace(line))
	})
	if err != nil || res.Code != 0 || len(lines) < 2 || lines[1] == "" {
		return "", fmt.Errorf("无法读取执行服务器架构或用户目录：%v %s", err, strings.TrimSpace(res.Stderr))
	}
	var arch string
	switch lines[0] {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("不支持的服务器架构 %s", lines[0])
	}
	bin, err := agentbin.Get(arch)
	if err != nil {
		return "", err
	}
	dir := strings.TrimRight(lines[1], "/") + "/.cache/ferry"
	agentPath := dir + "/ferry-agent-" + bin.SHA256[:12]
	checkCmd := fmt.Sprintf(`if [ -x %[1]s ] && actual=$(sha256sum < %[1]s 2>/dev/null) && [ "${actual%%%% *}" = %[2]s ]; then echo ok; fi`,
		shQuote(agentPath), shQuote(bin.SHA256))
	var okLine string
	if _, err := s.c.RunContext(ctx, checkCmd, nil, func(line string) { okLine = strings.TrimSpace(line) }); err == nil && okLine == "ok" {
		return agentPath, nil
	}
	if _, err := s.c.RunContext(ctx, "mkdir -p "+shQuote(dir)+" && chmod 700 "+shQuote(dir), nil, nil); err != nil {
		return "", fmt.Errorf("创建用户组件目录失败：%w", err)
	}
	tmpLocal, err := os.CreateTemp("", "ferry-user-agent-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmpLocal.Name()
	defer os.Remove(tmpPath)
	if _, err := tmpLocal.Write(bin.Data); err != nil {
		tmpLocal.Close()
		return "", err
	}
	if err := tmpLocal.Close(); err != nil {
		return "", err
	}
	part := agentPath + ".part"
	e.Rep.Step("上传用户态迁移组件（免 sudo）")
	if err := s.c.Upload(tmpPath, part, 0o700, nil); err != nil {
		return "", fmt.Errorf("上传用户态迁移组件失败：%w", err)
	}
	if res, err := s.c.RunContext(ctx, "mv -f "+shQuote(part)+" "+shQuote(agentPath)+" && chmod 700 "+shQuote(agentPath), nil, nil); err != nil || res.Code != 0 {
		return "", fmt.Errorf("就位用户态迁移组件失败：%v %s", err, strings.TrimSpace(res.Stderr))
	}
	return agentPath, nil
}
