package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

func (a *Agent) listJDKs() int {
	if r := a.req.Runtime; r != nil && r.Dir != "" {
		if err := checkJDKLocation(r.Dir); err != nil {
			return a.result(proto.CodeBadInput, err.Error(), nil)
		}
		list := []proto.InstalledJDK{}
		entries, err := os.ReadDir(r.Dir)
		if err != nil && !os.IsNotExist(err) {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		for _, entry := range entries {
			home := filepath.Join(r.Dir, entry.Name())
			if jdk, ok := customJDKAt(home); ok {
				list = append(list, jdk)
			}
		}
		sortJDKs(list)
		return a.result(proto.CodeOK, "", list)
	}
	return a.result(proto.CodeOK, "", a.jdkInventory(a.installedRuntimes()))
}

func readInstalledJDK(home string) (proto.Runtime, bool) {
	var r proto.Runtime
	b, err := os.ReadFile(filepath.Join(home, ".ferry-runtime.json"))
	if err != nil || json.Unmarshal(b, &r) != nil || r.Java == 0 {
		return r, false
	}
	if _, err := os.Stat(filepath.Join(home, "bin", "java")); err != nil {
		return r, false
	}
	r.ID = filepath.Base(home)
	return r, true
}

func installedJDK(home string, r proto.Runtime) proto.InstalledJDK {
	return proto.InstalledJDK{Runtime: r, Arch: r.ID[strings.LastIndexByte(r.ID, '-')+1:],
		Path: filepath.ToSlash(filepath.Join(home, "bin", "java"))}
}

// checkJDKLocation 验证自定义父目录。安装以 root 解包，目录链必须阻止普通用户替换目标。
func checkJDKLocation(dir string) error {
	return checkRuntimeLocation(dir, "JDK")
}

func checkRuntimeLocation(dir, label string) error {
	if !path.IsAbs(dir) || path.Clean(dir) != dir || dir == "/" {
		return fmt.Errorf("%s 安装目录要写服务器上的绝对路径：%q", label, dir)
	}
	if err := validInstallDir(path.Join(dir, "jdk-version")); err != nil {
		return err
	}
	if err := checkJDKParents(dir); err != nil {
		return err
	}
	if noExecMount(nearestDir(dir)) {
		return fmt.Errorf("%s 安装目录所在的挂载点设置了 noexec：%s", label, dir)
	}
	return nil
}

func checkJDKParents(dir string) error {
	parent := filepath.Dir(dir)
	if parent == dir {
		return nil
	}
	if err := checkJDKParents(parent); err != nil {
		return err
	}
	uid, mode, err := ownerOf(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if uid != 0 || !mode.IsDir() || mode.Perm()&0o022 != 0 {
		return fmt.Errorf("运行环境安装位置 %s 必须是 root 所有、其他用户不可写的真实目录", dir)
	}
	return nil
}

func customJDKAt(home string) (proto.InstalledJDK, bool) {
	if !reRuntimeID.MatchString(filepath.Base(home)) || checkJDKParents(home) != nil {
		return proto.InstalledJDK{}, false
	}
	r, ok := readInstalledJDK(home)
	if !ok {
		return proto.InstalledJDK{}, false
	}
	item := installedJDK(home, r)
	item.Custom = true
	return item, true
}

func (a *Agent) jdkLocationsDir() string { return filepath.Join(a.runtimesDir(), ".custom-jdks") }

func (a *Agent) customJDKs() []proto.InstalledJDK {
	dir := a.jdkLocationsDir()
	entries, _ := os.ReadDir(dir)
	var out []proto.InstalledJDK
	for _, entry := range entries {
		if !zipx.IsSHA256(entry.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		home := string(b)
		if !filepath.IsAbs(home) || filepath.Clean(home) != home || jdkLocationKey(home) != entry.Name() {
			continue
		}
		if jdk, ok := customJDKAt(home); ok {
			out = append(out, jdk)
		}
	}
	return out
}

func jdkLocationKey(home string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(home))) }

// installCustomJDK 在公共运行环境锁内调用。先登记路径，清单只返回已完整安装的目录。
func (a *Agent) installCustomJDK(r *proto.RuntimeSpec, stage string) int {
	if err := checkJDKLocation(r.Dir); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	home := filepath.Join(r.Dir, r.ID)
	if err := checkJDKParents(home); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	installed, exists := readInstalledJDK(home)
	if !exists || installed.Java != r.Java {
		exists = false
		if stage == "" {
			return a.result(proto.CodeFailed, "服务器上没有可用的 "+home, nil)
		}
		if entries, err := os.ReadDir(home); err == nil && len(entries) > 0 {
			return a.result(proto.CodeFailed, "JDK 目录已被占用："+home, nil)
		} else if err != nil && !os.IsNotExist(err) {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		if err := mkdirInstallParents(r.Dir); err != nil {
			return a.result(proto.CodeFailed, "创建 JDK 安装目录失败："+err.Error(), nil)
		}
	}
	location := filepath.Join(a.jdkLocationsDir(), jdkLocationKey(home))
	if !sameFile(location, []byte(home)) {
		if err := os.MkdirAll(a.jdkLocationsDir(), 0o700); err != nil {
			return a.result(proto.CodeFailed, "登记 JDK 安装位置失败："+err.Error(), nil)
		}
		if err := writeFileAtomic(location, []byte(home), 0o600); err != nil {
			return a.result(proto.CodeFailed, "登记 JDK 安装位置失败："+err.Error(), nil)
		}
	}
	if exists {
		a.info(proto.EvRuntime, "", "JDK %d 已在 %s", r.Java, home)
		return a.result(proto.CodeOK, "", installed)
	}
	if code, msg := a.unpackJDK(r, stage, home); code != proto.CodeOK {
		os.Remove(location)
		return a.result(code, msg, nil)
	}
	a.info(proto.EvRuntime, "", "JDK %d 已安装：%s", r.Java, home)
	return a.result(proto.CodeOK, "", proto.Runtime{ID: r.ID, Java: r.Java, Version: r.Version})
}
