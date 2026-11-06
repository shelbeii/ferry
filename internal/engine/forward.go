package engine

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/safe"
)

// 经 SSH 转发打开服务器上的网页（「软件」页「更多 → 打开网页」）：这台电脑上开一个只监听 127.0.0.1 的端口，
// 连进来的连接经到服务器的 SSH 连接转到服务器那边的地址，服务器不用在防火墙上放行这个端口。
// 转发开在 ferry serve 里（桌面版开着就一直在），空闲 forwardIdle 后自动关掉；同一个地址再打开时用同一个本机端口，
// 浏览器里的登录状态还在。

// ForwardSpec 转到哪里。
type ForwardSpec struct {
	Target string `json:"target"`
	Host   string `json:"host,omitempty"` // 从服务器上看的地址；空 = 127.0.0.1
	Port   int    `json:"port"`
	// Container 端口只在容器里、没有映射到主机：连接时查容器的 IP
	Container string `json:"container,omitempty"`
	// VHost 网站的域名（nginx 一个端口上有几个网站）：本机端口改成一个小的 HTTP 代理，请求里的 Host 换成它，
	// 跳转地址、Cookie 的域名换回本机的；TLS：服务器那边是 https（本机这边还是 http）
	VHost string `json:"vhost,omitempty"`
	TLS   bool   `json:"tls,omitempty"`
}

// ForwardInfo 一个开着的转发。
type ForwardInfo struct {
	Local  int       `json:"local"` // 本机端口（127.0.0.1）
	Target string    `json:"target"`
	To     string    `json:"to"` // 服务器那边的地址（容器时是「容器 名字:端口」）
	Server string    `json:"server"`
	Config string    `json:"config"`
	Since  time.Time `json:"since"`
	Active int       `json:"active"` // 正在转发的连接
}

const forwardIdle = 30 * time.Minute

var forwards = struct {
	sync.Mutex
	m map[string]*forward
}{m: map[string]*forward{}}

type forward struct {
	key, config string
	spec        ForwardSpec
	ln          net.Listener
	local       int
	since       time.Time
	// open 重新连服务器（到服务器的连接断了时）：用的是不跟随请求取消的 Engine
	open func() (*Tunnel, error)

	reopen sync.Mutex
	mu     sync.Mutex
	tn     *Tunnel
	addr   string // 解析好的服务器那边的地址
	active int
	last   time.Time
	closed bool
}

func (s ForwardSpec) to() string {
	if s.Container != "" {
		return fmt.Sprintf("容器 %s:%d", s.Container, s.Port)
	}
	h := s.Host
	if h == "" {
		h = "127.0.0.1"
	}
	to := net.JoinHostPort(h, strconv.Itoa(s.Port))
	if s.VHost != "" {
		scheme := "http"
		if s.TLS {
			scheme = "https"
		}
		to = scheme + "://" + s.VHost + "（" + to + "）"
	}
	return to
}

// OpenForward 开转发（已经开着的直接返回）：先试着从服务器那边连一次，连不上时报错，不开本机端口。
func (e *Engine) OpenForward(spec ForwardSpec) (*ForwardInfo, error) {
	if spec.Port <= 0 || spec.Port > 65535 {
		return nil, errors.New("端口不对")
	}
	if _, err := e.File.Target(spec.Target); err != nil {
		return nil, err
	}
	key := e.File.Path + "\n" + spec.Target + "\n" + spec.to()
	forwards.Lock()
	f := forwards.m[key]
	forwards.Unlock()
	if f != nil {
		f.mu.Lock()
		ok := !f.closed
		f.last = time.Now()
		f.mu.Unlock()
		if ok {
			c, err := f.dial(e.ctx())
			if err != nil {
				return nil, err
			}
			c.Close()
			return f.info(), nil
		}
	}

	// 之后重连用：不随这次请求取消，不往这次请求输出
	bg := *e
	bg.Ctx, bg.Rep = context.Background(), silent{}
	f = &forward{key: key, config: e.File.Path, spec: spec, since: time.Now(), last: time.Now(),
		open: func() (*Tunnel, error) { return bg.OpenTunnel(spec.Target) }}
	tn, err := e.OpenTunnel(spec.Target) // 第一次用这次请求的 Engine：要密码、核对主机公钥时能问用户
	if err != nil {
		return nil, err
	}
	f.tn = tn
	c, err := f.dial(e.ctx())
	if err != nil {
		tn.Close()
		return nil, err
	}
	c.Close()

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(preferredPort(key))))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		tn.Close()
		return nil, fmt.Errorf("这台电脑上开不了端口：%w", err)
	}
	f.ln, f.local = ln, ln.Addr().(*net.TCPAddr).Port
	forwards.Lock()
	if old := forwards.m[key]; old != nil && old != f {
		go old.close()
	}
	forwards.m[key] = f
	forwards.Unlock()
	if spec.VHost != "" {
		safe.Go("网站转发", f.serveHTTP)
	} else {
		safe.Go("端口转发", f.serve)
	}
	safe.Go("端口转发空闲关闭", f.reap)
	return f.info(), nil
}

