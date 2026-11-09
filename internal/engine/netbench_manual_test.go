//go:build manual

package engine

// 手动测量（不在平时的测试里跑）：经一个给每个方向加 30 毫秒延迟的本机代理连 e2e 容器（KEEP=1 跑完 e2e 后），
// 比较单独运行 agent 和交给常驻 agent 的耗时，并在真的 OpenSSH 上验证会话数开满后另建连接。
//
//	go test -tags manual -run NetBench -v ./internal/engine/

import (
	"context"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
)

// delayProxy 本机端口 → target，每个方向的数据晚 d 到。
func delayProxy(t *testing.T, target string, d time.Duration) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pipe := func(dst, src net.Conn) {
		type chunk struct {
			at time.Time
			b  []byte
		}
		q := make(chan chunk, 1024)
		go func() {
			for c := range q {
				time.Sleep(time.Until(c.at))
				if _, err := dst.Write(c.b); err != nil {
					return
				}
			}
			dst.Close()
		}()
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				q <- chunk{time.Now().Add(d), append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				close(q)
				return
			}
		}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			go pipe(s, c)
			go pipe(c, s)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func TestNetBench(t *testing.T) {
	key := "../../test/e2e/out/id_test"
	if _, err := os.Stat(key); err != nil {
		t.Skip("先 KEEP=1 跑一遍 e2e")
	}
	port := delayProxy(t, "127.0.0.1:2222", 30*time.Millisecond)
	hk, err := remote.ScanHostKey("127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	opts := remote.Options{Host: "127.0.0.1", Port: port, User: "root", IdentityFile: key, OnlyIdentity: true, HostKey: ssh.FingerprintSHA256(hk)}
	start := time.Now()
	c, err := remote.Dial(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cn := &conn{c: c, ferryDir: AgentDir, label: "root@e2e-a"}
	e := &Engine{Rep: &recRep{}}
	if err := e.ensureAgent(cn); err != nil {
		t.Fatal(err)
	}
	t.Logf("新建连接 + 确认 agent：%v", time.Since(start).Round(time.Millisecond))

	start = time.Now()
	c.Ping(5 * time.Second)
	t.Logf("复用前确认连接（省掉的那一次）：%v", time.Since(start).Round(time.Millisecond))

	req := &proto.Request{Proto: proto.Version, Root: "/opt/e2e", FerryDir: AgentDir}
	body := []byte(`{"proto":` + strconv.Itoa(proto.Version) + `,"root":"/opt/e2e"}`)
	var exec, serve []time.Duration
	for i := 0; i < 9; i++ {
		t0 := time.Now()
		if _, err := c.Run(cn.agent+" history", body, func(string) {}); err != nil {
			t.Fatal(err)
		}
		exec = append(exec, time.Since(t0))
	}
	srv, err := startAgentServer(c, cn.agent+" serve")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		t0 := time.Now()
		if _, err := srv.call(context.Background(), "history", req, func(string) {}); err != nil {
			t.Fatal(err)
		}
		serve = append(serve, time.Since(t0))
	}
	t.Logf("一次查看（history）：单独运行 %v，常驻 agent %v", median(exec).Round(time.Millisecond), median(serve).Round(time.Millisecond))

	// 真的 OpenSSH（默认 MaxSessions 10）：开 14 个一直不结束的会话，再执行命令
	var pipes []*remote.Pipe
	for i := 0; i < 14; i++ {
		p, err := c.StartPipe("cat")
		if err != nil {
			t.Fatalf("第 %d 个会话：%v", i+1, err)
		}
		pipes = append(pipes, p)
	}
	out, _, err := c.Output("echo ok")
	if err != nil || out != "ok\n" {
		t.Fatalf("开满后执行命令：%q %v", out, err)
	}
	t.Logf("开了 %d 个会话（加上常驻 agent 共 %d 个）后执行命令正常", len(pipes), len(pipes)+1)
	for _, p := range pipes {
		p.In.Close()
		io.Copy(io.Discard, p.Out)
		p.Close()
	}
}

// 非 root 登录、经 sudo 时常驻的 agent 也能用（e2e-b：deployer + 免密 sudo）。
func TestNetBenchSudo(t *testing.T) {
	key := "../../test/e2e/out/id_test"
	hk, err := remote.ScanHostKey("127.0.0.1", 2223)
	if err != nil {
		t.Skip("先 KEEP=1 跑一遍 e2e")
	}
	c, err := remote.Dial(remote.Options{Host: "127.0.0.1", Port: 2223, User: "deployer", IdentityFile: key, OnlyIdentity: true, HostKey: ssh.FingerprintSHA256(hk)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cn := &conn{c: c, ferryDir: AgentDir, sudo: "sudo -n ", label: "deployer@e2e-b"}
	rep := &recRep{}
	if err := (&Engine{Rep: rep}).ensureAgent(cn); err != nil {
		t.Fatal(err)
	}
	srv := cn.server()
	if srv == nil {
		t.Fatal("经 sudo 启动常驻的 agent 失败")
	}
	var got string
	code, err := srv.call(context.Background(), "history", &proto.Request{Proto: proto.Version, Root: "/opt/e2e"}, func(l string) { got = l })
	if err != nil || code != 0 || !strings.Contains(got, `"t":"result"`) {
		t.Fatalf("经 sudo 的常驻 agent：%d %v %q", code, err, got)
	}
	t.Logf("经 sudo 的常驻 agent 正常；sudo 提示：%v", rep.warns)
}
