package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// stuckSSHD 握手、登录都正常，之后什么都不回：不答应开会话，也不回保活（服务器卡住、网络只进不出时就是这样）。
func stuckSSHD(t *testing.T) (*net.TCPAddr, ssh.PublicKey) {
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
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { nc.Close() })
			go ssh.NewServerConn(nc, cfg) // 握手完以后不读 chans、reqs
		}
	}()
	return ln.Addr().(*net.TCPAddr), signer.PublicKey()
}

func setHandshakeTimeout(t *testing.T, d time.Duration) {
	old := handshakeTimeout
	handshakeTimeout = d
	t.Cleanup(func() { handshakeTimeout = old })
}

// TCP 连上了，服务器却不回 SSH 的问候：握手限时，不会一直等。
func TestDialHandshakeTimeout(t *testing.T) {
	setHandshakeTimeout(t, 200*time.Millisecond)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			defer nc.Close() // 接受了 TCP，什么都不发
		}
	}()
	start := time.Now()
	_, err = Dial(Options{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, User: "u", Auth: "password",
		Password: func() (string, error) { return "p", nil }, HostKey: "SHA256:x"})
	if err == nil {
		t.Fatal("应该失败")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("握手卡住时等了 %v", d)
	}
}

// 等用户输入密码的时间不算在握手限时里。
func TestDialPasswordPromptNotTimed(t *testing.T) {
	setHandshakeTimeout(t, 200*time.Millisecond)
	d := newFakeSSHD(t, 10)
	c, err := Dial(Options{Host: "127.0.0.1", Port: d.addr.Port, User: "u", Auth: "password",
		Password: func() (string, error) { time.Sleep(600 * time.Millisecond); return "p", nil }, HostKey: ssh.FingerprintSHA256(d.key)})
	if err != nil {
		t.Fatalf("输入密码慢了不该算超时：%v", err)
	}
	defer c.Close()
	// 握手完成后不再限时：过了期限照样能用
	time.Sleep(300 * time.Millisecond)
	if out, _, err := c.Output("echo"); err != nil || out != "ok\n" {
		t.Fatalf("握手后的连接：%q %v", out, err)
	}
}

// 服务器不答应开会话时，取消马上生效。
func TestRunContextCancelWhileOpening(t *testing.T) {
	addr, key := stuckSSHD(t)
	c, err := Dial(Options{Host: "127.0.0.1", Port: addr.Port, User: "u", Auth: "password",
		Password: func() (string, error) { return "p", nil }, HostKey: ssh.FingerprintSHA256(key)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.RunContext(ctx, "true", nil, nil); !errors.Is(err, ErrCanceled) {
		t.Fatalf("应该是已取消：%v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("取消后等了 %v", d)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := c.StartPipeContext(ctx2, "cat"); !errors.Is(err, ErrCanceled) {
		t.Fatalf("StartPipeContext 应该是已取消：%v", err)
	}
}

// 服务器不回保活时，反复量延迟只有一个请求在等，不会每量一次多一个。
func TestLatencySingleFlight(t *testing.T) {
	addr, key := stuckSSHD(t)
	c, err := Dial(Options{Host: "127.0.0.1", Port: addr.Port, User: "u", Auth: "password",
		Password: func() (string, error) { return "p", nil }, HostKey: ssh.FingerprintSHA256(key)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Latency(20*time.Millisecond, 0); err != ErrSlow {
		t.Fatalf("应该是 ErrSlow：%v", err)
	}
	c.probeMu.Lock()
	first := c.probe
	c.probeMu.Unlock()
	if first == nil {
		t.Fatal("应该有一个正在进行的测量")
	}
	for range 8 {
		if _, err := c.Latency(10*time.Millisecond, 0); err != ErrSlow {
			t.Fatalf("应该是 ErrSlow：%v", err)
		}
	}
	c.probeMu.Lock()
	same := c.probe == first
	c.probeMu.Unlock()
	if !same {
		t.Fatal("正在量的时候不该再发新的测量")
	}
	if !c.Alive() {
		t.Fatal("慢不等于断了")
	}
}
