//go:build !linux

package agent

import "os"

func fileAtime(path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return fi.ModTime().Unix(), true
}
