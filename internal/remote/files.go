package remote

import (
	"context"
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
)

// FileEntry 远程目录里的一项。
type FileEntry struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Mode  string    `json:"mode"` // 如 drwxr-xr-x
	Dir   bool      `json:"dir"`  // 目录（含指向目录的链接）
	Link  bool      `json:"link,omitempty"`
	MTime time.Time `json:"mtime"`
}

// Home 登录用户的主目录（SFTP 会话的起始目录）。
func (c *Client) Home() (string, error) {
	s, err := c.sftpClient()
	if err != nil {
		return "", err
	}
	return s.Getwd()
}

// ListDir 列出远程目录：目录在前，各自按名称排序。返回规范化后的绝对路径。
func (c *Client) ListDir(dir string) (string, []FileEntry, error) {
	s, err := c.sftpClient()
	if err != nil {
		return "", nil, err
	}
	if real, err := s.RealPath(dir); err == nil {
		dir = real
	}
	infos, err := s.ReadDir(dir)
	if err != nil {
		return dir, nil, fileErr("打开目录", dir, err)
	}
	out := make([]FileEntry, 0, len(infos))
	for _, fi := range infos {
		e := FileEntry{Name: fi.Name(), Size: fi.Size(), Mode: fi.Mode().String(), Dir: fi.IsDir(), MTime: fi.ModTime()}
		if fi.Mode()&fs.ModeSymlink != 0 {
			e.Link = true
			if st, err := s.Stat(path.Join(dir, fi.Name())); err == nil {
				e.Dir, e.Size = st.IsDir(), st.Size()
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return dir, out, nil
}

// Mkdir 建远程目录。
func (c *Client) Mkdir(p string) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	return fileErr("新建文件夹", p, s.Mkdir(p))
}

// Rename 改名或移动（目标已存在时报错）。
func (c *Client) Rename(from, to string) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	if _, err := s.Lstat(to); err == nil {
		return fmt.Errorf("%s 已经存在", to)
	}
	return fileErr("改名", from, s.Rename(from, to))
}

// RemoveAll 删除远程文件或整个目录。
func (c *Client) RemoveAll(p string) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	if p == "/" || p == "" {
		return errors.New("不能删除根目录")
	}
	if _, err := s.Lstat(p); err != nil {
		return fileErr("删除", p, err)
	}
	return fileErr("删除", p, s.RemoveAll(p))
}

// ExistsRemote 这些路径里已经存在的（上传前检查会不会覆盖）。
func (c *Client) ExistsRemote(paths []string) ([]string, error) {
	s, err := c.sftpClient()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range paths {
		if _, err := s.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// TransferProgress 传输进度：当前文件（相对路径）、全部已传字节、全部字节。
type TransferProgress func(file string, done, total int64)

// UploadTree 把本机的文件、文件夹（整个）传到远程目录 dir 下；同名的覆盖（保留原来的权限）。
func (c *Client) UploadTree(ctx context.Context, locals []string, dir string, progress TransferProgress) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	type item struct {
		local, remote string
		size          int64
		dir           bool
	}
	var items []item
	var total int64
	for _, l := range locals {
		l = filepath.Clean(l)
		base := filepath.Dir(l)
		err := filepath.WalkDir(l, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(base, p)
			it := item{local: p, remote: path.Join(dir, filepath.ToSlash(rel)), dir: d.IsDir()}
			if !d.IsDir() {
				if !d.Type().IsRegular() {
					return nil // 链接、设备等跳过
				}
				fi, err := d.Info()
				if err != nil {
					return err
				}
				it.size = fi.Size()
				total += it.size
			}
			items = append(items, it)
			return nil
		})
		if err != nil {
			return fmt.Errorf("读取 %s：%w", l, err)
		}
	}
	var done int64
	for _, it := range items {
		if ctx.Err() != nil {
			return ErrCanceled
		}
		if it.dir {
			if err := s.MkdirAll(it.remote); err != nil {
				return fileErr("新建文件夹", it.remote, err)
			}
			continue
		}
		mode := os.FileMode(0o644)
		if st, err := s.Stat(it.remote); err == nil {
			mode = st.Mode().Perm() // 覆盖时保留原来的权限
		}
		name := strings.TrimPrefix(strings.TrimPrefix(it.remote, dir), "/")
		start := done
		err := c.UploadContext(ctx, it.local, it.remote, mode, func(d, _ int64) {
			if progress != nil {
				progress(name, start+d, total)
			}
		})
		if err != nil {
			if errors.Is(err, ErrCanceled) {
				return err
			}
			return fileErr("上传", it.remote, err)
		}
		done += it.size
		if progress != nil {
			progress(name, done, total)
		}
	}
	return nil
}

