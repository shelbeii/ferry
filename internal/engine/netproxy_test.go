package engine

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 经这台电脑下载的代理：http 代为请求、https 建隧道；没有账号密码的拒绝；连不上的地址记下来
func TestNetProxy(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "plain %s", r.URL.Path)
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "secure %s", r.URL.Path)
	}))
	defer secure.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0") // 代替服务器上经 SSH 转发的端口
	if err != nil {
		t.Fatal(err)
	}
	p := serveNetProxy(ln)
	defer p.Close()
	if !strings.HasPrefix(p.URL, "http://ferry:") || !strings.Contains(p.URL, fmt.Sprintf("@127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)) {
		t.Fatalf("代理地址：%s", p.URL)
	}
	pu, _ := url.Parse(p.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	get := func(c *http.Client, u string) (int, string) {
		t.Helper()
		resp, err := c.Get(u)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for i := 0; i < 2; i++ { // 同一条连接上接着请求
		if code, body := get(client, plain.URL+"/repodata/repomd.xml"); code != 200 || body != "plain /repodata/repomd.xml" {
			t.Fatalf("http：%d %s", code, body)
		}
	}
	if code, body := get(client, secure.URL+"/epel/7/redis.rpm"); code != 200 || body != "secure /epel/7/redis.rpm" {
		t.Fatalf("https：%d %s", code, body)
	}

	// 密码不对：407
	bad := *pu
	bad.User = url.UserPassword("ferry", "wrong")
	if code, _ := get(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&bad)}}, plain.URL+"/x"); code != http.StatusProxyAuthRequired {
		t.Fatalf("密码不对应该 407：%d", code)
	}

	// 这台电脑也连不上的地址
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := dead.Addr().String()
	dead.Close()
	if code, body := get(client, "http://"+addr+"/x"); code != http.StatusBadGateway || !strings.Contains(body, "这台电脑也连不上") {
		t.Fatalf("连不上应该 502：%d %s", code, body)
	}
	bytes, reqs, failed := p.Summary()
	if bytes == 0 || reqs < 4 || len(failed) != 1 || !strings.HasPrefix(failed[0], addr) {
		t.Fatalf("Summary：%d %d %v", bytes, reqs, failed)
	}
}
