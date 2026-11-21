package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

//go:embed file_edit_linux.py
var fileEditProgram string

type editMetadata struct {
	Stamp   string `json:"stamp"`
	Links   int    `json:"links"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type FileCommand func(context.Context, string, []byte, func(string)) (RunResult, error)

// FileCommands lets an elevated SFTP client check and preserve metadata using
// exactly the same identity that will save the file.
func (c *FileClient) FileCommands(run FileCommand) {
	c.run = run
}

func (c *FileClient) editMetadata(ctx context.Context, operation string, paths []string, input []byte) (editMetadata, error) {
	var meta editMetadata
	if c.run == nil {
		return meta, errors.New("仅支持 SFTP 的连接无法验证并保留文件权限元数据")
	}
	args := []string{sudoQuote(fileEditProgram), sudoQuote(operation)}
	for _, p := range paths {
		args = append(args, sudoQuote(p))
	}
	queryCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	var out strings.Builder
	res, err := c.run(queryCtx, "/usr/bin/python3 -I -c "+strings.Join(args, " "), input, func(line string) { out.WriteString(line) })
	if err != nil {
		var sudo *SudoError
		if errors.As(err, &sudo) {
			switch sudo.Kind {
			case "sudo-policy-denied":
				return meta, &SudoError{Kind: sudo.Kind, Message: "服务器 sudoers 未授权检查和保留文件权限元数据"}
			case "sudo-auth-mode":
				return meta, &SudoError{Kind: sudo.Kind, Message: "当前 sudo 认证方式无法检查和保留文件权限元数据，请确认服务器授权"}
			}
		}
		return meta, err
	}
	if res.Code != 0 || json.Unmarshal([]byte(out.String()), &meta) != nil {
		return meta, errors.New("服务器无法运行文件权限元数据检查，需要允许执行 /usr/bin/python3")
	}
	switch meta.Kind {
	case "changed":
		return meta, &FileChangedError{}
	case "permission":
		return meta, FilePermissionError("保存 " + paths[0] + " 失败：没有权限")
	case "metadata":
		return meta, fmt.Errorf("无法安全保存 %s：%s", paths[0], meta.Message)
	case "":
		if meta.Stamp != "" {
			return meta, nil
		}
	}
	return meta, errors.New("服务器没有返回文件权限元数据")
}

func metadataRevision(target, digest, stamp string) string {
	sum := sha256.Sum256([]byte(target + "\x00" + digest + "\x00" + stamp))
	return hex.EncodeToString(sum[:])
}

// PreviewEdit keeps files readable on SFTP-only servers while making the
// unavailable metadata guarantee visible before allowing edits.
func (c *FileClient) PreviewEdit(ctx context.Context, p string) (*FilePreview, error) {
	doc, _, _, err := c.previewEdit(ctx, p)
	return doc, err
}

func (c *FileClient) previewEdit(ctx context.Context, p string) (*FilePreview, string, editMetadata, error) {
	var meta editMetadata
	s, err := c.sftpClient()
	if err != nil {
		return nil, "", meta, err
	}
	target, err := resolveFileTarget(s, p)
	if err != nil {
		return nil, "", meta, fileErr("读取", p, err)
	}
	doc, _, err := readSFTPPreview(ctx, s, p, target)
	if err != nil || doc.Revision == "" {
		return doc, target, meta, err
	}
	meta, err = c.editMetadata(ctx, "probe", []string{target}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, target, meta, ErrCanceled
		}
		doc.ReadOnly = err.Error()
	} else if meta.Links != 1 {
		doc.ReadOnly = "此文件有多个硬链接，替换后其他链接仍会指向旧内容，因此只能查看"
	} else {
		digest := doc.Revision
		doc.Revision = metadataRevision(target, digest, meta.Stamp)
	}
	return doc, target, meta, nil
}

// SaveEdit uses the server's metadata-aware transaction for live edits.
func (c *FileClient) SaveEdit(ctx context.Context, p string, edit FileEdit) (*FilePreview, error) {
	prepared, err := c.PrepareEdit(ctx, p, edit)
	if err != nil {
		return nil, err
	}
	return c.CommitEdit(ctx, prepared)
}

// PreparedEdit can be committed with another authorized identity while keeping
// the original path and version check.
type PreparedEdit struct {
	display, requested, target, stamp string
	content                           []byte
}

func (c *FileClient) PrepareEdit(ctx context.Context, p string, edit FileEdit) (*PreparedEdit, error) {
	if edit.Revision == "" {
		return nil, errors.New("缺少有效的文件版本，请重新读取文件后再保存")
	}
	original, target, meta, err := c.previewEdit(ctx, p)
	if err != nil {
		return nil, err
	}
	if original.ReadOnly != "" {
		if original.Revision != edit.Revision {
			return nil, &FileChangedError{}
		}
		return nil, errors.New(original.ReadOnly)
	}
	if original.Revision != edit.Revision {
		return nil, &FileChangedError{}
	}
	content, err := encodeFileEdit(edit.Content, original)
	if err != nil {
		return nil, err
	}
	requested := p
	if !path.IsAbs(p) {
		home, err := c.Home()
		if err != nil {
			return nil, err
		}
		requested = strings.TrimRight(home, "/") + "/" + p
	}
	return &PreparedEdit{p, requested, target, meta.Stamp, content}, nil
}

func (c *FileClient) CommitEdit(ctx context.Context, edit *PreparedEdit) (*FilePreview, error) {
	committed, err := c.editMetadata(ctx, "save", []string{edit.requested, edit.target, edit.stamp}, edit.content)
	if err != nil {
		return nil, err
	}
	out, _ := readFilePreview(bytes.NewReader(edit.content))
	out.Path, out.Size = edit.display, int64(len(edit.content))
	out.Revision = metadataRevision(edit.target, out.Revision, committed.Stamp)
	return out, nil
}
