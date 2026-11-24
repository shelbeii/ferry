package dbnative

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ManagedToolsRoot Ferry 在执行服务器上托管数据库迁移工具的默认根目录。
const ManagedToolsRoot = "/opt/ferry/tools"

// managedToolBinDirs 返回 Ferry 托管工具可能所在的 bin 目录（按优先级排序）。
func managedToolBinDirs() []string {
	var roots []string
	roots = append(roots, ManagedToolsRoot)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".ferry", "tools"))
	}
	var dirs []string
	seen := map[string]bool{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			for _, candidate := range []string{
				filepath.Join(root, e.Name(), "bin"),
				filepath.Join(root, e.Name()),
			} {
				if seen[candidate] {
					continue
				}
				if st, err := os.Stat(candidate); err == nil && st.IsDir() {
					seen[candidate] = true
					dirs = append(dirs, candidate)
				}
			}
		}
	}
	return dirs
}

// findTool 优先在 Ferry 托管工具目录查找可执行文件，找不到再查系统 PATH。
func findTool(name string) (string, error) {
	for _, dir := range managedToolBinDirs() {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return exec.LookPath(name)
}

// toolPathEnv 返回把 Ferry 托管工具目录拼接到最前面的 PATH 环境变量项。
func toolPathEnv() []string {
	dirs := managedToolBinDirs()
	if len(dirs) == 0 {
		return nil
	}
	cur := os.Getenv("PATH")
	if cur != "" {
		return []string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator)) + string(os.PathListSeparator) + cur}
	}
	return []string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator))}
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
