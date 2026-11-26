package mcpserver

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/flock"
)

var downloadsMu sync.Mutex

// DownloadsDir 通过 MCP files_download 下载到本机的目录（数据目录下的 downloads/）。
func DownloadsDir() string {
	return filepath.Join(appdir.Dir(), "downloads")
}

func downloadsLockFile() string {
	return filepath.Join(appdir.Dir(), "downloads.lock")
}

func withDownloadsLock(fn func()) {
	downloadsMu.Lock()
	defer downloadsMu.Unlock()
	unlock, err := flock.Lock(downloadsLockFile(), 3*time.Second, nil)
	if err == nil {
		defer unlock()
	}
	fn()
}

type downloadItem struct {
	name    string
	path    string
	modTime time.Time
	size    int64
}

func entrySize(path string) int64 {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	if !fi.IsDir() {
		return fi.Size()
	}
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func listDownloadItemsLocked() []downloadItem {
	dir := DownloadsDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	items := make([]downloadItem, 0, len(ents))
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, downloadItem{
			name:    e.Name(),
			path:    p,
			modTime: fi.ModTime(),
			size:    entrySize(p),
		})
	}
	return items
}

// DownloadsUsage 返回 MCP 下载目录中的批次数（子目录/条目数）与总占用字节数。
func DownloadsUsage() (count int, size int64) {
	withDownloadsLock(func() {
		items := listDownloadItemsLocked()
		count = len(items)
		for _, it := range items {
			size += it.size
		}
	})
	return count, size
}

// CleanDownloads 删除超过 days 天或超出 maxBytes 配额的旧下载目录（超出时从最旧的删起，删到配额的 90%）。
// keepDir（如果非空）不会被删除，防止正在返回的本次下载被清理。返回删掉的批次数。
func CleanDownloads(days int, maxBytes int64, keepDir string) int {
	var removed int
	withDownloadsLock(func() {
		items := listDownloadItemsLocked()
		if len(items) == 0 {
			return
		}
		cutoff := time.Now().AddDate(0, 0, -days)
		keep := make([]downloadItem, 0, len(items))
		var total int64
		for _, it := range items {
			protected := keepDir != "" && samePath(it.path, keepDir)
			if !protected && days > 0 && it.modTime.Before(cutoff) {
				if os.RemoveAll(it.path) == nil {
					removed++
					continue
				}
			}
			keep = append(keep, it)
			total += it.size
		}
		if maxBytes > 0 && total > maxBytes {
			sort.Slice(keep, func(i, j int) bool {
				if !keep[i].modTime.Equal(keep[j].modTime) {
					return keep[i].modTime.Before(keep[j].modTime)
				}
				return keep[i].name < keep[j].name
			})
			target := maxBytes * 9 / 10
			for _, it := range keep {
				if total <= target {
					break
				}
				if keepDir != "" && samePath(it.path, keepDir) {
					continue
				}
				if os.RemoveAll(it.path) == nil {
					total -= it.size
					removed++
				}
			}
		}
	})
	return removed
}

// CleanAllDownloads 删除全部通过 MCP 下载到本地的临时文件，返回删掉的批次数。
func CleanAllDownloads() int {
	var removed int
	withDownloadsLock(func() {
		items := listDownloadItemsLocked()
		for _, it := range items {
			if os.RemoveAll(it.path) == nil {
				removed++
			}
		}
	})
	return removed
}
