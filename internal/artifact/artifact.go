// Package artifact 上传前检查构建产物，打包静态目录。
package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/config"
)

// Info 检查通过的产物。
type Info struct {
	Path  string
	Size  int64 // 文件大小；目录为全部文件大小之和
	Files int   // 目录中的文件数
	Arch  string
	Notes []string
	// ClassMajor jar 主类的 class 文件版本（52 = Java 8，61 = Java 17）；0 = 不知道
	ClassMajor int
}

// JavaOf class 文件版本对应的 Java 版本（61 → 17）。
func JavaOf(classMajor int) int {
	if classMajor < 45 {
		return 0
	}
	return classMajor - 44
}

// Check 按应用类型与 verify 配置检查产物。notBefore 非零时，产物修改时间不能早于它（防止用到旧包）。
// targetArch 非空时检查 binary 的 CPU 架构。
func Check(a *config.App, p string, notBefore time.Time, targetArch string) (*Info, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("产物不存在：%s", p)
	}
	info := &Info{Path: p}
	if a.Type == "static" {
		if !st.IsDir() {
			return nil, fmt.Errorf("static 产物应是目录：%s", p)
		}
		if err := checkStatic(p, a.Artifact.Verify.Static, info); err != nil {
			return nil, err
		}
		if !notBefore.IsZero() {
			if newest, err := newestFile(p); err == nil && newest.Before(notBefore.Add(-2*time.Second)) {
				return nil, fmt.Errorf("%s 里的文件都早于本次构建开始时间，构建可能没有输出到这里", p)
			}
		}
		return info, nil
	}
	if a.Type == "python" || a.Type == "node" || a.Type == "dotnet" {
		if !st.IsDir() {
			return nil, fmt.Errorf("%s 的产物应是目录（源码目录或构建输出的目录）：%s", a.Type, p)
		}
		if err := checkDirApp(a, p, notBefore, info); err != nil {
			return nil, err
		}
		return info, nil
	}
	if st.IsDir() {
		return nil, fmt.Errorf("产物应是文件：%s", p)
	}
	info.Size = st.Size()
	if info.Size == 0 {
		return nil, fmt.Errorf("产物是空文件：%s", p)
	}
	if min := a.Artifact.Verify.MinSize; min > 0 && info.Size < min {
		return nil, fmt.Errorf("产物只有 %d 字节，小于 minSize %d", info.Size, min)
	}
	if !notBefore.IsZero() && st.ModTime().Before(notBefore.Add(-2*time.Second)) {
		return nil, fmt.Errorf("%s 修改时间 %s 早于本次构建开始，构建可能失败了或输出到了别处",
			p, st.ModTime().Format("2006-01-02 15:04:05"))
	}
	switch a.Type {
	case "java":
		if err := checkJar(p, a.Artifact.Verify.SpringBoot, info); err != nil {
			return nil, err
		}
	case "binary":
		arch, err := ELFArch(p)
		if err != nil {
			return nil, err
		}
		info.Arch = arch
		if targetArch != "" && arch != targetArch {
			return nil, fmt.Errorf("%s 是 %s 程序，目标服务器是 %s", filepath.Base(p), arch, targetArch)
		}
	}
	return info, nil
}

// ELFArch 返回 Linux 可执行文件的架构（amd64 / arm64）。
func ELFArch(p string) (string, error) {
	f, err := elf.Open(p)
	if err != nil {
		return "", fmt.Errorf("%s 不是 Linux 可执行文件（ELF）：%w", filepath.Base(p), err)
	}
	defer f.Close()
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return "", fmt.Errorf("%s 不是可执行文件（ELF 类型 %s）", filepath.Base(p), f.Type)
	}
	switch f.Machine {
	case elf.EM_X86_64:
		return "amd64", nil
	case elf.EM_AARCH64:
		return "arm64", nil
	default:
		return "", fmt.Errorf("%s 的 CPU 架构 %s 不受支持", filepath.Base(p), f.Machine)
	}
}

