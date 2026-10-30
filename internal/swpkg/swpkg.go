// Package swpkg 本机的软件安装包库：下载、导入任何安装包，按模板认出是哪个软件、哪个版本，查最新版本。
// 安装软件时 Ferry 把这里的安装包上传到服务器。
package swpkg

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/artifact"
	"github.com/captain/ferry/internal/jdk"
	"github.com/captain/ferry/internal/swtpl"
)

// Sources 下载源。
type Sources struct {
	GitHubMirror string // GitHub 加速前缀，如 https://ghfast.top/
	MySQL        string // 替换 https://cdn.mysql.com 的镜像；空 = 官方
}

// Rewrite 按下载源改写地址。
func (s Sources) Rewrite(u string) string {
	if s.GitHubMirror != "" && strings.HasPrefix(u, "https://github.com/") {
		return strings.TrimRight(s.GitHubMirror, "/") + "/" + u
	}
	if s.MySQL != "" && strings.HasPrefix(u, "https://cdn.mysql.com/") {
		return strings.TrimRight(s.MySQL, "/") + strings.TrimPrefix(u, "https://cdn.mysql.com")
	}
	return u
}

// ---------------------------------------------------------------- 库

// Meta 库里的一个安装包。
type Meta struct {
	ID       string    `json:"id"`                 // 文件名
	Template string    `json:"template,omitempty"` // 认出的模板；空 = 没认出（自定义安装用）
	Version  string    `json:"version,omitempty"`
	File     string    `json:"file"`
	SHA256   string    `json:"sha256"`
	Size     int64     `json:"size"`
	Source   string    `json:"source,omitempty"`
	Added    time.Time `json:"added"`
	FullPath string    `json:"path,omitempty"`
	// 以下在列出时按文件内容填，不存进记录
	Format string `json:"format,omitempty"` // tar.gz / tar.xz / zip / jar / binary
	Arch   string `json:"arch,omitempty"`   // binary 是 Linux 程序时：amd64 / arm64
	Desc   string `json:"desc,omitempty"`   // 给人看的：Linux 程序（arm64）、tar.gz 压缩包……
}

// describe 按文件内容补上格式、程序架构。
func (m *Meta) describe() {
	m.Format = swtpl.FormatOf("", m.File)
	switch m.Format {
	case "jar":
		m.Desc = "jar 包"
	case "binary":
		arch, err := artifact.ELFArch(m.FullPath)
		switch {
		case err == nil:
			m.Arch, m.Desc = arch, "Linux 程序（"+arch+"）"
		case strings.HasPrefix(FileHead(m.FullPath, 4), "#!"):
			m.Desc = "脚本"
		case FileHead(m.FullPath, 4) == "\x7fELF":
			m.Desc = "Linux 程序（架构不支持）"
		default:
			m.Desc = "不是 Linux 程序"
		}
	default:
		m.Desc = m.Format + " 压缩包"
	}
}

// FileHead 文件开头 n 个字节（读不了时为空）。
func FileHead(p string, n int) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, n)
	k, _ := io.ReadFull(f, b)
	return string(b[:k])
}

func metaFile(dir, file string) string { return filepath.Join(dir, ".meta", file+".json") }

// List 库里的安装包（按模板、版本从新到旧）。
func List(dir string) []Meta {
	migrate(dir)
	ents, _ := os.ReadDir(filepath.Join(dir, ".meta"))
	var out []Meta
	for _, e := range ents {
		var m Meta
		b, err := os.ReadFile(filepath.Join(dir, ".meta", e.Name()))
		if err != nil || json.Unmarshal(b, &m) != nil {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, m.File)); err != nil || fi.Size() != m.Size {
			continue
		}
		m.FullPath = filepath.Join(dir, m.File)
		m.describe()
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Template != out[j].Template {
			return out[i].Template < out[j].Template
		}
		if out[i].Version != out[j].Version {
			return Less(out[j].Version, out[i].Version)
		}
		return out[i].File < out[j].File
	})
	return out
}

// migrate 0.12.7 的记录（库目录下的 <名>.meta.json，种类叫 kind）搬到 .meta 下。
func migrate(dir string) {
	old, _ := filepath.Glob(filepath.Join(dir, "*.meta.json"))
	for _, p := range old {
		var m struct {
			Meta
			Kind string `json:"kind"`
		}
		b, err := os.ReadFile(p)
		if err == nil && json.Unmarshal(b, &m) == nil && m.File != "" {
			m.Meta.Template, m.Meta.ID = m.Kind, m.File
			if save(dir, &m.Meta) != nil {
				continue
			}
		}
		os.Remove(p)
	}
}

