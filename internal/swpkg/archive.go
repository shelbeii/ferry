package swpkg

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ArchiveCandidate 压缩包内探测到的候选启动入口（可执行程序、JAR 或脚本）。
type ArchiveCandidate struct {
	Path   string `json:"path"`             // 应用 strip 后的相对路径
	Kind   string `json:"kind"`             // elf | script | jar | binary
	Arch   string `json:"arch,omitempty"`   // elf 时：amd64 | arm64 | other
	Exec   string `json:"exec"`             // 推荐的启动命令模板
	Daemon bool   `json:"daemon,omitempty"` // 脚本疑似后台启动（如 startup.sh / nohup / &）
	CRLF   bool   `json:"crlf,omitempty"`   // 脚本含 Windows CRLF 换行
}

type ArchivePreview struct {
	Files            []string           `json:"files"`
	Entries          int                `json:"entries"`
	Bundle           string             `json:"bundle,omitempty"`       // ZIP 里的 rpm 或 deb 软件包
	Strip            int                `json:"strip"`                  // 本次预览实际使用的剥离层数
	RecommendedStrip int                `json:"recommendedStrip"`       // 自动推荐的剥离层数（0 或 1）
	SingleTopDir     string             `json:"singleTopDir,omitempty"` // 若所有文件共享唯一顶层目录，记录目录名
	Candidates       []ArchiveCandidate `json:"candidates,omitempty"`   // 探测到的可执行程序 / jar / 脚本入口
	Configs          []string           `json:"configs,omitempty"`      // 探测到的配置文件路径（剥离后）
	Arches           []string           `json:"arches,omitempty"`       // 包内 ELF 可执行文件的架构集合
	HasSymlinks      bool               `json:"hasSymlinks,omitempty"`  // 是否包含符号链接
}

type rawArchiveEntry struct {
	name     string
	parts    []string
	mode     os.FileMode
	isLink   bool
	size     int64
	candKind string
	arch     string
	daemon   bool
	crlf     bool
}

