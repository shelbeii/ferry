//go:build !linux

package agent

import "errors"

// exchangeDirs 只有 Linux 有 renameat2；其他系统（测试用）退回到分两步改名。
func exchangeDirs(a, b string) error { return errors.New("不支持原子交换") }
