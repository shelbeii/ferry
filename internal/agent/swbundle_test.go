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
			args := packageManagerArgs(test.manager, files)
			switch test.manager {
			case "apt-get":
				// apt 2.4 的 --no-download 和本地 .deb 一起用会报 Pathname to install is not absolute
				if strings.Contains(args, "--no-download") || !strings.Contains(args, "Dir::Etc::sourcelist=/dev/null") || !strings.Contains(args, "Dir::Etc::sourceparts=/dev/null") {
					t.Fatalf("apt 离线安装命令没有关掉软件源：%s", args)
				}
			default:
				if !strings.Contains(args, "--disablerepo") {
					t.Fatalf("离线安装命令没有关闭软件源：%s", args)
				}
			}
			if err := checkSystemBundle(test.manager, test.kind); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPackageManagerArgsOnline(t *testing.T) {
	for _, manager := range []string{"apt-get", "dnf", "yum"} {
		if got := packageManagerArgs(manager, nil); got != manager {
			t.Fatalf("在线安装时不该改命令：%s", got)
		}
	}
}