// InspectArchive 查看压缩包内容并探测推荐剥离层数、入口程序、架构与配置文件。
// 当 strip < 0 时，自动使用探测出的 RecommendedStrip。
func InspectArchive(ctx context.Context, file, format string, strip int) (ArchivePreview, error) {
	report := ArchivePreview{Files: []string{}}
	var rawEntries []rawArchiveEntry
	var rpm, deb, other int
	var hasSymlinks bool

	recordMeta := func(name string) []string {
		switch strings.ToLower(path.Ext(name)) {
		case ".rpm":
			rpm++
		case ".deb":
			deb++
		case ".txt", ".sha256", ".sha256sum", ".md", ".asc", ".sig":
		default:
			if !strings.HasPrefix(strings.ToLower(path.Base(name)), "sha256sums") {
				other++
			}
		}
		clean := path.Clean(strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./"))
		if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return nil
		}
		return strings.Split(clean, "/")
	}

	switch format {
	case "zip":
		z, err := zip.OpenReader(file)
		if err != nil {
			return report, err
		}
		defer z.Close()
		peeks := 0
		for _, entry := range z.File {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if entry.FileInfo().IsDir() {
				continue
			}
			isLink := entry.Mode()&os.ModeSymlink != 0
			if isLink {
				hasSymlinks = true
			}
			parts := recordMeta(entry.Name)
			switch strings.ToLower(path.Ext(entry.Name)) {
			case ".rpm":
				if !hasPackageHeader(entry, "\xed\xab\xee\xdb") {
					other++
				}
			case ".deb":
				if !hasPackageHeader(entry, "!<arch>\n") {
					other++
				}
			}
			if len(parts) == 0 {
				continue
			}
			re := rawArchiveEntry{
				name:   strings.Join(parts, "/"),
				parts:  parts,
				mode:   entry.Mode(),
				isLink: isLink,
				size:   int64(entry.UncompressedSize64),
			}
			if !isLink && entry.Mode().IsRegular() {
				classifyFromPath(&re)
				if re.candKind == "" && shouldPeekContent(re.name, re.mode, re.size) && peeks < 64 {
					peeks++
					if rc, err := entry.Open(); err == nil {
						peekEntryContent(rc, &re)
						rc.Close()
					}
				} else if re.candKind == "script" && peeks < 64 {
					peeks++
					if rc, err := entry.Open(); err == nil {
						peekEntryContent(rc, &re)
						rc.Close()
					}
				}
			}
			rawEntries = append(rawEntries, re)
		}
		if other == 0 {
			switch {
			case rpm > 0 && deb == 0:
				report.Bundle = "rpm"
			case deb > 0 && rpm == 0:
				report.Bundle = "deb"
			}
		}

	case "tar.gz", "tar.bz2", "tar.zst", "tar":
		f, err := os.Open(file)
		if err != nil {
			return report, err
		}
		defer f.Close()
		var r io.Reader = f
		switch format {
		case "tar.gz":
			gz, err := gzip.NewReader(f)
			if err != nil {
				return report, err
			}
			defer gz.Close()
			r = gz
		case "tar.bz2":
			r = bzip2.NewReader(f)
		case "tar.zst":
			zr, err := zstd.NewReader(f)
			if err != nil {
				return report, err
			}
			defer zr.Close()
			r = zr
		}
		tr := tar.NewReader(r)
		peeks := 0
		for {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return report, err
			}
			if h.Typeflag == tar.TypeDir {
				continue
			}
			isLink := h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink
			if isLink {
				hasSymlinks = true
			}
			parts := recordMeta(h.Name)
			if len(parts) == 0 {
				continue
			}
			re := rawArchiveEntry{
				name:   strings.Join(parts, "/"),
				parts:  parts,
				mode:   os.FileMode(h.Mode),
				isLink: isLink,
				size:   h.Size,
			}
			if h.Typeflag == tar.TypeReg {
				classifyFromPath(&re)
				if (re.candKind == "" && shouldPeekContent(re.name, re.mode, re.size) || re.candKind == "script") && peeks < 96 {
					peeks++
					peekEntryContent(tr, &re)
				}
			}
			rawEntries = append(rawEntries, re)
		}

	case "tar.xz":
		// 使用系统 tar -tJf 列出 .tar.xz 目录结构（Windows 10+ / Linux / macOS 均内置 bsdtar/GNU tar）。
		// 若当前环境没有可用 tar，则优雅返回空预览而不报错，不阻断安装。
		if lines, err := listTarXZ(ctx, file); err == nil {
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasSuffix(line, "/") {
					continue
				}
				parts := recordMeta(line)
				if len(parts) == 0 {
					continue
				}
				re := rawArchiveEntry{
					name:  strings.Join(parts, "/"),
					parts: parts,
				}
				classifyFromPath(&re)
				if re.candKind == "" && looksLikeBinPath(re.name) {
					re.candKind = "binary"
				}
				rawEntries = append(rawEntries, re)
			}
		}
	}

	report.HasSymlinks = hasSymlinks
	report.RecommendedStrip, report.SingleTopDir = recommendStrip(rawEntries)

	useStrip := strip
	if useStrip < 0 {
		useStrip = report.RecommendedStrip
	}
	report.Strip = useStrip

	archSeen := map[string]bool{}
	var candidates []ArchiveCandidate
	var configs []string

	for _, re := range rawEntries {
		if len(re.parts) <= useStrip {
			continue
		}
		rel := strings.Join(re.parts[useStrip:], "/")
		report.Entries++
		if len(report.Files) < 48 {
			report.Files = append(report.Files, rel)
		}
		if re.candKind != "" && report.Bundle == "" {
			if re.arch != "" && !archSeen[re.arch] {
				archSeen[re.arch] = true
				report.Arches = append(report.Arches, re.arch)
			}
			candidates = append(candidates, ArchiveCandidate{
				Path:   rel,
				Kind:   re.candKind,
				Arch:   re.arch,
				Exec:   suggestedExec(rel, re.candKind),
				Daemon: re.daemon,
				CRLF:   re.crlf,
			})
		} else if !re.isLink && isConfigFile(rel) && len(configs) < 16 {
			configs = append(configs, rel)
		}
	}

	sort.Strings(report.Arches)
	sortCandidates(candidates, filepath.Base(file))
	if len(candidates) > 12 {
		candidates = candidates[:12]
	}
	report.Candidates = candidates
	report.Configs = configs
	return report, nil
}

