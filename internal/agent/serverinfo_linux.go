//go:build linux

package agent

import (
	"bytes"
	"syscall"
)

func unameInfo() (kernel, arch, nodename string) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "", "", ""
	}
	return utsStr(u.Release[:]), utsStr(u.Machine[:]), utsStr(u.Nodename[:])
}

func utsStr[T ~int8 | ~uint8](raw []T) string {
	b := make([]byte, 0, len(raw))
	for _, c := range raw {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(bytes.TrimSpace(b))
}

func statMount(p string) (mountStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return mountStat{}, err
	}
	bsize := uint64(st.Frsize)
	if bsize == 0 {
		bsize = uint64(st.Bsize)
	}
	return mountStat{
		bsize:  bsize,
		blocks: st.Blocks,
		bfree:  st.Bfree,
		bavail: st.Bavail,
		files:  st.Files,
		ffree:  st.Ffree,
	}, nil
}
