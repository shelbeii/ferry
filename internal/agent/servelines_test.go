package agent

import (
	"bytes"
	"strings"
	"testing"
)

// 一行输出超过上限：丢掉这一行、发一条提示，后面的行照常输出（不能让内存被一行撑爆）。
func TestServeLinesTooLong(t *testing.T) {
	saved := maxServeLine
	maxServeLine = 32
	defer func() { maxServeLine = saved }()
	var buf bytes.Buffer
	l := &serveLines{id: "r1", out: &serveOut{w: &buf}}
	l.Write([]byte("ok\n"))
	l.Write([]byte(strings.Repeat("x", 100)))
	l.Write([]byte("more\n"))
	l.Write([]byte("after\n"))
	l.flush()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("应当只剩三行：%q", lines)
	}
	if lines[0] != "r1 ok" || lines[2] != "r1 after" {
		t.Fatalf("前后的行受影响：%q", lines)
	}
	if !strings.HasPrefix(lines[1], "r1 {") || !strings.Contains(lines[1], "已丢弃") {
		t.Fatalf("超长行没有换成提示：%q", lines[1])
	}
}

// 超长行没有换行就结束（flush）：也要发提示，不能把半截内容发出去。
func TestServeLinesTooLongFlush(t *testing.T) {
	saved := maxServeLine
	maxServeLine = 16
	defer func() { maxServeLine = saved }()
	var buf bytes.Buffer
	l := &serveLines{id: "r2", out: &serveOut{w: &buf}}
	l.Write([]byte(strings.Repeat("y", 50)))
	l.flush()
	got := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(got, "r2 {") || !strings.Contains(got, "已丢弃") {
		t.Fatalf("%q", got)
	}
}

// 正常输出按行加请求号。
func TestServeLinesNormal(t *testing.T) {
	var buf bytes.Buffer
	l := &serveLines{id: "r3", out: &serveOut{w: &buf}}
	l.Write([]byte("一\n二\n三"))
	l.flush()
	if got := buf.String(); got != "r3 一\nr3 二\nr3 三\n" {
		t.Fatalf("%q", got)
	}
}