func checkJar(p string, v *config.SpringBootVerify, info *Info) error {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return fmt.Errorf("%s 不是有效的 JAR：%w", filepath.Base(p), err)
	}
	defer zr.Close()
	var manifest *zip.File
	libs := map[string]*zip.File{}
	hasBoot := false
	for _, f := range zr.File {
		switch {
		case f.Name == "META-INF/MANIFEST.MF":
			manifest = f
		case strings.HasPrefix(f.Name, "BOOT-INF/"):
			hasBoot = true
			if strings.HasPrefix(f.Name, "BOOT-INF/lib/") && strings.HasSuffix(f.Name, ".jar") {
				libs[path.Base(f.Name)] = f
			}
		}
	}
	if manifest == nil {
		return fmt.Errorf("%s 没有 META-INF/MANIFEST.MF", filepath.Base(p))
	}
	mf, err := readZipFile(manifest, 1<<20)
	if err != nil {
		return err
	}
	if !bytes.Contains(mf, []byte("Main-Class:")) {
		return fmt.Errorf("%s 不是可执行 JAR（MANIFEST 没有 Main-Class）", filepath.Base(p))
	}
	info.ClassMajor = classMajor(zr.File, mf)
	if j := JavaOf(info.ClassMajor); j > 0 {
		info.Notes = append(info.Notes, fmt.Sprintf("需要 Java %d+", j))
	}
	if v == nil {
		return nil
	}
	if !hasBoot || !bytes.Contains(mf, []byte("org.springframework.boot.loader")) {
		return fmt.Errorf("%s 不是 Spring Boot 可执行 JAR（没有 repackage？）", filepath.Base(p))
	}
	info.Notes = append(info.Notes, fmt.Sprintf("%d 个依赖", len(libs)))
	names := make([]string, 0, len(libs))
	for n := range libs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, pattern := range v.RequireLibs {
		var matched []string
		for _, n := range names {
			if ok, _ := path.Match(pattern, n); ok {
				matched = append(matched, n)
			}
		}
		if len(matched) == 0 {
			return fmt.Errorf("%s 缺少依赖 %s（BOOT-INF/lib 下没有匹配的 jar；多模块项目是不是没带 -am 构建？）", filepath.Base(p), pattern)
		}
		if !v.NonEmptyLibs {
			continue
		}
		for _, n := range matched {
			ok, err := jarHasClasses(libs[n])
			if err != nil {
				return fmt.Errorf("检查依赖 %s：%w", n, err)
			}
			if !ok {
				return fmt.Errorf("%s 里的依赖 %s 没有任何 .class（模块被打成了空包）", filepath.Base(p), n)
			}
		}
	}
	return nil
}

// classMajor 应用主类（Spring Boot 的 Start-Class，否则 Main-Class）的 class 文件版本。
func classMajor(files []*zip.File, manifest []byte) int {
	attr := func(k string) string {
		// MANIFEST 的长行会折行（下一行以空格开头）：先接回去
		for _, l := range strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(manifest), "\r\n", "\n"), "\n ", ""), "\n") {
			if v, ok := strings.CutPrefix(l, k+":"); ok {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	var names []string
	if c := attr("Start-Class"); c != "" {
		names = append(names, "BOOT-INF/classes/"+strings.ReplaceAll(c, ".", "/")+".class")
	}
	if c := attr("Main-Class"); c != "" {
		names = append(names, strings.ReplaceAll(c, ".", "/")+".class")
	}
	for _, n := range names {
		for _, f := range files {
			if f.Name != n {
				continue
			}
			b, err := readZipFile(f, 1<<20)
			if err == nil && len(b) >= 8 && b[0] == 0xCA && b[1] == 0xFE && b[2] == 0xBA && b[3] == 0xBE {
				return int(b[6])<<8 | int(b[7])
			}
		}
	}
	return 0
}

func jarHasClasses(f *zip.File) (bool, error) {
	data, err := readZipFile(f, 512<<20)
	if err != nil {
		return false, err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false, err
	}
	for _, e := range zr.File {
		if strings.HasSuffix(e.Name, ".class") {
			return true, nil
		}
	}
	return false, nil
}

func readZipFile(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

func checkStatic(dir string, v *config.StaticVerify, info *Info) error {
	index := "index.html"
	if v != nil && v.Index != "" {
		index = v.Index
	}
	if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(index))); err != nil || st.IsDir() {
		return fmt.Errorf("%s 里没有 %s", dir, index)
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("静态目录里不能有符号链接：%s", p)
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			info.Size += fi.Size()
			info.Files++
		}
		return nil
	})
}

func newestFile(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return err
	})
	return newest, err
}

// PackDir 把目录打成 tar.gz（条目排序、路径用 /，不含符号链接）。
func PackDir(dir, out string) error { return PackDirOnly(dir, out, nil) }

