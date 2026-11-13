// Package update 检查 GitHub 上有没有 Ferry 的新版本，下载这台电脑用的安装包并校验，再装上。
//
// 最新版本看 github.com/<仓库>/releases/latest 跳转到哪个标签（不用 API：同一出口 IP 每小时只能调 60 次）；
// 发布说明尽量从 API 取，取不到就只给发布页地址。安装包可以经「GitHub 加速」（设置 → 运行环境）下载，
// 校验和（Release 里的 SHA256SUMS.txt）先直接从 GitHub 取，取不到才经加速取；校验和要用编进程序的公钥验过签名（sig.go）。
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo 发布 Ferry 的 GitHub 仓库。
const Repo = "shelbeii/ferry"

// Source 从哪里取。
type Source struct {
	Mirror string // GitHub 加速前缀，如 https://ghfast.top/；空 = 直接下载
	// 下面两个只给测试用（环境变量 FERRY_UPDATE_BASE、FERRY_UPDATE_API）：代替 https://github.com、https://api.github.com
	Base string
	API  string
}

// FromEnv 加上测试用的地址（环境变量）。
func (s Source) FromEnv(getenv func(string) string) Source {
	if v := getenv("FERRY_UPDATE_BASE"); v != "" {
		s.Base = v
	}
	if v := getenv("FERRY_UPDATE_API"); v != "" {
		s.API = v
	}
	return s
}

func (s Source) base() string {
	if s.Base != "" {
		return strings.TrimRight(s.Base, "/")
	}
	return "https://github.com"
}

func (s Source) api() string {
	if s.API != "" {
		return strings.TrimRight(s.API, "/")
	}
	return "https://api.github.com"
}

// mirrored 经加速的地址；没设加速（或测试地址）时为空。
func (s Source) mirrored(u string) string {
	if s.Mirror == "" || !strings.HasPrefix(u, s.base()+"/") {
		return ""
	}
	return strings.TrimRight(s.Mirror, "/") + "/" + u
}

func (s Source) download(version, file string) string {
	return s.base() + "/" + Repo + "/releases/download/v" + version + "/" + file
}

// Info 检查的结果。
type Info struct {
	Current   string `json:"current"`             // 这台电脑上的版本
	Latest    string `json:"latest"`              // GitHub 上的最新版本
	Newer     bool   `json:"newer"`               // 最新版本比这台电脑上的新
	Page      string `json:"page"`                // 发布页
	Notes     string `json:"notes,omitempty"`     // 发布说明（Markdown）；取不到时为空
	Published string `json:"published,omitempty"` // 发布时间（RFC 3339）
	Asset     string `json:"asset,omitempty"`     // 这台电脑用的安装包文件名；没有时为空
	Size      int64  `json:"size,omitempty"`      // 安装包大小；取不到时为 0
	// Install 怎么装：installer（Windows：运行安装包，装好后自动重新打开）、dmg（macOS：打开 .dmg，拖进「应用程序」）、
	// replace（Linux：替换 ferry 程序）；空 = 这种系统没有安装包
	Install string `json:"install"`
	// Downloaded 已经下载好、校验过的安装包（再点更新时不用重新下载）
	Downloaded string `json:"downloaded,omitempty"`
}

// Asset 某个版本在 goos/goarch 上用的文件和装法。
func Asset(version, goos, goarch string) (file, install string) {
	switch {
	case goos == "windows" && goarch == "amd64":
		return "Ferry_" + version + "_x64-setup.exe", "installer"
	case goos == "darwin" && goarch == "arm64":
		return "Ferry_" + version + "_aarch64.dmg", "dmg"
	case goos == "darwin" && goarch == "amd64":
		return "Ferry_" + version + "_x64.dmg", "dmg"
	case goos == "linux" && (goarch == "amd64" || goarch == "arm64"):
		return "ferry-linux-" + goarch, "replace"
	}
	return "", ""
}