// Find 某个模板的安装包：version 为空取最新。
func Find(dir, template, version string) *Meta {
	for _, m := range List(dir) {
		if m.Template == template && (version == "" || m.Version == version) {
			return &m
		}
	}
	return nil
}

// FindFile 按文件名找。
func FindFile(dir, file string) *Meta {
	for _, m := range List(dir) {
		if m.File == file {
			return &m
		}
	}
	return nil
}

// Remove 从库里删除。
func Remove(dir, id string) error {
	m := FindFile(dir, id)
	if m == nil {
		return fmt.Errorf("本机没有 %s", id)
	}
	os.Remove(filepath.Join(dir, m.File))
	return os.Remove(metaFile(dir, m.File))
}

// Less 按数字逐段比较版本。
func Less(a, b string) bool {
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

// Identify 按官方文件名认出是哪个模板的安装包、哪个版本；认不出返回空。
func Identify(name string, templates []*swtpl.Template) (template, version string, pkg *swtpl.Package) {
	for _, t := range templates {
		for _, v := range t.Variants {
			if ver, ok := v.Package.MatchFile(name); ok {
				return t.ID, ver, v.Package
			}
		}
	}
	return "", "", nil
}

// Check 检查安装包：格式对不对、里面有没有 check 指定的文件（tar.xz 只看文件头）。
func Check(file, format string, strip int, check string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	base := filepath.Base(file)
	inside := func(name string) bool {
		parts := strings.Split(path.Clean(strings.TrimPrefix(name, "./")), "/")
		if len(parts) <= strip {
			return false
		}
		return strings.HasPrefix(strings.Join(parts[strip:], "/"), check)
	}
	switch format {
	case "jar", "zip":
		fi, _ := f.Stat()
		z, err := zip.NewReader(f, fi.Size())
		if err != nil {
			return fmt.Errorf("%s 不是 %s：%w", base, format, err)
		}
		if check == "" {
			return nil
		}
		for _, e := range z.File {
			if format == "jar" && strings.Contains(e.Name, check) || format == "zip" && inside(e.Name) {
				return nil
			}
		}
		return fmt.Errorf("%s 里没有 %s：不是这个软件的安装包", base, check)
	case "tar.gz":
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("%s 不是 tar.gz：%w", base, err)
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				if check == "" {
					return nil
				}
				return fmt.Errorf("%s 里没有 %s：不是这个软件的安装包", base, check)
			}
			if err != nil {
				return fmt.Errorf("%s 已损坏：%w", base, err)
			}
			if check != "" && inside(h.Name) {
				return nil
			}
		}
	case "tar.xz":
		head := make([]byte, 6)
		if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, []byte{0xFD, '7', 'z', 'X', 'Z', 0}) {
			return fmt.Errorf("%s 不是 tar.xz", base)
		}
	}
	return nil
}

// Import 把本机的安装包加入库：官方文件名的按模板认出版本并检查内容；其他的当作自定义安装包。
func Import(file, dir string, templates []*swtpl.Template) (*Meta, error) {
	name := filepath.Base(file)
	tpl, ver, pkg := Identify(name, templates)
	if pkg != nil {
		vs := swtpl.Vars{"version": ver}
		check, _ := vs.Expand(pkg.Check)
		if err := Check(file, swtpl.FormatOf(pkg.Format, name), pkg.Strip, check); err != nil {
			return nil, err
		}
	} else if err := Check(file, swtpl.FormatOf("", name), 0, ""); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, ".meta"), 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, name)
	in, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	out, err := os.Create(dst + ".part")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, h), in)
	out.Close()
	if err != nil {
		os.Remove(dst + ".part")
		return nil, err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		return nil, err
	}
	m := &Meta{ID: name, Template: tpl, Version: ver, File: name, SHA256: hex.EncodeToString(h.Sum(nil)), Size: size, Source: "导入：" + file}
	return m, save(dir, m)
}

func save(dir string, m *Meta) error {
	m.Added = time.Now()
	m.FullPath = filepath.Join(dir, m.File)
	m.describe()
	rec := *m
	rec.FullPath, rec.Format, rec.Arch, rec.Desc = "", "", "", ""
	b, _ := json.MarshalIndent(rec, "", "  ")
	os.MkdirAll(filepath.Join(dir, ".meta"), 0o755)
	return os.WriteFile(metaFile(dir, m.File), b, 0o644)
}

