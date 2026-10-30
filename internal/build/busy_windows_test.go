package build

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 正在运行的 jar：别的进程开着它（不允许删除），构建前要能看出来、说出是哪个进程
func TestFileBusy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(p, []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if FileBusy(p) {
		t.Fatal("没人用时不应算占用")
	}
	f, err := os.Open(p) // Go 打开文件时不允许别人删除，和 java 运行 jar 一样
	if err != nil {
		t.Fatal(err)
	}
	if !FileBusy(p) {
		t.Fatal("打开着的文件应算占用")
	}
	found := false
	for _, h := range FileHolders(p) {
		if h.PID == os.Getpid() {
			found = true
		}
	}
	if !found {
		t.Fatalf("应查到占用它的是本进程 %d：%+v", os.Getpid(), FileHolders(p))
	}
	f.Close()
	if FileBusy(p) {
		t.Fatal("关掉后不应再算占用")
	}
	if FileBusy(filepath.Join(t.TempDir(), "none.jar")) {
		t.Fatal("不存在的文件不算占用")
	}
}

// Maven 转述的系统报错是 GBK：转成 UTF-8，不再是乱码
func TestOutputLineGBK(t *testing.T) {
	want := "另一个程序正在使用此文件，进程无法访问。"
	gbk, _ := simplifiedchinese.GBK.NewEncoder().String(want)
	if got := outputLine("[ERROR] " + gbk); got != "[ERROR] "+want {
		t.Fatalf("%q", got)
	}
	if got := outputLine("[INFO] 已经是 UTF-8"); got != "[INFO] 已经是 UTF-8" {
		t.Fatalf("%q", got)
	}
}
