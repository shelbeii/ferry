// Package flock 本机跨进程文件锁（构建锁）。进程退出时操作系统自动释放。
package flock

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ErrTimeout 等锁超时。
var ErrTimeout = errors.New("等待锁超时")

// Lock 加排他锁。拿不到时先调用一次 onWait，再每 500ms 重试，最多等 wait。
func Lock(path string, wait time.Duration, onWait func()) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	notified := false
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return func() { unlock(f); f.Close() }, nil
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, ErrTimeout
		}
		if !notified && onWait != nil {
			onWait()
			notified = true
		}
		time.Sleep(500 * time.Millisecond)
	}
}
