// Package appdir 决定 ferry 的数据目录：主机公钥记录、密钥、构建锁、各项目的构建日志与本机部署历史、设置、JDK 缓存。
//
// 查找顺序：
//  1. 环境变量 FERRY_HOME（测试、特殊部署用）
//  2. 手动设置的位置：默认数据目录下 location.json 里记录的目录（设置里修改）
//  3. 默认：程序所在目录（安装目录）下的 data —— 所有数据跟着安装位置走，不散落到别的盘
//  4. 安装目录不可写（如装在 Program Files）或是开发时的临时编译目录：用户目录（Windows %APPDATA%\Ferry，
//     macOS ~/Library/Application Support/Ferry，Linux ~/.config/ferry）
//
// 安装程序升级、卸载只删除它自己装的文件，data 不受影响；卸载时勾选「删除应用程序数据」才会删除。
package appdir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// 数据目录的来源。
const (
	ModeEnv     = "env"     // 环境变量 FERRY_HOME
	ModeCustom  = "custom"  // 手动设置的位置
	ModeInstall = "install" // 安装目录下的 data（默认）
	ModeUser    = "user"    // 用户目录（安装目录不可写时）
)

// Info 当前数据目录。
type Info struct {
	Dir        string `json:"dir"`
	Mode       string `json:"mode"`
	DefaultDir string `json:"defaultDir"`           // 默认位置（恢复默认时用的）
	InstallDir string `json:"installDir,omitempty"` // 程序所在目录
	Note       string `json:"note,omitempty"`       // 为什么没有用安装目录
}

var (
	once   sync.Once
	cached Info
	err0   error
)

// Current 当前数据目录（进程内只解析一次；Reset 后重新解析）。
func Current() Info {
	once.Do(func() { cached, err0 = resolve() })
	return cached
}

// Reset 清除缓存（修改位置后、测试中使用）。
func Reset() { once = sync.Once{} }

// Dir 数据目录。
func Dir() string { return Current().Dir }

// ExeDir 程序所在目录（测试时可以替换）。
var ExeDir = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(exe)
}

// UserDir 用户目录下的数据目录（旧版本的默认位置；安装目录不可写时用）。
func UserDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".ferry")
	}
	name := "Ferry"
	if runtime.GOOS == "linux" {
		name = "ferry"
	}
	return filepath.Join(base, name)
}

// noInstallData 不在安装目录放数据的系统：macOS 的应用是 .app 包。测试里可以改。
var noInstallData = runtime.GOOS == "darwin"

// installData 安装目录下的 data；不可用时返回空与原因。
func installData() (dir, note string) {
	if noInstallData {
		// 数据不能放进 .app 包（会破坏签名，升级时整个包被替换）
		return "", "macOS 上数据放在 ~/Library/Application Support/Ferry"
	}
	exe := ExeDir()
	if exe == "" {
		return "", "找不到程序所在目录"
	}
	// go run / go test 编译在临时的 go-build 目录里：不在那里放数据
	if strings.Contains(filepath.ToSlash(exe), "/go-build") {
		return "", "程序在临时目录里（开发时）"
	}
	d := filepath.Join(exe, "data")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", "安装目录不可写（" + exe + "）"
	}
	probe := filepath.Join(d, ".write-test")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return "", "安装目录不可写（" + exe + "）"
	}
	os.Remove(probe)
	return d, ""
}

// DefaultDir 默认位置：安装目录下的 data，不可用时用户目录。
func DefaultDir() string {
	if d, _ := installData(); d != "" {
		return d
	}
	return UserDir()
}

func resolve() (Info, error) {
	inst, note := installData()
	def := inst
	if def == "" {
		def = UserDir()
	}
	info := Info{DefaultDir: def, InstallDir: ExeDir(), Note: note}
	if d := os.Getenv("FERRY_HOME"); d != "" {
		info.Dir, info.Mode = abs(d), ModeEnv
		return info, nil
	}
	// 旧版本把手动设置的位置记在用户目录：挪到现在的默认位置
	if inst != "" {
		user := UserDir()
		if _, err := os.Stat(filepath.Join(def, "location.json")); errors.Is(err, os.ErrNotExist) {
			if b, err := os.ReadFile(filepath.Join(user, "location.json")); err == nil {
				if WriteFileAtomic(filepath.Join(def, "location.json"), b, 0o600) == nil {
					os.Remove(filepath.Join(user, "location.json"))
				}
			}
		}
	}
	if loc, err := readLocation(def); err == nil && loc != "" && !samePath(abs(loc), def) {
		info.Dir, info.Mode = abs(loc), ModeCustom
		return info, nil
	}
	info.Dir, info.Mode = def, ModeInstall
	if inst == "" {
		info.Mode = ModeUser
	}
	return info, nil
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

type location struct {
	DataDir string `json:"dataDir"`
}

func readLocation(def string) (string, error) {
	b, err := os.ReadFile(filepath.Join(def, "location.json"))
	if err != nil {
		return "", err
	}
	var l location
	if err := json.Unmarshal(b, &l); err != nil {
		return "", err
	}
	return l.DataDir, nil
}

// SetLocation 把数据目录改到 dir（空 = 恢复默认位置）。只记录位置，不搬数据（见 Move）。
func SetLocation(dir string) error {
	def := Current().DefaultDir
	if err := os.MkdirAll(def, 0o700); err != nil {
		return err
	}
	p := filepath.Join(def, "location.json")
	if dir == "" || samePath(abs(dir), def) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		Reset()
		return nil
	}
	b, _ := json.MarshalIndent(location{DataDir: abs(dir)}, "", "  ")
	if err := WriteFileAtomic(p, append(b, '\n'), 0o600); err != nil {
		return err
	}
	Reset()
	return nil
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// ---------------------------------------------------------------- 子路径

// Path 数据目录下的路径（自动创建上级目录）。
func Path(elem ...string) string {
	p := filepath.Join(append([]string{Dir()}, elem...)...)
	os.MkdirAll(filepath.Dir(p), 0o700)
	return p
}

// KnownHosts ferry 记录的主机公钥。
func KnownHosts() string { return Path("known_hosts") }

// SubDir 数据目录下的子目录（自动创建）。
func SubDir(elem ...string) string {
	p := filepath.Join(append([]string{Dir()}, elem...)...)
	os.MkdirAll(p, 0o700)
	return p
}

// TempDir ferry 自己的临时文件（部署时打包、上传 agent 等），也放在数据目录里。
func TempDir() string { return SubDir("tmp") }

// KeysDir 默认的密钥目录。
func KeysDir() string { return SubDir("keys") }

// LocksDir 构建锁目录。
func LocksDir() string { return SubDir("locks") }

// SettingsFile 设置。
func SettingsFile() string { return Path("settings.json") }

// ProjectDir 某个配置文件对应的项目目录：projects/<项目名>-<配置路径 sha256 前 8 位>（构建日志、本机部署历史、本机设置）。
func ProjectDir(project, configPath string) string {
	p := ProjectPath(project, configPath)
	os.MkdirAll(p, 0o700)
	return p
}

// ProjectPath 同 ProjectDir，但不创建目录（只读的时候用）。
func ProjectPath(project, configPath string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(abs(configPath))))
	name := fmt.Sprintf("%s-%s", project, hex.EncodeToString(sum[:])[:8])
	return filepath.Join(Dir(), "projects", name)
}

// WriteFileAtomic 写临时文件再改名。
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
