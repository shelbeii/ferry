//go:build !windows

package agent

import (
	"strings"
	"testing"
	"time"
)

// 命令留下后台进程占着输出时不能一直卡住；超时连子进程一起杀；输出太多时只留最后的部分。
func TestRealShell(t *testing.T) {
	var s realSystem
	start := time.Now()
	out, err := s.Shell("sleep 30 & echo hi", 20*time.Second)
	if err != nil || !strings.Contains(out, "hi") || time.Since(start) > 10*time.Second {
		t.Errorf("后台进程：%q %v %s", out, err, time.Since(start))
	}
	start = time.Now()
	if _, err := s.Shell("sleep 30; echo x", time.Second); err == nil || time.Since(start) > 10*time.Second {
		t.Errorf("超时：%v %s", err, time.Since(start))
	}
	out, err = s.Shell("yes aaaaaaaaa | head -c 20000000; echo END", time.Minute)
	if err != nil || len(out) > shellOutputMax+200 || !strings.HasSuffix(strings.TrimSpace(out), "END") || !strings.HasPrefix(out, "（前面") {
		t.Errorf("输出太多：长度 %d %v", len(out), err)
	}
}