// ParseVersion 把 0.13.66（或 v0.13.66、0.13.66+abc.dirty）拆成三个数；不是这种格式时 ok 为 false。
func ParseVersion(v string) (n [3]int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

// IsNewer latest 比 current 新。current 不是正式的版本号（如开发时的 dev）时为 false。
func IsNewer(latest, current string) bool {
	a, ok1 := ParseVersion(latest)
	b, ok2 := ParseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

var short = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Latest GitHub 上最新发布的版本号（不带 v）。
func Latest(ctx context.Context, s Source) (string, error) {
	u := s.base() + "/" + Repo + "/releases/latest"
	v, err := latestFrom(ctx, u)
	if err != nil {
		if m := s.mirrored(u); m != "" {
			if v2, err2 := latestFrom(ctx, m); err2 == nil {
				return v2, nil
			}
		}
		return "", err
	}
	return v, nil
}

func latestFrom(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ferry")
	resp, err := short.Do(req)
	if err != nil {
		return "", fmt.Errorf("连不上 GitHub：%w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		if resp.StatusCode == http.StatusNotFound {
			return "", errors.New("GitHub 上还没有发布过版本")
		}
		return "", fmt.Errorf("查询最新版本失败：HTTP %d", resp.StatusCode)
	}
	v := strings.TrimPrefix(loc[i+5:], "v")
	if _, ok := ParseVersion(v); !ok {
		return "", fmt.Errorf("最新版本的标签看不懂：%s", loc[i+5:])
	}
	return v, nil
}

// Check 查最新版本，和 current 比较。dir 是下载安装包的目录：已经下载好的会记在 Downloaded 里，旧的删掉。
func Check(ctx context.Context, s Source, current, dir string) (*Info, error) {
	latest, err := Latest(ctx, s)
	if err != nil {
		return nil, err
	}
	info := &Info{
		Current: current, Latest: latest, Newer: IsNewer(latest, current),
		Page: "https://github.com/" + Repo + "/releases/tag/v" + latest,
	}
	info.Asset, info.Install = Asset(latest, runtime.GOOS, runtime.GOARCH)
	if !info.Newer {
		Clean(dir, "")
		return info, nil
	}
	notes(ctx, s, info)
	if info.Asset != "" {
		Clean(dir, info.Asset)
		if p := filepath.Join(dir, info.Asset); fileOK(p, info.Size) {
			info.Downloaded = p
		}
	}
	return info, nil
}

// notes 尽量从 API 取发布说明、发布时间、安装包大小（取不到不算错）。
func notes(ctx context.Context, s Source, info *Info) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.api()+"/repos/"+Repo+"/releases/tags/v"+info.Latest, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "ferry")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := short.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var r struct {
		Body        string `json:"body"`
		PublishedAt string `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r) != nil {
		return
	}
	info.Notes, info.Published = r.Body, r.PublishedAt
	for _, a := range r.Assets {
		if a.Name == info.Asset {
			info.Size = a.Size
		}
	}
}

// fileOK 下载好的安装包在、大小对（不知道大小时只看在不在），并且旁边记着的校验和与内容一致。
func fileOK(p string, size int64) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || (size > 0 && st.Size() != size) {
		return false
	}
	want, err := os.ReadFile(p + ".sha256")
	if err != nil {
		return false
	}
	got, err := fileSum(p)
	return err == nil && got == strings.TrimSpace(string(want))
}

func fileSum(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Clean 删掉 dir 里 keep 以外的文件（以前下载的旧版本、没下载完的）。
func Clean(dir, keep string) {
	if dir == "" {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if keep != "" && (n == keep || n == keep+".sha256" || n == keep+".part") {
			continue
		}
		os.RemoveAll(filepath.Join(dir, n))
	}
}

// Sums 某个版本的 SHA256SUMS.txt（文件名 → sha256），用编进程序的公钥验过签名（SHA256SUMS.txt.sig）。
// 先直接从 GitHub 取，不行再经加速取（验签挡住了加速被换内容）。没有签名、签名不对都不装。
func Sums(ctx context.Context, s Source, version string) (map[string]string, error) {
	b, err := s.small(ctx, version, "SHA256SUMS.txt")
	if err != nil {
		return nil, fmt.Errorf("取校验和失败：%w", err)
	}
	sig, err := s.small(ctx, version, SumsSigFile)
	if err != nil {
		return nil, fmt.Errorf("%w（取不到签名文件 %s：%v）", ErrBadSig, SumsSigFile, err)
	}
	if err := VerifyTrusted(b, sig); err != nil {
		return nil, err
	}
	sums := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && len(f[0]) == 64 {
			sums[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	if len(sums) == 0 {
		return nil, errors.New("取校验和失败：SHA256SUMS.txt 是空的")
	}
	return sums, nil
}

// small 取 Release 里的小文件：先直接从 GitHub，不行再经加速。
func (s Source) small(ctx context.Context, version, file string) ([]byte, error) {
	u := s.download(version, file)
	b, err := getSmall(ctx, u)
	if err != nil {
		if m := s.mirrored(u); m != "" {
			if b2, err2 := getSmall(ctx, m); err2 == nil {
				return b2, nil
			}
		}
		return nil, err
	}
	return b, nil
}

func getSmall(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ferry")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

// Progress 下载进度。
type Progress func(done, total int64)

// Download 下载 version 版本这台电脑用的安装包到 dir，按 SHA256SUMS.txt 校验，返回文件路径。
// 设了加速时先经加速下载，失败（或内容不对）再直接下载。已经下载好的不再下载。
func Download(ctx context.Context, s Source, version, dir string, progress Progress) (string, error) {
	file, _ := Asset(version, runtime.GOOS, runtime.GOARCH)
	if file == "" {
		return "", fmt.Errorf("没有 %s/%s 的安装包", runtime.GOOS, runtime.GOARCH)
	}
	return DownloadFile(ctx, s, version, file, dir, progress)
}

// DownloadFile 同 Download，指定文件名。
func DownloadFile(ctx context.Context, s Source, version, file, dir string, progress Progress) (string, error) {
	sums, err := Sums(ctx, s, version)
	if err != nil {
		return "", err
	}
	want := sums[file]
	if want == "" {
		return "", fmt.Errorf("%s 版本里没有 %s", version, file)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	Clean(dir, file)
	dst := filepath.Join(dir, file)
	if got, err := fileSum(dst); err == nil && got == want {
		os.WriteFile(dst+".sha256", []byte(want), 0o644)
		return dst, nil
	}
	u := s.download(version, file)
	var errs []string
	for _, link := range []string{s.mirrored(u), u} {
		if link == "" {
			continue
		}
		err := fetchTo(ctx, link, dst, want, progress)
		if err == nil {
			os.WriteFile(dst+".sha256", []byte(want), 0o644)
			return dst, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		errs = append(errs, err.Error())
	}
	return "", errors.New(strings.Join(errs, "；"))
}

func fetchTo(ctx context.Context, u, dst, want string, progress Progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ferry")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("下载 %s 失败：%w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s 失败：HTTP %d", u, resp.StatusCode)
	}
	part := dst + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	h := sha256.New()
	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	var done int64
	last := time.Time{}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(part)
				return werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil && time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(part)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("下载 %s 中断：%w", u, rerr)
		}
	}
	if progress != nil {
		progress(done, total)
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(part)
		return fmt.Errorf("从 %s 下载的文件校验不对（sha256 %s，应为 %s）", u, got[:12], want[:12])
	}
	os.Remove(dst)
	return os.Rename(part, dst)
}
