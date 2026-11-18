package agent

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Node.js 和 Python 的官方包有一层顶级目录，.NET 的包直接从安装根目录开始。
func extractManagedRuntime(archive, dst, kind string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()

	type link struct {
		name, target string
		hard         bool
	}
	var links []link
	var expanded int64
	prefix := ""
	tr := tar.NewReader(gz)
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Size > 4<<30-expanded || entries >= 200_000 {
			return errors.New("运行环境解压大小或文件数量超出限制")
		}
		expanded += h.Size
		name := path.Clean(h.Name)
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		if !runtimeArchivePath(name) || h.Name != strings.TrimLeft(h.Name, "/") {
			return fmt.Errorf("不安全的归档路径：%s", h.Name)
		}
		if kind != "dotnet" {
			top, rest, _ := strings.Cut(name, "/")
			if prefix == "" {
				prefix = top
			}
			if top != prefix || (rest == "" && h.Typeflag != tar.TypeDir) {
				return errors.New("归档需要包含一个顶级安装目录")
			}
			if rest == "" {
				continue
			}
			name = rest
		}
		if name == managedRuntimeMarker {
			return errors.New("归档包含保留的 Ferry 运行环境记录")
		}
		if h.Typeflag == tar.TypeDir {
			if err := mkdirRuntimeArchiveDirs(root, name); err != nil {
				return err
			}
			continue
		}
		if err := mkdirRuntimeArchiveDirs(root, path.Dir(name)); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeReg:
			mode := os.FileMode(h.Mode)&0o755 | 0o600
			out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if err = errors.Join(err, out.Close()); err != nil {
				return err
			}
			if err := root.Chmod(name, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target := h.Linkname
			if path.IsAbs(target) || strings.Contains(target, "\\") ||
				!runtimeArchivePath(path.Join(path.Dir(name), target)) {
				return fmt.Errorf("不安全的归档链接：%s → %s", name, target)
			}
			links = append(links, link{name, target, false})
		case tar.TypeLink:
			target := path.Clean(h.Linkname)
			if prefix != "" {
				rel, ok := strings.CutPrefix(target, prefix+"/")
				if !ok {
					return fmt.Errorf("不安全的归档硬链接：%s → %s", name, target)
				}
				target = rel
			}
			if !runtimeArchivePath(target) {
				return fmt.Errorf("不安全的归档硬链接：%s → %s", name, target)
			}
			links = append(links, link{name, target, true})
		default:
			return fmt.Errorf("不支持的归档文件类型：%s", name)
		}
	}
	// 先建立硬链接再建立符号链接，确保文件内容与硬链接目标均来自归档内的普通文件。
	for _, l := range links {
		if !l.hard {
			continue
		}
		if st, err := root.Lstat(l.target); err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("归档硬链接的目标不是普通文件：%s", l.target)
		}
		if err := root.Link(l.target, l.name); err != nil {
			return err
		}
	}
	for _, l := range links {
		if !l.hard {
			if err := root.Symlink(l.target, l.name); err != nil {
				return err
			}
		}
	}
	for _, l := range links {
		if !l.hard {
			if _, err := root.Stat(l.name); err != nil {
				return fmt.Errorf("归档链接需要指向包内有效文件：%s → %s：%w", l.name, l.target, err)
			}
		}
	}
	// 读完 gzip 流并校验尾部，避免 tar 提前结束时接受已损坏的文件。
	_, err = io.CopyN(io.Discard, gz, 1<<20)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("归档尾部超出大小限制")
	}
	return err
}

func runtimeArchivePath(name string) bool {
	return name != "." && name != ".." && !strings.HasPrefix(name, "../") &&
		!path.IsAbs(name) && !strings.Contains(name, "\\")
}

// sshd 有时继承严格的 umask。无论归档内目录的先后顺序，应用运行用户都应能进入。
func mkdirRuntimeArchiveDirs(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	if err := root.Mkdir(name, 0o755); err == nil {
		return root.Chmod(name, 0o755)
	} else if errors.Is(err, os.ErrExist) {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := mkdirRuntimeArchiveDirs(root, path.Dir(name)); err != nil {
		return err
	}
	return mkdirRuntimeArchiveDirs(root, name)
}
