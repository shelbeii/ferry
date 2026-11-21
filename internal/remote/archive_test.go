package remote

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteZIPRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "reports", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"reports/.hidden": "hello", "数据.txt": "世界"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := previewTestClient(t, dir)
	ctx := context.Background()
	progress := func(string) {}
	sources := []string{"reports", "数据.txt"}
	if err := c.Files().CompressFiles(ctx, sources, "backup.zip", progress); err != nil {
		t.Fatal(err)
	}
	if err := c.Files().ExtractArchive(ctx, "backup.zip", "unpacked", progress); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"reports/.hidden": "hello", "数据.txt": "世界"} {
		b, err := os.ReadFile(filepath.Join(dir, "unpacked", name))
		if err != nil || string(b) != want {
			t.Fatalf("内容 %s = %q，%v", name, b, err)
		}
	}
	if st, err := os.Stat(filepath.Join(dir, "unpacked", "reports", "empty")); err != nil || !st.IsDir() {
		t.Fatalf("空文件夹未保留：%v", err)
	}
	if err := c.Files().CompressFiles(ctx, sources, "backup.zip", progress); err == nil || !strings.Contains(err.Error(), "已经存在") {
		t.Fatalf("不能覆盖原有压缩包：%v", err)
	}
	if err := c.Files().ExtractArchive(ctx, "backup.zip", "unpacked", progress); err == nil || !strings.Contains(err.Error(), "已经存在") {
		t.Fatalf("不能覆盖原有文件夹：%v", err)
	}
	raced := filepath.Join(dir, "race.zip")
	if err := c.Files().CompressFiles(ctx, sources[:1], "race.zip", func(string) {
		if err := os.WriteFile(raced, []byte("keep"), 0o600); err != nil {
			t.Error(err)
		}
	}); err == nil {
		t.Fatal("压缩期间出现的同名文件不能被覆盖")
	}
	if b, err := os.ReadFile(raced); err != nil || string(b) != "keep" {
		t.Fatalf("同名文件被改动：%q，%v", b, err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := c.Files().ExtractArchive(cancelCtx, "backup.zip", "canceled", func(string) { cancel() }); !errors.Is(err, ErrCanceled) {
		t.Fatalf("解压应能取消：%v", err)
	}
	assertArchiveAbsent(t, dir, "canceled")
}

func TestRemoteArchiveExtraction(t *testing.T) {
	dir := t.TempDir()
	writeArchive := func(name string, write func(*os.File) error) {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(write(f), f.Close()); err != nil {
			t.Fatal(err)
		}
	}
	writeArchive("logs.tar.gz", func(f *os.File) error {
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "./logs/app.log", Mode: 0o644, Size: 3}); err != nil {
			return err
		}
		_, err := tw.Write([]byte("log"))
		return errors.Join(err, tw.Close(), gz.Close())
	})
	writeArchive("bad.zip", func(f *os.File) error {
		z := zip.NewWriter(f)
		if _, err := z.Create("ordinary.txt"); err != nil {
			return err
		}
		if _, err := z.CreateHeader(&zip.FileHeader{Name: "sub/../outside.txt"}); err != nil {
			return err
		}
		return z.Close()
	})
	c := previewTestClient(t, dir)
	if err := c.Files().ExtractArchive(context.Background(), "logs.tar.gz", "logs", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "logs", "logs", "app.log")); err != nil || string(b) != "log" {
		t.Fatalf("TAR.GZ 内容错误：%q，%v", b, err)
	}
	if err := c.Files().ExtractArchive(context.Background(), "bad.zip", "bad", func(string) {}); err == nil {
		t.Fatal("不安全的归档名称应被拒绝")
	}
	assertArchiveAbsent(t, dir, "bad")
	assertArchiveAbsent(t, dir, "outside.txt")
}

func assertArchiveAbsent(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == name || strings.HasPrefix(entry.Name(), ".ferry-extract-") || strings.HasPrefix(entry.Name(), ".ferry-archive-") {
			t.Errorf("操作留下了未完成的项目：%s", entry.Name())
		}
	}
}
