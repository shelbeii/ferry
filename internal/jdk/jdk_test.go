package jdk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FakeJDK 一个最小的 JDK 压缩包（release + bin/java）。
func FakeJDK(t *testing.T, version, osArch string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	top := "jdk-" + version + "/"
	add := func(name, body string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: top + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: top, Mode: 0o755, Typeflag: tar.TypeDir})
	java, _, _ := strings.Cut(version, "+")
	add("release", fmt.Sprintf("IMPLEMENTOR=\"Eclipse Adoptium\"\nJAVA_RUNTIME_VERSION=\"%s\"\nJAVA_VERSION=\"%s\"\nOS_ARCH=\"%s\"\nOS_NAME=\"Linux\"\n", version, java, osArch), 0o644)
	add("bin/java", "#!/bin/sh\necho java\n", 0o755)
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestInspectAndImport(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.tar.gz")
	data := FakeJDK(t, "17.0.13+11", "x86_64")
	os.WriteFile(p, data, 0o644)
	m, err := Inspect(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Java != 17 || m.Version != "17.0.13+11" || m.Arch != "x64" || m.ID != "jdk-17.0.13_11-x64" || m.SHA256 != sum(data) {
		t.Fatalf("%+v", m)
	}
	cache := filepath.Join(dir, "cache")
	if _, err := Import(p, cache); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, FakeJDK(t, "17.0.9+9", "x86_64"), 0o644)
	Import(p, cache)
	os.WriteFile(p, FakeJDK(t, "21.0.5+11", "aarch64"), 0o644)
	Import(p, cache)
	if f := Find(cache, 17, "x64"); f == nil || f.Version != "17.0.13+11" {
		t.Fatalf("应找到最新的 17：%+v", f)
	}
	if Find(cache, 17, "aarch64") != nil || Find(cache, 21, "aarch64") == nil {
		t.Fatal("按架构查找")
	}
	if len(List(cache)) != 3 {
		t.Fatalf("%+v", List(cache))
	}
	if err := Remove(cache, "jdk-17.0.9_9-x64"); err != nil || len(List(cache)) != 2 {
		t.Fatal("删除")
	}

	// 不是 JDK、不是 Linux
	os.WriteFile(p, []byte("not gzip"), 0o644)
	if _, err := Inspect(p); err == nil {
		t.Fatal("不是 tar.gz 应报错")
	}
}

func TestMajor(t *testing.T) {
	for v, want := range map[string]int{"1.8.0_412": 8, "17.0.13": 17, "21": 21, "": 0} {
		if Major(v) != want {
			t.Errorf("%q", v)
		}
	}
	if !versionLess("17.0.9+9", "17.0.13+11") || versionLess("17.0.13+11", "17.0.9+9") {
		t.Error("versionLess")
	}
}

func TestDownloadAdoptiumAPI(t *testing.T) {
	data := FakeJDK(t, "17.0.13+11", "x86_64")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v3/assets/latest/17/hotspot"):
			if r.URL.Query().Get("architecture") != "x64" || r.URL.Query().Get("image_type") != "jdk" {
				http.Error(w, "bad query", 400)
				return
			}
			fmt.Fprintf(w, `[{"binary":{"package":{"link":"%s/file.tar.gz","checksum":"%s","size":%d}}}]`, srv.URL, sum(data), len(data))
		case r.URL.Path == "/file.tar.gz":
			w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cache := t.TempDir()
	var last int64
	m, err := Download(context.Background(), srv.URL, 17, "x64", cache, func(d, _ int64) { last = d })
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "jdk-17.0.13_11-x64" || last != int64(len(data)) || Find(cache, 17, "x64") == nil {
		t.Fatalf("%+v %d", m, last)
	}
	// 要 21 但源里没有
	if _, err := Download(context.Background(), srv.URL, 21, "x64", cache, nil); err == nil {
		t.Fatal("应报错")
	}
}

