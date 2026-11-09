package agent

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity 文件的身份：设备、inode、大小、ctime（纳秒）。文件被替换、改动后一定会变；读不到为空。
func fileIdentity(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d.%09d", st.Dev, st.Ino, st.Size, st.Ctim.Sec, st.Ctim.Nsec)
}
