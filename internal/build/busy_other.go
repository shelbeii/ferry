//go:build !windows

package build

import "errors"

// FileBusy 只有 Windows 会锁住正在运行的文件；其他系统删得掉。
func FileBusy(string) bool { return false }

// Holder 占着文件的进程。
type Holder struct {
	PID  int
	Name string
}

// FileHolders 只在 Windows 上需要。
func FileHolders(string) []Holder { return nil }

// ProcessCommands 只在 Windows 上需要。
func ProcessCommands([]int) map[int]string { return map[int]string{} }

// KillProcess 只在 Windows 上需要（其他系统不会因为文件占用而构建失败）。
func KillProcess(int) error { return errors.New("只在 Windows 上使用") }
