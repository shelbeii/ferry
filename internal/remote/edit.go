package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/captain/ferry/internal/filetext"
	"github.com/pkg/sftp"
)

// FileEdit 通过 sidecar 的请求环境传入，文件内容不放进命令行参数。
type FileEdit struct {
	Revision string `json:"revision"`
	Content  string `json:"content"`
}

type FileChangedError struct{}

func (*FileChangedError) Error() string {
	return "服务器上的文件已发生变化，尚未保存。请先复制需要保留的修改，再重新读取文件。"
}

func (*FileChangedError) JSONDetail() map[string]any {
	return map[string]any{"kind": "file-changed"}
}

// resolveFileTarget 保留文件链接，编辑时替换它指向的文件。
func resolveFileTarget(s *sftp.Client, p string) (string, error) {
	p, err := s.RealPath(p)
	if err != nil {
		return "", err
	}
	for range 40 {
		st, err := s.Lstat(p)
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink == 0 {
			return p, nil
		}
		link, err := s.ReadLink(p)
		if err != nil {
			return "", err
		}
		if !path.IsAbs(link) {
			link = path.Join(path.Dir(p), link)
		}
		p, err = s.RealPath(link)
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("文件链接过多，无法定位目标")
}

func fileRevision(target, digest string, st os.FileInfo) string {
	metadata := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", target, digest, st.Size(), st.ModTime().Unix(), st.Mode())
	if stat, ok := st.Sys().(*sftp.FileStat); ok {
		metadata += fmt.Sprintf("\x00%d:%d", stat.UID, stat.GID)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(metadata)))
}

func encodeFileEdit(text string, original *FilePreview) ([]byte, error) {
	return filetext.Encode(text, original)
}

// SaveFile 在同目录写完临时文件，再原子替换；保存前再次核对内容指纹。
func (c *Client) SaveFile(ctx context.Context, p string, edit FileEdit) (*FilePreview, error) {
	if b, err := hex.DecodeString(edit.Revision); err != nil || len(b) != sha256.Size {
		return nil, errors.New("缺少有效的文件版本，请重新读取文件后再保存")
	}
	original, err := c.PreviewFile(ctx, p)
	if err != nil {
		return nil, err
	}
	if original.Truncated || original.Unsupported != "" {
		return nil, errors.New("仅支持编辑完整加载的文本文件，最大 1 MiB")
	}
	if original.Revision != edit.Revision {
		return nil, &FileChangedError{}
	}
	content, err := encodeFileEdit(edit.Content, original)
	if err != nil {
		return nil, err
	}
	s, err := c.sftpClient()
	if err != nil {
		return nil, err
	}
	if _, ok := s.HasExtension("posix-rename@openssh.com"); !ok {
		return nil, errors.New("服务器 SFTP 不支持原子替换文件，无法安全保存；请下载后编辑并上传")
	}
	target, err := resolveFileTarget(s, p)
	if err != nil {
		return nil, fileErr("保存", p, err)
	}
	st, err := s.Stat(target)
	if err != nil {
		return nil, fileErr("保存", p, err)
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("仅支持保存普通文本文件")
	}
	// 核对原文件的写权限，避免仅凭父目录的写权限替换只读文件。
	check, err := s.OpenFile(target, os.O_WRONLY)
	if err != nil {
		return nil, fileErr("保存", p, err)
	}
	if err := check.Close(); err != nil {
		return nil, fileErr("保存", p, err)
	}
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	tmp := path.Join(path.Dir(target), ".ferry-edit-"+hex.EncodeToString(token[:])+".tmp")
	dst, err := s.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return nil, fileErr("创建保存临时文件", p, err)
	}
	defer s.Remove(tmp)
	defer dst.Close()
	stop := context.AfterFunc(ctx, func() { dst.Close() })
	defer stop()
	if err := dst.Chmod(0o600); err != nil {
		return nil, fileErr("设置临时文件权限", p, err)
	}
	if _, err := io.Copy(dst, bytes.NewReader(content)); err != nil {
		if ctx.Err() != nil {
			return nil, ErrCanceled
		}
		return nil, fileErr("保存", p, err)
	}
	tmpInfo, err := dst.Stat()
	if err != nil {
		return nil, fileErr("读取保存结果", p, err)
	}
	if owner, ok := st.Sys().(*sftp.FileStat); ok {
		current, ok := tmpInfo.Sys().(*sftp.FileStat)
		if !ok || current.UID != owner.UID || current.GID != owner.GID {
			if err := dst.Chown(int(owner.UID), int(owner.GID)); err != nil {
				return nil, fileErr("保留文件所有者", p, err)
			}
		}
	}
	if err := dst.Chmod(st.Mode()); err != nil {
		return nil, fileErr("保留文件权限", p, err)
	}
	if _, ok := s.HasExtension("fsync@openssh.com"); ok {
		if err := dst.Sync(); err != nil {
			return nil, fileErr("同步保存内容", p, err)
		}
	}
	tmpInfo, err = dst.Stat()
	if err != nil {
		return nil, fileErr("读取保存结果", p, err)
	}
	if err := dst.Close(); err != nil {
		return nil, fileErr("完成保存", p, err)
	}
	// 写临时文件期间也可能有人改过原文件；校验通过才提交替换。
	latest, err := c.PreviewFile(ctx, p)
	if err != nil {
		return nil, err
	}
	if latest.Revision != edit.Revision {
		return nil, &FileChangedError{}
	}
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}
	if err := s.PosixRename(tmp, target); err != nil {
		return nil, fileErr("替换文件", p, err)
	}
	// 提交后返回已经写入的内容，不因额外读取失败而把成功保存报告成失败。
	out, _ := readFilePreview(bytes.NewReader(content))
	out.Path, out.Size = p, int64(len(content))
	out.Revision = fileRevision(target, out.Revision, tmpInfo)
	return out, nil
}
