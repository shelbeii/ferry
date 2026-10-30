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
	"unicode/utf16"
	"unicode/utf8"

	"github.com/pkg/sftp"
)

func utf16Text(text string, order binary.ByteOrder) []byte {
	units := append([]uint16{0xfeff}, utf16.Encode([]rune(text))...)
	b := make([]byte, 2*len(units))
	for i, u := range units {
		order.PutUint16(b[2*i:], u)
	}
	return b
}

func TestFilePreviewText(t *testing.T) {
	for _, tc := range []struct {
		name, text, encoding string
		data                 []byte
	}{
		{"empty", "", "UTF-8", nil},
		{"json", "{\"name\":\"中文😀\"}\r\n", "UTF-8", []byte("{\"name\":\"中文😀\"}\r\n")},
		{"utf8-bom", "配置\n", "UTF-8", append([]byte{0xef, 0xbb, 0xbf}, []byte("配置\n")...)},
		{"utf16le", "中文😀\r\n", "UTF-16 LE", utf16Text("中文😀\r\n", binary.LittleEndian)},
		{"utf16be", "中文😀\n", "UTF-16 BE", utf16Text("中文😀\n", binary.BigEndian)},
		{"literal-html", "<script>alert('x')</script>\n\tserver {}", "UTF-8", []byte("<script>alert('x')</script>\n\tserver {}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := readFilePreview(bytes.NewReader(tc.data))
			if err != nil || p.Unsupported != "" || p.Content != tc.text || p.Encoding != tc.encoding || p.Truncated {
				t.Fatalf("preview=%+v err=%v", p, err)
			}
		})
	}
}

func TestFilePreviewRejectsBinaryAndMalformedText(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("\x7fELF\x02\x01\x00"), []byte("text\x00more"), {0x89, 'P', 'N', 'G'},
		{0xff, 0xfe, 0, 0, 0x41, 0, 0, 0}, {0, 0, 0xfe, 0xff, 0, 0, 0, 0x41},
		{0xff, 0xfe, 0x41}, {0xff, 0xfe, 0, 0xd8}, {0xff, 0xfe, 0, 0xdc},
		[]byte("invalid\xe4\xb8"),
	} {
		p, err := readFilePreview(bytes.NewReader(data))
		if err != nil || p.Unsupported == "" || p.Content != "" || p.Encoding != "" {
			t.Fatalf("二进制或损坏的编码不能变成乱码：%x => %+v, %v", data, p, err)
		}
	}
}

func TestFilePreviewTruncation(t *testing.T) {
	for _, width := range []int{1, 2, 3} {
		text := strings.Repeat("a", FilePreviewLimit-width) + "😀后面的内容"
		r := bytes.NewReader([]byte(text))
		p, err := readFilePreview(r)
		if err != nil || !p.Truncated || p.Unsupported != "" || !utf8.ValidString(p.Content) || strings.ContainsRune(p.Content, utf8.RuneError) {
			t.Fatalf("UTF-8 不应从字符中间截断：%+v, %v", p, err)
		}
		if len(p.Content) != FilePreviewLimit-width || r.Size()-int64(r.Len()) != FilePreviewLimit+1 {
			t.Fatal("必须限制读取量，并丢弃预览边界的不完整字符")
		}
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		// BOM + 文本 + 代理对的前半个恰好占满预览。
		text := strings.Repeat("a", (FilePreviewLimit-4)/2) + "😀后面"
		p, err := readFilePreview(bytes.NewReader(utf16Text(text, order)))
		if err != nil || !p.Truncated || p.Unsupported != "" || strings.ContainsRune(p.Content, utf8.RuneError) || len(p.Content) != (FilePreviewLimit-4)/2 {
			t.Fatalf("UTF-16 不能拆开代理对：encoding=%s err=%v", p.Encoding, err)
		}
	}
	p, err := readFilePreview(strings.NewReader(strings.Repeat("a", FilePreviewLimit)))
	if err != nil || p.Truncated || len(p.Content) != FilePreviewLimit {
		t.Fatal("刚好达到上限的文件应完整显示")
	}
}

type previewReadError struct{}

func (previewReadError) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestFilePreviewReadError(t *testing.T) {
	if _, err := readFilePreview(previewReadError{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("读取失败必须传回错误：%v", err)
	}
}

// 用真实 SFTP 会话验证打开、读取、文件类型和错误路径，无需连接外部服务器。
func TestPreviewFileSFTP(t *testing.T) {
	dir := t.TempDir()
	content := "{\"name\":\"中文\",\"version\":\"1.30.5\"}\n"
	if err := os.WriteFile(filepath.Join(dir, ".ferry-runtime.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c := previewTestClient(t, dir)
	p, err := c.PreviewFile(context.Background(), ".ferry-runtime.json")
	if err != nil || p.Content != content || p.Size != int64(len(content)) || p.Path != ".ferry-runtime.json" || p.Unsupported != "" {
		t.Fatalf("SFTP 预览失败：%+v, %v", p, err)
	}
	if _, err := c.PreviewFile(context.Background(), "."); err == nil || !strings.Contains(err.Error(), "普通文件") {
		t.Fatalf("不能读取文件夹：%v", err)
	}
	if _, err := c.PreviewFile(context.Background(), "missing.json"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失文件应说明原因：%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.PreviewFile(ctx, ".ferry-runtime.json"); !errors.Is(err, ErrCanceled) {
		t.Fatalf("已取消的请求不应读取文件：%v", err)
	}
	// Windows 未开启开发人员模式时不能建链接，其他情况下验证链接照常只读。
	if os.Symlink(filepath.Join(dir, ".ferry-runtime.json"), filepath.Join(dir, "config-link")) == nil {
		if linked, err := c.PreviewFile(context.Background(), "config-link"); err != nil || linked.Content != content {
			t.Fatalf("应能读取指向文本文件的链接：%v", err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, ".ferry-runtime.json"))
	if err != nil || string(got) != content {
		t.Fatal("预览不能改动原文件")
	}
}

func previewTestClient(t *testing.T, dir string, opts ...sftp.ServerOption) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	opts = append(opts, sftp.WithServerWorkingDirectory(dir))
	server, err := sftp.NewServer(serverConn, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientConn.Close(); serverConn.Close(); server.Close() })
	go server.Serve()
	ftp, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ftp.Close() })
	return &Client{sftp: ftp}
}
