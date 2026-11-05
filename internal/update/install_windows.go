package update

import (
	"os/exec"
	"syscall"
)

// Windows 的进程创建标志
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	breakawayFromJob      = 0x01000000
)

// Install 运行安装包（Ferry 安装程序的更新模式：只显示进度、不卸载旧版本、关掉正在运行的 Ferry；restart 时装好后重新打开 Ferry）。
// 安装包单独运行，不随 ferry 退出而结束。
func Install(path string, restart bool) (string, error) {
	args := []string{"/P", "/UPDATE"}
	if restart {
		args = append(args, "/R")
	}
	start := func(flags uint32) error {
		cmd := exec.Command(path, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
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
