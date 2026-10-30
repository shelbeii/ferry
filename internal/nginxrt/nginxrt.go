// Package nginxrt 本机的 nginx 库：静态链接的 nginx 单文件程序（任何 Linux 发行版都能直接运行），
// 从 nginx-binaries（或镜像）下载、手动导入、按版本系列与架构查找。
// 部署前端时 Ferry 把这里的 nginx 上传到服务器的部署目录（static.port），单独运行站点，不碰服务器上已有的 nginx。
package nginxrt

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/jdk"
)

// DefaultSource 默认的下载源：jirutka/nginx-binaries（目录下有 index.json 与各个程序文件）。
const DefaultSource = "https://jirutka.github.io/nginx-binaries"

// Meta 本机库里的一个 nginx。
type Meta struct {
	ID      string    `json:"id"`      // 如 nginx-1.30.5-x86_64（也是服务器上的目录名）
	Version string    `json:"version"` // 1.30.5
	Series  string    `json:"series"`  // 1.30
	Arch    string    `json:"arch"`    // x86_64 / aarch64
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	File    string    `json:"file"` // 库目录里的文件名
	Source  string    `json:"source,omitempty"`
	AddedAt time.Time `json:"addedAt"`
}

// Stable 稳定版（次版本号为偶数，如 1.30）；奇数为开发版（mainline）。
func (m Meta) Stable() bool { return Stable(m.Version) }

// ArchOf 服务器架构（uname -m 或 Go 的写法）→ nginx-binaries 的写法。
func ArchOf(serverArch string) string {
	switch serverArch {
	case "arm64", "aarch64":
		return "aarch64"
	case "ppc64le":
		return "ppc64le"
	}
	return "x86_64"
}

var (
	reVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	reSeries  = regexp.MustCompile(`^\d+\.\d+$`)
	reArch    = regexp.MustCompile(`^(x86_64|aarch64|ppc64le)$`)
	// nginx-binaries 的文件名：nginx-1.30.5-x86_64-linux
	reFileName = regexp.MustCompile(`nginx-(\d+\.\d+\.\d+)-(x86_64|aarch64|ppc64le)-linux`)
)

// ValidSeries 版本系列的写法：1.30。
func ValidSeries(s string) bool { return reSeries.MatchString(s) }

// SeriesOf 1.30.5 → 1.30。
func SeriesOf(v string) string {
	p := strings.SplitN(v, ".", 3)
	if len(p) < 2 {
		return v
	}
	return p[0] + "." + p[1]
}

// Stable 版本（或系列）是否稳定版。
func Stable(v string) bool {
	p := strings.SplitN(v, ".", 3)
	if len(p) < 2 {
		return false
	}
	n, err := strconv.Atoi(p[1])
	return err == nil && n%2 == 0
}

// Less 版本比较：1.9.0 < 1.30.5。
func Less(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

// List 库里的全部 nginx（版本从新到旧）。
func List(dir string) []Meta {
	ents, _ := os.ReadDir(dir)
	var out []Meta
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) != nil || !reVersion.MatchString(m.Version) || m.File == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, m.File)); err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return Less(out[j].Version, out[i].Version) })
	return out
}

// Find 库里某个系列（空 = 最新的稳定版；没有稳定版时最新的）、某个架构的最新版本。
func Find(dir, series, arch string) *Meta {
	var any *Meta
	for _, m := range List(dir) {
		if m.Arch != arch {
			continue
		}
		if series != "" {
			if m.Series == series {
				return &m
			}
			continue
		}
		if m.Stable() {
			return &m
		}
		if any == nil {
			mm := m
			any = &mm
		}
	}
	return any
}

// Remove 从库里删除。
func Remove(dir, id string) error {
	for _, m := range List(dir) {
		if m.ID == id {
			os.Remove(filepath.Join(dir, m.File))
			return os.Remove(filepath.Join(dir, m.ID+".json"))
		}
	}
	return fmt.Errorf("库里没有 %s", id)
}

// Import 把下载好的 nginx 程序加入库。版本与架构从文件名里取（nginx-1.30.5-x86_64-linux）；
// 文件名不是这个格式时用 version、arch 参数。
func Import(file, dir, version, arch string) (*Meta, error) {
	if m := reFileName.FindStringSubmatch(filepath.Base(file)); m != nil {
		if version == "" {
			version = m[1]
		}
		if arch == "" {
			arch = m[2]
		}
	}
	if !reVersion.MatchString(version) {
		return nil, errors.New("看不出 nginx 的版本：文件名应像 nginx-1.30.5-x86_64-linux，或者指定版本（如 1.30.5）")
	}
	if arch == "" {
		arch = "x86_64"
	}
	if !reArch.MatchString(arch) {
		return nil, fmt.Errorf("架构只能是 x86_64、aarch64、ppc64le（当前 %q）", arch)
	}
	if err := checkELF(file); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := newMeta(version, arch)
	if err := copyFile(file, filepath.Join(dir, m.File)); err != nil {
		return nil, err
	}
	sum, size, err := fileSums(filepath.Join(dir, m.File))
	if err != nil {
		return nil, err
	}
	m.SHA256, m.Size, m.Source = sum.sha256, size, file
	return m, saveMeta(dir, m)
}

