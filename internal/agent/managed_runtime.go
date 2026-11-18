package agent

import (
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/rundist"
	"github.com/captain/ferry/internal/zipx"
)

const managedRuntimeMarker = ".ferry-managed-runtime.json"

func (a *Agent) runtimeRegistry() string {
	return filepath.Join(a.runtimesDir(), ".managed-runtimes")
}

func (a *Agent) listServerRuntimes() int {
	out := []proto.InstalledRuntime{}
	for _, j := range a.jdkInventory(a.installedRuntimes()) {
		out = append(out, proto.InstalledRuntime{
			Kind: "jdk", ID: j.ID, Version: j.Version, Arch: j.Arch, Home: strings.TrimSuffix(j.Path, "/bin/java"),
			Path: j.Path, Java: j.Java, Shared: j.Shared, Custom: j.Custom,
		})
	}
	out = append(out, a.managedRuntimes()...)
	return a.result(proto.CodeOK, "", out)
}

func (a *Agent) managedRuntimes() []proto.InstalledRuntime {
	registry := a.runtimeRegistry()
	entries, _ := os.ReadDir(registry)
	var out []proto.InstalledRuntime
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !zipx.IsSHA256(entry.Name()) {
			continue
		}
		ref, err := os.ReadFile(filepath.Join(registry, entry.Name()))
		home := string(ref)
		if err != nil || !filepath.IsAbs(home) || filepath.Clean(home) != home || jdkLocationKey(home) != entry.Name() {
			continue
		}
		if item, ok := a.readManagedRuntime(home); ok {
			out = append(out, item)
		}
	}
	return out
}

func (a *Agent) readManagedRuntime(home string) (proto.InstalledRuntime, bool) {
	var item proto.InstalledRuntime
	if checkJDKParents(home) != nil {
		return item, false
	}
	data, err := os.ReadFile(filepath.Join(home, managedRuntimeMarker))
	if err != nil || json.Unmarshal(data, &item) != nil || !rundist.ValidVersion(item.Version) {
		return item, false
	}
	s := rundist.Selection{Kind: item.Kind, Component: item.Component, Version: item.Version, Arch: item.Arch}
	if s.Validate() != nil || item.ID != s.ID() || filepath.Base(home) != item.ID {
		return item, false
	}
	item.Home = filepath.ToSlash(home)
	item.Path = filepath.ToSlash(filepath.Join(home, rundist.Executable(item.Kind)))
	item.Custom = filepath.Dir(home) != a.runtimesDir()
	st, err := os.Stat(item.Path)
	item.Unavailable = err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0
	return item, true
}

func (a *Agent) runtimeInstallDir(dir string) (string, error) {
	shared := a.runtimesDir()
	if err := checkJDKParents(shared); err != nil {
		return "", err
	}
	if dir == "" || dir == shared {
		if noExecMount(nearestDir(shared)) {
			return "", fmt.Errorf("公共运行环境目录所在的挂载点设置了 noexec：%s", shared)
		}
		return shared, nil
	}
	return dir, checkRuntimeLocation(dir, "运行环境")
}

func (a *Agent) checkRuntimeInstall() int {
	r := a.req.Runtime
	if r == nil {
		return a.result(proto.CodeBadInput, "缺少运行环境安装参数", nil)
	}
	if _, err := a.runtimeInstallDir(r.Dir); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	if err := a.runtimePrerequisites(r.Kind, r.Arch); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	return a.result(proto.CodeOK, "", nil)
}

var glibcVersion = regexp.MustCompile(`^glibc ([0-9]+)\.([0-9]+)`)

func (a *Agent) runtimePrerequisites(kind, arch string) error {
	if rundist.Executable(kind) == "" || arch != map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH] {
		return fmt.Errorf("安装包与目标服务器的运行环境类型或 CPU 架构不匹配")
	}
	out, err := a.Sys.Shell("LC_ALL=C getconf GNU_LIBC_VERSION", 5*time.Second)
	match := glibcVersion.FindStringSubmatch(strings.TrimSpace(out))
	if err != nil || len(match) != 3 {
		return errors.New("需要使用 glibc 的 Linux 服务器，当前未检测到 glibc")
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if kind == "node" && (major < 2 || (major == 2 && minor < 28)) {
		return fmt.Errorf("Node.js 官方发行包需要 glibc 2.28 或更新版本，服务器为 %s", strings.TrimSpace(out))
	}
	if kind == "dotnet" {
		libs, err := a.Sys.Shell("LC_ALL=C ldconfig -p", 10*time.Second)
		if err != nil {
			return errors.New("无法核对 .NET 所需的系统库，请确认服务器上有 ldconfig")
		}
		var missing []string
		for _, name := range []string{"libicuuc.so.", "libicui18n.so.", "libssl.so.", "libstdc++.so.6", "libz.so.1"} {
			if !strings.Contains(libs, name) {
				missing = append(missing, strings.TrimSuffix(name, "."))
			}
		}
		if len(missing) != 0 {
			return fmt.Errorf(".NET 缺少系统库 %s，请通过服务器的系统软件源安装后重试", strings.Join(missing, "、"))
		}
	}
	return nil
}

