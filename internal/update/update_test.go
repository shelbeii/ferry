package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.13.70", "0.13.66", true},
		{"0.14.0", "0.13.99", true},
		{"1.0.0", "0.99.99", true},
		{"0.13.66", "0.13.66", false},
		{"0.13.66", "0.13.66+abc123.dirty", false},
		{"0.13.67", "0.13.66+abc123.dirty", true},
		{"0.13.65", "0.13.66", false},
		{"0.13.70", "dev", false},
		{"v0.13.70", "v0.13.9", true},
		{"0.13", "0.12.1", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.latest, c.current); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v，应为 %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestAsset(t *testing.T) {
	cases := map[string]string{
		"windows/amd64": "Ferry_1.2.3_x64-setup.exe",
		"darwin/arm64":  "Ferry_1.2.3_aarch64.dmg",
		"darwin/amd64":  "Ferry_1.2.3_x64.dmg",
		"linux/arm64":   "ferry-linux-arm64",
		"windows/arm64": "",
	}
	for k, want := range cases {
		os, arch, _ := strings.Cut(k, "/")
		if got, _ := Asset("1.2.3", os, arch); got != want {
			t.Errorf("Asset(%s) = %q，应为 %q", k, got, want)
		}
	}
}

// fakeGitHub 假的 GitHub：最新版本 latest，Release 里有这台电脑用的安装包（内容 body）。
type fakeGitHub struct {
	latest    string
	body      []byte
	sum       string // SHA256SUMS.txt 里写的；空 = 按 body 算
	downloads atomic.Int32
	broken    bool // 下载安装包返回错误内容
	srv       *httptest.Server
}

func newFake(t *testing.T, latest string) *fakeGitHub {
	f := &fakeGitHub{latest: latest, body: []byte("new ferry " + latest)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) asset() string { a, _ := Asset(f.latest, runtime.GOOS, runtime.GOARCH); return a }

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	pre := "/" + Repo + "/releases/"
	switch {
	case p == pre+"latest":
		http.Redirect(w, r, f.srv.URL+pre+"tag/v"+f.latest, http.StatusFound)
	case p == "/repos/"+Repo+"/releases/tags/v"+f.latest:
		fmt.Fprintf(w, `{"body":"## 新功能\n- 检查更新","published_at":"2026-11-05T00:00:00Z","assets":[{"name":%q,"size":%d}]}`, f.asset(), len(f.body))
	case p == pre+"download/v"+f.latest+"/SHA256SUMS.txt":
		sum := f.sum
		if sum == "" {
			h := sha256.Sum256(f.body)
			sum = hex.EncodeToString(h[:])
		}
		fmt.Fprintf(w, "%s  %s\n%s  other-file\n", sum, f.asset(), strings.Repeat("0", 64))
	case p == pre+"download/v"+f.latest+"/"+f.asset():
		f.downloads.Add(1)
		if f.broken {
			w.Write([]byte("tampered"))
			return
		}
		w.Write(f.body)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) source() Source { return Source{Base: f.srv.URL, API: f.srv.URL} }

func TestCheckAndDownload(t *testing.T) {
	f := newFake(t, "0.13.70")
	if f.asset() == "" {
		t.Skip("这种系统没有安装包")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Ferry_0.13.60_x64-setup.exe"), []byte("old"), 0o644)
	ctx := context.Background()

	info, err := Check(ctx, f.source(), "0.13.66+abc", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Newer || info.Latest != "0.13.70" || info.Asset != f.asset() || !strings.Contains(info.Notes, "检查更新") || info.Size != int64(len(f.body)) || info.Downloaded != "" {
		t.Fatalf("检查结果不对：%+v", info)
	}
	if _, err := os.Stat(filepath.Join(dir, "Ferry_0.13.60_x64-setup.exe")); !os.IsNotExist(err) {
		t.Error("旧版本的安装包没删掉")
	}

	var last int64
	p, err := DownloadFile(ctx, f.source(), info.Latest, info.Asset, dir, func(done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != string(f.body) || last != int64(len(f.body)) {
		t.Fatalf("下载的内容不对：%q（进度 %d）", b, last)
	}
	// 再检查：记着已经下载好；再下载：不重新下载
	info, _ = Check(ctx, f.source(), "0.13.66", dir)
	if info.Downloaded != p {
		t.Errorf("Downloaded = %q，应为 %q", info.Downloaded, p)
	}
	if _, err := DownloadFile(ctx, f.source(), info.Latest, info.Asset, dir, nil); err != nil || f.downloads.Load() != 1 {
		t.Errorf("下载好的又下载了一次（%d 次，%v）", f.downloads.Load(), err)
	}
	// 文件被改过：不算下载好
	os.WriteFile(p, []byte("changed"), 0o644)
	if info, _ = Check(ctx, f.source(), "0.13.66", dir); info.Downloaded != "" {
		t.Error("改过的文件算成了下载好的")
	}

	// 已是最新：清空下载目录
	info, err = Check(ctx, f.source(), "0.13.70", dir)
	if err != nil || info.Newer {
		t.Fatalf("应为已是最新：%+v %v", info, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("已是最新时下载目录里还有 %d 个文件", len(ents))
	}
}

func TestDownloadBadSum(t *testing.T) {
	f := newFake(t, "0.13.70")
	if f.asset() == "" {
		t.Skip("这种系统没有安装包")
	}
	f.broken = true
	dir := t.TempDir()
	_, err := DownloadFile(context.Background(), f.source(), "0.13.70", f.asset(), dir, nil)
	if err == nil || !strings.Contains(err.Error(), "校验不对") {
		t.Fatalf("内容不对时应报校验错：%v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("校验不对的文件留下了 %d 个", len(ents))
	}
}

// 加速下载的内容不对（或打不开）：改为直接下载
func TestDownloadMirrorFallback(t *testing.T) {
	f := newFake(t, "0.13.70")
	if f.asset() == "" {
		t.Skip("这种系统没有安装包")
	}
	var mirrored atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrored.Add(1)
		w.Write([]byte("bad mirror"))
	}))
	defer mirror.Close()
	src := f.source()
	src.Mirror = mirror.URL + "/"
	p, err := DownloadFile(context.Background(), src, "0.13.70", f.asset(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != string(f.body) || mirrored.Load() != 1 || f.downloads.Load() != 1 {
		t.Fatalf("应先试加速再直接下载：内容 %q，加速 %d 次，直接 %d 次", b, mirrored.Load(), f.downloads.Load())
	}
}

func TestLatestErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := Latest(context.Background(), Source{Base: srv.URL}); err == nil || !strings.Contains(err.Error(), "还没有发布") {
		t.Errorf("没有发布时：%v", err)
	}
	srv.Close()
	if _, err := Latest(context.Background(), Source{Base: srv.URL}); err == nil || !strings.Contains(err.Error(), "连不上") {
		t.Errorf("连不上时：%v", err)
	}
}
