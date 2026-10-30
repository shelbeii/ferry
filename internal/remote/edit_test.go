package remote

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
)

func TestSaveFileSFTPPreservesFormat(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after []byte
	}{
		{"utf8", []byte("旧值\n"), []byte("新值😀\n第二行\n")},
		{"utf8-crlf-bom", append([]byte{0xef, 0xbb, 0xbf}, []byte("旧值\r\n")...), append([]byte{0xef, 0xbb, 0xbf}, []byte("新值😀\r\n第二行\r\n")...)},
		{"utf8-cr", []byte("旧值\r"), []byte("新值😀\r第二行\r")},
		{"utf16le", utf16Text("旧值\r\n", binary.LittleEndian), utf16Text("新值😀\r\n第二行\r\n", binary.LittleEndian)},
		{"utf16be", utf16Text("旧值\n", binary.BigEndian), utf16Text("新值😀\n第二行\n", binary.BigEndian)},
		{"empty", nil, []byte("新值😀\n第二行\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, ".env")
			if err := os.WriteFile(file, tc.before, 0o640); err != nil {
				t.Fatal(err)
			}
			beforeInfo, _ := os.Stat(file)
			c := previewTestClient(t, dir)
			p, err := c.PreviewFile(context.Background(), ".env")
			if err != nil {
				t.Fatal(err)
			}
			saved, err := c.SaveFile(context.Background(), ".env", FileEdit{Revision: p.Revision, Content: "新值😀\n第二行\n"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(file)
			if err != nil || !bytes.Equal(got, tc.after) {
				t.Fatalf("编码、BOM、换行必须保留：%x, %v", got, err)
			}
			afterInfo, _ := os.Stat(file)
			if afterInfo.Mode() != beforeInfo.Mode() {
				t.Fatal("保存不能改变文件权限")
			}
			again, err := c.PreviewFile(context.Background(), ".env")
			if err != nil || saved.Revision == p.Revision || saved.Revision != again.Revision || saved.Content != again.Content || saved.Size != int64(len(got)) {
				t.Fatalf("保存结果应能作为下次保存的版本：%+v, %v", saved, err)
			}
			// 使用返回的新版本继续保存为空文件。
			if _, err := c.SaveFile(context.Background(), ".env", FileEdit{Revision: saved.Revision, Content: ""}); err != nil {
				t.Fatal(err)
			}
			assertNoEditTemps(t, dir)
		})
	}
}

func TestSaveFileRejectsChangedContent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := previewTestClient(t, dir)
	p, err := c.PreviewFile(context.Background(), "config.json")
	if err != nil {
		t.Fatal(err)
	}
	// 同样长度、同一秒内的变化也必须检测出来。
	if err := os.WriteFile(file, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = c.SaveFile(context.Background(), "config.json", FileEdit{Revision: p.Revision, Content: "user edit"})
	var changed *FileChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("应拒绝覆盖外部修改：%v", err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "new" {
		t.Fatal("不能覆盖其他人的修改")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SaveFile(context.Background(), "config.json", FileEdit{Revision: p.Revision, Content: "user edit"}); err == nil {
		t.Fatal("已删除的文件不能被重新创建")
	}
	assertNoEditTemps(t, dir)
}

func TestSaveFileRejectsUnsafeContent(t *testing.T) {
	dir := t.TempDir()
	c := previewTestClient(t, dir)
	file := filepath.Join(dir, "text.txt")
	for _, tc := range []struct{ original, edited string }{
		{"normal", "text\x00binary"},
		{"normal", strings.Repeat("a", FilePreviewLimit+1)},
		{strings.Repeat("a", FilePreviewLimit+1), "truncated replacement"},
		{"\x7fELF\x00", "text"},
	} {
		if err := os.WriteFile(file, []byte(tc.original), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := c.PreviewFile(context.Background(), "text.txt")
		if err != nil {
			t.Fatal(err)
		}
		revision := p.Revision
		if revision == "" {
			revision = strings.Repeat("0", 64)
		}
		if _, err := c.SaveFile(context.Background(), "text.txt", FileEdit{Revision: revision, Content: tc.edited}); err == nil {
			t.Fatal("不应保存不完整、二进制或过大的文本")
		}
		got, _ := os.ReadFile(file)
		if string(got) != tc.original {
			t.Fatal("拒绝保存时必须保留原内容")
		}
	}
	if _, err := c.SaveFile(context.Background(), "text.txt", FileEdit{Content: "new"}); err == nil {
		t.Fatal("没有版本不能保存")
	}
	assertNoEditTemps(t, dir)
}

func TestSaveFileDeniedAndCanceled(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "text.txt")
	if err := os.WriteFile(file, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := previewTestClient(t, dir, sftp.ReadOnly())
	p, err := c.PreviewFile(context.Background(), "text.txt")
	if err != nil {
		t.Fatal(err)
	}
	edit := FileEdit{Revision: p.Revision, Content: "edited"}
	if _, err := c.SaveFile(context.Background(), "text.txt", edit); err == nil || !strings.Contains(err.Error(), "没有权限") {
		t.Fatalf("只读服务器应明确拒绝保存：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SaveFile(ctx, "text.txt", edit); !errors.Is(err, ErrCanceled) {
		t.Fatalf("取消后不能写入：%v", err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "original" {
		t.Fatal("失败不能破坏原文件")
	}
	assertNoEditTemps(t, dir)
}

func TestSaveFilePreservesSymlink(t *testing.T) {
	c := editMemoryClient(t, sftp.InMemHandler())
	writeEditFixture(t, c, "/real.conf", "original")
	if err := c.sftp.Symlink("real.conf", "/link.conf"); err != nil {
		t.Fatal(err)
	}
	p, err := c.PreviewFile(context.Background(), "/link.conf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SaveFile(context.Background(), "/link.conf", FileEdit{Revision: p.Revision, Content: "edited"}); err != nil {
		t.Fatal(err)
	}
	info, _ := c.sftp.Lstat("/link.conf")
	got, err := c.PreviewFile(context.Background(), "/real.conf")
	if err != nil || info.Mode()&os.ModeSymlink == 0 || got.Content != "edited" {
		t.Fatal("必须更新链接的目标并保留链接本身")
	}
	// 链接改为指向内容相同的另一个文件，也不能用原版本覆盖它。
	writeEditFixture(t, c, "/other.conf", "edited")
	p, err = c.PreviewFile(context.Background(), "/link.conf")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sftp.Remove("/link.conf"); err != nil {
		t.Fatal(err)
	}
	if err := c.sftp.Symlink("other.conf", "/link.conf"); err != nil {
		t.Fatal(err)
	}
	_, err = c.SaveFile(context.Background(), "/link.conf", FileEdit{Revision: p.Revision, Content: "user edit"})
	var changed *FileChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("链接目标变化后不应保存：%v", err)
	}
}

type rejectEditRename struct{ sftp.FileCmder }

func (rejectEditRename) PosixRename(*sftp.Request) error { return os.ErrPermission }

func TestSaveFileCommitFailureKeepsOriginal(t *testing.T) {
	handlers := sftp.InMemHandler()
	handlers.FileCmd = rejectEditRename{handlers.FileCmd}
	c := editMemoryClient(t, handlers)
	writeEditFixture(t, c, "/text.txt", "original")
	p, err := c.PreviewFile(context.Background(), "/text.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SaveFile(context.Background(), "/text.txt", FileEdit{Revision: p.Revision, Content: "edited"}); err == nil {
		t.Fatal("提交失败应报告错误")
	}
	got, err := c.PreviewFile(context.Background(), "/text.txt")
	if err != nil || got.Content != "original" {
		t.Fatal("原子替换失败必须保留完整原内容")
	}
	entries, err := c.sftp.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ferry-edit-") {
			t.Fatal("提交失败也要清理临时文件")
		}
	}
}

func editMemoryClient(t *testing.T, handlers sftp.Handlers) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := sftp.NewRequestServer(serverConn, handlers)
	t.Cleanup(func() { clientConn.Close(); serverConn.Close(); server.Close() })
	go server.Serve()
	ftp, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ftp.Close() })
	return &Client{sftp: ftp}
}

func writeEditFixture(t *testing.T, c *Client, p, content string) {
	t.Helper()
	f, err := c.sftp.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertNoEditTemps(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".ferry-edit-*.tmp"))
	if err != nil || len(files) > 0 {
		t.Fatalf("保存后不应留下临时文件：%v, %v", files, err)
	}
}
