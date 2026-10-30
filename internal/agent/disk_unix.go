//go:build !windows

package agent

import (
	"os"
	"path/filepath"
	"syscall"
)

// diskUsage path 所在的分区：挂载点（用设备号区分）、用了百分之几、剩多少字节。
func diskUsage(path string) (mount string, pct int, free uint64, err error) {
	var fs syscall.Statfs_t
	if err = syscall.Statfs(path, &fs); err != nil {
		return
	}
	bs := uint64(fs.Bsize)
	used := (fs.Blocks - fs.Bfree) * bs
	free = fs.Bavail * bs
	if used+free > 0 {
		pct = int((used*100 + used + free - 1) / (used + free)) // 和 df 的 Use% 一样：已用 /（已用 + 普通用户可用），向上取整
	}
	mount = mountOf(path)
	return
}

// mountOf 往上找到设备号变了的那一级：就是挂载点。
func mountOf(p string) string {
	p, _ = filepath.Abs(p)
	st, err := os.Stat(p)
	if err != nil {
		return p
	}
	dev := st.Sys().(*syscall.Stat_t).Dev
	for p != "/" {
		parent := filepath.Dir(p)
		ps, err := os.Stat(parent)
		if err != nil || ps.Sys().(*syscall.Stat_t).Dev != dev {
			return p
		}
		p = parent
	}
	return "/"
}
