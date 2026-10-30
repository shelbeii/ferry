package agent

import "errors"

// agent 只在 Linux 上运行；Windows 上只为能编译。
func diskUsage(string) (string, int, uint64, error) {
	return "", 0, 0, errors.New("只在 Linux 上")
}
