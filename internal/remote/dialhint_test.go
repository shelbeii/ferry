package remote

import (
	"net"
	"strings"
	"testing"
	"time"
)

// 连不上时的说明：端口上没有服务（拒绝连接）
func TestDialHintRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // 端口空出来：连接会被拒绝
	_, err = net.DialTimeout("tcp", addr, 3*time.Second)
	if err == nil {
		t.Skip("端口又被占用了")
	}
	got := dialError("连接 root@"+addr+" 失败", err).Error()
	if !strings.Contains(got, "拒绝连接") || !strings.Contains(got, "SSH 端口") || !strings.Contains(got, "（dial tcp") {
		t.Fatal(got)
	}
	if dialHint(&net.DNSError{Err: "no such host", Name: "x"}) == "" {
		t.Fatal("找不到地址要有说明")
	}
}
