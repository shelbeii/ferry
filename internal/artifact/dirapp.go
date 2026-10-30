package artifact

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/config"
)

// DefaultExclude 各类型默认不上传的（开发时的虚拟环境、依赖、缓存、版本库）。
var DefaultExclude = map[string][]string{
	"python": {".git", ".venv", "venv", "env", "__pycache__", "*.pyc", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".idea", ".vscode", "node_modules", "ferry.yaml"},
	"node":   {".git", "node_modules", ".next/cache", "coverage", ".turbo", ".idea", ".vscode", "ferry.yaml"},
	"dotnet": {},
}

// Excludes 这个应用不上传的：类型默认的加上 artifact.exclude。
func Excludes(a *config.App) []string {
	return append(append([]string{}, DefaultExclude[a.Type]...), a.Artifact.Exclude...)
}

// excluded rel（相对目录，用 /）要不要跳过：按文件名或相对路径匹配。
func excluded(rel string, patterns []string) bool {
	base := path.Base(rel)
	for _, p := range patterns {
		p = strings.Trim(p, "/")
		if ok, _ := path.Match(p, base); ok {
			return true
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
	}
	return false
}

// checkDirApp python、node、dotnet 的产物：要是目录、不能是空的；有构建时里面要有这次构建写的文件。
func checkDirApp(a *config.App, p string, notBefore time.Time, info *Info) error {
	ex := Excludes(a)
	var newest time.Time
	err := filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(p, f)
		rel = filepath.ToSlash(rel)
		if f != p && excluded(rel, ex) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			info.Files++
			info.Size += fi.Size()
			if fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if info.Files == 0 {
		return fmt.Errorf("%s 里没有要上传的文件", p)
	}
	if !notBefore.IsZero() && newest.Before(notBefore.Add(-2*time.Second)) {
		return fmt.Errorf("%s 里的文件都早于本次构建开始时间，构建可能没有输出到这里", p)
	}
	switch a.Type {
	case "python":
		if !exists(filepath.Join(p, "requirements.txt")) && exists(filepath.Join(p, "pyproject.toml")) && !a.Deps.Off {
			info.Notes = append(info.Notes, "没有 requirements.txt：服务器上不装依赖（用 uv export / poetry export 导出一份 requirements.txt）")
		}
	case "node":
		if !exists(filepath.Join(p, "package.json")) {
			info.Notes = append(info.Notes, "没有 package.json：服务器上不装依赖")
		}
	}
	return nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// PackDirExclude 把目录打成 tar.gz，跳过 exclude 匹配的；exec 里的文件（相对路径）标成可执行。
func PackDirExclude(dir, out string, exclude []string, exec map[string]bool) error {
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
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if excluded(rel, exclude) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("目录里不能有符号链接：%s（不需要上传的话写进 artifact.exclude）", p)
		}
		if d.IsDir() || d.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	if err == nil {
		sort.Strings(paths)
		for _, p := range paths {
			rel, _ := filepath.Rel(dir, p)
			if exec[filepath.ToSlash(rel)] {
				err = addTarFile(tw, p, filepath.ToSlash(rel), true)
			} else {
				err = addTar(tw, dir, p)
			}
			if err != nil {
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
