//go:build !windows

package build

import (
	"os/exec"
	"syscall"
)

func cmdExe(command string) *exec.Cmd { return exec.Command("sh", "-c", command) }

func defaultShell(command string) *exec.Cmd { return exec.Command("sh", "-c", command) }

// ownGroup 让构建在自己的进程组里运行，取消时整组结束。
func ownGroup(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
}

// killTree 结束构建进程及其子进程。
func killTree(c *exec.Cmd) {
	if c.Process != nil {
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
