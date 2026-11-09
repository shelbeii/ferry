package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeSSHD 本机上的一个 sshd：每条连接最多开 maxSessions 个会话（像 OpenSSH 的 MaxSessions），exec 一律输出 ok。
type fakeSSHD struct {
	addr  *net.TCPAddr
	key   ssh.PublicKey
	conns atomic.Int32 // 建立过的连接数
}

func newFakeSSHD(t *testing.T, maxSessions int) *fakeSSHD {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	d := &fakeSSHD{addr: ln.Addr().(*net.TCPAddr), key: signer.PublicKey()}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			d.conns.Add(1)
			go d.serve(nc, cfg, maxSessions)
		}
	}()
	return d
}

func (d *fakeSSHD) serve(nc net.Conn, cfg *ssh.ServerConfig, maxSessions int) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	var mu sync.Mutex
	open := 0
	for nch := range chans {
		mu.Lock()
		full := open >= maxSessions
		if !full {
			open++
		}
		mu.Unlock()
		if full {
			nch.Reject(ssh.ConnectionFailed, "open failed") // OpenSSH 会话数到了 MaxSessions 时就这样回
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer func() {
				mu.Lock()
				open--
				mu.Unlock()
			}()
			for r := range creqs {
				if r.Type != "exec" {
					r.Reply(false, nil)
					continue
				}
				r.Reply(true, nil)
				ch.Write([]byte("ok\n"))
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				ch.Close()
			}
		}()
	}
}

func (d *fakeSSHD) dial(t *testing.T) *Client {
	t.Helper()
	c, err := Dial(Options{Host: "127.0.0.1", Port: d.addr.Port, User: "u", Auth: "password",
		Password: func() (string, error) { return "p", nil }, HostKey: ssh.FingerprintSHA256(d.key)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// 会话数到了服务器的上限：另建一条连接接着开，不报错；主连接关闭时一起关。
func TestSessionLimitOverflow(t *testing.T) {
	d := newFakeSSHD(t, 2)
	c := d.dial(t)
	var held []*ssh.Session
	for i := 0; i < 5; i++ {
		s, err := c.newSession()
		if err != nil {
			t.Fatalf("第 %d 个会话：%v", i+1, err)
		}
		held = append(held, s)
	}
	if n := d.conns.Load(); n != 3 {
		t.Fatalf("5 个会话、每条最多 2 个：应共 3 条连接，实际 %d", n)
	}
	out, res, err := c.Output("x")
	if err != nil || res.Code != 0 || out != "ok\n" {
		t.Fatalf("开满后执行命令：%q %v %v", out, res, err)
	}
	for _, s := range held {
		s.Close()
	}
	c.Close()
	c.extraMu.Lock()
	defer c.extraMu.Unlock()
	if len(c.extra) != 0 {
		t.Fatalf("主连接关闭后另建的连接应一起关闭")
	}
}

// 另建的连接也有上限：最多 maxExtra 条，再满就报服务器的原错误。
func TestSessionLimitMaxExtra(t *testing.T) {
	d := newFakeSSHD(t, 1)
	c := d.dial(t)
	for i := 0; i < 1+maxExtra; i++ {
		if _, err := c.newSession(); err != nil {
			t.Fatalf("第 %d 个会话：%v", i+1, err)
		}
	}
	if _, err := c.newSession(); err == nil || !isSessionLimit(err) {
		t.Fatalf("超过上限应报会话数上限：%v", err)
	}
	if n := d.conns.Load(); n != int32(1+maxExtra) {
		t.Fatalf("连接数：%d", n)
	}
}