func TestDownloadMirror(t *testing.T) {
	old := FakeJDK(t, "17.0.9+9", "x86_64")
	cur := FakeJDK(t, "17.0.13+11", "x86_64")
	arm := FakeJDK(t, "17.0.13+11", "aarch64")
	files := map[string][]byte{
		"OpenJDK17U-jdk_x64_linux_hotspot_17.0.9_9.tar.gz":       old,
		"OpenJDK17U-jdk_x64_linux_hotspot_17.0.13_11.tar.gz":     cur,
		"OpenJDK17U-jdk_aarch64_linux_hotspot_17.0.13_11.tar.gz": arm,
	}
	tampered := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if r.URL.Path == "/Adoptium/17/jdk/x64/linux/" || r.URL.Path == "/Adoptium/17/jdk/aarch64/linux/" {
			for n := range files {
				fmt.Fprintf(w, `<a href="%s">%s</a><br>`, n, n)
			}
			return
		}
		if b, ok := strings.CutSuffix(name, ".sha256.txt"); ok {
			if d, ok := files[b]; ok {
				s := sum(d)
				if tampered {
					s = strings.Repeat("0", 64)
				}
				fmt.Fprintf(w, "%s  %s\n", s, b)
				return
			}
		}
		if d, ok := files[name]; ok {
			w.Write(d)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	cache := t.TempDir()
	m, err := Download(context.Background(), srv.URL+"/Adoptium/{java}/jdk/{arch}/linux/", 17, "x64", cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "17.0.13+11" || m.Arch != "x64" {
		t.Fatalf("应取最新的 x64：%+v", m)
	}
	tampered = true
	if _, err := Download(context.Background(), srv.URL+"/Adoptium/{java}/jdk/{arch}/linux/", 17, "aarch64", cache, nil); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("sha256 不符应报错：%v", err)
	}
}

// 锁定版本：按版本查官方 API、在镜像目录里找这个版本，缓存里按版本找；ID 与版本对得上
func TestDownloadVersion(t *testing.T) {
	cur := FakeJDK(t, "17.0.13+11-LTS", "x86_64")
	old := FakeJDK(t, "17.0.9+9", "x86_64")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/assets/release_name/eclipse/jdk-17.0.9+9":
			if r.URL.Query().Get("architecture") != "x64" || r.URL.Query().Get("os") != "linux" {
				http.Error(w, "bad query", 400)
				return
			}
			fmt.Fprintf(w, `{"binaries":[{"package":{"link":"%s/old.tar.gz","checksum":"%s"}}]}`, srv.URL, sum(old))
		case r.URL.Path == "/old.tar.gz":
			w.Write(old)
		case r.URL.Path == "/mirror/17/x64/":
			fmt.Fprint(w, `<a href="OpenJDK17U-jdk_x64_linux_hotspot_17.0.13_11.tar.gz">x</a><a href="OpenJDK17U-jdk_x64_linux_hotspot_17.0.9_9.tar.gz">y</a>`)
		case strings.HasSuffix(r.URL.Path, "_17.0.9_9.tar.gz"):
			w.Write(old)
		case strings.HasSuffix(r.URL.Path, "_17.0.13_11.tar.gz"):
			w.Write(cur)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	cache := t.TempDir()
	m, err := DownloadVersion(ctx, srv.URL, "17.0.9+9", "x64", cache, nil)
	if err != nil || m.Version != "17.0.9+9" {
		t.Fatalf("官方 API：%+v %v", m, err)
	}
	if _, err := DownloadVersion(ctx, srv.URL, "17.0.10+7", "x64", cache, nil); err == nil || !strings.Contains(err.Error(), "没有 JDK 17.0.10+7") {
		t.Fatalf("没有的版本应报错：%v", err)
	}
	// 镜像目录里有好几个版本：取指定的那个，不取最新的
	m, err = DownloadVersion(ctx, srv.URL+"/mirror/{java}/{arch}/", "17.0.13+11", "x64", cache, nil)
	if err != nil || m.Version != "17.0.13+11-LTS" || m.ID != "jdk-17.0.13_11-LTS-x64" {
		t.Fatalf("镜像：%+v %v", m, err)
	}
	if _, err := DownloadVersion(ctx, srv.URL+"/mirror/{java}/{arch}/", "17.0.12+7", "x64", cache, nil); err == nil || !strings.Contains(err.Error(), "里没有 JDK 17.0.12+7") {
		t.Fatalf("镜像里没有的版本应报错：%v", err)
	}
	// 直接给的压缩包地址：下载后核对版本
	if _, err := DownloadVersion(ctx, srv.URL+"/x_17.0.13_11.tar.gz?{arch}", "17.0.9+9", "x64", t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "需要 17.0.9+9") {
		t.Fatalf("版本不对应报错：%v", err)
	}
	// 缓存里按版本找（后缀 -LTS 不影响）；最新的不是想要的也能找到旧的
	if f := FindVersion(cache, "17.0.13+11", "x64"); f == nil || f.ID != "jdk-17.0.13_11-LTS-x64" {
		t.Fatalf("FindVersion：%+v", f)
	}
	if f := FindVersion(cache, "17.0.9+9", "x64"); f == nil || FindVersion(cache, "17.0.9+9", "aarch64") != nil {
		t.Fatalf("FindVersion 旧版本 / 架构：%+v", f)
	}
	// 服务器上的 ID
	for _, c := range []struct {
		id, ver, arch string
		want          bool
	}{
		{"jdk-25.0.4.1_1-LTS-x64", "25.0.4.1+1", "x64", true},
		{"jdk-25.0.4.1_1-LTS-x64", "25.0.4.1+1-LTS", "x64", true},
		{"jdk-25.0.4.1_1-LTS-x64", "25.0.4.1+1", "aarch64", false},
		{"jdk-25.0.4_7-LTS-x64", "25.0.4.1+1", "x64", false},
		{"jdk-17.0.13_11-x64", "17.0.13+11", "x64", true},
		{"jdk-17.0.13_110-x64", "17.0.13+11", "x64", false},
	} {
		if got := MatchID(c.id, c.ver, c.arch); got != c.want {
			t.Errorf("MatchID(%s, %s, %s) = %v", c.id, c.ver, c.arch, got)
		}
	}
}
