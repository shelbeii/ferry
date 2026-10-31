//go:build !windows

package agent

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
	"time"
)

// flockFile 对 path 加排他 flock；wait 为 0 时拿不到立即返回 ErrLocked。
// 进程退出（包括被杀）时内核自动释放，不会留下僵尸锁。
func flockFile(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK || !time.Now().Before(deadline) {
			f.Close()
			if err == syscall.EWOULDBLOCK {
				return nil, ErrLocked
			}
			return nil, err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// diskInfo 路径所在分区的可用空间（MB）、可用 inode（文件个数；不限制的文件系统为 -1）、是否只读；读不到时空间为 -1。
func diskInfo(path string) (freeMB, inodes int64, readOnly bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1, -1, false
	}
	inodes = int64(st.Ffree)
	if st.Files == 0 { // btrfs 等不限制 inode
		inodes = -1
	}
	return int64(st.Bavail) * int64(st.Bsize) / (1 << 20), inodes, uint64(st.Flags)&1 != 0 // ST_RDONLY / MNT_RDONLY
}

// diskFreeMB 路径所在分区的可用空间。
func diskFreeMB(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize) / (1 << 20)
}

// ownershipOf 文件的属主、组（名字查不到时用数字）与权限。
func ownershipOf(path string) (Ownership, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Ownership{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Ownership{}, fmt.Errorf("读不到 %s 的属主", path)
	}
	o := Ownership{UID: int(st.Uid), GID: int(st.Gid), User: strconv.Itoa(int(st.Uid)), Group: strconv.Itoa(int(st.Gid)), Perm: fi.Mode()}
	if u, err := user.LookupId(o.User); err == nil {
		o.User = u.Username
	}
	if g, err := user.LookupGroupId(o.Group); err == nil {
		o.Group = g.Name
	}
	return o, nil
}

// ownerOf 文件属主 uid 与权限。
func ownerOf(path string) (uid int, mode os.FileMode, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), fi.Mode(), nil
	}
	return -1, fi.Mode(), nil
}

// fileOwner 文件的属主 uid、gid。
func fileOwner(fi os.FileInfo) (int, int, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}
