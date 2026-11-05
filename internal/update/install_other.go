//go:build !windows && !darwin

package update

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Install 用下载好的程序替换正在运行的 ferry（先写到旁边，再改名覆盖：正在运行的 ferry 不受影响，下次运行就是新版本）。
func Install(path string, _ bool) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	tmp := exe + ".new"
	if err := copyExec(path, tmp); err != nil {
		os.Remove(tmp)
		if os.IsPermission(err) {
			return "", fmt.Errorf("没有权限改 %s：用 sudo 运行 ferry update install", exe)
		}
		return "", err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return "已更新 " + exe, nil
}

func copyExec(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
