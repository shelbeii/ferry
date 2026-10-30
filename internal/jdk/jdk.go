// Package jdk 本机的 JDK 库：从 Adoptium（或镜像）下载、手动导入、按大版本与架构查找。
// 部署时 Ferry 把这里的 JDK 上传到服务器（service.runtime: {java: N}）。
package jdk

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultSource 默认的下载源：Adoptium（Eclipse Temurin）官方 API。
const DefaultSource = "https://api.adoptium.net"

// Meta 本机缓存里的一个 JDK。
type Meta struct {
	ID       string    `json:"id"`      // 如 jdk-17.0.13_11-x64
	Java     int       `json:"java"`    // 大版本
	Version  string    `json:"version"` // 完整版本，如 17.0.13+11
	Arch     string    `json:"arch"`    // x64 / aarch64
	Vendor   string    `json:"vendor,omitempty"`
	SHA256   string    `json:"sha256"`
	Size     int64     `json:"size"`
	File     string    `json:"file"` // 缓存目录里的文件名
	Source   string    `json:"source,omitempty"`
	Added    time.Time `json:"added"`
	FullPath string    `json:"path,omitempty"` // 给界面看的完整路径
}

// ArchOf 服务器架构（amd64 / arm64）→ JDK 发行包的架构名（x64 / aarch64）。
func ArchOf(serverArch string) string {
	if serverArch == "arm64" || serverArch == "aarch64" {
		return "aarch64"
	}
	return "x64"
}

// List 缓存里的全部 JDK（大版本从新到旧）。
func List(dir string) []Meta {
	ents, _ := os.ReadDir(dir)
	var out []Meta
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var m Meta
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || json.Unmarshal(b, &m) != nil {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, m.File)); err != nil || fi.Size() != m.Size {
			continue // 压缩包不在或不完整
		}
		m.FullPath = filepath.Join(dir, m.File)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Java != out[j].Java {
			return out[i].Java > out[j].Java
		}
		return versionLess(out[j].Version, out[i].Version)
	})
	return out
}

// Find 缓存里大版本为 java、架构为 arch 的最新 JDK。
func Find(dir string, java int, arch string) *Meta {
	for _, m := range List(dir) {
		if m.Java == java && m.Arch == arch {
			return &m
		}
	}
	return nil
}

// FindVersion 缓存里版本为 version（如 25.0.4.1+1）、架构为 arch 的 JDK。
func FindVersion(dir, version, arch string) *Meta {
	for _, m := range List(dir) {
		if m.Arch == arch && SameVersion(m.Version, version) {
			return &m
		}
	}
	return nil
}

// SameVersion 两个完整版本是不是同一个：不管 -LTS 之类的后缀（25.0.4.1+1-LTS 与 25.0.4.1+1 相同）。
func SameVersion(a, b string) bool {
	a, _, _ = strings.Cut(a, "-")
	b, _, _ = strings.Cut(b, "-")
	return a != "" && a == b
}

// MatchID 服务器上的 JDK（ID，如 jdk-25.0.4.1_1-LTS-x64）是不是这个版本、这个架构的。
func MatchID(id, version, arch string) bool {
	v, _, _ := strings.Cut(version, "-")
	head := "jdk-" + safe(v) + "-"
	return strings.HasPrefix(id, head) && strings.HasSuffix(id, "-"+arch)
}

// Remove 从缓存删除。
func Remove(dir, id string) error {
	for _, m := range List(dir) {
		if m.ID == id {
			os.Remove(filepath.Join(dir, m.File))
			return os.Remove(filepath.Join(dir, m.ID+".json"))
		}
	}
	return fmt.Errorf("本机没有 %s", id)
}

// ---------------------------------------------------------------- 检查压缩包

var reRelease = regexp.MustCompile(`(?m)^([A-Z_]+)="?([^"\n]*)"?`)

