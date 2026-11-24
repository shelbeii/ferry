//go:build linux

package agent

import (
	"os"
	"syscall"
)

func fileAtime(path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Atim.Sec, true
}
