package swpkg

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type ArchivePreview struct {
	Files   []string `json:"files"`
	Entries int      `json:"entries"`
	Bundle  string   `json:"bundle,omitempty"` // ZIP 里的 rpm 或 deb 软件包
}

// InspectArchive 查看应用 strip 后的文件名，不执行或解压其中的程序。
func InspectArchive(ctx context.Context, file, format string, strip int) (ArchivePreview, error) {
	report := ArchivePreview{Files: []string{}}
	var rpm, deb, other int
	inspect := func(name string) {
		switch strings.ToLower(path.Ext(name)) {
		case ".rpm":
			rpm++
		case ".deb":
			deb++
		case ".txt", ".sha256", ".sha256sum", ".md", ".asc", ".sig":
		default:
			if !strings.HasPrefix(strings.ToLower(path.Base(name)), "sha256sums") {
				other++
			}
		}
		parts := strings.Split(path.Clean(strings.TrimPrefix(name, "./")), "/")
		if len(parts) <= strip {
			return
		}
		report.Entries++
		if len(report.Files) < 24 {
			report.Files = append(report.Files, strings.Join(parts[strip:], "/"))
		}
	}
	switch format {
	case "zip":
		z, err := zip.OpenReader(file)
		if err != nil {
			return report, err
		}
		defer z.Close()
		for _, entry := range z.File {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if entry.FileInfo().IsDir() {
				continue
			}
			inspect(entry.Name)
			switch strings.ToLower(path.Ext(entry.Name)) {
			case ".rpm":
				if !hasPackageHeader(entry, "\xed\xab\xee\xdb") {
					other++
				}
			case ".deb":
				if !hasPackageHeader(entry, "!<arch>\n") {
					other++
				}
			}
		}
		if other == 0 {
			switch {
			case rpm > 0 && deb == 0:
				report.Bundle = "rpm"
			case deb > 0 && rpm == 0:
				report.Bundle = "deb"
			}
		}
	case "tar.gz":
		f, err := os.Open(file)
		if err != nil {
			return report, err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return report, err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return report, err
			}
			if h.Typeflag != tar.TypeDir {
				inspect(h.Name)
			}
		}
	}
	return report, nil
}

func hasPackageHeader(entry *zip.File, magic string) bool {
	if !entry.Mode().IsRegular() {
		return false
	}
	r, err := entry.Open()
	if err != nil {
		return false
	}
	defer r.Close()
	head := make([]byte, len(magic))
	_, err = io.ReadFull(r, head)
	return err == nil && string(head) == magic
}

// CheckSystemBundle 检查「离线系统包」入口接收的 ZIP；普通导入仍可保存源码或其他归档。
func CheckSystemBundle(ctx context.Context, file, kind string) error {
	var needed string
	switch kind {
	case "rpm", "deb":
		needed = strings.ToUpper(kind)
	case "auto":
		needed = "RPM 或 DEB"
	default:
		return fmt.Errorf("离线包类型只能是 rpm、deb 或 auto")
	}
	name := filepath.Base(file)
	if !strings.EqualFold(filepath.Ext(file), ".zip") {
		return fmt.Errorf("%s 无法用于离线系统安装：请选择装有目标服务器所需 %s 软件包及依赖的 ZIP。源码需要先编译", name, needed)
	}
	preview, err := InspectArchive(ctx, file, "zip", 0)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%w", name, err)
	}
	if preview.Bundle == "" || (kind != "auto" && preview.Bundle != kind) {
		return fmt.Errorf("%s 需要包含有效的 %s 软件包才能在此处使用", name, needed)
	}
	return nil
}
