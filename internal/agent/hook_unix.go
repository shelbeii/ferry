//go:build !windows

package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

func (realSystem) RunHook(h HookExec, line func(string)) error {
	cmd := exec.Command("/bin/sh", "-c", h.Cmd)
	cmd.Dir = h.Dir
	cmd.Env = h.Env
	attr := &syscall.SysProcAttr{Setpgid: true} // 自己一组：超时时连同它启动的子进程一起结束
	if h.UID != 0 || h.GID != 0 {
		groups := make([]uint32, 0, len(h.Groups))
		for _, g := range h.Groups {
			groups = append(groups, uint32(g))
		}
		attr.Credential = &syscall.Credential{Uid: uint32(h.UID), Gid: uint32(h.GID), Groups: groups}
	}
	cmd.SysProcAttr = attr
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("启动失败：%v", err)
	}
	pw.Close()

	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line(sc.Text())
		}
	}()

	var timedOut atomic.Bool
	timer := time.AfterFunc(h.Timeout, func() {
		timedOut.Store(true)
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	})
	err = cmd.Wait()
	timer.Stop()
	// 放到后台还开着输出的子进程会一直占着管道：命令结束后最多再读 2 秒
	select {
	case <-read:
	case <-time.After(2 * time.Second):
	}
	pr.Close()
	<-read
	if timedOut.Load() {
		return fmt.Errorf("超时（%s），已结束", humanDur(h.Timeout))
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			return fmt.Errorf("被信号 %v 结束", st.Signal())
		}
		return fmt.Errorf("退出码 %d", ee.ExitCode())
	}
	return err
}

// setPgid 命令自己一组；超时（ctx 结束）时结束整组。
func setPgid(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}