// ReachResult 这台电脑连一个域名的端口。
type ReachResult struct {
	IPs []string `json:"ips"`
	OK  bool     `json:"ok"`
	Why string   `json:"why,omitempty"` // resolve（解析不了）| refused | timeout | error
	Msg string   `json:"msg,omitempty"`
}

// Reach 解析域名、连一下端口（用域名直接打开网站前看看行不行）。
func Reach(host string, port int, timeout time.Duration) ReachResult {
	var r ReachResult
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(ips) == 0 {
		r.Why, r.Msg = "resolve", fmt.Sprint(err)
		return r
	}
	r.IPs = ips
	p := probePort(host, port, timeout)
	r.OK, r.Why, r.Msg = p.OK, p.Why, p.Msg
	return r
}

// preferredPort 同一个地址尽量用同一个本机端口（20000～39999）：浏览器按地址记登录状态，换了端口要重新登录。
func preferredPort(key string) int {
	h := fnv.New32a()
	io.WriteString(h, key)
	return 20000 + int(h.Sum32()%20000)
}

// dial 从服务器那边连目标地址；到服务器的连接断了就重连。
func (f *forward) dial(ctx context.Context) (net.Conn, error) {
	f.reopen.Lock() // 断线时同时进来的几个连接只重连一次
	f.mu.Lock()
	tn, addr, closed := f.tn, f.addr, f.closed
	f.mu.Unlock()
	if closed {
		f.reopen.Unlock()
		return nil, errors.New("转发已关闭")
	}
	if tn == nil || !tn.Alive() {
		if tn != nil {
			tn.Close()
		}
		var err error
		if tn, err = f.open(); err != nil {
			f.reopen.Unlock()
			return nil, err
		}
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			f.reopen.Unlock()
			tn.Close()
			return nil, errors.New("转发已关闭")
		}
		f.tn, f.addr, addr = tn, "", ""
		f.mu.Unlock()
	}
	f.reopen.Unlock()
	if addr == "" {
		host := f.spec.Host
		if host == "" {
			host = "127.0.0.1"
		}
		addr = net.JoinHostPort(host, strconv.Itoa(f.spec.Port))
		if f.spec.Container != "" {
			var err error
			if addr, err = tn.ContainerAddr(ctx, f.spec.Container, f.spec.Port); err != nil {
				return nil, err
			}
		}
		f.mu.Lock()
		f.addr = addr
		f.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := tn.Dial(ctx, addr)
	if err != nil {
		f.mu.Lock()
		f.addr = "" // 容器重启后 IP 会变：下次重新查
		f.mu.Unlock()
		if f.spec.Container != "" {
			return nil, fmt.Errorf("容器 %s：%w", f.spec.Container, err)
		}
		return nil, err // Tunnel.Dial 的错误已经说了哪台服务器、哪个地址
	}
	return c, nil
}

func (f *forward) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.active++
		f.last = time.Now()
		f.mu.Unlock()
		safe.Go("端口转发连接", func() {
			defer func() {
				f.mu.Lock()
				f.active--
				f.last = time.Now()
				f.mu.Unlock()
			}()
			defer c.Close()
			r, err := f.dial(context.Background())
			if err != nil {
				return
			}
			defer r.Close()
			done := make(chan struct{}, 2)
			pipe := func(dst, src net.Conn) {
				io.Copy(dst, src)
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
				done <- struct{}{}
			}
			go pipe(r, c)
			go pipe(c, r)
			<-done
			<-done
		})
	}
}

