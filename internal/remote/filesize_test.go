package remote

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/pkg/sftp"
)

func TestFileSizeIncludesHiddenFilesAndDoesNotExpandInternalLinks(t *testing.T) {
	c := editMemoryClient(t, sftp.InMemHandler())
	if err := c.sftp.MkdirAll("/srv/site/sub/empty"); err != nil {
		t.Fatal(err)
	}
	writeEditFixture(t, c, "/srv/site/index.html", "hello")
	writeEditFixture(t, c, "/srv/site/.hidden", "hidden")
	writeEditFixture(t, c, "/srv/site/sub/app.js", "javascript")
	writeEditFixture(t, c, "/outside.bin", strings.Repeat("outside", 1000))
	for link, target := range map[string]string{"/srv/site/loop": "/srv/site", "/srv/site/external": "/outside.bin", "/selected-site": "/srv/site"} {
		if err := c.sftp.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	expected := int64(len("hellohiddenjavascript"))
	for _, p := range []string{"/srv/site", "/srv/site/sub", "/srv/site/sub/empty", "/srv/site/loop", "/srv/site/external"} {
		st, err := c.sftp.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		expected += st.Size()
	}
	for _, p := range []string{"/srv/site", "/selected-site"} {
		result, err := c.FileSize(context.Background(), p)
		if err != nil || result == nil || result.Bytes != expected || !result.Dir || result.Path != p {
			t.Fatalf("目录与选中的目录链接应统计完整内容，内部链接不展开：%+v, %v, expected %d", result, err, expected)
		}
	}
	file, err := c.FileSize(context.Background(), "/srv/site/index.html")
	if err != nil || file.Bytes != 5 || file.Dir {
		t.Fatalf("普通文件按实际长度读取：%+v, %v", file, err)
	}
	writeEditFixture(t, c, "/empty", "")
	zero, err := c.FileSize(context.Background(), "/empty")
	if err != nil || zero.Bytes != 0 {
		t.Fatalf("空文件必须显示 0 字节：%+v, %v", zero, err)
	}
	if result, err := c.FileSize(context.Background(), "/missing"); err == nil || result != nil {
		t.Fatal("不存在的条目不能显示为 0 字节")
	}
}

type sizeListHandler struct {
	sftp.FileLister
	denied string
	onList func()
}

func (h sizeListHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if r.Method == "List" {
		if h.onList != nil {
			h.onList()
		}
		if r.Filepath == h.denied {
			return nil, os.ErrPermission
		}
	}
	return h.FileLister.Filelist(r)
}

func TestFileSizeUnreadableDirectoryDoesNotReturnPartialTotal(t *testing.T) {
	handlers := sftp.InMemHandler()
	handlers.FileList = sizeListHandler{FileLister: handlers.FileList, denied: "/site/locked"}
	c := editMemoryClient(t, handlers)
	c.sftp.MkdirAll("/site/locked")
	writeEditFixture(t, c, "/site/visible", "content")
	result, err := c.FileSize(context.Background(), "/site")
	if result != nil || err == nil || !strings.Contains(err.Error(), "没有权限") || !strings.Contains(err.Error(), "/site/locked") {
		t.Fatalf("无权限必须明确报告，不能把部分大小当作总数：%+v %v", result, err)
	}
}

func TestFileSizeCancelKeepsSFTPSessionUsable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlers := sftp.InMemHandler()
	handlers.FileList = sizeListHandler{FileLister: handlers.FileList, onList: cancel}
	c := editMemoryClient(t, handlers)
	c.sftp.MkdirAll("/site")
	if result, err := c.FileSize(ctx, "/site"); result != nil || !errors.Is(err, ErrCanceled) {
		t.Fatalf("目录统计中可取消：%+v %v", result, err)
	}
	if _, err := c.sftp.Stat("/site"); err != nil {
		t.Fatal("取消统计不能关闭共用的 SFTP 会话：", err)
	}
	if _, err := (&Client{}).FileSize(ctx, "/"); !errors.Is(err, ErrCanceled) {
		t.Fatal("已取消的请求不应建立连接：", err)
	}
}

func TestFileSizeCommandAndParsing(t *testing.T) {
	p := "/site/it's $(touch forbidden)\n--help"
	if command := fileSizeCommand(p); command != "LC_ALL=C timeout -k 5s 120s du -sblP -- '/site/it'\\''s $(touch forbidden)\n--help'" {
		t.Fatal("路径必须作为单个字面量参数，不能解释文件名里的命令：", command)
	}
	for _, out := range []string{"42\t/normal\n", "42\t/path with spaces\n123\tfile\n"} {
		if n, err := parseFileSize(out); err != nil || n != 42 {
			t.Fatalf("大小应取文件名之前的数值，不受特殊文件名影响：%d %v", n, err)
		}
	}
	for _, out := range []string{"", "permission denied", "-1\t/site", "9223372036854775808\t/site", "not a number\t/site"} {
		if _, err := parseFileSize(out); err == nil {
			t.Fatalf("无效结果不能当成大小：%q", out)
		}
	}
}
