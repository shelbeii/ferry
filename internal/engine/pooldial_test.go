package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/captain/ferry/internal/config"
)

// 服务器连不上时，同时等着连同一台服务器的几个请求共用这一次失败，不再一个接一个各连一次、各等一次。
func TestPoolSharesDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() { time.Sleep(300 * time.Millisecond); nc.Close() }() // 接受了 TCP，一会儿后断开（握手失败）
		}
	}()
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "id")
	os.WriteFile(key, pem.EncodeToMemory(block), 0o600)
	p := NewPool(0)
	defer p.Close()
	f := &config.File{Path: filepath.Join(dir, "ferry.yaml")}
	tg := &config.Target{Name: "t", Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, User: "root",
		IdentityFile: key, HostKey: "SHA256:x", Plain: true}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	start := time.Now()
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = p.Acquire(&Engine{File: f, Pool: p, Rep: &recRep{}}, tg)
		}()
		if i == 0 {
			time.Sleep(50 * time.Millisecond) // 第一个先开始连
		}
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("第 %d 个请求应该失败", i+1)
		}
	}
	if n := accepted.Load(); n != 1 {
		t.Fatalf("应该只连一次，实际连了 %d 次", n)
	}
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Fatalf("等的请求不该再各连一次：共 %v", d)
	}
}
