package artifact

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackFiles(t *testing.T) {
	dir := t.TempDir()
	put := func(rel, text string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
	}
	put("config.yaml", "port: 1")
	put("deploy/certs/a.pem", "k")
	put("deploy/certs/sub/b.pem", "kk")
	put("start.sh", "#!/bin/sh")
	out := filepath.Join(t.TempDir(), "api.files.tar.gz")
	n, size, err := PackFiles([]ExtraFile{
		{Local: filepath.Join(dir, "config.yaml"), To: "conf/app.yaml"},
		{Local: filepath.Join(dir, "deploy", "certs"), To: "certs"},
		{Local: filepath.Join(dir, "start.sh"), To: "start.sh", Exec: true},
	}, out)
	if err != nil || n != 4 || size != 7+1+2+9 {
		t.Fatalf("%d %d %v", n, size, err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	var got []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%o", h.Name, h.Mode))
	}
	want := "certs/:755,certs/a.pem:644,certs/sub/:755,certs/sub/b.pem:644,conf/:755,conf/app.yaml:644,start.sh:755"
	if strings.Join(got, ",") != want {
		t.Fatalf("条目：%s", strings.Join(got, ","))
	}
	if _, _, err := PackFiles([]ExtraFile{{Local: filepath.Join(dir, "nope.yml"), To: "nope.yml"}}, out); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("没有的文件应报错：%v", err)
	}
}
