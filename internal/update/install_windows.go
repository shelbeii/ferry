package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Windows 的进程创建标志
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	breakawayFromJob      = 0x01000000
)

// Install 运行安装包（Ferry 安装程序的更新模式：只显示进度、不卸载旧版本、关掉正在运行的 Ferry；restart 时装好后重新打开 Ferry）。
// 会显式通过 /D=<当前安装目录> 指向当前程序所在目录，避免多副本安装或目录搬迁后装到注册表记的旧位置。
// 安装包单独运行，不随 ferry 退出而结束。
func Install(path string, restart bool) (string, error) {
	exe, _ := os.Executable()
	targetDir := resolveInstallDir(exe)
	cmdLine := windowsInstallerCmdLine(path, targetDir, restart)
	start := func(flags uint32) error {
		cmd := exec.Command(path)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: flags,
			CmdLine:       cmdLine,
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	// 所在的作业不允许脱离时（CreateProcess 报拒绝访问）不带这个标志再试
	if err := start(detachedProcess | createNewProcessGroup | breakawayFromJob); err != nil {
		if err := start(detachedProcess | createNewProcessGroup); err != nil {
			return "", err
		}
	}
	return "安装程序已启动：Ferry 会关闭，装好后自动重新打开", nil
}

// resolveInstallDir 根据当前可执行文件路径推断 Ferry 的安装目录。
// 只有目录下存在 ferry-desktop.exe 或 uninstall.exe 时才视为有效安装目录，
// 避免在开发目录、临时目录或单独复制 ferry.exe 到 go/bin 等路径时错误传给 /D=。
func resolveInstallDir(exePath string) string {
	if exePath == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = real
	}
	dir := filepath.Clean(filepath.Dir(exePath))
	if !isSafeInstallTarget(dir) {
		return ""
	}
	if strings.Contains(strings.ToLower(filepath.ToSlash(dir)), "/go-build") {
		return ""
	}
	if regularFileExists(filepath.Join(dir, "ferry-desktop.exe")) || regularFileExists(filepath.Join(dir, "uninstall.exe")) {
		return dir
	}
	return ""
}

func isSafeInstallTarget(dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) == clean {
		return false // 盘符根目录（如 C:\）不能作为安装目录
	}
	if strings.ContainsAny(clean, "\"\r\n") {
		return false
	}
	return true
}

func regularFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// windowsInstallerCmdLine 构造传给 NSIS 安装程序的完整命令行。
// NSIS 规定 /D= 必须是最后一个参数且路径不能加引号（即使包含空格），否则会被忽略；
// 因此使用 SysProcAttr.CmdLine 原样传递，不让 syscall.EscapeArg 自动加引号。
func windowsInstallerCmdLine(installerPath, targetDir string, restart bool) string {
	var b strings.Builder
	b.WriteString(syscall.EscapeArg(installerPath))
	b.WriteString(" /P /UPDATE")
	if restart {
		b.WriteString(" /R")
	}
	if targetDir != "" {
		clean := filepath.Clean(targetDir)
		if isSafeInstallTarget(clean) {
			b.WriteString(" /D=")
			b.WriteString(clean)
		}
	}
	return b.String()
}
