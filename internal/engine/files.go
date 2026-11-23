package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/remote"
)

// 服务器上的文件默认以登录用户的身份操作。新建文件夹及选择自动提权的保存可以使用目标配置的 sudo 重试。
// 用连接池里的连接；「新建连接」的服务器只登录，不装 agent。

// DirListing 一个目录的内容。
type DirListing struct {
	Path     string             `json:"path"`
	Home     string             `json:"home"`
	Entries  []remote.FileEntry `json:"entries"`
	Focus    string             `json:"focus,omitempty"`
	Identity string             `json:"identity"`
	CanSudo  bool               `json:"canSudo"`
	Login    string             `json:"login"`
	RunAs    string             `json:"runAs,omitempty"`
}

// ExistsError 要传的文件在那边已经有了：界面问过用户后带 --overwrite 重试。
type ExistsError struct {
	Names []string
}

type fileAccessError struct{ cause error }

func (e *fileAccessError) Error() string { return e.cause.Error() }
func (e *fileAccessError) Unwrap() error { return e.cause }
func (e *fileAccessError) JSONDetail() map[string]any {
	return map[string]any{"kind": "files-permission", "canSudo": true}
}

func (s *session) explainFileError(err error) error {
	var denied remote.FilePermissionError
	if !errors.As(err, &denied) {
		return err
	}
	if s.fileIdentity == "sudo:root" {
		return fmt.Errorf("%w（以 sudo 身份操作）", err)
	}
	err = fmt.Errorf("%w（以登录用户的身份操作）", err)
	if s.canSudoFiles() {
		return &fileAccessError{err}
	}
	return err
}

func (e *ExistsError) Error() string {
	return fmt.Sprintf("已经存在：%s", strings.Join(e.Names, "、"))
}

// JSONDetail --json 输出的附加信息。
func (e *ExistsError) JSONDetail() map[string]any {
	return map[string]any{"kind": "files-exist", "names": e.Names}
}

func (s *session) canSudoFiles() bool {
	return s.allowsSudo() && (s.t.Sudo || s.t.SudoAuth != "")
}

func (s *session) fileClient(asSudo bool) (*remote.FileClient, func(), error) {
	return s.openFileClient(asSudo, false)
}

func (s *session) openFileClient(asSudo, readOnly bool) (*remote.FileClient, func(), error) {
	s.fileIdentity = s.t.User
	if !asSudo || s.t.User == "root" {
		return s.c.Files(), func() {}, nil
	}
	if !s.canSudoFiles() {
		return nil, nil, errors.New("此服务器的文件提权未启用，请在目标或连接中设置 sudo 认证")
	}
	s.fileIdentity = "sudo:root"
	// Only invoke an OpenSSH server installed in a system directory. 'internal-sftp'
	// is an sshd subsystem and cannot be started as an external executable.
	ctx, cancel := context.WithTimeout(s.e.ctx(), 15*time.Second)
	defer cancel()
	var server string
	if cached := s.sftpServer.Load(); cached != nil {
		server = *cached
	} else {
		cmd := `for p in /usr/libexec/openssh/sftp-server /usr/lib/openssh/sftp-server /usr/lib/ssh/sftp-server; do if [ -x "$p" ]; then printf '%s\n' "$p"; exit 0; fi; done; exit 1`
		res, err := s.c.RunContext(ctx, cmd, nil, func(line string) { server = line })
		if err != nil {
			return nil, nil, err
		}
		if res.Code != 0 || (server != "/usr/libexec/openssh/sftp-server" && server != "/usr/lib/openssh/sftp-server" && server != "/usr/lib/ssh/sftp-server") {
			return nil, nil, errors.New("服务器未找到可执行的 OpenSSH sftp-server，无法打开提权文件会话")
		}
		s.sftpServer.Store(&server)
	}
	var pipe *remote.Pipe
	var err error
	if readOnly {
		server += " -R"
	}
	if s.passwordSudo() {
		pipe, err = s.c.StartSudoPipe(s.e.ctx(), "exec "+server, s.sudoPassword)
	} else {
		pipe, err = s.c.StartPipeContext(ctx, s.sudo+"-- "+server)
	}
	if err != nil {
		return nil, nil, s.sudoError(err)
	}
	files, closeFiles, err := remote.FilesOverPipe(s.e.ctx(), pipe)
	if err == nil && !readOnly {
		files.FileCommands(s.runPriv)
	}
	return files, closeFiles, err
}

func (e *Engine) withClient(target string, fn func(f *remote.FileClient, s *session) error) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	s, err := e.connectSSH(t)
	if err != nil {
		return err
	}
	defer s.close()
	f, closeFiles, err := s.fileClient(e.FileSudo)
	if err == nil {
		defer closeFiles()
		err = fn(f, s)
	}
	if err != nil && !s.c.Alive() {
		s.broken.Store(true)
	}
	return s.explainFileError(err)
}

