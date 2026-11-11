package agent

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func TestHealthHTTPConnectionReuse(t *testing.T) {
	for _, tls := range []bool{false, true} {
		name := "HTTP"
		if tls {
			name = "HTTPS"
		}
		t.Run(name, func(t *testing.T) {
			var connections atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "health.example" {
					t.Errorf("Host = %q", r.Host)
				}
				w.Write([]byte("ready"))
			}))
			srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			if tls {
				srv.StartTLS() // 健康检查仍接受本机服务的自签名证书。
			} else {
				srv.Start()
			}
			t.Cleanup(srv.Close)
			sys := NewSystem()
			const checks = 2000
			for range checks {
				if err := sys.Probe(proto.Health{HTTP: srv.URL, Host: "health.example", Contains: "ready"}); err != nil {
					t.Fatal(err)
				}
			}
			if n := connections.Load(); n != 1 {
				t.Fatalf("%d 次健康检查应复用一条连接，实际建立 %d 条", checks, n)
			}
		})
	}
}

func TestHealthHTTPIdleTimeout(t *testing.T) {
	client := newHealthHTTPClient()
	t.Cleanup(client.CloseIdleConnections)
	tr := client.Transport.(*http.Transport)
	if tr.IdleConnTimeout <= 0 || tr.IdleConnTimeout > 2*time.Minute {
		t.Fatalf("健康检查的空闲连接必须有有限回收期限：%v", tr.IdleConnTimeout)
	}
	// 缩短这个独立客户端的期限，验证自动回收；不修改生产共享客户端。
	tr.IdleConnTimeout = 50 * time.Millisecond
	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	if err := probeHTTP(client, proto.Health{HTTP: srv.URL}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("停止检查后，空闲连接没有自动关闭")
	}
}

func TestHealthHTTPIdleLimits(t *testing.T) {
	t.Run("同一地址的并发检查", func(t *testing.T) {
		client := newHealthHTTPClient()
		t.Cleanup(client.CloseIdleConnections)
		limit := client.Transport.(*http.Transport).MaxIdleConnsPerHost
		if limit <= 0 || limit > 8 {
			t.Fatalf("每地址空闲连接上限不合理：%d", limit)
		}
		n := limit + 4
		arrived := make(chan struct{}, n)
		closed := make(chan struct{}, n)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			arrived <- struct{}{}
			<-release
			w.Write([]byte("ok"))
		}))
		srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				closed <- struct{}{}
			}
		}
		srv.Start()
		t.Cleanup(srv.Close)
		errs := make(chan error, n)
		for range n {
			go func() { errs <- probeHTTP(client, proto.Health{HTTP: srv.URL}) }()
		}
		for range n {
			select {
			case <-arrived:
			case <-time.After(2 * time.Second):
				t.Fatal("检查没有并发到达服务器")
			}
		}
		unblock()
		for range n {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		for range n - limit {
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("并发检查结束后，多余的空闲连接没有关闭")
			}
		}
	})

	t.Run("多个地址", func(t *testing.T) {
		client := newHealthHTTPClient()
		t.Cleanup(client.CloseIdleConnections)
		limit := client.Transport.(*http.Transport).MaxIdleConns
		if limit <= 0 || limit > 128 {
			t.Fatalf("空闲连接总上限不合理：%d", limit)
		}
		n := limit + 2
		closed := make(chan struct{}, n)
		for range n {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte("ok"))
			}))
			srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			srv.Start()
			t.Cleanup(srv.Close)
			if err := probeHTTP(client, proto.Health{HTTP: srv.URL}); err != nil {
				t.Fatal(err)
			}
		}
		for range n - limit {
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("跨地址的空闲连接总数超过上限")
			}
		}
	})
}

func TestHealthHTTPResponseChecks(t *testing.T) {
	var redirected atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/target", http.StatusFound)
		case "/target":
			redirected.Add(1)
		case "/unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		default:
			w.Write([]byte("ready"))
		}
	}))
	t.Cleanup(srv.Close)
	sys := NewSystem()
	for _, tc := range []struct {
		name, path, contains string
		status               []int
		wantError            string
	}{
		{name: "状态码失败", path: "/unavailable", wantError: "返回 503"},
		{name: "自定义状态码", path: "/unavailable", status: []int{503}, contains: "not ready"},
		{name: "内容检查失败", path: "/", contains: "missing", wantError: "响应中没有"},
		{name: "失败后再次成功", path: "/", contains: "ready"},
		{name: "不跟随重定向", path: "/redirect", status: []int{302}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sys.Probe(proto.Health{HTTP: srv.URL + tc.path, Status: tc.status, Contains: tc.contains})
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("预期错误包含 %q，实际 %v", tc.wantError, err)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("健康检查不应跟随重定向")
	}
}
