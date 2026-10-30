//go:build linux

package agent

import "golang.org/x/sys/unix"

// exchangeDirs 原子交换两个路径（renameat2 的 RENAME_EXCHANGE）：不支持的内核或文件系统返回错误。
func exchangeDirs(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
