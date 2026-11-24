package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGlibcVersion(t *testing.T) {
	cases := []struct {
		line     string
		maj, min int
		ok       bool
	}{
		{"ldd (GNU libc) 2.38", 2, 38, true},
		{"ldd (Ubuntu GLIBC 2.39-0ubuntu8.3) 2.39", 2, 39, true},
		{"ldd (GNU libc) 2.17", 2, 17, true},
		{"musl libc (x86_64)", 0, 0, false},
	}
	for _, c := range cases {
		maj, min, ok := parseGlibcVersion(c.line)
		if ok != c.ok || maj != c.maj || min != c.min {
			t.Errorf("parseGlibcVersion(%q) = (%d,%d,%v)，应为 (%d,%d,%v)", c.line, maj, min, ok, c.maj, c.min, c.ok)
		}
	}
}

func TestMySQLShellPackageSelection(t *testing.T) {
	file, urls, err := mysqlShellPackage("x86_64", "ldd (GNU libc) 2.38", "https://mirrors.tuna.tsinghua.edu.cn/mysql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(file, "glibc2.28-x86-64bit.tar.gz") {
		t.Fatalf("文件名不对：%q", file)
	}
	if len(urls) < 3 || !strings.HasPrefix(urls[0], "https://mirrors.tuna.tsinghua.edu.cn/mysql/") || !strings.HasPrefix(urls[len(urls)-1], "https://cdn.mysql.com/") {
		t.Fatalf("镜像优先 + 官方回退顺序不对：%v", urls)
	}

	legacyFile, _, err := mysqlShellPackage("aarch64", "ldd (GNU libc) 2.17", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(legacyFile, "glibc2.17-arm-64bit.tar.gz") {
		t.Fatalf("CentOS 7 / arm64 文件名不对：%q", legacyFile)
	}

	if _, _, err := mysqlShellPackage("s390x", "ldd (GNU libc) 2.38", ""); err == nil {
		t.Fatal("不支持的架构应报错")
	}
	if _, _, err := mysqlShellPackage("x86_64", "ldd (GNU libc) 2.12", ""); err == nil {
		t.Fatal("glibc < 2.17 应报错")
	}
}

func TestCachedGzipOK(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.tar.gz")
	buf := make([]byte, (1<<20)+16)
	buf[0], buf[1] = 0x1f, 0x8b
	if err := os.WriteFile(good, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if !cachedGzipOK(good) {
		t.Fatal("有效 gzip 缓存应通过校验")
	}
	bad := filepath.Join(dir, "bad.tar.gz")
	if err := os.WriteFile(bad, []byte("not-gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cachedGzipOK(bad) {
		t.Fatal("损坏的小文件不应视为有效缓存")
	}
}
