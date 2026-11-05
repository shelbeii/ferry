package proto

import (
	"fmt"
	"path"
	"strings"
)

// KeepItem 部署时保留服务器上的一个文件或目录（不用包里的）：
//   - Shared 为 false：从运行中的版本复制到新版本，跟着版本走（回滚时回到那个版本的样子）
//   - Shared 为 true：放在应用的 data/ 下，每个版本里放一个链接指向它（只有一份，回滚时不动）
//
// 服务器上还没有时用包里的（第一次部署）。Path 相对版本目录（静态站点为站点目录），用 /；
// 不共用的可以用 * ? 匹配（如 conf/*.json）。
type KeepItem struct {
	Path   string `json:"path" yaml:"path"`
	Shared bool   `json:"shared,omitempty" yaml:"shared,omitempty"`
}

// keepReserved 不能保留的：Ferry 自己的文件、运行日志、服务器上装的依赖。
var keepReserved = map[string]string{
	".ferry":       "Ferry 自己的目录",
	"logs":         "程序运行日志的目录（要留着用 service.sharedDirs）",
	".venv":        "服务器上装的 Python 依赖",
	"node_modules": "服务器上装的 Node 依赖",
}

// CleanKeepPath 整理保留的路径：去掉开头的 ./、末尾的 /。
func CleanKeepPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	return strings.TrimRight(p, "/")
}

// CheckKeep 检查一项保留：相对路径、不越界、共用的不能有通配符；program 是程序文件名（java / binary，空 = 不查）。
// static 为真时是静态站点（站点目录里没有日志、依赖，只查 .ferry）。
func CheckKeep(k KeepItem, program string, static bool) error {
	p := k.Path
	if p == "" {
		return fmt.Errorf("保留的路径不能为空")
	}
	if strings.Contains(p, `\`) || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("保留的路径 %q 里有不能用的字符（路径用 /）", p)
	}
	if path.IsAbs(p) {
		return fmt.Errorf("保留的路径 %q 要写相对路径（相对版本目录）", p)
	}
	if c := path.Clean(p); c != p || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("保留的路径 %q 无效：不能有 .、..、多余的 /", p)
	}
	if _, err := path.Match(p, ""); err != nil {
		return fmt.Errorf("保留的路径 %q 里的通配符写错了", p)
	}
	if k.Shared && strings.ContainsAny(p, "*?[") {
		return fmt.Errorf("共用的 %q 不能用通配符（要写确定的路径）", p)
	}
	top, _, _ := strings.Cut(p, "/")
	if program != "" {
		if ok, _ := path.Match(top, program); ok {
			return fmt.Errorf("保留的路径 %q 是程序文件，不能保留", p)
		}
	}
	for name, why := range keepReserved {
		if static && name != ".ferry" {
			continue
		}
		if ok, _ := path.Match(top, name); ok {
			return fmt.Errorf("%q 不能保留：%s 是%s", p, name, why)
		}
	}
	return nil
}

// KeepMatch rel（相对站点目录 / 版本目录，用 /）是不是在要保留的里面：等于某一项，或在某一项（目录）下。
func KeepMatch(keep []KeepItem, rel string) bool {
	if len(keep) == 0 {
		return false
	}
	segs := strings.Split(rel, "/")
	for _, k := range keep {
		n := strings.Count(k.Path, "/") + 1
		if n > len(segs) {
			continue
		}
		if ok, _ := path.Match(k.Path, strings.Join(segs[:n], "/")); ok {
			return true
		}
	}
	return false
}

// KeepState 服务器上运行中的版本里一项保留的情况（部署前给人看）。
type KeepState struct {
	KeepItem
	// Server 服务器上的：file | dir | link（已经是共用的）| none；通配符匹配到几个
	Server  string `json:"server"`
	Matches int    `json:"matches,omitempty"`
	SHA256  string `json:"sha256,omitempty"` // 单个文件的 sha256（和包里的比）
	Size    int64  `json:"size,omitempty"`
	// Data 共用目录（data/ 下）里已经有了
	Data bool `json:"data,omitempty"`
}

// KeepEntry 运行中的版本里的一个文件或目录（从服务器上选要保留的）。
type KeepEntry struct {
	Name string `json:"name"`
	Path string `json:"path"` // 相对站点目录 / 版本目录
	Dir  bool   `json:"dir,omitempty"`
	Link bool   `json:"link,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// KeepScan keepscan 的结果：各项保留在服务器上的情况；KeepDir 下的文件和目录。
type KeepScan struct {
	App     string      `json:"app"`
	Release string      `json:"release,omitempty"` // 运行中的版本；空 = 还没部署过
	States  []KeepState `json:"states,omitempty"`
	Entries []KeepEntry `json:"entries,omitempty"`
	More    bool        `json:"more,omitempty"` // 目录里东西太多，只列了一部分
}