// Inspect 读 JDK 压缩包里的 release 文件，确认是 Linux JDK 并得到版本与架构；顺便算 sha256。
func Inspect(p string) (*Meta, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	gz, err := gzip.NewReader(io.TeeReader(f, h))
	if err != nil {
		return nil, fmt.Errorf("%s 不是 tar.gz：%w", filepath.Base(p), err)
	}
	tr := tar.NewReader(gz)
	var release map[string]string
	top, hasJava := "", false
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s 已损坏：%w", filepath.Base(p), err)
		}
		name := strings.TrimPrefix(path.Clean(hd.Name), "./")
		first, rest, _ := strings.Cut(name, "/")
		if top == "" {
			top = first
		}
		switch rest {
		case "release":
			b, _ := io.ReadAll(io.LimitReader(tr, 64<<10))
			release = map[string]string{}
			for _, m := range reRelease.FindAllStringSubmatch(string(b), -1) {
				release[m[1]] = m[2]
			}
		case "bin/java":
			hasJava = true
		}
	}
	io.Copy(io.Discard, f) // 读完，sha256 才完整
	if release == nil || !hasJava {
		return nil, fmt.Errorf("%s 不是 JDK 压缩包（没有 release 或 bin/java）", filepath.Base(p))
	}
	if os := release["OS_NAME"]; os != "" && !strings.EqualFold(os, "Linux") {
		return nil, fmt.Errorf("%s 是 %s 的 JDK，服务器需要 Linux 版", filepath.Base(p), os)
	}
	ver := release["JAVA_RUNTIME_VERSION"]
	if ver == "" {
		ver = release["JAVA_VERSION"]
	}
	major := Major(release["JAVA_VERSION"])
	if major == 0 {
		return nil, fmt.Errorf("%s 的 release 里读不出 Java 版本", filepath.Base(p))
	}
	arch := ""
	switch strings.ToLower(release["OS_ARCH"]) {
	case "x86_64", "amd64", "x64":
		arch = "x64"
	case "aarch64", "arm64":
		arch = "aarch64"
	default:
		return nil, fmt.Errorf("%s 的架构 %s 不受支持（需要 x86_64 或 aarch64）", filepath.Base(p), release["OS_ARCH"])
	}
	fi, _ := os.Stat(p)
	m := &Meta{Java: major, Version: ver, Arch: arch, Vendor: strings.Trim(release["IMPLEMENTOR"], `"`),
		SHA256: hex.EncodeToString(h.Sum(nil)), Size: fi.Size()}
	m.ID = "jdk-" + safe(ver) + "-" + arch
	m.File = m.ID + ".tar.gz"
	return m, nil
}

var reUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safe(s string) string { return reUnsafe.ReplaceAllString(s, "_") }

// Major 1.8.0_412 → 8；17.0.13 → 17；21 → 21。
func Major(v string) int {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r < '0' || r > '9' })
	if len(parts) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(parts[0])
	if n == 1 && len(parts) > 1 {
		n, _ = strconv.Atoi(parts[1])
	}
	return n
}

// versionLess 按数字逐段比较版本（17.0.9+9 < 17.0.13+11）。
func versionLess(a, b string) bool {
	num := func(s string) []int {
		var out []int
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
			n, _ := strconv.Atoi(p)
			out = append(out, n)
		}
		return out
	}
	x, y := num(a), num(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// ---------------------------------------------------------------- 导入

// Import 把本机的 JDK 压缩包（Linux 版 tar.gz）加入缓存。
func Import(file, dir string) (*Meta, error) {
	m, err := Inspect(file)
	if err != nil {
		return nil, err
	}
	m.Source = "导入：" + file
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, m.File)
	if err := copyFile(file, dst+".part"); err != nil {
		return nil, err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		return nil, err
	}
	return m, saveMeta(dir, m)
}

