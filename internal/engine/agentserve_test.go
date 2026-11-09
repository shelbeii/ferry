package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
)

// serveSSHD 本机上的一个 sshd：exec 「… serve」时在进程里跑 agent.Serve，其他命令立刻失败。
// kill 关掉正在运行的 serve 会话（模拟常驻的 agent 退出）。
type serveSSHD struct {
	port  int
	key   ssh.PublicKey
	mu    sync.Mutex
	chs   []ssh.Channel
	execs []string
}

func newServeSSHD(t *testing.T) *serveSSHD {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	d := &serveSSHD{port: ln.Addr().(*net.TCPAddr).Port, key: signer.PublicKey()}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, creqs, err := nch.Accept()
					if err != nil {
						continue
					}
					go d.session(ch, creqs)
				}
			}()
		}
	}()
	return d
}

func (d *serveSSHD) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for r := range reqs {
		if r.Type != "exec" {
			r.Reply(false, nil)
			continue
		}
		var p struct{ Cmd string }
		ssh.Unmarshal(r.Payload, &p)
		r.Reply(true, nil)
		d.mu.Lock()
		d.execs = append(d.execs, p.Cmd)
		d.mu.Unlock()
		go func() {
			code := uint32(127)
			if strings.HasSuffix(p.Cmd, " serve") {
				d.mu.Lock()
				d.chs = append(d.chs, ch)
				d.mu.Unlock()
				code = uint32(agent.Serve(agent.NewSystem(), ch, ch))
			}
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
			ch.Close()
		}()
	}
}

func (d *serveSSHD) kill() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ch := range d.chs {
		ch.Close()
	}
	d.chs = nil
}

func (d *serveSSHD) dial(t *testing.T) *remote.Client {
	t.Helper()
	c, err := remote.Dial(remote.Options{Host: "127.0.0.1", Port: d.port, User: "u", Auth: "password",
		Password: func() (string, error) { return "p", nil }, HostKey: ssh.FingerprintSHA256(d.key)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// 常驻的 agent：同一个会话里同时做几个请求，结果各回各的；退出后报 errServeDown（调用方改为单独运行）。
func TestAgentServerCalls(t *testing.T) {
	d := newServeSSHD(t)
	c := d.dial(t)
	srv, err := startAgentServer(c, "/usr/local/lib/ferry/ferry-agent-x serve")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result *proto.Result
			code, err := srv.call(context.Background(), "history", &proto.Request{Proto: proto.Version, Root: root}, func(line string) {
				var r proto.Result
				if json.Unmarshal([]byte(line), &r) == nil && r.T == "result" {
					result = &r
				}
			})
			if err != nil || code != 0 || result == nil || result.RC != proto.CodeOK {
				errs <- errors.New("请求失败")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := srv.served.Load(); n != 10 {
		t.Fatalf("做完的请求数：%d", n)
	}

	// 取消：马上返回
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.call(ctx, "history", &proto.Request{Proto: proto.Version, Root: root}, func(string) {}); !errors.Is(err, remote.ErrCanceled) {
		t.Fatalf("取消：%v", err)
	}

	d.kill()
	select {
	case <-srv.done:
	case <-time.After(5 * time.Second):
		t.Fatal("常驻的 agent 退出后应能发现")
	}
	if _, err := srv.call(context.Background(), "history", &proto.Request{Proto: proto.Version, Root: root}, func(string) {}); !errors.Is(err, errServeDown) {
		t.Fatalf("退出后：%v", err)
	}
}

// session.call：只读的查看交给常驻的 agent；它启动不了时照原来的办法单独运行；改动服务器的命令始终单独运行。
func TestSessionCallUsesServer(t *testing.T) {
	d := newServeSSHD(t)
	c := d.dial(t)
	s := &session{conn: &conn{c: c, agent: "/opt/ferry/ferry-agent-x", ferryDir: "/opt/ferry"},
		e: &Engine{Rep: &recRep{}, File: &config.File{Project: "p"}}, t: &config.Target{Name: "t", Root: t.TempDir()}, release: func() {}}
	r, err := s.call("history", &proto.Request{})
	if err != nil || r.RC != proto.CodeOK {
		t.Fatalf("经常驻的 agent：%+v %v", r, err)
	}
	s.call("history", &proto.Request{})
	d.mu.Lock()
	execs := append([]string(nil), d.execs...)
	d.mu.Unlock()
	if len(execs) != 1 || execs[0] != "/opt/ferry/ferry-agent-x serve" {
		t.Fatalf("两次查看只应启动一次常驻的 agent：%v", execs)
	}
	// 改动服务器的命令：单独运行（这个假 sshd 上会失败，说明没有交给常驻的）
	if _, err := s.call("control", &proto.Request{}); err == nil {
		t.Fatal("control 不该交给常驻的 agent")
	}
	d.mu.Lock()
	last := d.execs[len(d.execs)-1]
	d.mu.Unlock()
	if last != "/opt/ferry/ferry-agent-x control" {
		t.Fatalf("control 应单独运行：%s", last)
	}
	// 常驻的退出了、又启动不起来时：单独运行
	d.kill()
	<-s.srv.done
	s.srv.served.Store(0)
	if _, err := s.call("history", &proto.Request{}); err == nil {
		t.Fatal("这个假 sshd 上单独运行 history 会失败")
	}
	d.mu.Lock()
	last = d.execs[len(d.execs)-1]
	d.mu.Unlock()
	if last != "/opt/ferry/ferry-agent-x history" {
		t.Fatalf("常驻的用不了时应单独运行：%v", d.execs)
	}
}
