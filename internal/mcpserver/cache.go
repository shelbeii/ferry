package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/settings"
)

// racyStatAge 文件修改后至少过了这么久，才允许只凭 (ModTime, Size) 命中缓存（防同毫秒/同秒同字节覆盖导致读旧数据）。
const racyStatAge = 2 * time.Second

type fileSig struct {
	exists bool
	mod    time.Time
	size   int64
}

func statSig(p string) (fileSig, bool) {
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fileSig{exists: false}, true
		}
		return fileSig{}, false
	}
	if fi.IsDir() {
		return fileSig{}, false
	}
	return fileSig{exists: true, mod: fi.ModTime(), size: fi.Size()}, true
}

func (a fileSig) freshMatch(b fileSig, now time.Time) bool {
	if a.exists != b.exists {
		return false
	}
	if !a.exists {
		return true
	}
	if a.size != b.size || !a.mod.Equal(b.mod) {
		return false
	}
	return now.Sub(b.mod) > racyStatAge
}

var settingsCache struct {
	mu   sync.Mutex
	path string
	sig  fileSig
	val  settings.Settings
	ok   bool
}

// loadSettingsCached 按 settings.json 的路径 + (ModTime, Size) 做防竞态缓存；任何变更或 2 秒内的写入都会重新完整读取，确保不会漏读或读到旧设置。
func loadSettingsCached() (settings.Settings, error) {
	p := appdir.SettingsFile()
	sig, statOK := statSig(p)
	now := time.Now()
	if statOK {
		settingsCache.mu.Lock()
		if settingsCache.ok && settingsCache.path == p && settingsCache.sig.freshMatch(sig, now) {
			v := settingsCache.val
			settingsCache.mu.Unlock()
			return v, nil
		}
		settingsCache.mu.Unlock()
	}
	st, err := settings.Load()
	if err != nil {
		return st, err
	}
	if sig2, ok2 := statSig(p); ok2 {
		settingsCache.mu.Lock()
		settingsCache.path = p
		settingsCache.sig = sig2
		settingsCache.val = st
		settingsCache.ok = true
		settingsCache.mu.Unlock()
	}
	return st, nil
}

type cachedConfig struct {
	cfgSig    fileSig
	localPath string
	localSig  fileSig
	file      *config.File
}

var configCache struct {
	mu      sync.Mutex
	entries map[string]cachedConfig
}

// loadConfigCached 按 ferry.yaml 与对应的 local 本机设置文件双签名做防竞态缓存：
// 任一文件发生变化、或修改时间在 2 秒以内、或路径变动，都会立即重新完整解析，保证数据完整不缺失；
// 每次调用方若需要 MachineProblems() 仍实时检测磁盘，不缓存磁盘存在性结果。
func loadConfigCached(cfgPath string) (*config.File, error) {
	clean := filepath.Clean(cfgPath)
	key := clean
	if filepath.Separator == '\\' {
		key = strings.ToLower(clean)
	}
	sig, statOK := statSig(clean)
	now := time.Now()
	if statOK && sig.exists {
		configCache.mu.Lock()
		if ent, ok := configCache.entries[key]; ok && ent.cfgSig.freshMatch(sig, now) {
			if ent.localPath == "" {
				f := ent.file
				configCache.mu.Unlock()
				return f, nil
			}
			if lsig, lok := statSig(ent.localPath); lok && ent.localSig.freshMatch(lsig, now) {
				f := ent.file
				configCache.mu.Unlock()
				return f, nil
			}
		}
		configCache.mu.Unlock()
	}
	f, err := config.LoadWithLocal(clean)
	if err != nil {
		return nil, err
	}
	if sig2, ok2 := statSig(clean); ok2 && sig2.exists {
		var lp string
		var lsig fileSig
		if f.Kind != config.KindConnections {
			lp = config.LocalFile(f.Project, f.Path)
			lsig, _ = statSig(lp)
		}
		configCache.mu.Lock()
		if configCache.entries == nil {
			configCache.entries = map[string]cachedConfig{}
		}
		if len(configCache.entries) > 128 {
			clear(configCache.entries)
		}
		configCache.entries[key] = cachedConfig{
			cfgSig:    sig2,
			localPath: lp,
			localSig:  lsig,
			file:      f,
		}
		configCache.mu.Unlock()
	}
	return f, nil
}
