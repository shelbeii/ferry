package agent

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/safe"
)

// 软件源探测：从软件源装软件前看服务器自己连不连得上它用的软件源（dnf / yum 的 .repo，apt 的 sources），
// 连不上的经这台电脑下载（见 proto.SoftwarePlan.Proxy）。

var (
	yumRepoDirs = []string{"/etc/yum.repos.d"}
	aptSources  = []string{"/etc/apt/sources.list", "/etc/apt/sources.list.d"}
	probeTime   = 4 * time.Second
)

func (a *Agent) repoprobe() int {
	out := proto.RepoProbe{Hosts: []proto.RepoHost{}}
	for _, m := range []string{"dnf", "yum", "apt-get"} {
		if _, err := a.Sys.Shell("command -v "+m, 10*time.Second); err == nil {
			out.Manager = m
			break
		}
	}
	var urls []string
	switch out.Manager {
	case "dnf", "yum":
		for _, d := range yumRepoDirs {
			files, _ := filepath.Glob(filepath.Join(d, "*.repo"))
			for _, f := range files {
				if b, err := os.ReadFile(f); err == nil {
					urls = append(urls, yumRepoURLs(string(b))...)
				}
			}
		}
	case "apt-get":
		for _, p := range aptSources {
			files := []string{p}
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				files, _ = filepath.Glob(filepath.Join(p, "*"))
			}
			for _, f := range files {
				if b, err := os.ReadFile(f); err == nil {
					urls = append(urls, aptSourceURLs(string(b))...)
				}
			}
		}
	}
	out.Hosts = probeHosts(repoHosts(urls))
	return a.result(proto.CodeOK, "", out)
}

// yumRepoURLs .repo 文件里启用了的软件源地址（baseurl 可以有好几个、跨行写；mirrorlist、metalink）。
func yumRepoURLs(text string) []string {
	var out, cur []string
	enabled := true
	key := ""
	flush := func() {
		if enabled {
			out = append(out, cur...)
		}
		cur, enabled, key = nil, true, ""
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
			continue
		case strings.HasPrefix(line, "["):
			flush()
			continue
		case raw[0] == ' ' || raw[0] == '\t': // 上一个设置的续行
			if key == "baseurl" {
				cur = append(cur, strings.Fields(line)...)
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch key {
		case "enabled":
			enabled = v != "0" && !strings.EqualFold(v, "false") && !strings.EqualFold(v, "no")
		case "baseurl", "mirrorlist", "metalink":
			cur = append(cur, strings.Fields(strings.ReplaceAll(v, ",", " "))...)
		}
	}
	flush()
	return out
}

var reAptOpts = regexp.MustCompile(`\[[^\]]*\]`)

// aptSourceURLs sources.list 的 deb 行、deb822（.sources）的 URIs。
func aptSourceURLs(text string) []string {
	var out []string
	enabled := true
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			enabled = true // deb822 的段落之间空一行
			continue
		}
		if line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.Contains(k, " ") {
			switch strings.ToLower(k) {
			case "enabled":
				enabled = !strings.EqualFold(strings.TrimSpace(v), "no")
			case "uris":
				if enabled {
					out = append(out, strings.Fields(v)...)
				}
			}
			continue
		}
		f := strings.Fields(reAptOpts.ReplaceAllString(line, " "))
		if len(f) >= 2 && (f[0] == "deb" || f[0] == "deb-src") {
			out = append(out, f[1])
		}
	}
	return out
}

// repoHosts 地址里的主机和端口（去重）；本地文件、带变量的主机名跳过。
func repoHosts(urls []string) []proto.RepoHost {
	seen := map[string]bool{}
	var out []proto.RepoHost
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || strings.Contains(u.Host, "$") {
			continue
		}
		port := 0
		switch u.Scheme {
		case "http":
			port = 80
		case "https":
			port = 443
		case "ftp":
			port = 21
		default:
			continue
		}
		if p := u.Port(); p != "" {
			port, _ = strconv.Atoi(p)
		}
		key := u.Scheme + "://" + u.Hostname() + ":" + strconv.Itoa(port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, proto.RepoHost{Scheme: u.Scheme, Host: u.Hostname(), Port: port})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// probeHosts 同时试着访问每个地址（最多等 probeTime）。http(s) 要真的收到 HTTP 回应才算连得上（https 还要证书对）：
// 有的网络里透明代理、防火墙会先接下所有连接，只看端口能不能连上会误判。回 5xx（多半是网关、代理的错误）也算连不上。
func probeHosts(hosts []proto.RepoHost) []proto.RepoHost {
	if len(hosts) > 30 {
		hosts = hosts[:30]
	}
	client := &http.Client{
		Timeout:       probeTime,
		Transport:     &http.Transport{Proxy: nil, TLSHandshakeTimeout: probeTime, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var wg sync.WaitGroup
	for i := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Recover("探测软件源", nil)
			h := &hosts[i]
			addr := net.JoinHostPort(h.Host, strconv.Itoa(h.Port))
			if h.Scheme != "http" && h.Scheme != "https" {
				c, err := net.DialTimeout("tcp", addr, probeTime)
				if err != nil {
					h.Err = probeErr(err)
					return
				}
				c.Close()
				h.OK = true
				return
			}
			resp, err := client.Get(h.Scheme + "://" + addr + "/")
			if err != nil {
				h.Err = probeErr(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				h.Err = "回应 HTTP " + strconv.Itoa(resp.StatusCode)
				return
			}
			h.OK = true
		}()
	}
	wg.Wait()
	if hosts == nil {
		hosts = []proto.RepoHost{}
	}
	return hosts
}

func probeErr(err error) string {
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.As(err, &dns):
		return "域名解析失败"
	case errors.As(err, &ne) && ne.Timeout():
		return "超时"
	case strings.Contains(err.Error(), "refused"):
		return "连接被拒绝"
	case strings.Contains(err.Error(), "unreachable"):
		return "网络不通"
	case strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "x509"):
		return "证书不对（可能被拦截）"
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(10054)) || errors.Is(err, syscall.Errno(10053))):
		// Windows 使用 WSA 错误码，文本不一定包含 reset，不能只按英文字符串判断。
		return "连上就被断开"
	case errors.Is(err, syscall.EPIPE) || strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "reset") ||
		strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "server closed idle connection"):
		// 对方接下连接马上关掉：还没发完请求（broken pipe）或还没收到回应（EOF、closed idle connection），看时机
		return "连上就被断开"
	}
	return err.Error()
}

// proxyEnvPrefix 下载经这台电脑时命令前面加的环境变量；不经代理时为空。
func proxyEnvPrefix(pl *proto.SoftwarePlan) string {
	if pl == nil || !reProxyURL.MatchString(pl.Proxy) {
		return ""
	}
	no := []string{"127.0.0.1", "localhost", "::1"}
	for _, h := range pl.NoProxy {
		if reNoProxyHost.MatchString(h) {
			no = append(no, h)
		}
	}
	p, n := shellQuote(pl.Proxy), shellQuote(strings.Join(no, ","))
	return "export http_proxy=" + p + " https_proxy=" + p + " ftp_proxy=" + p + " HTTP_PROXY=" + p + " HTTPS_PROXY=" + p +
		" no_proxy=" + n + " NO_PROXY=" + n + "; "
}

var (
	reProxyURL    = regexp.MustCompile(`^http://ferry:[0-9a-f]{16,64}@127\.0\.0\.1:[0-9]{1,5}$`)
	reNoProxyHost = regexp.MustCompile(`^[A-Za-z0-9.:_-]{1,253}$`)
)