// ArchiveHasPath 检查压缩包在应用 strip 后是否包含指定相对路径。
// 对无法列出内容的 tar.xz 环境返回 (true, nil) 以避免误报。
func ArchiveHasPath(ctx context.Context, file, format string, strip int, relPath string) (bool, error) {
	relPath = path.Clean(strings.TrimPrefix(strings.ReplaceAll(relPath, "\\", "/"), "./"))
	if relPath == "" || relPath == "." {
		return false, nil
	}
	match := func(name string) bool {
		clean := path.Clean(strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./"))
		parts := strings.Split(clean, "/")
		if len(parts) <= strip {
			return false
		}
		return strings.Join(parts[strip:], "/") == relPath
	}
	switch format {
	case "zip":
		z, err := zip.OpenReader(file)
		if err != nil {
			return false, err
		}
		defer z.Close()
		for _, e := range z.File {
			if !e.FileInfo().IsDir() && match(e.Name) {
				return true, nil
			}
		}
		return false, nil
	case "tar.gz", "tar.bz2", "tar.zst", "tar":
		f, err := os.Open(file)
		if err != nil {
			return false, err
		}
		defer f.Close()
		var r io.Reader = f
		switch format {
		case "tar.gz":
			gz, err := gzip.NewReader(f)
			if err != nil {
				return false, err
			}
			defer gz.Close()
			r = gz
		case "tar.bz2":
			r = bzip2.NewReader(f)
		case "tar.zst":
			zr, err := zstd.NewReader(f)
			if err != nil {
				return false, err
			}
			defer zr.Close()
			r = zr
		}
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if h.Typeflag != tar.TypeDir && match(h.Name) {
				return true, nil
			}
		}
	case "tar.xz":
		lines, err := listTarXZ(ctx, file)
		if err != nil {
			return true, nil
		}
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l != "" && !strings.HasSuffix(l, "/") && match(l) {
				return true, nil
			}
		}
		return false, nil
	}
	return true, nil
}

func listTarXZ(ctx context.Context, file string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "tar", "-tJf", file)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n"), nil
}

func recommendStrip(entries []rawArchiveEntry) (int, string) {
	if len(entries) == 0 {
		return 0, ""
	}
	top := ""
	for _, e := range entries {
		if len(e.parts) < 2 {
			// 有位于根目录的文件，不能剥离顶层目录
			return 0, ""
		}
		if e.parts[0] == "__MACOSX" {
			continue
		}
		if top == "" {
			top = e.parts[0]
		} else if e.parts[0] != top {
			return 0, ""
		}
	}
	if top == "" {
		return 0, ""
	}
	return 1, top
}

func classifyFromPath(re *rawArchiveEntry) {
	lower := strings.ToLower(re.name)
	base := path.Base(lower)
	ext := path.Ext(base)
	switch ext {
	case ".jar", ".war":
		if strings.HasSuffix(base, "-sources.jar") || strings.HasSuffix(base, "-javadoc.jar") {
			return
		}
		// 跳过位于深层依赖目录（如 lib/xxx.jar、plugins/xxx.jar）的普通库包，除非在根/一层目录或带 boot/server/app 关键词
		if len(re.parts) <= 3 || strings.Contains(base, "server") || strings.Contains(base, "app") || strings.Contains(base, "boot") {
			re.candKind = "jar"
		}
	case ".sh":
		re.candKind = "script"
		if looksLikeDaemonScriptName(base) {
			re.daemon = true
		}
	}
}

