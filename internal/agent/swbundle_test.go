package agent

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfflinePackageExtractionAndManager(t *testing.T) {
	for _, test := range []struct {
		kind, manager, file, header string
	}{
		{"rpm", "dnf", "nested/perl-Tabs+Wrap-1.0.noarch.rpm", "\xed\xab\xee\xdb"},
		{"deb", "apt-get", "nginx_1%3a1.24.deb", "!<arch>\n"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "packages.zip")
			out, err := os.Create(input)
			if err != nil {
				t.Fatal(err)
			}
			z := zip.NewWriter(out)
			entry, err := z.Create(test.file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write([]byte(test.header)); err != nil {
				t.Fatal(err)
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			files, err := unpackSystemBundle(input, dir, test.kind)
			if err != nil || len(files) != 1 || filepath.Dir(files[0]) != dir {
				t.Fatalf("离线包未放进指定目录：%v %v", files, err)
			}
			args := packageManagerArgs(test.manager, files, false)
			if !(strings.Contains(args, "--disablerepo") || strings.Contains(args, "--no-download")) {
				t.Fatalf("离线安装命令没有关闭下载：%s", args)
			}
			if err := checkSystemBundle(test.manager, test.kind); err != nil {
				t.Fatal(err)
			}
		})
	}
}