func saveMeta(dir string, m *Meta) error {
	m.Added = time.Now()
	m.FullPath = ""
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(filepath.Join(dir, m.ID+".json"), b, 0o644)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// ---------------------------------------------------------------- 下载

// Progress 下载进度：已下载、总大小（不知道时为 0）。
type Progress func(done, total int64)

// Download 下载大版本为 java、架构为 arch 的 JDK 到缓存目录。source：
//   - 空或 Adoptium 兼容的 API 地址（默认 https://api.adoptium.net）
//   - 含 {java}、{arch} 的地址：以 / 结尾为镜像的目录列表（取其中最新的 jdk_<arch>_linux 压缩包），否则是压缩包本身
//
// 有 sha256 时校验（Adoptium API 自带；镜像取同名的 .sha256.txt / .sha256）。
func Download(ctx context.Context, source string, java int, arch, dir string, progress Progress) (*Meta, error) {
	if source == "" {
		source = DefaultSource
	}
	link, sum, err := resolve(ctx, source, java, arch)
	if err != nil {
		return nil, err
	}
	return downloadTo(ctx, link, sum, dir, progress, func(m *Meta) error {
		if m.Java != java || m.Arch != arch {
			return fmt.Errorf("下载到的是 Java %d（%s），需要 Java %d（%s）：请检查下载源", m.Java, m.Arch, java, arch)
		}
		return nil
	})
}

// DownloadVersion 下载指定版本（如 25.0.4.1+1）、架构为 arch 的 JDK 到缓存目录。source 同 Download：
// Adoptium API 按版本查；镜像的目录列表里找这个版本的压缩包；直接给的压缩包地址下载后核对版本。
func DownloadVersion(ctx context.Context, source, version, arch, dir string, progress Progress) (*Meta, error) {
	if source == "" {
		source = DefaultSource
	}
	link, sum, err := resolveVersion(ctx, source, version, arch)
	if err != nil {
		return nil, err
	}
	return downloadTo(ctx, link, sum, dir, progress, func(m *Meta) error {
		if !SameVersion(m.Version, version) || m.Arch != arch {
			return fmt.Errorf("下载到的是 JDK %s（%s），需要 %s（%s）：请检查下载源", m.Version, m.Arch, version, arch)
		}
		return nil
	})
}

// downloadTo 下载 link 到缓存目录，校验 sha256（有的话）和内容（check），存好元数据。
func downloadTo(ctx context.Context, link, sum, dir string, progress Progress, check func(*Meta) error) (*Meta, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".download-*.tar.gz")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	got, err := fetch(ctx, link, tmp, progress)
	tmp.Close()
	if err != nil {
		return nil, err
	}
	if sum != "" && !strings.EqualFold(sum, got) {
		return nil, fmt.Errorf("下载的文件校验失败（sha256 %s，应为 %s），请重试或换一个下载源", got[:12], sum[:12])
	}
	m, err := Inspect(tmp.Name())
	if err != nil {
		return nil, err
	}
	if err := check(m); err != nil {
		return nil, err
	}
	m.Source = link
	if err := os.Rename(tmp.Name(), filepath.Join(dir, m.File)); err != nil {
		return nil, err
	}
	return m, saveMeta(dir, m)
}

// resolve 下载地址与 sha256（可能为空）。
func resolve(ctx context.Context, source string, java int, arch string) (string, string, error) {
	if strings.Contains(source, "{java}") || strings.Contains(source, "{arch}") {
		u := strings.NewReplacer("{java}", strconv.Itoa(java), "{arch}", arch).Replace(source)
		if strings.HasSuffix(u, "/") {
			name, err := latestInListing(ctx, u, java, arch)
			if err != nil {
				return "", "", err
			}
			u += name
		}
		return u, mirrorSum(ctx, u), nil
	}
	api := strings.TrimRight(source, "/") + fmt.Sprintf("/v3/assets/latest/%d/hotspot?architecture=%s&image_type=jdk&os=linux&vendor=eclipse",
		java, url.QueryEscape(arch))
	body, err := get(ctx, api, 1<<20)
	if err != nil {
		return "", "", fmt.Errorf("查询 JDK %d 失败：%w", java, err)
	}
	var assets []struct {
		Binary struct {
			Package struct {
				Link     string `json:"link"`
				Checksum string `json:"checksum"`
			} `json:"package"`
		} `json:"binary"`
	}
	if err := json.Unmarshal(body, &assets); err != nil {
		return "", "", fmt.Errorf("下载源返回的不是 Adoptium API 的格式：%w", err)
	}
	if len(assets) == 0 || assets[0].Binary.Package.Link == "" {
		return "", "", fmt.Errorf("下载源没有 Java %d 的 Linux %s JDK", java, arch)
	}
	return assets[0].Binary.Package.Link, assets[0].Binary.Package.Checksum, nil
}

