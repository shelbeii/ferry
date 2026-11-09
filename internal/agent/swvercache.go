package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// verCache 「软件」页取到的程序版本，存在服务器上（<ferry 目录>/cache/versions.json）。
// 取版本要运行程序（mysqld --version、minio --version……），大的程序一次要几百毫秒到一秒多；
// 程序文件没变，版本就不会变，下次直接用。
//
// 按程序文件的身份记：inode、大小、ctime（fileID）。升级、重装、替换文件后身份一定会变
// （cp -p、tar 解压能保留 mtime，但改不了 ctime），下次就重新取。只缓存版本号；
// 在不在运行、端口、容器、系统服务每次都现读。
type verCache struct {
	path string

	mu    sync.Mutex
	old   map[string]verCached // 上次存的
	used  map[string]verCached // 这次用到的（存的时候只留这些：卸掉的软件不会一直留着）
	dirty bool
}

// fileID 程序文件的身份（测试时换成假的）。
var fileID = fileIdentity

type verCached struct {
	ID  string `json:"id"` // 程序文件的身份（fileID）
	V   string `json:"v"`
	Raw string `json:"raw,omitempty"` // 程序输出的开头（认 MariaDB 用）
}

// loadVerCache 读缓存；文件没有、坏了都当作空的。
func loadVerCache(path string) *verCache {
	c := &verCache{path: path, old: map[string]verCached{}, used: map[string]verCached{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c.old)
	}
	return c
}

// get 缓存里 key 的版本；file 已经不是当时那个文件（或认不出文件身份）时没有。
func (c *verCache) get(key, file string) (v, raw string, ok bool) {
	if c == nil {
		return "", "", false
	}
	id := fileID(file)
	if id == "" {
		return "", "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.old[key]
	if !ok || e.ID != id {
		return "", "", false
	}
	c.used[key] = e
	return e.V, e.Raw, true
}

// put 记下 key 的版本（file 的身份认不出时不记）。
func (c *verCache) put(key, file, v, raw string) {
	if c == nil {
		return
	}
	id := fileID(file)
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.used[key] = verCached{ID: id, V: v, Raw: raw}
	c.dirty = true
}

// save 有新取的、或有不再用到的时写回（先写临时文件再改名，写到一半不会留下坏文件）。
func (c *verCache) save() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty && len(c.used) == len(c.old) {
		return
	}
	b, err := json.Marshal(c.used)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(c.path), 0o755) != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) != nil {
		os.Remove(tmp)
		return
	}
	if os.Rename(tmp, c.path) != nil {
		os.Remove(tmp)
	}
}
