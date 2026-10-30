package engine

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/remote"
)

// 服务器上的文件（SFTP，以登录用户的身份）：列目录、预览、编辑保存、上传、下载、新建文件夹、改名、删除。
// 用连接池里的连接；「新建连接」的服务器只登录，不装 agent。

// DirListing 一个目录的内容。
type DirListing struct {
	Path    string             `json:"path"`
	Home    string             `json:"home"`
	Entries []remote.FileEntry `json:"entries"`
}

// ExistsError 要传的文件在那边已经有了：界面问过用户后带 --overwrite 重试。
type ExistsError struct {
	Names []string
}

func (e *ExistsError) Error() string {
	return fmt.Sprintf("已经存在：%s", strings.Join(e.Names, "、"))
}

// JSONDetail --json 输出的附加信息。
func (e *ExistsError) JSONDetail() map[string]any {
	return map[string]any{"kind": "files-exist", "names": e.Names}
}

func (e *Engine) withClient(target string, fn func(c *remote.Client) error) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	s, err := e.connect(t)
	if err != nil {
		return err
	}
	defer s.close()
	err = fn(s.c)
	if err != nil && !s.c.Alive() {
		s.broken.Store(true)
	}
	return err
}

// ListDir 列出目录；dir 为空时列登录用户的主目录。
func (e *Engine) ListDir(target, dir string) (*DirListing, error) {
	var out DirListing
	err := e.withClient(target, func(c *remote.Client) error {
		home, err := c.Home()
		if err != nil {
			return err
		}
		out.Home = home
		if dir == "" {
			dir = home
		}
		out.Path, out.Entries, err = c.ListDir(dir)
		return err
	})
	return &out, err
}

// PreviewFile 只读查看文本文件，不在服务器上安装 agent。
func (e *Engine) PreviewFile(target, p string) (*remote.FilePreview, error) {
	var out *remote.FilePreview
	err := e.withClient(target, func(c *remote.Client) error {
		var err error
		out, err = c.PreviewFile(e.ctx(), p)
		return err
	})
	return out, err
}

// SaveFile 保存完整文本，保留原编码、权限，拒绝覆盖已变化的版本。
func (e *Engine) SaveFile(target, p string, edit remote.FileEdit) (*remote.FilePreview, error) {
	var out *remote.FilePreview
	err := e.withClient(target, func(c *remote.Client) error {
		var err error
		out, err = c.SaveFile(e.ctx(), p, edit)
		return err
	})
	return out, err
}

// MakeDir 新建文件夹。
func (e *Engine) MakeDir(target, p string) error {
	return e.withClient(target, func(c *remote.Client) error { return c.Mkdir(p) })
}

// RenameFile 改名或移动。
func (e *Engine) RenameFile(target, from, to string) error {
	return e.withClient(target, func(c *remote.Client) error { return c.Rename(from, to) })
}

// RemoveFiles 删除文件或整个目录。
func (e *Engine) RemoveFiles(target string, paths []string) error {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" || path.Clean(p) == "/" {
			return fmt.Errorf("不能删除根目录")
		}
		quoted = append(quoted, shQuote(p))
	}
	return e.withClient(target, func(c *remote.Client) error {
		// 先用一条 rm -rf 删：SFTP 每个文件、目录都要一次往返，几百个文件的文件夹（如 JDK）经过隧道要好几分钟
		if _, res, err := c.Output("rm -rf -- " + strings.Join(quoted, " ")); err == nil && res.Code == 0 {
			return nil
		}
		// 没删干净（没权限），或这个账号不能执行命令（只开了 SFTP）：用 SFTP 删剩下的，出错时说清楚是哪个
		left, err := c.ExistsRemote(paths)
		if err != nil {
			return err
		}
		for _, p := range left {
			if err := c.RemoveAll(p); err != nil {
				return err
			}
		}
		return nil
	})
}

// Upload 把本机的文件、文件夹传到远程目录 dir；overwrite 为 false 时同名的先报出来（ExistsError）。
func (e *Engine) Upload(target string, locals []string, dir string, overwrite bool) error {
	for _, l := range locals {
		if _, err := os.Stat(l); err != nil {
			return fmt.Errorf("本机没有 %s", l)
		}
	}
	return e.withClient(target, func(c *remote.Client) error {
		if !overwrite {
			var names []string
			for _, l := range locals {
				names = append(names, path.Join(dir, filepath.Base(l)))
			}
			exist, err := c.ExistsRemote(names)
			if err != nil {
				return err
			}
			if len(exist) > 0 {
				return &ExistsError{Names: baseNames(exist)}
			}
		}
		return c.UploadTree(e.ctx(), locals, dir, func(file string, done, total int64) {
			e.Rep.Progress(file, done, total)
		})
	})
}

// Download 把远程的文件、目录下载到本机目录 dir；overwrite 为 false 时同名的先报出来（ExistsError）。
func (e *Engine) Download(target string, remotes []string, dir string, overwrite bool) error {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("本机没有文件夹 %s", dir)
	}
	if !overwrite {
		var exist []string
		for _, r := range remotes {
			if _, err := os.Lstat(filepath.Join(dir, path.Base(r))); err == nil {
				exist = append(exist, path.Base(r))
			}
		}
		if len(exist) > 0 {
			return &ExistsError{Names: exist}
		}
	}
	return e.withClient(target, func(c *remote.Client) error {
		return c.DownloadTree(e.ctx(), remotes, dir, func(file string, done, total int64) {
			e.Rep.Progress(file, done, total)
		})
	})
}

func baseNames(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = path.Base(p)
	}
	return out
}
