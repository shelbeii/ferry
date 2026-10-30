package remote

import (
	"context"
	"fmt"
	"github.com/captain/ferry/internal/filetext"
	"io"
)

const FilePreviewLimit = filetext.Limit

type FilePreview = filetext.Document

// PreviewFile 以登录用户的身份只读预览普通文件（允许指向普通文件的链接）。
func (c *Client) PreviewFile(ctx context.Context, p string) (*FilePreview, error) {
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}
	s, err := c.sftpClient()
	if err != nil {
		return nil, err
	}
	target, err := resolveFileTarget(s, p)
	if err != nil {
		return nil, fileErr("读取", p, err)
	}
	// 在打开之前排除目录、设备、管道，避免在特殊文件上一直等待读取。
	st, err := s.Stat(target)
	if err != nil {
		return nil, fileErr("读取", p, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s：仅支持查看普通文件的内容", p)
	}
	src, err := s.Open(target)
	if err != nil {
		return nil, fileErr("读取", p, err)
	}
	stop := context.AfterFunc(ctx, func() { src.Close() })
	defer stop()
	defer src.Close()
	st, err = src.Stat()
	if err == nil && !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s：仅支持查看普通文件的内容", p)
	}
	if err != nil {
		return nil, fileErr("读取", p, err)
	}
	out, err := readFilePreview(src)
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}
	if err != nil {
		return nil, fileErr("读取", p, err)
	}
	out.Path, out.Size = p, st.Size()
	if out.Revision != "" {
		out.Revision = fileRevision(target, out.Revision, st)
	}
	return out, nil
}

func readFilePreview(r io.Reader) (*FilePreview, error)           { return filetext.Decode(r) }
func previewText(b []byte, truncated bool) (string, string, bool) { return filetext.Text(b, truncated) }