// ListDir 列出目录；空路径使用登录用户的主目录，文件路径打开所在目录并返回定位项。
func (e *Engine) ListDir(target, dir string) (*DirListing, error) {
	var out DirListing
	err := e.withClient(target, func(c *remote.FileClient, s *session) error {
		home, err := c.Home()
		if err != nil {
			return err
		}
		out.Home = home
		out.Identity, out.CanSudo = s.fileIdentity, s.canSudoFiles()
		out.Login, out.RunAs = s.t.User, s.t.RunAs
		if dir == "" {
			dir = home
		}
		dir, out.Focus, err = c.FileLocation(dir)
		if err != nil {
			return err
		}
		out.Path, out.Entries, err = c.ListDir(dir)
		return err
	})
	return &out, err
}

// PreviewFile 只读查看文本文件，不在服务器上安装 agent。
func (e *Engine) PreviewFile(target, p string) (*remote.FilePreview, error) {
	var out *remote.FilePreview
	err := e.withClient(target, func(c *remote.FileClient, _ *session) error {
		var err error
		out, err = c.PreviewEdit(e.ctx(), p)
		return err
	})
	return out, err
}

// FileSize 按需统计文件或目录大小；连接管理与文件浏览相同，不安装 agent。
func (e *Engine) FileSize(target, p string) (*remote.FileSize, error) {
	var out *remote.FileSize
	err := e.withClient(target, func(c *remote.FileClient, _ *session) error {
		var err error
		out, err = c.FileSize(e.ctx(), p)
		return err
	})
	return out, err
}

// SaveFile 保存完整文本，保留原编码、权限，拒绝覆盖已变化的版本。
func (e *Engine) SaveFile(target, p string, edit remote.FileEdit, sudoIfNeeded bool) (*remote.FilePreview, error) {
	ctx := e.ctx()
	var out *remote.FilePreview
	err := e.withChange(target, "file-save", p, fmt.Sprintf("%d 字节", len(edit.Content)), func(s *session, c *remote.FileClient) error {
		defer func() {
			if out != nil {
				out.Identity = s.fileIdentity
			}
		}()
		prepared, err := c.PrepareEdit(ctx, p, edit)
		if err == nil {
			out, err = c.CommitEdit(ctx, prepared)
		}
		var denied remote.FilePermissionError
		if !sudoIfNeeded || e.FileSudo || !s.canSudoFiles() || !errors.As(err, &denied) {
			return err
		}
		if ctx.Err() != nil {
			return remote.ErrCanceled
		}
		e.Rep.Step("文件写入权限不足，正在以 sudo 保存")
		if prepared != nil {
			s.fileIdentity = "sudo:root"
			elevated := s.c.Files()
			elevated.FileCommands(s.runPriv)
			out, err = elevated.CommitEdit(ctx, prepared)
			return err
		}
		elevated, closeFiles, err := s.fileClient(true)
		if err != nil {
			return err
		}
		defer closeFiles()
		// SaveFile only returns a permission error before committing the replacement.
		// Reuse the original edit so the elevated attempt also checks its revision.
		out, err = elevated.SaveEdit(ctx, p, edit)
		return err
	})
	return out, err
}

// MakeDir 新建文件夹。
func (e *Engine) MakeDir(target, p string) error {
	return e.MakeDirFor(target, p, "")
}

func (e *Engine) MakeDirFor(target, p, owner string) error {
	return e.withChange(target, "file-mkdir", p, "", func(s *session, c *remote.FileClient) error {
		if owner != "" {
			if !e.FileSudo && s.t.User != "root" {
				return errors.New("请先切换到 sudo，再指定新目录属主")
			}
			user := s.t.User
			switch owner {
			case "login":
			case "run-as":
				user = s.t.RunAs
			default:
				return errors.New("目录属主只能选择 login 或 run-as")
			}
			if user == "" {
				return errors.New("这个目标未设置默认运行用户")
			}
			var ids []string
			q := shQuote(user)
			res, err := s.c.RunContext(e.ctx(), "id -u -- "+q+" && id -g -- "+q, nil, func(line string) { ids = append(ids, line) })
			if err != nil {
				return err
			}
			if res.Code != 0 || len(ids) != 2 {
				return fmt.Errorf("无法读取服务器用户 %s 的 UID/GID：%s", user, strings.TrimSpace(res.Stderr))
			}
			uid, uidErr := strconv.Atoi(ids[0])
			gid, gidErr := strconv.Atoi(ids[1])
			if uidErr != nil || gidErr != nil || uid < 0 || gid < 0 {
				return errors.New("服务器返回了无效的 UID/GID")
			}
			return c.MkdirOwned(p, uid, gid)
		}
		err := c.Mkdir(p)
		if e.FileSudo || !errors.Is(err, os.ErrPermission) || !s.canSudoFiles() || p == "" || strings.ContainsRune(p, 0) {
			return err
		}
		remotePath := p
		if !path.IsAbs(p) {
			home, err := c.Home()
			if err != nil {
				return err
			}
			remotePath = strings.TrimRight(home, "/") + "/" + p
		}
		elevated, closeFiles, err := s.fileClient(true)
		if err != nil {
			return err
		}
		defer closeFiles()
		return elevated.Mkdir(remotePath)
	})
}