func newMeta(version, arch string) *Meta {
	id := "nginx-" + version + "-" + arch
	return &Meta{ID: id, Version: version, Series: SeriesOf(version), Arch: arch, File: id, AddedAt: time.Now()}
}

// checkELF 必须是 Linux 程序（ELF），防止导入网页、压缩包之类。
func checkELF(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil || string(head) != "\x7fELF" {
		return errors.New("不是 Linux 程序：请导入 nginx-binaries 上的单文件 nginx（如 nginx-1.30.5-x86_64-linux），不是压缩包或安装包")
	}
	return nil
}

// ---------------------------------------------------------------- 下载

// Entry index.json 里的一项。
type Entry struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Variant  string `json:"variant"`
	Arch     string `json:"arch"`
	OS       string `json:"os"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"` // sha1:… 或 sha256:…
}

// Available 下载源上某个架构的全部 nginx 版本（从新到旧）。
func Available(ctx context.Context, source, arch string) ([]Entry, error) {
	if source == "" {
		source = DefaultSource
	}
	b, err := jdk.Get(ctx, strings.TrimRight(source, "/")+"/index.json", 16<<20)
	if err != nil {
		return nil, fmt.Errorf("读取下载源的版本列表失败：%w", err)
	}
	var idx struct {
		Contents []Entry `json:"contents"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("下载源的 index.json 格式不对：%w", err)
	}
	var out []Entry
	for _, e := range idx.Contents {
		if e.Name == "nginx" && e.OS == "linux" && e.Variant == "" && e.Arch == arch && reVersion.MatchString(e.Version) && e.Filename != "" {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return Less(out[j].Version, out[i].Version) })
	return out, nil
}

// Pick 选版本：系列为空时取最新的稳定版。
func Pick(list []Entry, series string) *Entry {
	for _, e := range list {
		if (series == "" && Stable(e.Version)) || (series != "" && SeriesOf(e.Version) == series) {
			return &e
		}
	}
	return nil
}

// Download 下载某个系列（空 = 最新稳定版）、某个架构的最新 nginx 到库目录，校验下载源给的校验和。
func Download(ctx context.Context, source, series, arch, dir string, progress jdk.Progress) (*Meta, error) {
	if source == "" {
		source = DefaultSource
	}
	list, err := Available(ctx, source, arch)
	if err != nil {
		return nil, err
	}
	e := Pick(list, series)
	if e == nil {
		what := "稳定版"
		if series != "" {
			what = series + " 系列"
		}
		return nil, fmt.Errorf("下载源上没有 linux/%s 的 nginx %s", arch, what)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	link := strings.TrimRight(source, "/") + "/" + e.Filename
	_, err = jdk.Fetch(ctx, link, tmp, progress)
	tmp.Close()
	if err != nil {
		return nil, err
	}
	sum, size, err := fileSums(tmp.Name())
	if err != nil {
		return nil, err
	}
	if algo, want, ok := strings.Cut(e.Checksum, ":"); ok {
		got := map[string]string{"sha1": sum.sha1, "sha256": sum.sha256}[algo]
		if got != "" && !strings.EqualFold(got, want) {
			return nil, fmt.Errorf("下载的文件校验失败（%s 不符），请重试或换一个下载源", algo)
		}
	}
	if err := checkELF(tmp.Name()); err != nil {
		return nil, err
	}
	m := newMeta(e.Version, arch)
	m.SHA256, m.Size, m.Source = sum.sha256, size, link
	if err := os.Rename(tmp.Name(), filepath.Join(dir, m.File)); err != nil {
		return nil, err
	}
	return m, saveMeta(dir, m)
}

type sums struct{ sha1, sha256 string }

func fileSums(p string) (sums, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return sums{}, 0, err
	}
	defer f.Close()
	h1, h2 := sha1.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(h1, h2), f)
	if err != nil {
		return sums{}, 0, err
	}
	return sums{hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h2.Sum(nil))}, n, nil
}

func saveMeta(dir string, m *Meta) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(filepath.Join(dir, m.ID+".json"), b, 0o644)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
