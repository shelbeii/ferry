//go:build manual

package remote

// 手动测量（不在平时的测试里跑）：经一个每个方向加 30 毫秒延迟的本机代理连 e2e 容器（KEEP=1 跑完 e2e 后），
// 比较大文件上传每条连接同时在路上 1 次写入和 writeAhead 次写入的速度，以及小文件一个个传和同时传。
//
//	go test -tags manual -run UploadBench -v ./internal/remote/

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func benchDelayProxy(t *testing.T, target string, d time.Duration) int {
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
		q := make(chan chunk, 4096)
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

func TestUploadBench(t *testing.T) {
	key := "../../test/e2e/out/id_test"
	if _, err := os.Stat(key); err != nil {
		t.Skip("先 KEEP=1 跑一遍 e2e")
	}
	port := benchDelayProxy(t, "127.0.0.1:2222", 30*time.Millisecond)
	hk, err := ScanHostKey("127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Dial(Options{Host: "127.0.0.1", Port: port, User: "root", IdentityFile: key, OnlyIdentity: true, HostKey: ssh.FingerprintSHA256(hk)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	data := make([]byte, 32<<20)
	rand.Read(data)
	os.WriteFile(big, data, 0o644)
	c.MkdirAll("/tmp/ferry-bench")
	for _, ahead := range []int{1, writeAhead} {
		old := writeAhead
		writeAhead = ahead
		start := time.Now()
		err := UploadParallel(context.Background(), c, nil, []UploadItem{{Local: big, Remote: "/tmp/ferry-bench/big", Mode: 0o644, Size: int64(len(data))}}, nil, nil)
		writeAhead = old
		if err != nil {
			t.Fatal(err)
		}
		d := time.Since(start)
		t.Logf("大文件 32 MB，往返 60 毫秒，同时在路上 %d 次写入：%v（%.1f MB/s）", ahead, d.Round(time.Millisecond), 32/d.Seconds())
	}
	// 小文件：100 个 4 KB
	var small []string
	for i := range 100 {
		p := filepath.Join(dir, fmt.Sprint("s", i))
		os.WriteFile(p, data[:4096], 0o644)
		small = append(small, p)
	}
	start := time.Now()
	for i, p := range small {
		if err := c.UploadContext(context.Background(), p, fmt.Sprintf("/tmp/ferry-bench/s%d", i), 0o644, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("100 个 4 KB 小文件一个个传：%v", time.Since(start).Round(time.Millisecond))
	start = time.Now()
	sem := make(chan struct{}, 8)
	errs := make(chan error, len(small))
	for i, p := range small {
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			errs <- c.UploadContext(context.Background(), p, fmt.Sprintf("/tmp/ferry-bench/s%d", i), 0o644, nil)
		}()
	}
	for range small {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("100 个 4 KB 小文件同时传 8 个：%v", time.Since(start).Round(time.Millisecond))
}
