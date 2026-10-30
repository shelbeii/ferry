package build

import (
	"os/exec"
	"strconv"
	"syscall"
)

// cmd.exe 不按 CommandLineToArgv 规则解析参数，直接给出整条命令行。
func cmdExe(command string) *exec.Cmd {
	c := exec.Command("cmd.exe")
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + command + `"`}
	return c
}

func defaultShell(command string) *exec.Cmd { return cmdExe(command) }

func ownGroup(*exec.Cmd) {}

// killTree 结束构建进程及其子进程（cmd.exe 下的 mvn、java 等不会随父进程结束）。
func killTree(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(c.Process.Pid))
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if k.Run() != nil {
		c.Process.Kill()
	}
}
