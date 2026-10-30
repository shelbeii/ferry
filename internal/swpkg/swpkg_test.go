package swpkg

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/captain/ferry/internal/swtpl"
)

func TestIdentify(t *testing.T) {
	tpls := swtpl.Builtin()
	cases := map[string]string{
		"sentinel-dashboard-1.8.10.jar":                      "sentinel 1.8.10",
		"nacos-server-3.2.4.tar.gz":                          "nacos 3.2.4",
		"mysql-8.4.10-linux-glibc2.17-x86_64-minimal.tar.xz": "mysql 8.4.10",
		"mysql-8.4.10-linux-glibc2.28-aarch64.tar.xz":        " ",
		"nacos-server-3.2.4.zip":                             " ",
		"minio":                                              " ",
	}
	for name, want := range cases {
		tp, v, _ := Identify(name, tpls)
		if got := tp + " " + v; got != want {
			t.Errorf("%s → %q，期望 %q", name, got, want)
		}
	}
}

func TestRewrite(t *testing.T) {
	s := Sources{GitHubMirror: "https://ghfast.top/", MySQL: "https://mirror.local/mysql/"}
	if u := s.Rewrite("https://github.com/a/b"); u != "https://ghfast.top/https://github.com/a/b" {
		t.Error(u)
	}
	if u := s.Rewrite("https://cdn.mysql.com/Downloads/x"); u != "https://mirror.local/mysql/Downloads/x" {
		t.Error(u)
	}
	if !Less("1.8.9", "1.8.10") || Less("3.2.4", "3.2.4") {
		t.Error("Less 不对")
	}
}

func TestImportAndFind(t *testing.T) {
	tpls := swtpl.Builtin()
	src, lib := t.TempDir(), t.TempDir()
	jar := filepath.Join(src, "sentinel-dashboard-1.8.10.jar")
	f, _ := os.Create(jar)
	z := zip.NewWriter(f)
	w, _ := z.Create("BOOT-INF/classes/com/alibaba/csp/sentinel/dashboard/App.class")
	w.Write([]byte("x"))
	z.Close()
	f.Close()
	tgz := filepath.Join(src, "nacos-server-3.2.4.tar.gz")
	f, _ = os.Create(tgz)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "nacos/target/nacos-server.jar", Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	f.Close()
	// 自定义安装包：任意程序
	bin := filepath.Join(src, "myserver")
	os.WriteFile(bin, []byte("\x7fELF fake"), 0o755)
	for _, p := range []string{jar, tgz, bin} {
		if _, err := Import(p, lib, tpls); err != nil {
			t.Fatal(err)
		}
	}
	if m := Find(lib, "nacos", ""); m == nil || m.Version != "3.2.4" {
		t.Errorf("Find nacos：%+v", m)
	}
	if m := FindFile(lib, "myserver"); m == nil || m.Template != "" {
		t.Errorf("自定义安装包：%+v", m)
	}
	// 文件名对、内容不对
	bad := filepath.Join(src, "nacos-server-3.2.5.tar.gz")
	os.WriteFile(bad, []byte("<html>"), 0o644)
	if _, err := Import(bad, lib, tpls); err == nil {
		t.Error("坏的安装包应当拒绝")
	}
	if err := Remove(lib, "sentinel-dashboard-1.8.10.jar"); err != nil || len(List(lib)) != 2 {
		t.Errorf("Remove：%v %d", err, len(List(lib)))
	}
}

// 联网查最新版本：FERRY_NET_TEST=1 go test ./internal/swpkg -run Latest -v
func TestLatestNet(t *testing.T) {
	if os.Getenv("FERRY_NET_TEST") == "" {
		t.Skip("需要联网")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, tp := range swtpl.Builtin() {
		for _, v := range tp.Variants {
			if v.Package == nil || v.Package.URL == "" {
				continue
			}
			ver, err := Latest(ctx, Sources{}, v.Package)
			t.Logf("%s：%s %v", tp.ID, ver, err)
			if err != nil {
				t.Error(err)
			}
		}
	}
}

// 列出时按文件内容写明类型（Linux 程序的架构、脚本、不是 Linux 程序），这些不存进记录。
func TestListDescribe(t *testing.T) {
	src, lib := t.TempDir(), t.TempDir()
	ehdr := make([]byte, 64)
	copy(ehdr, "\x7fELF")
	ehdr[elf.EI_CLASS], ehdr[elf.EI_DATA], ehdr[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(ehdr[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(ehdr[18:], uint16(elf.EM_AARCH64))
	binary.LittleEndian.PutUint32(ehdr[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(ehdr[52:], 64)
	files := map[string][]byte{"minio": ehdr, "run": []byte("#!/bin/sh\n"), "tool.exe": []byte("MZ\x90\x00")}
	for name, b := range files {
		p := filepath.Join(src, name)
		os.WriteFile(p, b, 0o644)
		m, err := Import(p, lib, nil)
		if err != nil {
			t.Fatal(err)
		}
		if name == "minio" && (m.Arch != "arm64" || m.Desc != "Linux 程序（arm64）") {
			t.Errorf("导入返回的也要写明类型：%+v", m)
		}
	}
	want := map[string]string{"minio": "Linux 程序（arm64）", "run": "脚本", "tool.exe": "不是 Linux 程序"}
	for _, m := range List(lib) {
		if m.Format != "binary" || m.Desc != want[m.File] {
			t.Errorf("%s：format=%s desc=%s", m.File, m.Format, m.Desc)
		}
	}
	b, _ := os.ReadFile(metaFile(lib, "minio"))
	var rec map[string]any
	json.Unmarshal(b, &rec)
	for _, k := range []string{"arch", "desc", "format", "path"} {
		if _, ok := rec[k]; ok {
			t.Errorf("记录里不该存 %s：%s", k, b)
		}
	}
}
