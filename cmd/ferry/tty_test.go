package main

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUTF8Complete(t *testing.T) {
	zh := []byte("中文") // 每个字 3 字节
	cases := []struct {
		b    []byte
		want int
	}{
		{[]byte("abc"), 3},
		{zh, 6},
		{zh[:5], 3},
		{zh[:4], 3},
		{zh[:3], 3},
		{[]byte{0xff, 0xfe}, 2}, // 不是 UTF-8：原样发
		{nil, 0},
	}
	for _, c := range cases {
		if got := utf8Complete(c.b); got != c.want {
			t.Errorf("utf8Complete(%v) = %d，期望 %d", c.b, got, c.want)
		}
	}
}

type collect struct {
	mu  sync.Mutex
	out []string
}

func (c *collect) add(s string) { c.mu.Lock(); c.out = append(c.out, s); c.mu.Unlock() }
func (c *collect) all() string  { c.mu.Lock(); defer c.mu.Unlock(); return strings.Join(c.out, "") }
func (c *collect) n() int       { c.mu.Lock(); defer c.mu.Unlock(); return len(c.out) }

func TestTTYOutputBatchesAndKeepsRunesWhole(t *testing.T) {
	var c collect
	o := newTTYOutput(c.add)
	zh := []byte("你好，世界")
	o.Write(zh[:4]) // 「好」被切开
	time.Sleep(5 * ttyWait)
	if got := c.all(); got != "你" {
		t.Fatalf("第一批 %q，期望只有完整的「你」", got)
	}
	o.Write(zh[4:])
	o.Close()
	if got := c.all(); got != "你好，世界" {
		t.Fatalf("合起来是 %q", got)
	}
	for _, s := range c.out {
		if strings.ContainsRune(s, '�') {
			t.Fatalf("有被切坏的字符：%q", s)
		}
	}
}

func TestTTYOutputFlushesWhenFull(t *testing.T) {
	var c collect
	o := newTTYOutput(c.add)
	o.Write([]byte(strings.Repeat("x", ttyMax)))
	if c.n() != 1 {
		t.Fatalf("攒满后应立即发出，实际发了 %d 批", c.n())
	}
	o.Close()
}

func TestTTYOutputPause(t *testing.T) {
	var c collect
	o := newTTYOutput(c.add)
	o.setPaused(true)
	done := make(chan struct{})
	go func() {
		o.Write([]byte("abc"))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("暂停时 Write 应该等着")
	case <-time.After(50 * time.Millisecond):
	}
	o.setPaused(false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("继续后 Write 没有返回")
	}
	// 暂停中关闭：Write 不再等
	o.setPaused(true)
	done = make(chan struct{})
	go func() {
		o.Write([]byte("def"))
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	o.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("关闭后 Write 仍在等")
	}
	if got := c.all(); got != "abc" {
		t.Fatalf("输出 %q", got)
	}
}

func TestTTYInput(t *testing.T) {
	in := newTTYInput()
	in.push([]byte("ls"))
	in.push(nil)
	in.push([]byte("\r"))
	buf := make([]byte, 16)
	n, err := in.Read(buf)
	if err != nil || string(buf[:n]) != "ls\r" {
		t.Fatalf("读到 %q %v", buf[:n], err)
	}
	got := make(chan error, 1)
	go func() {
		_, err := in.Read(buf)
		got <- err
	}()
	time.Sleep(20 * time.Millisecond)
	in.Close()
	select {
	case err := <-got:
		if err != io.EOF {
			t.Fatalf("关闭后应读到 EOF，实际 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("关闭后 Read 没有返回")
	}
	in.push([]byte("x")) // 关闭后的输入丢弃，不出错
}

func TestTTYSetSizeKeepsLatest(t *testing.T) {
	tt := newTTY(func(string) {})
	tt.setSize(80, 24)
	tt.setSize(120, 30)
	if sz := <-tt.resize; sz != [2]int{120, 30} {
		t.Fatalf("窗口大小 %v", sz)
	}
}

// 安静了一阵之后的输出（打字的回显）马上发出；紧接着来的攒一会儿一起发
func TestTTYOutputEchoNotDelayed(t *testing.T) {
	var c collect
	o := newTTYOutput(c.add)
	o.Write([]byte("a"))
	if c.all() != "a" {
		t.Fatalf("第一个字应马上发出，实际 %q", c.all())
	}
	o.Write([]byte("b"))
	o.Write([]byte("c"))
	if c.n() != 1 {
		t.Fatalf("紧接着来的应攒着，实际已发 %d 批", c.n())
	}
	time.Sleep(5 * ttyWait)
	if c.all() != "abc" || c.n() != 2 {
		t.Fatalf("攒的应一起发出：%q，%d 批", c.all(), c.n())
	}
	time.Sleep(2 * ttyWait)
	o.Write([]byte("d"))
	if c.all() != "abcd" {
		t.Fatalf("又安静了一阵，应马上发出：%q", c.all())
	}
	o.Close()
}