func shouldPeekContent(rel string, mode os.FileMode, size int64) bool {
	if size <= 4 {
		return false
	}
	lower := strings.ToLower(rel)
	base := path.Base(lower)
	if strings.HasPrefix(base, ".") || isSharedLib(base) {
		return false
	}
	// 只检查顶层、次层或 bin/sbin 目录中的候选文件，避免扫描海量资源/文档文件
	ext := path.Ext(base)
	switch ext {
	case ".txt", ".md", ".rst", ".html", ".htm", ".css", ".js", ".mjs", ".cjs", ".ts", ".map",
		".json", ".yaml", ".yml", ".toml", ".ini", ".conf", ".cfg", ".xml", ".properties", ".env",
		".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".woff", ".woff2", ".ttf", ".eot",
		".pdf", ".doc", ".docx", ".csv", ".sql", ".log", ".lock", ".sum", ".mod",
		".c", ".h", ".cc", ".cpp", ".go", ".rs", ".py", ".rb", ".php", ".java", ".class",
		".o", ".a", ".lib", ".dll", ".exe", ".bat", ".cmd", ".ps1", ".dylib",
		".rpm", ".deb", ".zip", ".gz", ".xz", ".bz2", ".zst", ".tar", ".pem", ".crt", ".key":
		return false
	}
	if mode.Perm()&0o111 != 0 {
		return true
	}
	return looksLikeBinPath(rel)
}

func looksLikeBinPath(rel string) bool {
	lower := strings.ToLower(rel)
	base := path.Base(lower)
	if isSharedLib(base) || strings.HasPrefix(base, "license") || strings.HasPrefix(base, "readme") || strings.HasPrefix(base, "notice") || strings.HasPrefix(base, "changelog") || strings.HasPrefix(base, "authors") || strings.HasPrefix(base, "copying") || base == "makefile" || base == "dockerfile" {
		return false
	}
	ext := path.Ext(base)
	if ext != "" && ext != ".bin" && ext != ".run" {
		return false
	}
	parts := strings.Split(lower, "/")
	if len(parts) <= 2 {
		return true
	}
	dir := parts[len(parts)-2]
	return dir == "bin" || dir == "sbin" || dir == "libexec"
}

func isSharedLib(base string) bool {
	return strings.HasSuffix(base, ".so") || strings.Contains(base, ".so.")
}

func peekEntryContent(r io.Reader, re *rawArchiveEntry) {
	buf := make([]byte, 2048)
	n, _ := io.ReadFull(r, buf)
	if n == 0 {
		return
	}
	data := buf[:n]
	if n >= 20 && bytes.Equal(data[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		if arch, ok := parseELFHeader(data); ok {
			re.candKind = "elf"
			re.arch = arch
		}
		return
	}
	if bytes.HasPrefix(data, []byte("#!")) {
		re.candKind = "script"
		firstLine, _, _ := bytes.Cut(data, []byte("\n"))
		if bytes.Contains(firstLine, []byte("\r")) {
			re.crlf = true
		}
		if looksLikeDaemonScriptName(strings.ToLower(path.Base(re.name))) || looksLikeDaemonScriptBody(string(data)) {
			re.daemon = true
		}
	}
}

// parseELFHeader 从 ELF 文件头（至少 20 字节）解析可执行架构。
func parseELFHeader(h []byte) (string, bool) {
	if len(h) < 20 || !bytes.Equal(h[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return "", false
	}
	var bo binary.ByteOrder
	switch h[5] {
	case 1:
		bo = binary.LittleEndian
	case 2:
		bo = binary.BigEndian
	default:
		return "", false
	}
	eType := bo.Uint16(h[16:18])
	// ET_EXEC (2) 或 ET_DYN (3, PIE 可执行文件)；跳过 ET_REL (1)
	if eType != 2 && eType != 3 {
		return "", false
	}
	eMachine := bo.Uint16(h[18:20])
	switch eMachine {
	case 62, 3: // EM_X86_64, EM_386
		return "amd64", true
	case 183, 40: // EM_AARCH64, EM_ARM
		return "arm64", true
	default:
		return "other", true
	}
}

func looksLikeDaemonScriptName(base string) bool {
	return strings.Contains(base, "startup") || strings.Contains(base, "daemon")
}

func looksLikeDaemonScriptBody(body string) bool {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "nohup ") {
		return true
	}
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasSuffix(t, "&") && !strings.HasSuffix(t, "&&") && !strings.HasSuffix(t, ">&1") && !strings.HasSuffix(t, ">&2") {
			return true
		}
	}
	return false
}

