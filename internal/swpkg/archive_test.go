package swpkg

import (
	"archive/zip"
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchivePreviewAndOfflineBundle(t *testing.T) {
	for _, tc := range []struct {
		name, bundle string
		files        []string
	}{
		{"rpm", "rpm", []string{"nginx.rpm", "nginx-filesystem.rpm", "SHA256SUMS.txt"}},
		{"deb", "deb", []string{"bundle/nginx.deb", "bundle/SHA256SUMS"}},
		{"program", "", []string{"nginx/bin/nginx", "nginx/conf/default.conf"}},
		{"mixed", "", []string{"nginx.deb", "nginx.rpm"}},
		{"renamed", "", []string{"source.rpm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "input.zip")
			f, err := os.Create(file)
			if err != nil {
				t.Fatal(err)
			}
			z := zip.NewWriter(f)
			for _, entry := range tc.files {
				w, err := z.Create(entry)
				if err != nil {
					t.Fatal(err)
				}
				header := "file contents"
				if tc.name != "renamed" {
					switch path.Ext(entry) {
					case ".rpm":
						header = "\xed\xab\xee\xdb"
					case ".deb":
						header = "!<arch>\n"
					}
				}
				if _, err := w.Write([]byte(header)); err != nil {
					t.Fatal(err)
				}
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			state, err := InspectArchive(context.Background(), file, "zip", 0)
			if err != nil || state.Bundle != tc.bundle || state.Entries != len(tc.files) {
				t.Fatalf("预览：%+v %v", state, err)
			}
			for _, kind := range []string{"rpm", "deb", "auto"} {
				err := CheckSystemBundle(context.Background(), file, kind)
				wantValid := tc.bundle != "" && (kind == tc.bundle || kind == "auto")
				if (err == nil) != wantValid {
					t.Errorf("检查 %s 离线包，期望通过 %v：%v", kind, wantValid, err)
				}
			}
			if tc.name == "program" {
				state, err = InspectArchive(context.Background(), file, "zip", 1)
				if err != nil || state.Files[0] != "bin/nginx" {
					t.Fatalf("剥离层数：%+v %v", state, err)
				}
			}
		})
	}
}

func TestSystemBundleRejectsSourceArchive(t *testing.T) {
	err := CheckSystemBundle(context.Background(), "nginx-1.30.6.tar.gz", "rpm")
	if err == nil || !strings.Contains(err.Error(), "RPM") || !strings.Contains(err.Error(), "源码需要先编译") {
		t.Fatalf("源码包错误提示：%v", err)
	}
}