// ---------------------------------------------------------------- 版本与下载

var noRedirect = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Latest 安装包的最新版本。GitHub 的看 releases/latest 跳转到哪个标签（不用 API：同一出口 IP 每小时只能调 60 次）；
// probe 从 fallback 起把最后一段加 1，试地址是否存在。都没有时返回 fallback。
func Latest(ctx context.Context, src Sources, p *swtpl.Package) (string, error) {
	if p == nil {
		return "", errors.New("这种装法不用安装包")
	}
	switch {
	case p.GitHub != "":
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/"+p.GitHub+"/releases/latest", nil)
		req.Header.Set("User-Agent", "ferry")
		resp, err := noRedirect.Do(req)
		if err != nil {
			return "", fmt.Errorf("查询最新版本失败：%w", err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		i := strings.LastIndex(loc, "/tag/")
		if i < 0 {
			return "", fmt.Errorf("查询最新版本失败：HTTP %d", resp.StatusCode)
		}
		return strings.TrimPrefix(loc[i+5:], "v"), nil
	case p.Probe != "" && p.Fallback != "":
		dot := strings.LastIndexByte(p.Fallback, '.')
		n, err := strconv.Atoi(p.Fallback[dot+1:])
		if dot < 0 || err != nil {
			return p.Fallback, nil
		}
		best := ""
		for i := n; i < n+12; i++ {
			v := p.Fallback[:dot+1] + strconv.Itoa(i)
			u, _ := swtpl.Vars{"version": v}.Expand(p.Probe)
			if exists(ctx, src.Rewrite(u)) {
				best = v
			} else if best != "" {
				break
			}
		}
		if best == "" {
			return "", errors.New("查询最新版本失败：下载地址都打不开")
		}
		return best, nil
	}
	if p.Fallback != "" {
		return p.Fallback, nil
	}
	return "", errors.New("这个模板没有写怎么查最新版本：请填写版本")
}

func exists(ctx context.Context, u string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	req.Header.Set("User-Agent", "ferry")
	resp, err := noRedirect.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Download 下载某个模板的安装包（version 版本）到库里。
func Download(ctx context.Context, src Sources, template string, p *swtpl.Package, version, dir string, progress jdk.Progress) (*Meta, error) {
	if p == nil || p.URL == "" {
		return nil, swtpl.ErrNoPackage
	}
	vs := swtpl.Vars{"version": version}
	file, err := vs.Expand(p.File)
	if err != nil || file == "" {
		file = path.Base(p.URL)
		file, _ = vs.Expand(file)
	}
	var urls []string
	for _, u := range append([]string{p.URL}, p.Mirrors...) {
		x, err := vs.Expand(u)
		if err != nil {
			return nil, err
		}
		urls = append(urls, src.Rewrite(x))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	var sum, used string
	var lastErr error
	for _, u := range urls {
		tmp.Truncate(0)
		tmp.Seek(0, io.SeekStart)
		sum, lastErr = jdk.Fetch(ctx, u, tmp, progress)
		if lastErr == nil {
			used = u
			break
		}
		if errors.Is(lastErr, context.Canceled) {
			break
		}
	}
	tmp.Close()
	if lastErr != nil {
		return nil, lastErr
	}
	check, _ := vs.Expand(p.Check)
	if err := Check(tmp.Name(), swtpl.FormatOf(p.Format, file), p.Strip, check); err != nil {
		return nil, fmt.Errorf("下载的文件不对（%v）：可能是下载源返回了错误页面，换个下载源或手动下载后导入", err)
	}
	if p.MD5URL != "" {
		if mu, err := vs.Expand(p.MD5URL); err == nil {
			if b, err := jdk.Get(ctx, src.Rewrite(mu), 4096); err == nil {
				want := strings.ToLower(strings.TrimSpace(strings.Fields(string(b) + " ")[0]))
				if got := fileMD5(tmp.Name()); len(want) == 32 && got != want {
					return nil, fmt.Errorf("下载的安装包校验失败（md5 %s，应为 %s），请重试", got[:8], want[:8])
				}
			}
		}
	}
	fi, err := os.Stat(tmp.Name())
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, file)); err != nil {
		return nil, err
	}
	m := &Meta{ID: file, Template: template, Version: version, File: file, SHA256: sum, Size: fi.Size(), Source: used}
	return m, save(dir, m)
}

func fileMD5(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := md5.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}
