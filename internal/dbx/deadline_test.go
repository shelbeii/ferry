package dbx

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// noDeadline 像经 SSH 转发的连接：不支持读写超时。
type noDeadline struct{ net.Conn }

func (noDeadline) SetDeadline(time.Time) error {
	return errors.New("ssh: tcpChan: deadline not supported")
}
func (noDeadline) SetReadDeadline(time.Time) error {
	return errors.New("ssh: tcpChan: deadline not supported")
}
func (noDeadline) SetWriteDeadline(time.Time) error {
	return errors.New("ssh: tcpChan: deadline not supported")
}

func TestWithDeadlines(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 5)
		n, _ := c.Read(buf)
		c.Write(buf[:n]) // 回一次，之后不说话：看读超时
		time.Sleep(2 * time.Second)
		c.Close()
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := withDeadlines(noDeadline{raw})
	if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("接过之后应该能设超时：%v", err)
	}
	if c.RemoteAddr().String() != ln.Addr().String() {
		t.Errorf("地址：%v", c.RemoteAddr())
	}
	c.Write([]byte("hello"))
	buf := make([]byte, 5)
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("读到 %q %v", buf[:n], err)
	}
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	start := time.Now()
	_, err = c.Read(buf)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || time.Since(start) > time.Second {
		t.Fatalf("应该读超时：%v（%v）", err, time.Since(start))
	}
	c.Close()
	// 本来就支持超时的连接原样返回
	raw2, _ := net.Dial("tcp", ln.Addr().String())
	if raw2 != nil {
		if withDeadlines(raw2) != raw2 {
			t.Error("支持超时的连接不该再包一层")
		}
		raw2.Close()
	}
}

// sshLikeTunnel 经「服务器」连：直接连本机，但连接不支持超时（和 SSH 转发一样）。
type sshLikeTunnel struct{}

func (sshLikeTunnel) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return noDeadline{c}, nil
}
func (sshLikeTunnel) Alive() bool { return true }
func (sshLikeTunnel) Close()      {}
func (sshLikeTunnel) ContainerAddr(context.Context, string, int) (string, error) {
	return "", errors.New("不支持")
}

// 经 SSH 转发连 Redis（以前报 ssh: tcpChan: deadline not supported）：
//
//	FERRY_REDIS_LIVE=127.0.0.1:63791 FERRY_REDIS_PASSWORD=… go test ./internal/dbx -run RedisViaTunnel
func TestRedisViaTunnelLive(t *testing.T) {
	addr := os.Getenv("FERRY_REDIS_LIVE")
	if addr == "" {
		t.Skip("没有设置 FERRY_REDIS_LIVE")
	}
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	c := &Conn{ID: "live-redis-tn", Name: "redis", Kind: Redis, Host: host, Port: pn, Via: &Via{Config: "x", Target: "t"}}
	m := &Manager{clients: map[string]*Client{}}
	cl, release, err := m.Open(context.Background(), c, os.Getenv("FERRY_REDIS_PASSWORD"), func(*Via) (Tunnel, error) { return sshLikeTunnel{}, nil })
	if err != nil {
		t.Fatalf("经转发连不上：%v", err)
	}
	defer release()
	defer m.Close("")
	if _, err := cl.RedisKeys(context.Background(), "0", RedisScan{}); err != nil {
		t.Fatalf("经转发列键：%v", err)
	}
}