// DownloadTree 把远程的文件、目录（整个）下载到本机目录 dir 下；同名的覆盖。先写 .part，完整后改名。
func (c *Client) DownloadTree(ctx context.Context, remotes []string, dir string, progress TransferProgress) error {
	s, err := c.sftpClient()
	if err != nil {
		return err
	}
	type item struct {
		remote, local string
		size          int64
		dir           bool
	}
	var items []item
	var total int64
	for _, r := range remotes {
		r = path.Clean(r)
		base := path.Dir(r)
		st, err := s.Stat(r)
		if err != nil {
			return fileErr("读取", r, err)
		}
		if !st.IsDir() {
			items = append(items, item{remote: r, local: filepath.Join(dir, path.Base(r)), size: st.Size()})
			total += st.Size()
			continue
		}
		w := s.Walk(r)
		for w.Step() {
			if err := w.Err(); err != nil {
				return fileErr("读取", w.Path(), err)
			}
			fi := w.Stat()
			rel := strings.TrimPrefix(strings.TrimPrefix(w.Path(), base), "/")
			it := item{remote: w.Path(), local: filepath.Join(dir, filepath.FromSlash(rel))}
			switch {
			case fi.IsDir():
				it.dir = true
			case fi.Mode()&fs.ModeSymlink != 0:
				// 目录里的链接：指向文件的照常下载，指向目录的跳过（避免绕圈）
				t, err := s.Stat(w.Path())
				if err != nil || !t.Mode().IsRegular() {
					continue
				}
				it.size = t.Size()
			case !fi.Mode().IsRegular():
				continue
			default:
				it.size = fi.Size()
			}
			total += it.size
			items = append(items, it)
		}
	}
	var done int64
	for _, it := range items {
		if ctx.Err() != nil {
			return ErrCanceled
		}
		if it.dir {
			if err := os.MkdirAll(it.local, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(it.local), 0o755); err != nil {
			return err
		}
		name := strings.TrimPrefix(strings.TrimPrefix(it.remote, path.Dir(remotesBase(remotes, it.remote))), "/")
		start := done
		n, err := c.downloadFile(ctx, it.remote, it.local, func(d int64) {
			if progress != nil {
				progress(name, start+d, total)
			}
		})
		if err != nil {
			return err
		}
		done += n
		if progress != nil {
			progress(name, done, total)
		}
	}
	return nil
}

// remotesBase it 属于 remotes 里的哪一个（显示相对路径用）。
func remotesBase(remotes []string, it string) string {
	for _, r := range remotes {
		r = path.Clean(r)
		if it == r || strings.HasPrefix(it, r+"/") {
			return r
		}
	}
	return it
}

func (c *Client) downloadFile(ctx context.Context, remotePath, local string, progress func(done int64)) (int64, error) {
	s, err := c.sftpClient()
	if err != nil {
		return 0, err
	}
	src, err := s.Open(remotePath)
	if err != nil {
		return 0, fileErr("下载", remotePath, err)
	}
	stop := context.AfterFunc(ctx, func() { src.Close() })
	defer stop()
	defer src.Close()
	tmp := local + ".part"
	dst, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	w := &progressWriter{w: dst, progress: progress}
	n, err := src.WriteTo(w)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		if ctx.Err() != nil {
			return n, ErrCanceled
		}
		return n, fileErr("下载", remotePath, err)
	}
	if err := os.Rename(tmp, local); err != nil {
		os.Remove(tmp)
		return n, err
	}
	return n, nil
}

type progressWriter struct {
	w        io.Writer
	done     int64
	last     time.Time
	progress func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.progress != nil && time.Since(p.last) > 200*time.Millisecond {
		p.last = time.Now()
		p.progress(p.done)
	}
	return n, err
}

// fileErr 把 SFTP 的错误换成看得懂的话。
func fileErr(op, p string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case errors.Is(err, fs.ErrPermission) || strings.Contains(msg, "permission denied"):
		return fmt.Errorf("%s %s 失败：没有权限（以登录用户的身份操作）", op, p)
	case errors.Is(err, fs.ErrNotExist) || strings.Contains(msg, "not exist") || strings.Contains(msg, "no such file"):
		return fmt.Errorf("%s %s 失败：不存在", op, p)
	}
	return fmt.Errorf("%s %s 失败：%v", op, p, err)
}