// serveHTTP 按域名转发网站：nginx 按请求里的 Host 选网站，原样转 TCP 的话 Host 是本机的地址，只能看到默认站点。
// 这里收 HTTP 请求，Host 换成网站的域名再经 SSH 发过去（WebSocket 也能用）；
// 网站跳转到自己域名的地址、Cookie 写了域名的，换回本机的地址，不然浏览器会跳走、存不下 Cookie。
func (f *forward) serveHTTP() {
	vhost := f.spec.VHost
	scheme := "http"
	if f.spec.TLS {
		scheme = "https"
	}
	hostPort := vhost
	if (scheme == "http" && f.spec.Port != 80) || (scheme == "https" && f.spec.Port != 443) {
		hostPort = net.JoinHostPort(vhost, strconv.Itoa(f.spec.Port))
	}
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return f.dial(ctx) },
		TLSClientConfig:     &tls.Config{ServerName: vhost, InsecureSkipVerify: true}, // 经 SSH 连服务器本机，证书多半是给域名签的或自签名的
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
	rp := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = scheme, hostPort, hostPort
			if pr.In.Header.Get("Origin") != "" { // 有的网站校验 Origin 和自己的地址一致
				pr.Out.Header.Set("Origin", scheme+"://"+hostPort)
			}
			if r := pr.In.Header.Get("Referer"); r != "" {
				pr.Out.Header.Set("Referer", localToSite(r, pr.In.Host, scheme+"://"+hostPort))
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			lh, _ := resp.Request.Context().Value(localHostKey{}).(string)
			local := "http://" + lh
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", siteToLocal(loc, vhost, local))
			}
			if cs := resp.Header.Values("Set-Cookie"); len(cs) > 0 {
				resp.Header.Del("Set-Cookie")
				for _, c := range cs {
					resp.Header.Add("Set-Cookie", reCookieDomain.ReplaceAllString(c, ""))
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Ferry：经服务器连不上 "+f.spec.to()+"："+err.Error(), http.StatusBadGateway)
		},
	}
	// 浏览器用的本机地址（如 nginx-38717.localhost:38717）：ModifyResponse 里换回地址时用
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), localHostKey{}, r.Host)))
	})
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState: func(_ net.Conn, st http.ConnState) {
			f.mu.Lock()
			defer f.mu.Unlock()
			switch st {
			case http.StateNew:
				f.active++
			case http.StateClosed, http.StateHijacked:
				f.active--
			}
			f.last = time.Now()
		},
	}
	srv.Serve(f.ln)
	tr.CloseIdleConnections()
}

var reCookieDomain = regexp.MustCompile(`(?i);\s*domain=[^;]*`)

// siteToLocal 跳转地址指向网站自己的域名时换成本机的地址（其他网站的不动）。
func siteToLocal(loc, vhost, local string) string {
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Hostname(), vhost) {
		return loc
	}
	l, err := url.Parse(local)
	if err != nil {
		return loc
	}
	u.Scheme, u.Host = l.Scheme, l.Host
	return u.String()
}

// localToSite 本机地址换成网站的地址（Referer）。
func localToSite(ref, localHost, site string) string {
	u, err := url.Parse(ref)
	if err != nil || !strings.EqualFold(u.Host, localHost) {
		return ref
	}
	s, err := url.Parse(site)
	if err != nil {
		return ref
	}
	u.Scheme, u.Host = s.Scheme, s.Host
	return u.String()
}

type localHostKey struct{}

func (f *forward) idle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active == 0 && time.Since(f.last) > forwardIdle
}

func (f *forward) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		f.mu.Lock()
		closed := f.closed
		f.mu.Unlock()
		if closed {
			return
		}
		if f.idle() {
			f.close()
			return
		}
	}
}

func (f *forward) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	tn := f.tn
	f.tn = nil
	f.mu.Unlock()
	if f.ln != nil {
		f.ln.Close()
	}
	if tn != nil {
		tn.Close()
	}
	forwards.Lock()
	if forwards.m[f.key] == f {
		delete(forwards.m, f.key)
	}
	forwards.Unlock()
}

func (f *forward) info() *ForwardInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	server := ""
	if f.tn != nil {
		server = f.tn.Label()
	}
	return &ForwardInfo{Local: f.local, Target: f.spec.Target, To: f.spec.to(), Server: server, Config: f.config, Since: f.since, Active: f.active}
}

// Forwards 开着的转发。
func Forwards() []ForwardInfo {
	forwards.Lock()
	list := make([]*forward, 0, len(forwards.m))
	for _, f := range forwards.m {
		list = append(list, f)
	}
	forwards.Unlock()
	out := make([]ForwardInfo, 0, len(list))
	for _, f := range list {
		out = append(out, *f.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Local < out[j].Local })
	return out
}

// CloseForwards 关掉本机端口是 local 的转发；local 为 0 时全部关掉（ferry serve 退出时）。返回关了几个。
func CloseForwards(local int) int {
	forwards.Lock()
	var list []*forward
	for _, f := range forwards.m {
		if local == 0 || f.local == local {
			list = append(list, f)
		}
	}
	forwards.Unlock()
	for _, f := range list {
		f.close()
	}
	return len(list)
}