func (a *Agent) installManagedRuntime(r *proto.RuntimeSpec) int {
	stage := filepath.Clean(a.req.Stage)
	if filepath.Dir(stage) != a.incomingDir() || !reID.MatchString(filepath.Base(stage)) {
		return a.result(proto.CodeBadInput, "无效的运行环境上传目录", nil)
	}
	defer a.finishUpload(stage)
	s := rundist.Selection{Kind: r.Kind, Component: r.Component, Version: r.Version, Arch: r.Arch}
	if err := s.Validate(); err != nil || r.Version == "" || r.ID != s.ID() || r.File != "runtime.tar.gz" ||
		r.Size <= 0 || r.Size > rundist.MaxArchive || !zipx.IsSHA256(r.SHA256) {
		return a.result(proto.CodeBadInput, "无效的运行环境安装包信息", nil)
	}
	dir, err := a.runtimeInstallDir(r.Dir)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	fail := func(err error) int { return a.result(proto.CodeFailed, err.Error(), nil) }
	if err := a.runtimePrerequisites(s.Kind, s.Arch); err != nil {
		return fail(err)
	}
	if err := mkdirInstallParents(a.runtimesDir()); err != nil {
		return fail(err)
	}
	unlock, err := a.Sys.Lock(filepath.Join(a.runtimesDir(), ".lock"), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "无法锁定运行环境："+err.Error(), nil)
	}
	defer unlock()
	code, item, err := a.publishRuntime(r, dir, filepath.Join(stage, r.File))
	if err != nil {
		return a.result(code, err.Error(), nil)
	}
	a.info(proto.EvRuntime, "", "运行环境已就绪：%s", item.Path)
	return a.result(proto.CodeOK, "", item)
}