func suggestedExec(rel, kind string) string {
	switch kind {
	case "jar":
		return "${java} -jar ${dir}/" + rel + " --server.port=${port}"
	case "script":
		return "${dir}/" + rel
	default:
		return "${dir}/" + rel
	}
}

func isConfigFile(rel string) bool {
	lower := strings.ToLower(rel)
	base := path.Base(lower)
	if base == "package.json" || base == "package-lock.json" || base == "tsconfig.json" || base == "composer.json" {
		return false
	}
	switch path.Ext(base) {
	case ".yaml", ".yml", ".conf", ".cfg", ".ini", ".toml", ".properties":
		return true
	case ".json":
		return strings.Contains(base, "config") || strings.Contains(base, "setting") || strings.Contains(lower, "conf/") || strings.Contains(lower, "config/") || strings.Contains(lower, "etc/")
	}
	return false
}

func sortCandidates(cands []ArchiveCandidate, archiveFile string) {
	hint := strings.ToLower(archiveFile)
	score := func(c ArchiveCandidate) int {
		s := 0
		switch c.Kind {
		case "elf":
			s += 100
		case "binary":
			s += 80
		case "jar":
			s += 70
		case "script":
			s += 50
		}
		lower := strings.ToLower(c.Path)
		base := path.Base(lower)
		depth := strings.Count(lower, "/")
		s -= depth * 12
		if strings.HasPrefix(lower, "bin/") || depth == 0 {
			s += 20
		}
		// 名字和压缩包名相似（如 caddy / frps / prometheus）优先
		stem := strings.TrimSuffix(base, path.Ext(base))
		if len(stem) >= 3 && strings.Contains(hint, stem) {
			s += 30
		}
		if strings.Contains(base, "server") || strings.Contains(base, "start") || strings.Contains(base, "run") {
			s += 10
		}
		if strings.Contains(base, "stop") || strings.Contains(base, "shutdown") || strings.Contains(base, "uninstall") || strings.Contains(base, "test") || strings.Contains(base, "cli") || strings.Contains(base, "tool") {
			s -= 35
		}
		if c.Daemon {
			s -= 15
		}
		return s
	}
	sort.SliceStable(cands, func(i, j int) bool {
		si, sj := score(cands[i]), score(cands[j])
		if si != sj {
			return si > sj
		}
		return cands[i].Path < cands[j].Path
	})
}

func hasPackageHeader(entry *zip.File, magic string) bool {
	if !entry.Mode().IsRegular() {
		return false
	}
	r, err := entry.Open()
	if err != nil {
		return false
	}
	defer r.Close()
	head := make([]byte, len(magic))
	_, err = io.ReadFull(r, head)
	return err == nil && string(head) == magic
}

// CheckSystemBundle 检查「离线系统包」入口接收的 ZIP；普通导入仍可保存源码或其他归档。
func CheckSystemBundle(ctx context.Context, file, kind string) error {
	var needed string
	switch kind {
	case "rpm", "deb":
		needed = strings.ToUpper(kind)
	case "auto":
		needed = "RPM 或 DEB"
	default:
		return fmt.Errorf("离线包类型只能是 rpm、deb 或 auto")
	}
	name := filepath.Base(file)
	if !strings.EqualFold(filepath.Ext(file), ".zip") {
		return fmt.Errorf("%s 无法用于离线系统安装：请选择装有目标服务器所需 %s 软件包及依赖的 ZIP。源码需要先编译", name, needed)
	}
	preview, err := InspectArchive(ctx, file, "zip", 0)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%w", name, err)
	}
	if preview.Bundle == "" || (kind != "auto" && preview.Bundle != kind) {
		return fmt.Errorf("%s 需要包含有效的 %s 软件包才能在此处使用", name, needed)
	}
	return nil
}
