package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestServeQuitExitsWhileStdinOpen(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer

	done := make(chan int, 1)
	go func() {
		done <- serve(pr, &out)
	}()

	// 不关闭 stdin，仅发送 op=quit：serve 应立即回执并优雅退出（更新前释放 ferry.exe 文件锁）
	if _, err := io.WriteString(pw, `{"id":"stop-1","op":"quit"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve 退出码 = %d，应为 0", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("发送 op=quit 后 serve 未在 3 秒内退出")
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("输出行数不足：%q", out.String())
	}
	var ack struct {
		ID   string `json:"id"`
		Exit *int   `json:"exit"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ack); err != nil {
		t.Fatalf("解析回执失败：%v（输出 %q）", err, out.String())
	}
	if ack.ID != "stop-1" || ack.Exit == nil || *ack.Exit != 0 {
		t.Fatalf("回执不对：%+v", ack)
	}
}