// resolveVersion 指定版本的下载地址与 sha256（可能为空）。
func resolveVersion(ctx context.Context, source, version, arch string) (string, string, error) {
	v, _, _ := strings.Cut(version, "-")
	java := Major(v)
	if strings.Contains(source, "{java}") || strings.Contains(source, "{arch}") {
		u := strings.NewReplacer("{java}", strconv.Itoa(java), "{arch}", arch).Replace(source)
		if strings.HasSuffix(u, "/") {
			name, err := versionInListing(ctx, u, v, arch)
			if err != nil {
				return "", "", err
			}
			u += name
		}
		return u, mirrorSum(ctx, u), nil // 直接给的地址：下载后核对版本
	}
	api := strings.TrimRight(source, "/") + "/v3/assets/release_name/eclipse/" + url.PathEscape("jdk-"+v) +
		"?architecture=" + url.QueryEscape(arch) + "&heap_size=normal&image_type=jdk&jvm_impl=hotspot&os=linux&project=jdk"
	body, err := get(ctx, api, 1<<20)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return "", "", fmt.Errorf("下载源没有 JDK %s（linux/%s）：检查版本号（java -version 里 build 后面那一段，如 25.0.4.1+1）", version, arch)
		}
		return "", "", fmt.Errorf("查询 JDK %s 失败：%w", version, err)
	}
	var rel struct {
		Binaries []struct {
			Package struct {
				Link     string `json:"link"`
				Checksum string `json:"checksum"`
			} `json:"package"`
		} `json:"binaries"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", "", fmt.Errorf("下载源返回的不是 Adoptium API 的格式：%w", err)
	}
	if len(rel.Binaries) == 0 || rel.Binaries[0].Package.Link == "" {
		return "", "", fmt.Errorf("下载源没有 JDK %s 的 Linux %s 版", version, arch)
	}
	return rel.Binaries[0].Package.Link, rel.Binaries[0].Package.Checksum, nil
}

// versionInListing 镜像目录列表里指定版本的 jdk_<arch>_linux 压缩包（文件名里版本的 + 写成 _，如 25.0.4.1_1）。
func versionInListing(ctx context.Context, dirURL, version, arch string) (string, error) {
	body, err := get(ctx, dirURL, 4<<20)
	if err != nil {
		return "", fmt.Errorf("读取镜像目录失败：%w", err)
	}
	want := "_" + safe(version) + ".tar.gz"
	for _, m := range reHref.FindAllStringSubmatch(string(body), -1) {
		name := path.Base(m[1])
		if strings.Contains(name, "jdk_"+arch+"_linux") && strings.HasSuffix(name, want) {
			return name, nil
		}
	}
	return "", fmt.Errorf("镜像目录 %s 里没有 JDK %s（linux/%s）：镜像一般只留最新的几个版本，换成 Adoptium 官方源，或手动导入这个版本的压缩包", dirURL, version, arch)
}

var reHref = regexp.MustCompile(`href="([^"?#]+\.tar\.gz)"`)

// latestInListing 镜像目录列表里最新的 jdk_<arch>_linux 压缩包。
func latestInListing(ctx context.Context, dirURL string, java int, arch string) (string, error) {
	body, err := get(ctx, dirURL, 4<<20)
	if err != nil {
		return "", fmt.Errorf("读取镜像目录失败：%w", err)
	}
	var best string
	for _, m := range reHref.FindAllStringSubmatch(string(body), -1) {
		name := path.Base(m[1])
		if !strings.Contains(name, "jdk_"+arch+"_linux") || strings.Contains(name, "debugimage") || strings.Contains(name, "testimage") {
			continue
		}
		if best == "" || versionLess(best, name) {
			best = name
		}
	}
	if best == "" {
		return "", fmt.Errorf("镜像目录 %s 里没有 jdk_%s_linux 的压缩包", dirURL, arch)
	}
	return best, nil
}

var reHex64 = regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`)

func mirrorSum(ctx context.Context, u string) string {
	for _, ext := range []string{".sha256.txt", ".sha256"} {
		if b, err := get(ctx, u+ext, 4096); err == nil {
			if m := reHex64.Find(b); m != nil {
				return string(m)
			}
		}
	}
	return ""
}

func get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ferry")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s：HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// fetch 下载到 w，返回 sha256。
func fetch(ctx context.Context, u string, w io.Writer, progress Progress) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ferry")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("下载 %s 失败：%w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 %s 失败：HTTP %d", u, resp.StatusCode)
	}
	h := sha256.New()
	total, done := resp.ContentLength, int64(0)
	if total < 0 {
		total = 0
	}
	br := bufio.NewReaderSize(resp.Body, 256<<10)
	buf := make([]byte, 256<<10)
	last := time.Time{}
	for {
		n, err := br.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return "", werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil && time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				progress(done, total)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("下载中断：%w", err)
		}
	}
	if progress != nil {
		progress(done, total)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Get 读取一个地址的内容（最多 limit 字节）；其他运行环境（nginx）的下载也用它。
func Get(ctx context.Context, u string, limit int64) ([]byte, error) { return get(ctx, u, limit) }

// Fetch 下载到 w（带进度），返回 sha256。
func Fetch(ctx context.Context, u string, w io.Writer, progress Progress) (string, error) {
	return fetch(ctx, u, w, progress)
}
