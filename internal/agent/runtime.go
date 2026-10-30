package agent

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// Ferry 提供的运行环境（JDK）：
//
//	<ferryDir>/runtimes/jdk-17.0.13_11-x64/        解开的 JDK（root 所有）
//	<ferryDir>/runtimes/jdk-17.0.13_11-x64/.ferry-runtime.json
//	<ferryDir>/runtimes/java-17 → jdk-17.0.13_11-x64   单元文件里用 java-17/bin/java，升级补丁版本只换这个链接

var reRuntimeID = regexp.MustCompile(`^jdk-[A-Za-z0-9._-]+$`)

// ferryDir 服务器上 ferry 组件的目录：请求里给的，否则 agent 自己所在的目录。
func (a *Agent) ferryDir() string {
	if a.req != nil && a.req.FerryDir != "" {
		return filepath.Clean(a.req.FerryDir)
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "/usr/local/lib/ferry"
}

func (a *Agent) runtimesDir() string { return filepath.Join(a.ferryDir(), "runtimes") }

// installedRuntimes 各大版本当前安装的 JDK（java-N 链接指向的）。从已解开的 JDK 目录得到有哪些大版本，再看各自的链接。
func (a *Agent) installedRuntimes() map[int]proto.Runtime {
	out := map[int]proto.Runtime{}
	dir := a.runtimesDir()
	ents, _ := os.ReadDir(dir)
	majors := map[int]bool{}
	for _, e := range ents {
		if !e.IsDir() || !reRuntimeID.MatchString(e.Name()) {
			continue
		}
		var m proto.Runtime
		if b, err := os.ReadFile(filepath.Join(dir, e.Name(), ".ferry-runtime.json")); err == nil && json.Unmarshal(b, &m) == nil && m.Java > 0 {
			majors[m.Java] = true
		}
	}
	for major := range majors {
		target, err := a.Sys.ReadLink(filepath.Join(dir, "java-"+strconv.Itoa(major)))
		if err != nil || !reRuntimeID.MatchString(target) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, target, "bin", "java")); err != nil {
			continue
		}
		r := proto.Runtime{ID: target, Java: major}
		if b, err := os.ReadFile(filepath.Join(dir, target, ".ferry-runtime.json")); err == nil {
			var m proto.Runtime
			if json.Unmarshal(b, &m) == nil {
				r.Version = m.Version
			}
		}
		out[major] = r
	}
	return out
}

// installedJDKs Ferry 装在 runtimes 下的全部 JDK，以及 links 里这些锁定版本的链接现在指向哪个 JDK（链接名 → ID）。
func (a *Agent) installedJDKs(links []string) ([]proto.Runtime, map[string]string) {
	dir := a.runtimesDir()
	ents, _ := os.ReadDir(dir)
	var jdks []proto.Runtime
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() || !reRuntimeID.MatchString(n) {
			continue
		}
		var m proto.Runtime
		b, err := os.ReadFile(filepath.Join(dir, n, ".ferry-runtime.json"))
		if err != nil || json.Unmarshal(b, &m) != nil || m.Java == 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, n, "bin", "java")); err != nil {
			continue
		}
		m.ID = n
		jdks = append(jdks, m)
	}
	pins := map[string]string{}
	for _, l := range links {
		if !reJavaPin.MatchString(l) {
			continue
		}
		target, err := a.Sys.ReadLink(filepath.Join(dir, l))
		if err != nil || !reRuntimeID.MatchString(target) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, target, "bin", "java")); err == nil {
			pins[l] = target
		}
	}
	return jdks, pins
}

// reJavaPin 锁定版本的链接名（proto.JavaPinLink）：java-25.0.4.1_1、java-21_35。java-17 这种只有大版本的不算。
var reJavaPin = regexp.MustCompile(`^java-[0-9]+[._][0-9][0-9._]*$`)

