package swpkg

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/binary"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
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

func fakeELF(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b[:4], []byte{0x7f, 'E', 'L', 'F'})
	b[4] = 2                                   // 64-bit
	b[5] = 1                                   // little-endian
	binary.LittleEndian.PutUint16(b[16:18], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(b[18:20], machine)
	return b
}

func TestInspectArchiveDeepDetection(t *testing.T) {
	// 1. 扁平 tar.gz（无顶层目录）含 amd64 ELF + config + CRLF 后台脚本
	flat := filepath.Join(t.TempDir(), "caddy_2.8.4_linux_amd64.tar.gz")
	f, err := os.Create(flat)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	writeTar := func(name string, mode int64, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	writeTar("caddy", 0o755, fakeELF(62)) // EM_X86_64
	writeTar("conf/caddy.yaml", 0o644, []byte("port: 8080\n"))
	writeTar("startup.sh", 0o644, []byte("#!/bin/sh\r\nnohup ./caddy &\r\n"))
	tw.Close()
	gz.Close()
	f.Close()

	prev, err := InspectArchive(context.Background(), flat, "tar.gz", -1)
	if err != nil {
		t.Fatal(err)
	}
	if prev.RecommendedStrip != 0 || prev.Strip != 0 {
		t.Fatalf("扁平包应推荐 strip=0，实际 %+v", prev)
	}
	if len(prev.Arches) != 1 || prev.Arches[0] != "amd64" {
		t.Fatalf("应识别到 amd64 架构，实际 %+v", prev.Arches)
	}
	if len(prev.Candidates) < 2 || prev.Candidates[0].Path != "caddy" || prev.Candidates[0].Kind != "elf" || prev.Candidates[0].Exec != "${dir}/caddy" {
		t.Fatalf("候选入口排序/识别错误：%+v", prev.Candidates)
	}
	if !prev.Candidates[1].Daemon || !prev.Candidates[1].CRLF {
		t.Fatalf("应检测出 startup.sh 为后台脚本且含 CRLF：%+v", prev.Candidates[1])
	}
	if len(prev.Configs) != 1 || prev.Configs[0] != "conf/caddy.yaml" {
		t.Fatalf("应识别出配置文件：%+v", prev.Configs)
	}

	// 2. 带统一顶层目录的 tar.zst（arm64 ELF）
	nested := filepath.Join(t.TempDir(), "mytool-1.0-arm64.tar.zst")
	nf, err := os.Create(nested)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(nf)
	if err != nil {
		t.Fatal(err)
	}
	tw2 := tar.NewWriter(zw)
	armELF := fakeELF(183) // EM_AARCH64
	if err := tw2.WriteHeader(&tar.Header{Name: "mytool-1.0/bin/mytool", Mode: 0o755, Size: int64(len(armELF))}); err != nil {
		t.Fatal(err)
	}
	tw2.Write(armELF)
	tw2.Close()
	zw.Close()
	nf.Close()

	if err := Check(nested, "tar.zst", 1, "bin/mytool"); err != nil {
		t.Fatalf("tar.zst 校验失败：%v", err)
	}
	prev2, err := InspectArchive(context.Background(), nested, "tar.zst", -1)
	if err != nil {
		t.Fatal(err)
	}
	if prev2.RecommendedStrip != 1 || prev2.SingleTopDir != "mytool-1.0" || prev2.Strip != 1 {
		t.Fatalf("单顶层目录包应推荐 strip=1：%+v", prev2)
	}
	if len(prev2.Candidates) != 1 || prev2.Candidates[0].Path != "bin/mytool" || prev2.Candidates[0].Arch != "arm64" {
		t.Fatalf("应剥离顶层目录并识别 arm64：%+v", prev2.Candidates)
	}
	has, err := ArchiveHasPath(context.Background(), nested, "tar.zst", 1, "bin/mytool")
	if err != nil || !has {
		t.Fatalf("ArchiveHasPath 应为 true：%v %v", has, err)
	}
	hasWrongStrip, err := ArchiveHasPath(context.Background(), nested, "tar.zst", 0, "bin/mytool")
	if err != nil || hasWrongStrip {
		t.Fatalf("strip=0 时不应包含 bin/mytool：%v %v", hasWrongStrip, err)
	}
}