// PackDirOnly 同 PackDir，但只放 only 里的文件（路径相对 dir，用 /）；目录全都放。only 为空时放全部文件。
func PackDirOnly(dir, out string, only map[string]bool) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	var paths []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("静态目录里不能有符号链接：%s", p)
		}
		if d.IsDir() {
			paths = append(paths, p)
		} else if d.Type().IsRegular() {
			if only != nil {
				rel, err := filepath.Rel(dir, p)
				if err != nil || !only[filepath.ToSlash(rel)] {
					return err
				}
			}
			paths = append(paths, p)
		}
		return nil
	})
	if err == nil {
		sort.Strings(paths)
		for _, p := range paths {
			if err = addTar(tw, dir, p); err != nil {
				break
			}
		}
	}
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out)
	}
	return err
}

// ExtraFile 一个要打包的附带文件或目录。
type ExtraFile struct {
	Local string // 本机路径（文件或目录）
	To    string // 放到版本目录里的相对路径（用 /）
	Exec  bool   // 可执行（本机文件有可执行位的也算）
}

// PackFiles 把附带文件打成 tar.gz：每项放到 To（目录整个放过去），上层目录也写进去。返回文件数与总大小。
func PackFiles(files []ExtraFile, out string) (int, int64, error) {
	type entry struct {
		local string // 空 = 目录
		exec  bool
	}
	entries := map[string]entry{}
	addDirs := func(name string) {
		for d := path.Dir(name); d != "."; d = path.Dir(d) {
			entries[d+"/"] = entry{}
		}
	}
	var count int
	var size int64
	addFile := func(local, name string, exec bool, fi fs.FileInfo) {
		entries[name] = entry{local: local, exec: exec || fi.Mode()&0o100 != 0}
		addDirs(name)
		count++
		size += fi.Size()
	}
	for _, x := range files {
		st, err := os.Lstat(x.Local)
		if err != nil {
			return 0, 0, fmt.Errorf("附带文件不存在：%s", x.Local)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			if st, err = os.Stat(x.Local); err != nil {
				return 0, 0, fmt.Errorf("附带文件是失效的链接：%s", x.Local)
			}
		}
		if !st.IsDir() {
			if !st.Mode().IsRegular() {
				return 0, 0, fmt.Errorf("附带文件不是普通文件：%s", x.Local)
			}
			addFile(x.Local, x.To, x.Exec, st)
			continue
		}
		entries[x.To+"/"] = entry{}
		addDirs(x.To)
		err = filepath.WalkDir(x.Local, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == x.Local {
				return nil
			}
			rel, err := filepath.Rel(x.Local, p)
			if err != nil {
				return err
			}
			name := x.To + "/" + filepath.ToSlash(rel)
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				return fmt.Errorf("附带的目录里不能有符号链接：%s", p)
			case d.IsDir():
				entries[name+"/"] = entry{}
			case d.Type().IsRegular():
				fi, err := d.Info()
				if err != nil {
					return err
				}
				addFile(p, name, x.Exec, fi)
			}
			return nil
		})
		if err != nil {
			return 0, 0, err
		}
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)

	f, err := os.Create(out)
	if err != nil {
		return 0, 0, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		en := entries[n]
		if en.local == "" {
			err = tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now().Truncate(time.Second), Format: tar.FormatPAX})
		} else {
			err = addTarFile(tw, en.local, n, en.exec)
		}
		if err != nil {
			break
		}
	}
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out)
		return 0, 0, err
	}
	return count, size, nil
}

func addTarFile(tw *tar.Writer, local, name string, exec bool) error {
	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	h := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: fi.Size(), ModTime: fi.ModTime().Truncate(time.Second), Format: tar.FormatPAX}
	if exec {
		h.Mode = 0o755
	}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err = io.CopyN(tw, src, fi.Size())
	return err
}

func addTar(tw *tar.Writer, root, p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return err
	}
	name := filepath.ToSlash(rel)
	if strings.HasPrefix(name, "../") {
		return errors.New("路径越界：" + name)
	}
	h := &tar.Header{Name: name, ModTime: fi.ModTime().Truncate(time.Second), Format: tar.FormatPAX}
	if fi.IsDir() {
		h.Typeflag = tar.TypeDir
		h.Name += "/"
		h.Mode = 0o755
		return tw.WriteHeader(h)
	}
	h.Typeflag = tar.TypeReg
	h.Mode = 0o644
	h.Size = fi.Size()
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	src, err := os.Open(p)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(tw, src)
	return err
}
