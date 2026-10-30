package agent

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestYumRepoURLs(t *testing.T) {
	text := `[base]
name=CentOS-$releasever - Base
baseurl=http://mirrors.aliyun.com/centos/$releasever/os/$basearch/
        http://mirrors.aliyuncs.com/centos/$releasever/os/$basearch/
gpgcheck=1

[cr]
enabled=0
baseurl=http://mirror.centos.org/centos/$releasever/cr/$basearch/

[epel]
metalink=https://mirrors.fedoraproject.org/metalink?repo=epel-7&arch=$basearch
enabled = 1

[media]
baseurl=file:///media/CentOS/
mirrorlist=http://mirrorlist.centos.org/?release=$releasever
enabled=false
`
	got := yumRepoURLs(text)
	want := []string{
		"http://mirrors.aliyun.com/centos/$releasever/os/$basearch/",
		"http://mirrors.aliyuncs.com/centos/$releasever/os/$basearch/",
		"https://mirrors.fedoraproject.org/metalink?repo=epel-7&arch=$basearch",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n%v", strings.Join(got, "\n"))
	}
}

func TestAptSourceURLs(t *testing.T) {
	list := `# 注释
deb http://mirrors.tuna.tsinghua.edu.cn/ubuntu/ jammy main restricted
deb [arch=amd64 signed-by=/usr/share/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu jammy stable
deb-src http://archive.ubuntu.com/ubuntu jammy main
`
	sources := `Types: deb
URIs: http://archive.ubuntu.com/ubuntu/ http://security.ubuntu.com/ubuntu/
Suites: noble
Components: main

Types: deb
Enabled: no
URIs: http://ppa.example.com/
`
	got := append(aptSourceURLs(list), aptSourceURLs(sources)...)
	want := []string{"http://mirrors.tuna.tsinghua.edu.cn/ubuntu/", "https://download.docker.com/linux/ubuntu", "http://archive.ubuntu.com/ubuntu",
		"http://archive.ubuntu.com/ubuntu/", "http://security.ubuntu.com/ubuntu/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n%v", strings.Join(got, "\n"))
	}
}

func TestRepoHostsAndProbe(t *testing.T) {
	hosts := repoHosts([]string{"http://a.example/x", "http://a.example/y", "https://a.example/z", "http://b.example:8081/r",
		"file:///media/", "http://$mirror/x", "ftp://f.example/pub"})
	var got []string
	for _, h := range hosts {
		got = append(got, h.Host+":"+strconv.Itoa(h.Port))
	}
	if strings.Join(got, " ") != "a.example:80 a.example:443 b.example:8081 f.example:21" {
		t.Fatalf("%v", got)
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer ok.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer gw.Close()
	// 透明代理那样：连接先接下来，马上断开
	trap, _ := net.Listen("tcp", "127.0.0.1:0")
	defer trap.Close()
	go func() {
		for {
			c, err := trap.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := dead.Addr().(*net.TCPAddr).Port
	dead.Close()
	port := func(u string) int { p, _ := strconv.Atoi(u[strings.LastIndex(u, ":")+1:]); return p }
	r := probeHosts([]proto.RepoHost{
		{Scheme: "http", Host: "127.0.0.1", Port: port(ok.URL)},
		{Scheme: "http", Host: "127.0.0.1", Port: port(gw.URL)},
		{Scheme: "http", Host: "127.0.0.1", Port: trap.Addr().(*net.TCPAddr).Port},
		{Scheme: "http", Host: "127.0.0.1", Port: deadPort},
	})
	if !r[0].OK || r[1].OK || r[2].OK || r[3].OK {
		t.Fatalf("%+v", r)
	}
	if r[1].Err != "回应 HTTP 502" || r[2].Err != "连上就被断开" {
		t.Fatalf("说明：%q %q", r[1].Err, r[2].Err)
	}
}

func TestProxyEnvPrefix(t *testing.T) {
	if proxyEnvPrefix(&proto.SoftwarePlan{}) != "" {
		t.Fatal("不经代理时不加")
	}
	if proxyEnvPrefix(&proto.SoftwarePlan{Proxy: "http://evil.example:3128"}) != "" {
		t.Fatal("只认 Ferry 自己的代理地址（服务器本机、带账号密码）")
	}
	u := "http://ferry:0123456789abcdef01234567@127.0.0.1:41234"
	got := proxyEnvPrefix(&proto.SoftwarePlan{Proxy: u, NoProxy: []string{"mirrors.aliyun.com", "bad host;rm"}})
	for _, w := range []string{"export http_proxy='" + u + "'", "https_proxy='" + u + "'", "no_proxy='127.0.0.1,localhost,::1,mirrors.aliyun.com'"} {
		if !strings.Contains(got, w) {
			t.Errorf("缺少 %s：%s", w, got)
		}
	}
	if strings.Contains(got, "rm") || !strings.HasSuffix(got, "; ") {
		t.Fatalf("%s", got)
	}
}

func TestRepoProbeConnectionResetErrorChain(t *testing.T) {
	codes := []syscall.Errno{syscall.ECONNRESET, syscall.ECONNABORTED}
	if runtime.GOOS == "windows" {
		codes = append(codes, syscall.Errno(10054), syscall.Errno(10053))
	}
	for _, code := range codes {
		err := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("recv", code)}
		if got := probeErr(err); got != "连上就被断开" {
			t.Fatalf("连接重置应按错误码识别（%v）：%s", code, got)
		}
	}
}
