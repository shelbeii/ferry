package engine

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 经这台电脑下载：服务器连不上软件源时，在服务器上开一个只监听 127.0.0.1 的端口（SSH 反向转发），
// 连进来的请求由这台电脑上的代理代为访问外网。服务器上的 dnf / yum / apt、模板里的 curl 照常工作
// （依赖由它们自己算），只是下载走这台电脑的网络。用一次性的账号密码，安装结束就关掉。

// netProxy 一次安装期间的代理。
type netProxy struct {
	ln    net.Listener
	URL   string // 给服务器用的代理地址（带账号密码）：http://ferry:<密码>@127.0.0.1:<端口>
	auth  string // Proxy-Authorization 的值
	bytes atomic.Int64
	reqs  atomic.Int64

	mu     sync.Mutex
	failed map[string]string // 这台电脑也连不上的地址 → 原因
	conns  map[net.Conn]bool
	ctx    context.Context
	cancel context.CancelFunc
	tr     *http.Transport
}

// listener 能在服务器上监听的连接（*remote.Client）。
type listener interface {
	ListenRemote(addr string) (net.Listener, error)
}

// startNetProxy 在服务器上开代理端口。服务器的 sshd 不允许端口转发时报错。
func startNetProxy(c listener) (*netProxy, error) {
	ln, err := c.ListenRemote("127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("服务器的 sshd 不允许端口转发（sshd_config 里 AllowTcpForwarding no？），没法经这台电脑下载：%w", err)
	}
	return serveNetProxy(ln), nil
}

// serveNetProxy 在 ln 上提供代理服务（ln 的端口就是服务器上的端口）。
func serveNetProxy(ln net.Listener) *netProxy {
	b := make([]byte, 12)
	rand.Read(b)
	pass := hex.EncodeToString(b)
	port := 0
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		port = a.Port
	}
	ctx, cancel := context.WithCancel(context.Background())
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	p := &netProxy{
		ln:     ln,
		URL:    fmt.Sprintf("http://ferry:%s@127.0.0.1:%d", pass, port),
		auth:   "Basic " + base64.StdEncoding.EncodeToString([]byte("ferry:"+pass)),
		failed: map[string]string{},
		conns:  map[net.Conn]bool{},
		ctx:    ctx,
		cancel: cancel,
		tr: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment, // 这台电脑自己要走代理时照样走
			DialContext:           dialer.DialContext,
			ResponseHeaderTimeout: 2 * time.Minute,
			DisableCompression:    true, // 原样转发
			MaxIdleConnsPerHost:   8,
		},
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.track(c, true)
			go func() {
				defer p.track(c, false)
				defer c.Close()
				p.serve(c)
			}()
		}
	}()
	return p
}

func (p *netProxy) track(c net.Conn, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		p.conns[c] = true
	} else {
		delete(p.conns, c)
	}
}

// Close 关掉服务器上的端口和还开着的连接。
func (p *netProxy) Close() {
	p.cancel()
	p.ln.Close()
	p.mu.Lock()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.tr.CloseIdleConnections()
}

// Summary 经这台电脑下载了多少；这台电脑也连不上的地址。
func (p *netProxy) Summary() (bytes, reqs int64, failed []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for host, why := range p.failed {
		failed = append(failed, host+"（"+why+"）")
	}
	sort.Strings(failed)
	return p.bytes.Load(), p.reqs.Load(), failed
}

func (p *netProxy) fail(host string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.failed) < 20 {
		p.failed[host] = shortNetErr(err)
	}
}

// serve 一条连接上的请求：CONNECT（https）建隧道；其余（http）代为请求，连接可以复用。
func (p *netProxy) serve(c net.Conn) {
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Header.Get("Proxy-Authorization") != p.auth {
			io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"ferry\"\r\nContent-Length: 0\r\n\r\n")
			if req.Body != nil {
				io.Copy(io.Discard, req.Body)
			}
			continue
		}
		p.reqs.Add(1)
		if req.Method == http.MethodConnect {
			p.tunnel(c, br, req.Host)
			return
		}
		if !p.forward(c, req) {
			return
		}
	}
}

// tunnel CONNECT：连上目标后原样双向转发（https 的内容这台电脑看不到）。
func (p *netProxy) tunnel(c net.Conn, br *bufio.Reader, host string) {
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}
	d := &net.Dialer{Timeout: 20 * time.Second}
	up, err := d.DialContext(p.ctx, "tcp", host)
	if err != nil {
		p.fail(host, err)
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer up.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(up, br) // 客户端在 CONNECT 后马上发的数据可能已经读进 br 里了
		if tc, ok := up.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(c, up)
		p.bytes.Add(n)
		done <- struct{}{}
	}()
	<-done
	c.SetDeadline(time.Now().Add(5 * time.Second))
	<-done
}

// forward 普通 http 请求：代为请求、原样写回。返回能不能接着在这条连接上读下一个请求。
func (p *netProxy) forward(c net.Conn, req *http.Request) bool {
	if req.URL == nil || req.URL.Host == "" {
		io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		return false
	}
	out := req.Clone(p.ctx)
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		p.fail(req.URL.Host, err)
		msg := "这台电脑也连不上 " + req.URL.Host + "：" + shortNetErr(err)
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s", len(msg), msg)
		return !req.Close
	}
	defer resp.Body.Close()
	resp.Close = req.Close
	cw := &countWriter{w: c}
	err = resp.Write(cw)
	p.bytes.Add(cw.n)
	return err == nil && !req.Close && !resp.Close
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// shortNetErr 网络错误的简短说法。
func shortNetErr(err error) string {
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns):
		return "域名解析失败"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout"):
		return "超时"
	case strings.Contains(err.Error(), "refused"):
		return "连接被拒绝"
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
