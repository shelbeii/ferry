package engine

import (
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeIPAddr(t *testing.T) {
	for raw, want := range map[string]string{
		"172.22.18.28":         "172.22.18.28",
		"::ffff:172.22.18.28":  "172.22.18.28",
		"[::ffff:192.168.1.7]": "192.168.1.7",
		"[fe80::1%eth0]":       "fe80::1",
		"127.0.0.1":            "127.0.0.1",
	} {
		addr, ok := normalizeIPAddr(raw)
		if !ok || addr.String() != want {
			t.Errorf("normalizeIPAddr(%q) = (%v, %v)，期望 %q", raw, addr, ok, want)
		}
	}
}

func TestFormatLocalProcOrigin(t *testing.T) {
	l, d := formatLocalProcOrigin(localTCPProc{PID: 42, Name: "ferry.exe", CmdLine: "ferry.exe mcp"}, 55000)
	if l != "本机 · Ferry (MCP)" || !strings.Contains(d, "PID 42") {
		t.Errorf("ferry mcp: label=%q detail=%q", l, d)
	}
	l2, d2 := formatLocalProcOrigin(localTCPProc{PID: 99, Name: "Xshell.exe"}, 55001)
	if l2 != "本机 · Xshell.exe" || !strings.Contains(d2, "55001") {
		t.Errorf("xshell: label=%q detail=%q", l2, d2)
	}
	l3, _ := formatLocalProcOrigin(localTCPProc{}, 55002)
	if l3 != "本机其他进程" {
		t.Errorf("unknown local proc: %q", l3)
	}
}

// TestLookupLocalTCPLoopback 真实建立一条本机回环 TCP 连接，验证 lookupLocalTCP 能准确反查到当前测试进程。
func TestLookupLocalTCPLoopback(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unsupported OS")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()

	localPort := client.LocalAddr().(*net.TCPAddr).Port
	serverPort := ln.Addr().(*net.TCPAddr).Port

	m, ok := lookupLocalTCP("127.0.0.1", serverPort)
	if !ok {
		t.Fatalf("lookupLocalTCP 返回 ok=false（OS=%s）", runtime.GOOS)
	}
	proc, found := m[localPort]
	if !found {
		t.Fatalf("lookupLocalTCP 未找到本机客户端端口 %d（目标 127.0.0.1:%d），实际结果：%v", localPort, serverPort, m)
	}
	if proc.PID != os.Getpid() {
		t.Errorf("反查到的 PID=%d，期望当前测试进程 PID=%d（proc=%+v）", proc.PID, os.Getpid(), proc)
	}
	if proc.Name == "" {
		t.Errorf("反查到的进程名为空（proc=%+v）", proc)
	}
}