// runtime 安装上传目录里的 JDK：校验 sha256，解到临时目录、确认有 bin/java 后改名，再把 java-N 指向它。
func (a *Agent) runtime() int {
	r := a.req.Runtime
	if r != nil && r.Kind == "nginx" {
		return a.installNginx(r)
	}
	if r != nil && r.Pin != "" {
		return a.pinRuntime(r)
	}
	if r == nil || !reRuntimeID.MatchString(r.ID) || !reSafe.MatchString(r.File) || !zipx.IsSHA256(r.SHA256) || r.Java < 8 {
		return a.result(proto.CodeBadInput, "运行环境信息无效", nil)
	}
	stage := filepath.Clean(a.req.Stage)
	if filepath.Dir(stage) != a.incomingDir() {
		return a.result(proto.CodeBadInput, "上传目录不在 "+a.incomingDir()+" 下", nil)
	}
	defer os.RemoveAll(stage)
	dir := a.runtimesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	// 同一台服务器上多个项目可能同时安装：互斥
	unlock, err := a.Sys.Lock(filepath.Join(dir, ".lock"), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "另一个部署正在安装运行环境："+err.Error(), nil)
	}
	defer unlock()

	// 装软件时指定了放 JDK 的目录：装到它下面以版本命名的文件夹（如 jdk-17.0.13_11-x64），不动 java-N 链接
	if r.Dir != "" {
		if err := validInstallDir(r.Dir); err != nil {
			return a.result(proto.CodeBadInput, "JDK "+err.Error(), nil)
		}
		final := filepath.Join(r.Dir, r.ID)
		if a.ferryJDKAt(final, r.Java) {
			a.info(proto.EvRuntime, "", "JDK %d 已在 %s", r.Java, filepath.ToSlash(final))
			return a.result(proto.CodeOK, "", proto.Runtime{ID: r.ID, Java: r.Java})
		}
		if ents, err := os.ReadDir(final); err == nil && len(ents) > 0 {
			return a.result(proto.CodeFailed, "JDK 安装目录 "+filepath.ToSlash(final)+" 已存在且不是 Ferry 装的 JDK：删掉它，或换一个目录", nil)
		}
		if err := os.MkdirAll(r.Dir, 0o755); err != nil {
			return a.result(proto.CodeFailed, "创建 JDK 安装目录失败："+err.Error(), nil)
		}
		os.Remove(final) // 空目录：解好后整个换上
		if code, msg := a.unpackJDK(r, stage, final); code != proto.CodeOK {
			return a.result(code, msg, nil)
		}
		a.info(proto.EvRuntime, "", "JDK %d 已安装：%s", r.Java, filepath.ToSlash(final))
		return a.result(proto.CodeOK, "", proto.Runtime{ID: r.ID, Java: r.Java})
	}
	final := filepath.Join(dir, r.ID)
	if _, err := os.Stat(filepath.Join(final, "bin", "java")); err != nil {
		if code, msg := a.unpackJDK(r, stage, final); code != proto.CodeOK {
			return a.result(code, msg, nil)
		}
	}
	if err := a.Sys.SwitchLink(filepath.Join(dir, fmt.Sprintf("java-%d", r.Java)), r.ID); err != nil {
		return a.result(proto.CodeFailed, "更新 java-"+strconv.Itoa(r.Java)+" 链接失败："+err.Error(), nil)
	}
	a.info(proto.EvRuntime, "", "JDK %d 已安装：%s", r.Java, final)
	return a.result(proto.CodeOK, "", proto.Runtime{ID: r.ID, Java: r.Java})
}

// pinRuntime 锁定版本的 JDK：服务器上还没有 <ID> 就装上传来的，然后让这个版本自己的链接（r.Pin）指向它。
// 不动 java-N：同一台服务器上用 java-N 的项目不受影响。
func (a *Agent) pinRuntime(r *proto.RuntimeSpec) int {
	if !reJavaPin.MatchString(r.Pin) || !reRuntimeID.MatchString(r.ID) || r.Java < 8 {
		return a.result(proto.CodeBadInput, "运行环境信息无效", nil)
	}
	upload := r.File != ""
	stage := ""
	if upload {
		if !reSafe.MatchString(r.File) || !zipx.IsSHA256(r.SHA256) {
			return a.result(proto.CodeBadInput, "运行环境信息无效", nil)
		}
		stage = filepath.Clean(a.req.Stage)
		if filepath.Dir(stage) != a.incomingDir() {
			return a.result(proto.CodeBadInput, "上传目录不在 "+a.incomingDir()+" 下", nil)
		}
		defer os.RemoveAll(stage)
	}
	dir := a.runtimesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	unlock, err := a.Sys.Lock(filepath.Join(dir, ".lock"), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "另一个部署正在安装运行环境："+err.Error(), nil)
	}
	defer unlock()

	final := filepath.Join(dir, r.ID)
	if _, err := os.Stat(filepath.Join(final, "bin", "java")); err != nil {
		if !upload {
			return a.result(proto.CodeFailed, "服务器上没有 "+r.ID+"，需要上传", nil)
		}
		if code, msg := a.unpackJDK(r, stage, final); code != proto.CodeOK {
			return a.result(code, msg, nil)
		}
		a.info(proto.EvRuntime, "", "JDK %s 已安装：%s", r.Version, filepath.ToSlash(final))
	}
	if err := a.Sys.SwitchLink(filepath.Join(dir, r.Pin), r.ID); err != nil {
		return a.result(proto.CodeFailed, "更新 "+r.Pin+" 链接失败："+err.Error(), nil)
	}
	a.info(proto.EvRuntime, "", "锁定 JDK %s：%s → %s", r.Version, r.Pin, r.ID)
	return a.result(proto.CodeOK, "", proto.Runtime{ID: r.ID, Java: r.Java, Version: r.Version})
}

