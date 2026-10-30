package agent

import (
	"errors"
	"os/exec"
)

// agent 只在 Linux 上运行；Windows 上只为能编译、跑单元测试（测试用假系统）。
func (realSystem) RunHook(HookExec, func(string)) error {
	return errors.New("部署钩子只能在 Linux 上执行")
}

func setPgid(*exec.Cmd) {}
