package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
)

// FileSize 文件或目录的逻辑大小；目录包含目录项和链接本身，内部符号链接不展开。
type FileSize struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Dir   bool   `json:"dir"`
}

func fileSizeCommand(p string) string {
	quoted := "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	// 远端也设时限：SSH 通道关闭后仍未结束的 du 不会一直运行。
	// -b 为逻辑字节数；-l 让硬链接按目录项计数，与 SFTP 回退一致。
	return "LC_ALL=C timeout -k 5s 120s du -sblP -- " + quoted
}

func parseFileSize(out string) (int64, error) {
	first, _, ok := strings.Cut(out, "\t")
	if !ok {
		return 0, errors.New("服务器未返回有效的大小统计")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("服务器返回的大小统计无效")
	}
	return n, nil
}

// FileSize 以登录用户的权限按需统计，不读取文件内容、不安装 agent。
// 选中的链接定位到其目标；目录内部的链接只计链接自身，避免循环和越界统计。
func (c *Client) FileSize(parent context.Context, p string) (*FileSize, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	canceled := func() error {
		if parent.Err() != nil {
			return ErrCanceled
		}
		if ctx.Err() != nil {
			return errors.New("大小统计超过 2 分钟，请选择子目录后重试")
		}
		return nil
	}
	if err := canceled(); err != nil {
		return nil, err
	}
	if p == "" || strings.ContainsRune(p, '\x00') {
		return nil, errors.New("请提供有效的文件或目录路径")
	}
	s, err := c.sftpClient()
	if err != nil {
		return nil, err
	}
	target, err := resolveFileTarget(s, p)
	if err != nil {
		return nil, fileErr("统计大小", p, err)
	}
	st, err := s.Stat(target)
	if stopped := canceled(); stopped != nil {
		return nil, stopped
	}
	if err != nil {
		return nil, fileErr("统计大小", p, err)
	}
	result := &FileSize{Path: p, Bytes: st.Size(), Dir: st.IsDir()}
	if !st.IsDir() {
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s：仅支持统计普通文件或目录", p)
		}
		return result, nil
	}
	if c.ssh != nil && path.IsAbs(target) {
		var out bytes.Buffer
		limited := &limitedBuffer{buf: &out, max: 8192}
		r, err := c.RunContext(ctx, fileSizeCommand(target), nil, func(line string) {
			fmt.Fprintln(limited, line)
		})
		if err := canceled(); err != nil {
			return nil, err
		}
		if err == nil && r.Code == 0 {
			if n, err := parseFileSize(out.String()); err == nil {
				result.Bytes = n
				return result, nil
			}
		}
		// 没有 du、不能执行命令或命令未完整统计成功：通过 SFTP 读取目录元数据。
	}
	dirs := []string{target}
	visited := 1
	for len(dirs) > 0 {
		if err := canceled(); err != nil {
			return nil, err
		}
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		entries, err := s.ReadDirContext(ctx, dir)
		if stopped := canceled(); stopped != nil {
			return nil, stopped
		}
		if err != nil {
			return nil, fileErr("统计大小", dir, err)
		}
		for _, entry := range entries {
			visited++
			if visited > 250000 {
				return nil, errors.New("目录条目过多，请选择子目录后查看大小")
			}
			if entry.Size() < 0 || result.Bytes > math.MaxInt64-entry.Size() {
				return nil, errors.New("目录大小超出可统计范围")
			}
			result.Bytes += entry.Size()
			if entry.IsDir() {
				dirs = append(dirs, path.Join(dir, entry.Name()))
			}
		}
	}
	return result, nil
}
