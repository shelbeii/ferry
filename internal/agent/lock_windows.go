package agent

import (
	"errors"
	"os"
	"time"
)

// agent 只在 Linux 上运行；Windows 上只为了能编译和跑单元测试（测试使用假 System）。
func flockFile(string, time.Duration) (func(), error) {
	return nil, errors.New("flock 只支持 Linux")
}

func diskFreeMB(string) int64 { return -1 }

func diskInfo(string) (int64, int64, bool) { return -1, -1, false }

func ownershipOf(string) (Ownership, error) { return Ownership{}, errors.New("只支持 Linux") }

func ownerOf(path string) (int, os.FileMode, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	return 0, fi.Mode(), nil
}

func fileOwner(os.FileInfo) (int, int, bool) { return 0, 0, false }
