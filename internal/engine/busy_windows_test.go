package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/build"
)

// 本机正运行着要构建的 jar（Windows 锁住它，mvn clean 删不掉）：构建前停下，说清楚是哪个进程
func TestBusyArtifactStopsBuild(t *testing.T) {
	f, err := loadYAML(t, `version: 1
project: p
targets:
  t1: {host: 10.0.0.1, root: /opt/p, runAs: u}
builds:
  mvn: {command: "echo 不应执行到这里 > built.txt"}
apps:
  - name: Auth
    type: java
    build: mvn
    artifact: {path: target/auth.jar}
`)
	if err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(f.Dir, "target", "auth.jar")
	os.MkdirAll(filepath.Dir(jar), 0o755)
	os.WriteFile(jar, []byte("jar"), 0o644)
	h, err := os.Open(jar) // 像 java -jar 那样开着它
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	tg, _ := f.Target("t1")
	steps, err := build.Plan(f, tg, f.Apps)
	if err != nil || len(steps) != 1 {
		t.Fatal(err)
	}
	e := &Engine{File: f, Rep: &recRep{}}
	err = e.checkBusyArtifacts(tg, steps[0])
	if err == nil || !strings.Contains(err.Error(), "Auth：") || !strings.Contains(err.Error(), fmt.Sprintf("进程号 %d", os.Getpid())) ||
		!strings.Contains(err.Error(), "先停掉它们再部署") {
		t.Fatalf("应说明是哪个进程占着：%v", err)
	}
	h.Close()
	if err := e.checkBusyArtifacts(tg, steps[0]); err != nil {
		t.Fatalf("关掉后应能构建：%v", err)
	}
}

// 弹窗里「结束进程」：只能结束正占着产物的进程；结束后不再占用
func TestKillBusy(t *testing.T) {
	f, err := loadYAML(t, `version: 1
project: p
targets:
  t1: {host: 10.0.0.1, root: /opt/p, runAs: u}
builds:
  mvn: {command: "echo x"}
apps:
  - name: Auth
    type: java
    build: mvn
    artifact: {path: target/auth.jar}
`)
	if err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(f.Dir, "target", "auth.jar")
	os.MkdirAll(filepath.Dir(jar), 0o755)
	os.WriteFile(jar, []byte("jar"), 0o644)
	// 另一个进程开着它（不许删除），像 java -jar 一样
	ps := exec.Command("powershell.exe", "-NoProfile", "-Command",
		"$f=[IO.File]::Open('"+jar+"','Open','Read','Read'); Start-Sleep 60")
	if err := ps.Start(); err != nil {
		t.Skip("没有 powershell：", err)
	}
	defer ps.Process.Kill()
	e := &Engine{File: f, Rep: &recRep{}}
	var items []BusyItem
	for i := 0; i < 50; i++ {
		if items, _ = e.BusyArtifacts("t1", nil); len(items) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(items) != 1 || items[0].App != "Auth" || len(items[0].Holders) != 1 || items[0].Holders[0].PID != ps.Process.Pid {
		t.Fatalf("应查到占着的进程 %d：%+v", ps.Process.Pid, items)
	}
	if !strings.Contains(strings.ToLower(items[0].Holders[0].Cmd), "powershell") {
		t.Errorf("应带上命令行：%q", items[0].Holders[0].Cmd)
	}
	if _, err := e.KillBusy("t1", nil, os.Getpid()); err == nil || !strings.Contains(err.Error(), "不能由 Ferry 结束") {
		t.Fatalf("没占着产物的进程不能结束：%v", err)
	}
	left, err := e.KillBusy("t1", nil, ps.Process.Pid)
	if err != nil || len(left) != 0 {
		t.Fatalf("结束后不应再占用：%v %+v", err, left)
	}
}
