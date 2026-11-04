package remote

import (
	"testing"

	"github.com/pkg/sftp"
)

func TestFileLocation(t *testing.T) {
	c := editMemoryClient(t, sftp.InMemHandler())
	if err := c.sftp.MkdirAll("/srv/软件"); err != nil {
		t.Fatal(err)
	}
	writeEditFixture(t, c, "/srv/软件/server.jar", "program")
	if err := c.sftp.Symlink("server.jar", "/srv/软件/current.jar"); err != nil {
		t.Fatal(err)
	}
	if err := c.sftp.Symlink("软件", "/srv/current"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, dir, focus string }{
		{"/srv/软件/", "/srv/软件", ""},
		{"/srv/软件/server.jar", "/srv/软件", "server.jar"},
		{"/srv/软件/current.jar", "/srv/软件", "current.jar"},
		{"/srv/current", "/srv/current", ""},
		{"/", "/", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			dir, focus, err := c.FileLocation(tc.input)
			if err != nil || dir != tc.dir || focus != tc.focus {
				t.Fatalf("FileLocation(%q) = %q, %q, %v", tc.input, dir, focus, err)
			}
			if _, entries, err := c.ListDir(dir); err != nil || entries == nil {
				t.Fatalf("位置必须可以列目录：%v", err)
			}
		})
	}
	if _, _, err := c.FileLocation("/missing"); err == nil {
		t.Fatal("不存在的位置应报错，不能打开其他目录")
	}
}
