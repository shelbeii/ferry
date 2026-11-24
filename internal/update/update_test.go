package update

import (
	"context"
	"crypto/ed25519"
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
	broken    bool   // 下载安装包返回错误内容
	seed      []byte // 签名用的私钥；nil = 不放签名文件
	original  []byte // 不为 nil：签名签的是这份校验和（安装包和校验和被一起换掉了，签名还是原来的）
	srv       *httptest.Server
}

// testKey 生成一把测试钥，这个测试里当作编进程序的公钥。
func testKey(t *testing.T) []byte {
	pub, priv, _ := ed25519.GenerateKey(nil)
	old := trustedKeys
	trustedKeys = []ed25519.PublicKey{pub}
	t.Cleanup(func() { trustedKeys = old })
	return priv.Seed()
}

func newFake(t *testing.T, latest string) *fakeGitHub {
	f := &fakeGitHub{latest: latest, body: []byte("new ferry " + latest), seed: testKey(t)}
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
		w.Write(f.sums())
	case p == pre+"download/v"+f.latest+"/"+SumsSigFile && f.seed != nil:
		sig, _ := SignSums(f.seed, f.signed())
		w.Write(sig)
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

func (f *fakeGitHub) sums() []byte {
	sum := f.sum
	if sum == "" {
		h := sha256.Sum256(f.body)
		sum = hex.EncodeToString(h[:])
	}
	return []byte(fmt.Sprintf("%s  %s\n%s  other-file\n", sum, f.asset(), strings.Repeat("0", 64)))
}

// signed 签名时用的校验和：平时就是 sums()；swapped（攻击者把安装包和校验和一起换了）时是原来的
func (f *fakeGitHub) signed() []byte {
	if f.original != nil {
		return f.original
	}
	return f.sums()
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

func TestNotesFallbackToReleaseMeta(t *testing.T) {
	f := newFake(t, "0.18.6")
	if f.asset() == "" {
		t.Skip("这种系统没有安装包")
	}
	var apiHits, metaHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + Repo + "/releases/latest":
			http.Redirect(w, r, "/"+Repo+"/releases/tag/v0.18.6", http.StatusFound)
		case "/repos/" + Repo + "/releases/tags/v0.18.6":
			apiHits.Add(1)
			http.Error(w, "API rate limit exceeded", http.StatusForbidden)
		case "/" + Repo + "/releases/download/v0.18.6/" + ReleaseMetaFile:
			metaHits.Add(1)
			fmt.Fprintf(w, `{"notes":"## 本次更新\n\n- 发布 Ferry 0.18.6\n","published":"2026-11-24T02:43:06Z","assets":{%q:42000}}`, f.asset())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	info, err := Check(context.Background(), Source{Base: srv.URL, API: srv.URL}, "0.18.5", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if apiHits.Load() != 1 || metaHits.Load() != 1 {
		t.Fatalf("应先试 API 再回退 release.json：api=%d meta=%d", apiHits.Load(), metaHits.Load())
	}
	if !strings.Contains(info.Notes, "发布 Ferry 0.18.6") || info.Published != "2026-11-24T02:43:06Z" || info.Size != 42000 {
		t.Fatalf("回退 release.json 结果不对：%+v", info)
	}
}

func TestNotesFallbackToAtom(t *testing.T) {
	atomXML := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>tag:github.com,2008:Repository/12345/v0.18.6</id>
    <updated>2026-11-24T02:43:06Z</updated>
    <link rel="alternate" type="text/html" href="https://github.com/` + Repo + `/releases/tag/v0.18.6"/>
    <content type="html">&lt;h2&gt;&lt;a class="anchor" href="#update"&gt;&lt;svg&gt;&lt;path/&gt;&lt;/svg&gt;&lt;/a&gt;本次更新&lt;/h2&gt;
&lt;ul&gt;
&lt;li&gt;发布 Ferry 0.18.6&lt;/li&gt;
&lt;li&gt;避免软件列表无匹配系统服务时回退全量枚举&lt;/li&gt;
&lt;/ul&gt;
&lt;h2&gt;下载&lt;/h2&gt;
&lt;table&gt;&lt;thead&gt;&lt;tr&gt;&lt;th&gt;文件&lt;/th&gt;&lt;th&gt;用途&lt;/th&gt;&lt;/tr&gt;&lt;/thead&gt;&lt;tbody&gt;&lt;tr&gt;&lt;td&gt;&lt;code&gt;Ferry_0.18.6_x64-setup.exe&lt;/code&gt;&lt;/td&gt;&lt;td&gt;Windows 安装包&lt;/td&gt;&lt;/tr&gt;&lt;/tbody&gt;&lt;/table&gt;
&lt;p&gt;&lt;strong&gt;Full Changelog&lt;/strong&gt;: &lt;a href="https://github.com/` + Repo + `/compare/v0.18.5...v0.18.6"&gt;v0.18.5...v0.18.6&lt;/a&gt;&lt;/p&gt;</content>
  </entry>
</feed>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + Repo + "/releases/latest":
			http.Redirect(w, r, "/"+Repo+"/releases/tag/v0.18.6", http.StatusFound)
		case "/repos/" + Repo + "/releases/tags/v0.18.6":
			http.Error(w, "API rate limit exceeded", http.StatusForbidden)
		case "/" + Repo + "/releases.atom":
			w.Header().Set("Content-Type", "application/atom+xml")
			w.Write([]byte(atomXML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	info, err := Check(context.Background(), Source{Base: srv.URL, API: srv.URL}, "0.18.5", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.Published != "2026-11-24T02:43:06Z" {
		t.Fatalf("发布时间不对：%q", info.Published)
	}
	for _, want := range []string{
		"## 本次更新",
		"- 发布 Ferry 0.18.6",
		"- 避免软件列表无匹配系统服务时回退全量枚举",
		"| `Ferry_0.18.6_x64-setup.exe` | Windows 安装包 |",
		"**Full Changelog**: [v0.18.5...v0.18.6](https://github.com/" + Repo + "/compare/v0.18.5...v0.18.6)",
	} {
		if !strings.Contains(info.Notes, want) {
			t.Errorf("Atom 转 Markdown 缺少 %q，实际：\n%s", want, info.Notes)
		}
	}
}

func TestNotesViaMirrorWhenDirectFails(t *testing.T) {
	f := newFake(t, "0.18.6")
	if f.asset() == "" {
		t.Skip("这种系统没有安装包")
	}
	// 直连地址直接关掉，模拟国内直连不上只能走 GitHub 加速
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	var mirrorMetaHits atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+Repo+"/releases/latest"):
			http.Redirect(w, r, deadURL+"/"+Repo+"/releases/tag/v0.18.6", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/"+Repo+"/releases/download/v0.18.6/"+ReleaseMetaFile):
			mirrorMetaHits.Add(1)
			fmt.Fprintf(w, `{"notes":"## 本次更新\n\n- 加速节点说明","published":"2026-11-24T02:43:06Z","assets":{%q:12345}}`, f.asset())
		default:
			http.NotFound(w, r)
		}
	}))
	defer mirror.Close()

	src := Source{Base: deadURL, API: deadURL, Mirror: mirror.URL + "/"}
	info, err := Check(context.Background(), src, "0.18.5", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if mirrorMetaHits.Load() != 1 || !strings.Contains(info.Notes, "加速节点说明") || info.Size != 12345 {
		t.Fatalf("走加速取说明结果不对：hits=%d info=%+v", mirrorMetaHits.Load(), info)
	}
}

func TestNotesAPIRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + Repo + "/releases/latest":
			http.Redirect(w, r, "/"+Repo+"/releases/tag/v0.18.6", http.StatusFound)
		case "/repos/" + Repo + "/releases/tags/v0.18.6":
			http.Redirect(w, r, "/canonical/releases/tags/v0.18.6", http.StatusMovedPermanently)
		case "/canonical/releases/tags/v0.18.6":
			w.Write([]byte(`{"body":"## 跳转后说明","published_at":"2026-11-24T02:43:06Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	info, err := Check(context.Background(), Source{Base: srv.URL, API: srv.URL}, "0.18.5", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.Notes != "## 跳转后说明" {
		t.Fatalf("API 跳转后没取到说明：%q", info.Notes)
	}
}

func TestCheckWithOptionsIncludeDetails(t *testing.T) {
	f := newFake(t, "0.18.9")
	if f.asset() == "" {
		t.Skip("这种系统没有安装包")
	}
	dir := t.TempDir()
	ctx := context.Background()

	// 默认 Check 在版本相同时不拉取说明与大小
	plain, err := Check(ctx, f.source(), "0.18.9", dir)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Newer || plain.Notes != "" || plain.Size != 0 {
		t.Fatalf("默认 Check 同版本应跳过详情：%+v", plain)
	}

	// 先下载一份到 dir，再验证 IncludeDetails 不会误删且能返回详情与 Downloaded
	downloaded, err := DownloadFile(ctx, f.source(), "0.18.9", f.asset(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	forced, err := CheckWithOptions(ctx, f.source(), "0.18.9", dir, CheckOptions{IncludeDetails: true})
	if err != nil {
		t.Fatal(err)
	}
	if forced.Newer {
		t.Fatalf("同版本 Newer 仍应为 false：%+v", forced)
	}
	if !strings.Contains(forced.Notes, "检查更新") || forced.Size != int64(len(f.body)) || forced.Downloaded != downloaded {
		t.Fatalf("IncludeDetails 结果不对：%+v", forced)
	}
}