type FileAccess struct {
	Identity    string `json:"identity"`
	Exists      bool   `json:"exists"`
	Read        bool   `json:"read"`
	Write       bool   `json:"write"`
	Execute     bool   `json:"execute"`
	ParentWrite *bool  `json:"parentWrite,omitempty"`
}

func (e *Engine) FileAccess(target, p string) (FileAccess, error) {
	var out FileAccess
	if !path.IsAbs(p) || strings.ContainsRune(p, 0) {
		return out, errors.New("检查权限需要服务器上的绝对路径")
	}
	t, err := e.File.Target(target)
	if err != nil {
		return out, err
	}
	s, err := e.connectSSH(t)
	if err != nil {
		return out, err
	}
	defer s.close()
	out.Identity = t.User
	run := s.c.RunContext
	if e.FileSudo && t.User != "root" {
		if !s.canSudoFiles() {
			return out, errors.New("此目标未启用文件提权")
		}
		out.Identity = "sudo:root"
		run = s.runPriv
	}
	var checks []string
	script := "p=" + shQuote(p) + `; for flag in e r w x; do if test "-$flag" "$p"; then printf '%s\n' "$flag"; fi; done
if test -f "$p" && resolved=$(readlink -f -- "$p"); then
  parent=${resolved%/*}
  if test -w "${parent:-/}" && test -x "${parent:-/}"; then echo parent-yes; else echo parent-no; fi
fi`
	res, err := run(e.ctx(), "sh -c "+shQuote(script), nil, func(line string) { checks = append(checks, line) })
	if err != nil {
		return out, err
	}
	if res.Code != 0 {
		return out, fmt.Errorf("检查服务器文件权限失败：%s", strings.TrimSpace(res.Stderr))
	}
	for _, check := range checks {
		switch check {
		case "e":
			out.Exists = true
		case "r":
			out.Read = true
		case "w":
			out.Write = true
		case "x":
			out.Execute = true
		case "parent-yes", "parent-no":
			writable := check == "parent-yes"
			out.ParentWrite = &writable
		}
	}
	return out, nil
}

// RenameFile 改名或移动。
func (e *Engine) RenameFile(target, from, to string) error {
	return e.withChange(target, "file-rename", from, "改成 "+to, func(_ *session, c *remote.FileClient) error { return c.Rename(from, to) })
}

// CompressFiles packs files in the same directory into a new ZIP.
func (e *Engine) CompressFiles(target string, sources []string, destination string) error {
	return e.withChange(target, "file-compress", destination, strings.Join(baseNames(sources), "、"), func(_ *session, c *remote.FileClient) error {
		return c.CompressFiles(e.ctx(), sources, destination, e.archiveProgress)
	})
}

// ExtractArchive writes the contents of an archive into a new directory.
func (e *Engine) ExtractArchive(target, source, destination string) error {
	return e.withChange(target, "file-extract", source, destination, func(_ *session, c *remote.FileClient) error {
		return c.ExtractArchive(e.ctx(), source, destination, e.archiveProgress)
	})
}

func (e *Engine) archiveProgress(name string) {
	e.Rep.Progress(name, 0, 0)
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
	return e.withChange(target, "file-remove", strings.Join(paths, "、"), "", func(s *session, c *remote.FileClient) error {
		if e.FileSudo {
			var failures []error
			var completed []string
			for _, p := range paths {
				if err := e.ctx().Err(); err != nil {
					failures = append(failures, err)
					break
				}
				if err := c.RemoveAll(p); err != nil {
					failures = append(failures, err)
				} else {
					completed = append(completed, p)
				}
			}
			if len(failures) > 0 && len(completed) > 0 {
				return fmt.Errorf("已删除 %s，其他项处理失败：%w", strings.Join(completed, "、"), errors.Join(failures...))
			}
			return errors.Join(failures...)
		}
		// 先用一条 rm -rf 删：SFTP 每个文件、目录都要一次往返，几百个文件的文件夹（如 JDK）经过隧道要好几分钟
		if _, res, err := s.c.Output("rm -rf -- " + strings.Join(quoted, " ")); err == nil && res.Code == 0 {
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
	return e.withChange(target, "file-upload", dir, uploadDetail(locals, overwrite), func(_ *session, c *remote.FileClient) error {
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
		return c.UploadTree(e.ctx(), locals, dir, overwrite, func(file string, done, total int64) {
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
	return e.withClient(target, func(c *remote.FileClient, _ *session) error {
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

// uploadDetail 审计里记传了哪些（本机的文件名、总大小），不记内容。
func uploadDetail(locals []string, overwrite bool) string {
	var names []string
	var size int64
	for _, l := range locals {
		names = append(names, filepath.Base(l))
		filepath.Walk(l, func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				size += fi.Size()
			}
			return nil
		})
	}
	d := strings.Join(names, "、") + "，共 " + humanBytes(size)
	if overwrite {
		d += "，覆盖同名的"
	}
	return d
}
