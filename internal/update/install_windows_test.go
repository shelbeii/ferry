package update

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestResolveInstallDir(t *testing.T) {
	t.Run("含 ferry-desktop.exe 的目录", func(t *testing.T) {
		dir := t.TempDir()
		exe := filepath.Join(dir, "ferry.exe")
		os.WriteFile(exe, []byte("cli"), 0o644)
		os.WriteFile(filepath.Join(dir, "ferry-desktop.exe"), []byte("ui"), 0o644)
		if got := resolveInstallDir(exe); got != dir {
			t.Fatalf("resolveInstallDir = %q，应为 %q", got, dir)
		}
	})

	t.Run("含 uninstall.exe 的目录", func(t *testing.T) {
		dir := t.TempDir()
		exe := filepath.Join(dir, "ferry.exe")
		os.WriteFile(exe, []byte("cli"), 0o644)
		os.WriteFile(filepath.Join(dir, "uninstall.exe"), []byte("un"), 0o644)
		if got := resolveInstallDir(exe); got != dir {
			t.Fatalf("resolveInstallDir = %q，应为 %q", got, dir)
		}
	})

	t.Run("只有 ferry.exe 的非安装目录", func(t *testing.T) {
		dir := t.TempDir()
		exe := filepath.Join(dir, "ferry.exe")
		os.WriteFile(exe, []byte("cli"), 0o644)
		if got := resolveInstallDir(exe); got != "" {
			t.Fatalf("非安装目录不应返回路径：%q", got)
		}
	})

	t.Run("go-build 临时目录不视为安装目录", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "go-build123")
		os.MkdirAll(dir, 0o755)
		exe := filepath.Join(dir, "ferry.exe")
		os.WriteFile(exe, []byte("cli"), 0o644)
		os.WriteFile(filepath.Join(dir, "ferry-desktop.exe"), []byte("ui"), 0o644)
		if got := resolveInstallDir(exe); got != "" {
			t.Fatalf("go-build 目录不应返回路径：%q", got)
		}
	})

	t.Run("盘符根目录不安全", func(t *testing.T) {
		if isSafeInstallTarget(`C:\`) {
			t.Fatal("盘符根目录不应视为安全安装目录")
		}
		if isSafeInstallTarget(`relative\Ferry`) {
			t.Fatal("相对路径不应视为安全安装目录")
		}
		if isSafeInstallTarget(`C:\Ferry"bad`) {
			t.Fatal("含引号路径不应视为安全安装目录")
		}
	})
}

func TestWindowsInstallerCmdLine(t *testing.T) {
	t.Run("包含空格的安装包与目标目录", func(t *testing.T) {
		got := windowsInstallerCmdLine(`D:\My Ferry\data\updates\Ferry_0.19.0_x64-setup.exe`, `D:\My Tools\Ferry App`, true)
		want := `"D:\My Ferry\data\updates\Ferry_0.19.0_x64-setup.exe" /P /UPDATE /R /D=D:\My Tools\Ferry App`
		if got != want {
			t.Fatalf("cmdline =\n  %s\n应为:\n  %s", got, want)
		}
		if !strings.HasSuffix(got, `/D=D:\My Tools\Ferry App`) {
			t.Fatalf("/D= 必须是最后一个参数且不带引号：%s", got)
		}
	})

	t.Run("不重启且无目标目录", func(t *testing.T) {
		got := windowsInstallerCmdLine(`C:\updates\setup.exe`, "", false)
		want := `C:\updates\setup.exe /P /UPDATE`
		if got != want {
			t.Fatalf("cmdline = %q，应为 %q", got, want)
		}
	})

	t.Run("不安全的目标目录自动忽略 /D=", func(t *testing.T) {
		got := windowsInstallerCmdLine(`C:\updates\setup.exe`, `C:\`, true)
		if strings.Contains(got, "/D=") {
			t.Fatalf("盘符根目录不应拼入 /D=：%s", got)
		}
	})
}

func TestInspectRunningInCleanupAndLock(t *testing.T) {
	dir := t.TempDir()
	keepExe := filepath.Join(dir, "ferry.exe")
	os.WriteFile(keepExe, []byte("current"), 0o644)
	os.WriteFile(filepath.Join(dir, "ferry.exe.old-111"), []byte("old1"), 0o644)
	os.WriteFile(filepath.Join(dir, "FERRY.EXE.OLD-222"), []byte("old2"), 0o644)
	os.MkdirAll(filepath.Join(dir, "ferry.exe.old-dir"), 0o755)

	// 模拟被外部进程占用（不共享 DELETE 权限，Windows 下无法删除）的旧版二进制
	lockedPath := filepath.Join(dir, "ferry.exe.old-999")
	os.WriteFile(lockedPath, []byte("locked"), 0o644)
	p, err := syscall.UTF16PtrFromString(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)

	state := inspectRunningIn(dir, keepExe, os.Getpid())
	if state.StaleBinaries != 1 {
		t.Fatalf("StaleBinaries = %d，应为 1", state.StaleBinaries)
	}
	if _, err := os.Stat(filepath.Join(dir, "ferry.exe.old-111")); !os.IsNotExist(err) {
		t.Error("未占用的 ferry.exe.old-111 应被删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "FERRY.EXE.OLD-222")); !os.IsNotExist(err) {
		t.Error("未占用的 FERRY.EXE.OLD-222 应被删除")
	}
	if _, err := os.Stat(lockedPath); err != nil {
		t.Error("仍被占用的 ferry.exe.old-999 应保留")
	}
	if _, err := os.Stat(keepExe); err != nil {
		t.Error("正常 ferry.exe 不应被删")
	}
	if _, err := os.Stat(filepath.Join(dir, "ferry.exe.old-dir")); err != nil {
		t.Error("同名目录不应被删")
	}

	// 释放锁后再检查：应自动删掉，StaleBinaries 归零
	syscall.CloseHandle(h)
	state = inspectRunningIn(dir, keepExe, os.Getpid())
	if state.StaleBinaries != 0 {
		t.Fatalf("释放锁后 StaleBinaries = %d，应为 0", state.StaleBinaries)
	}
	if _, err := os.Stat(lockedPath); !os.IsNotExist(err) {
		t.Error("释放锁后的 ferry.exe.old-999 应被清理")
	}
}