func (a *Agent) publishRuntime(r *proto.RuntimeSpec, dir, archive string) (int, proto.InstalledRuntime, error) {
	var none proto.InstalledRuntime
	if err := checkJDKParents(dir); err != nil {
		return proto.CodeBadInput, none, err
	}
	home := filepath.Join(dir, r.ID)
	registry := a.runtimeRegistry()
	if err := checkJDKParents(registry); err != nil {
		return proto.CodeBadInput, none, err
	}
	entry := filepath.Join(registry, jdkLocationKey(home))
	if _, err := os.Lstat(home); err == nil {
		if existing, ok := a.readManagedRuntime(home); ok && !existing.Unavailable && sameFile(entry, []byte(home)) {
			return proto.CodeOK, existing, nil
		}
		return proto.CodeFailed, none, fmt.Errorf("安装目录已被占用：%s", home)
	} else if !os.IsNotExist(err) {
		return proto.CodeFailed, none, err
	}
	got, size, err := zipx.FileSHA256(archive)
	if err != nil || got != r.SHA256 || size != r.Size {
		return proto.CodeVerify, none, errors.New("上传的运行环境安装包校验失败")
	}
	if err := mkdirInstallParents(dir); err != nil {
		return proto.CodeFailed, none, err
	}
	tmp, err := os.MkdirTemp(dir, ".ferry-runtime-")
	if err != nil {
		return proto.CodeFailed, none, err
	}
	defer os.RemoveAll(tmp)
	a.info(proto.EvRuntime, "", "解压并验证 %s", r.ID)
	if err := extractManagedRuntime(archive, tmp, r.Kind); err != nil {
		return proto.CodeVerify, none, fmt.Errorf("解压运行环境失败：%w", err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return proto.CodeFailed, none, err
	}
	if err := a.verifyRuntime(tmp, r); err != nil {
		return proto.CodeVerify, none, err
	}
	item := proto.InstalledRuntime{Kind: r.Kind, Component: r.Component, ID: r.ID, Version: r.Version, Arch: r.Arch,
		Home: home, Path: filepath.Join(home, rundist.Executable(r.Kind)), Custom: dir != a.runtimesDir(), SHA256: r.SHA256}
	data, _ := json.Marshal(item)
	if err := writeFileAtomic(filepath.Join(tmp, managedRuntimeMarker), data, 0o644); err != nil {
		return proto.CodeFailed, none, err
	}
	if err := os.MkdirAll(registry, 0o700); err != nil {
		return proto.CodeFailed, none, err
	}
	if err := writeFileAtomic(entry, []byte(home), 0o600); err != nil {
		return proto.CodeFailed, none, err
	}
	if err := os.Rename(tmp, home); err != nil {
		os.Remove(entry)
		return proto.CodeFailed, none, err
	}
	return proto.CodeOK, item, nil
}

func (a *Agent) verifyRuntime(home string, r *proto.RuntimeSpec) error {
	program := filepath.Join(home, rundist.Executable(r.Kind))
	want := map[string]elf.Machine{"x64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[r.Arch]
	f, err := elf.Open(program)
	if err != nil {
		return fmt.Errorf("安装包缺少适用的 Linux 程序 %s：%w", filepath.Base(program), err)
	}
	machine := f.Machine
	f.Close()
	if machine != want {
		return fmt.Errorf("程序架构 %s 与服务器架构 %s 不匹配", machine, r.Arch)
	}
	// 版本探测使用无特权账号，避免以 root 执行离线包里的程序或脚本。
	run := func(args ...string) (string, error) {
		command := "runuser -u nobody -- env -i PATH=" + shellQuote(servicePath) +
			" HOME=/nonexistent LANG=C DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1 " + shellQuote(program)
		for _, arg := range args {
			command += " " + shellQuote(arg)
		}
		out, err := a.Sys.Shell(command, 20*time.Second)
		if err != nil {
			return "", fmt.Errorf("运行环境验证失败：%s", appInfoCommandError(err, out))
		}
		return strings.TrimSpace(out), nil
	}
	var found string
	switch r.Kind {
	case "node":
		found, err = run("--version")
		found = strings.TrimPrefix(found, "v")
		if err == nil && found == r.Version {
			_, err = run(filepath.Join(home, "bin/npm"), "--version")
		}
	case "python":
		found, err = run("-I", "-B", "-c", "import sys, ssl, venv, ensurepip; ensurepip.version(); print('.'.join(map(str, sys.version_info[:3])))")
	case "dotnet":
		if r.Component == "sdk" {
			found, err = run("--list-sdks")
			for line := range strings.SplitSeq(found, "\n") {
				if strings.HasPrefix(line, r.Version+" [") {
					found = r.Version
					break
				}
			}
		} else {
			found, err = run("--list-runtimes")
			hasCore := strings.Contains(found, "Microsoft.NETCore.App "+r.Version+" [")
			hasASP := strings.Contains(found, "Microsoft.AspNetCore.App "+r.Version+" [")
			if hasCore && hasASP == (r.Component == "aspnetcore") && !realDir(filepath.Join(home, "sdk")) {
				found = r.Version
			}
		}
	}
	if err != nil {
		return err
	}
	if found != r.Version {
		return fmt.Errorf("安装包内容与所选版本或组件不符（期望 %s，检测到 %s）", r.Version, clipUTF8(found, 256))
	}
	return nil
}

func (a *Agent) removeManagedRuntime() int {
	program := a.req.RuntimePath
	dir := a.runtimesDir()
	if !filepath.IsAbs(program) || filepath.Clean(program) != program {
		return a.result(proto.CodeBadInput, "请指定运行环境清单中的程序绝对路径", nil)
	}
	if err := checkJDKParents(a.runtimeRegistry()); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	if !realDir(dir) {
		return a.result(proto.CodeBadInput, "服务器上没有该 Ferry 运行环境", nil)
	}
	unlock, err := a.Sys.Lock(filepath.Join(dir, ".lock"), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "无法锁定运行环境："+err.Error(), nil)
	}
	defer unlock()
	items := a.managedRuntimes()
	for _, item := range items {
		if item.Path != program {
			continue
		}
		for _, other := range items {
			if strings.HasPrefix(other.Home, item.Home+"/") {
				return a.result(proto.CodeBadInput, "请先卸载目录内的其他运行环境："+other.Path, nil)
			}
		}
		for _, jdk := range a.customJDKs() {
			if strings.HasPrefix(jdk.Path, item.Home+"/") {
				return a.result(proto.CodeBadInput, "请先卸载目录内的其他运行环境："+jdk.Path, nil)
			}
		}
		if err := checkRuntimeRemovalMounts(item.Home); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		label := map[string]string{"node": "Node.js", "python": "Python", "dotnet": ".NET"}[item.Kind]
		if err := a.checkRuntimeInUse(item.Home, nil, label); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		entry := filepath.Join(a.runtimeRegistry(), jdkLocationKey(item.Home))
		if err := detachRuntime(item.Home, entry, nil); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		a.info(proto.EvRuntime, "", "已卸载 %s：%s", item.ID, item.Home)
		return a.result(proto.CodeOK, "", item)
	}
	return a.result(proto.CodeBadInput, "服务器上没有该 Ferry 运行环境："+program, nil)
}