// unpackJDK 校验上传的 JDK，解到 final 旁边的临时目录、确认有 bin/java 后改名为 final。
func (a *Agent) unpackJDK(r *proto.RuntimeSpec, stage, final string) (int, string) {
	src := filepath.Join(stage, r.File)
	got, size, err := zipx.FileSHA256(src)
	if err != nil || got != r.SHA256 || size != r.Size {
		return proto.CodeVerify, "上传的 JDK 校验失败（传输中损坏），请重试"
	}
	a.info(proto.EvRuntime, "", "解压 %s", r.ID)
	tmp := filepath.Join(filepath.Dir(final), ".tmp-"+filepath.Base(final))
	os.RemoveAll(tmp)
	if err := extractJDK(src, tmp); err != nil {
		os.RemoveAll(tmp)
		return proto.CodeFailed, "解压 JDK 失败：" + err.Error()
	}
	// （Windows 上只跑单元测试，文件没有可执行位）
	if fi, err := os.Stat(filepath.Join(tmp, "bin", "java")); err != nil || fi.Mode()&0o111 == 0 && goruntime.GOOS != "windows" {
		os.RemoveAll(tmp)
		return proto.CodeFailed, "压缩包里没有可执行的 bin/java"
	}
	ver := r.Version // 完整版本（如 25.0.4.1+1-LTS）；以前的引擎不传，用 ID 代替
	if ver == "" {
		ver = strings.TrimPrefix(r.ID, "jdk-")
	}
	meta, _ := json.Marshal(proto.Runtime{ID: r.ID, Java: r.Java, Version: ver})
	os.WriteFile(filepath.Join(tmp, ".ferry-runtime.json"), meta, 0o644)
	os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return proto.CodeFailed, err.Error()
	}
	return proto.CodeOK, ""
}

// extractJDK 解开 JDK 的 tar.gz（去掉最外层目录），保留可执行位与目录内的符号链接；越界的路径、链接一律拒绝。
func extractJDK(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		_, rest, ok := strings.Cut(name, "/") // 去掉 jdk-17.0.13+11/
		if !ok || rest == "" {
			continue
		}
		if path.IsAbs(rest) || rest == ".." || strings.HasPrefix(rest, "../") {
			return fmt.Errorf("压缩包里有越界路径：%s", h.Name)
		}
		p := filepath.Join(dst, filepath.FromSlash(rest))
		if !within(dst, p) {
			return fmt.Errorf("压缩包里有越界路径：%s", h.Name)
		}
		// 去掉组和其他用户的写权限，保留可执行位
		mode := os.FileMode(h.Mode).Perm()&0o755 | 0o600
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			os.Chmod(p, mode)
		case tar.TypeSymlink, tar.TypeLink:
			target := h.Linkname
			var resolved string
			if h.Typeflag == tar.TypeSymlink {
				if path.IsAbs(target) {
					return fmt.Errorf("压缩包里有指向外部的链接：%s → %s", h.Name, target)
				}
				resolved = filepath.Join(filepath.Dir(p), filepath.FromSlash(target))
			} else {
				_, lr, _ := strings.Cut(path.Clean(target), "/")
				resolved = filepath.Join(dst, filepath.FromSlash(lr))
			}
			if !within(dst, resolved) {
				return fmt.Errorf("压缩包里有指向外部的链接：%s → %s", h.Name, target)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if h.Typeflag == tar.TypeSymlink {
				err = os.Symlink(target, p)
			} else {
				err = os.Link(resolved, p)
			}
			if err != nil {
				return err
			}
		}
	}
}
