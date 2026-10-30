package agent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

// 读日志文件：当前运行的（shared 下）、归档还没压缩的、归档 tar.gz 里的。
// 按天切分的旧日志常被压成 .gz，读的时候直接解压；diagLogs 等可以写通配符（取最近修改的那个）。

const maxLogLines = 5000

// sortLogFiles 最近修改的在前（没有修改时间的老归档按文件名）。
func sortLogFiles(files []proto.LogFile) {
	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if !a.MTime.IsZero() && !b.MTime.IsZero() && !a.MTime.Equal(b.MTime) {
			return a.MTime.After(b.MTime)
		}
		return a.Path < b.Path
	})
}

// hasGlob 路径里有通配符。
func hasGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

// newestGlob 绝对路径通配符匹配到的、最近修改的普通文件；没有返回空。
func newestGlob(pattern string) string {
	ms, _ := filepath.Glob(pattern)
	best, bestT := "", int64(0)
	for _, m := range ms {
		fi, err := os.Stat(m)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if t := fi.ModTime().UnixNano(); best == "" || t > bestT || (t == bestT && m > best) {
			best, bestT = m, t
		}
	}
	return best
}

// logSource 某次运行的日志：当前运行（Base 为当前版本目录或 data）、版本目录里的、以前没压缩的（Base 为待压缩目录）或 tar.gz。
type logSource struct {
	Base  string // 普通目录
	TarGz string // 归档文件
	Files []proto.LogFile
}

func (a *Agent) logSource(s *proto.AppSpec, rel, run string) (*logSource, error) {
	if run == "" {
		// 当前运行：日志目录里的文件；具体指定的文件可以在 Base 下的任何地方
		cur := a.current(s.Name)
		if cur == "" {
			return nil, errors.New("还没有部署过")
		}
		base, dirs := a.currentLogs(s, cur)
		files, _ := listFiles(base, dirs)
		return &logSource{Base: base, Files: files}, nil
	}
	if !reRunID.MatchString(run) || !reID.MatchString(rel) {
		return nil, errors.New("归档名无效")
	}
	for _, r := range a.archivedRuns(s.Name, rel) {
		if r.ID != run {
			continue
		}
		if r.Dir != "" { // 放在版本目录里的
			return &logSource{Base: r.Dir, Files: r.Files}, nil
		}
		dir := a.logsDir(s.Name, rel)
		if p := filepath.Join(dir, ".pending-"+run); isDir(p) {
			return &logSource{Base: p, Files: r.Files}, nil
		}
		return &logSource{TarGz: filepath.Join(dir, run+".tar.gz"), Files: r.Files}, nil
	}
	return nil, errors.New("没有这次运行的日志归档")
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// match 要读的文件：pattern 为空 = 这次运行的全部日志文件；可以是具体路径或通配符（最近修改的在前）。
func (src *logSource) match(pattern string) []proto.LogFile {
	if pattern == "" {
		out := append([]proto.LogFile{}, src.Files...)
		sortLogFiles(out)
		return out
	}
	var out []proto.LogFile
	if src.Base != "" {
		ms, _ := filepath.Glob(filepath.Join(src.Base, filepath.FromSlash(pattern)))
		for _, m := range ms {
			fi, err := os.Stat(m)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			rel, err := filepath.Rel(src.Base, m)
			if err != nil {
				continue
			}
			out = append(out, proto.LogFile{Path: filepath.ToSlash(rel), Size: fi.Size(), MTime: fi.ModTime()})
		}
	} else {
		for _, f := range src.Files {
			if ok, _ := path.Match(pattern, f.Path); ok {
				out = append(out, f)
			}
		}
	}
	sortLogFiles(out)
	return out
}

// each 依次读这些文件，.gz 解压后交给 fn；fn 返回 errStop 时停下。tar.gz 只能按包里的顺序读。
func (src *logSource) each(files []proto.LogFile, fn func(name string, r io.Reader) error) error {
	want := map[string]bool{}
	for _, f := range files {
		want[f.Path] = true
	}
	if src.TarGz == "" {
		for _, f := range files {
			file, err := openRegular(src.Base, filepath.FromSlash(f.Path))
			if err != nil {
				return fmt.Errorf("%s：%v", f.Path, err)
			}
			err = withGz(f.Path, file, func(r io.Reader) error { return fn(f.Path, r) })
			file.Close()
			if err == errStop {
				return nil
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	f, err := os.Open(src.TarGz)
	if err != nil {
		return errors.New("没有这次运行的日志归档")
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return errors.New("日志归档已损坏：" + err.Error())
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("日志归档已损坏：" + err.Error())
		}
		if h.Typeflag != tar.TypeReg || !want[h.Name] {
			continue
		}
		err = withGz(h.Name, tr, func(r io.Reader) error { return fn(h.Name, r) })
		if err == errStop {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

var errStop = errors.New("够了")

// withGz name 以 .gz 结尾时解压。
func withGz(name string, r io.Reader, fn func(io.Reader) error) error {
	if !strings.HasSuffix(strings.ToLower(name), ".gz") {
		return fn(r)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%s 不是有效的 gzip 文件：%v", name, err)
	}
	defer gz.Close()
	return fn(gz)
}

// openRegular 打开 base 下的普通文件。目录由服务的运行用户写入，可能有符号链接：解析后必须仍在 base 里。
func openRegular(base, rel string) (*os.File, error) {
	p := filepath.Join(base, rel)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, errors.New("没有这个日志文件")
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("不是普通文件")
	}
	realBase, err1 := filepath.EvalSymlinks(base)
	realP, err2 := filepath.EvalSymlinks(p)
	if err1 != nil || err2 != nil || !within(realBase, realP) {
		return nil, errors.New("日志文件不在日志目录里")
	}
	return os.Open(p)
}

// validLogPath 请求里的日志路径：相对、不越界（可以有通配符）。
func validLogPath(file string) bool {
	return file != "" && !path.IsAbs(file) && !strings.Contains(file, "..") && path.Clean(file) == file
}

func (a *Agent) logread() int {
	if len(a.req.Apps) != 1 {
		return a.result(proto.CodeBadInput, "一次只能查看一个应用的日志", nil)
	}
	s := &a.req.Apps[0]
	file := a.req.LogFile
	if !validLogPath(file) {
		return a.result(proto.CodeBadInput, "日志文件路径无效："+file, nil)
	}
	n := a.req.Lines
	if n <= 0 {
		n = 300
	}
	n = min(n, maxLogLines)
	src, err := a.logSource(s, a.req.LogRelease, a.req.LogRun)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	text, err := src.tail(file, n, a.req.Around)
	if err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	return a.result(proto.CodeOK, "", text)
}

// tail 读文件 file（可以有通配符：最近修改的那个）的最后 n 行，或第 around 行附近。
func (src *logSource) tail(file string, n, around int) (proto.LogText, error) {
	files := src.match(file)
	if len(files) == 0 {
		if hasGlob(file) {
			return proto.LogText{}, errors.New("没有匹配 " + file + " 的日志文件")
		}
		return proto.LogText{}, errors.New("没有这个日志文件：" + file)
	}
	file = files[0].Path
	var text proto.LogText
	found := false
	err := src.each(files[:1], func(_ string, r io.Reader) error {
		found = true
		switch f, ok := r.(*os.File); {
		case around > 0:
			text = aroundLines(r, around, n/2)
		case ok: // 没压缩的文件：从末尾往前读，大文件不用从头读
			text = tailOpenFile(f, n)
		default:
			text = tailLines(r, n)
		}
		return nil
	})
	if err != nil {
		return proto.LogText{}, err
	}
	if !found {
		return proto.LogText{}, errors.New("没有这个日志文件：" + file)
	}
	text.File = file
	return text, nil
}

// tailOpenFile 普通文件的最后 n 行：从末尾一块一块往前读，数够 n 个换行就停（单行特别长时最多往前读 64MB）。
func tailOpenFile(f *os.File, n int) proto.LogText {
	st, err := f.Stat()
	if err != nil {
		return tailLines(f, n)
	}
	size := st.Size()
	return tailEnd(f, size, n)
}

func tailEnd(f io.ReaderAt, size int64, n int) proto.LogText {
	const chunk = 64 << 10
	const maxBack = 64 << 20
	off, nl := size, 0
	buf := make([]byte, chunk)
	for off > 0 && size-off < maxBack {
		step := min(int64(chunk), off)
		off -= step
		b := buf[:step]
		if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
			break
		}
		for i := len(b) - 1; i >= 0; i-- {
			if b[i] != '\n' || off+int64(i) == size-1 { // 文件末尾的换行不算分隔
				continue
			}
			if nl++; nl == n {
				start := off + int64(i) + 1
				text := tailLines(io.NewSectionReader(f, start, size-start), n)
				text.Truncated = true
				return text
			}
		}
	}
	text := tailLines(io.NewSectionReader(f, off, size-off), n)
	if off > 0 { // 往前读到上限还没数够：第一行不完整
		if !text.Truncated && len(text.Lines) > 0 {
			text.Lines = text.Lines[1:]
		}
		text.Truncated = true
	}
	return text
}

// aroundLines 第 line 行前后各 half 行（单行过长时截断）。
func aroundLines(r io.Reader, line, half int) proto.LogText {
	if half < 20 {
		half = 20
	}
	from := max(1, line-half)
	to := line + half
	var out []string
	sc := newLineScanner(r)
	for i := 1; sc.Scan(); i++ {
		if i < from {
			continue
		}
		if i > to {
			break
		}
		out = append(out, clipLine(sc.Text()))
	}
	return proto.LogText{Lines: out, Truncated: from > 1, First: from}
}

const maxLine = 16 << 10

func clipLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if len(s) > maxLine {
		return s[:maxLine] + " …（这一行太长，已截断）"
	}
	return s
}

// newLineScanner 按行读；超长的行不会让它停下（截断处理在 clipLine）。
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	return sc
}

// ---------------------------------------------------------------- 搜索

const maxGrepMatches = 1000

// loggrep 在某次运行的日志文件里找包含关键字的行（LogFile 为空 = 全部文件，可以写通配符）。
func (a *Agent) loggrep() int {
	if len(a.req.Apps) != 1 {
		return a.result(proto.CodeBadInput, "一次只能查看一个应用的日志", nil)
	}
	s := &a.req.Apps[0]
	kw := a.req.Grep
	if strings.TrimSpace(kw) == "" {
		return a.result(proto.CodeBadInput, "要写搜索的关键字", nil)
	}
	file := a.req.LogFile
	if file != "" && !validLogPath(file) {
		return a.result(proto.CodeBadInput, "日志文件路径无效："+file, nil)
	}
	limit := a.req.Lines
	if limit <= 0 || limit > maxGrepMatches {
		limit = 300
	}
	ctx := min(max(a.req.GrepContext, 0), 10)
	src, err := a.logSource(s, a.req.LogRelease, a.req.LogRun)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	files := src.match(file)
	// 每个文件的结果先分开放，最后按「最近修改的文件在前」排（tar.gz 只能按包里的顺序读）
	per := map[string][]proto.LogMatch{}
	res := proto.LogSearch{Matches: []proto.LogMatch{}}
	total := 0
	err = src.each(files, func(name string, r io.Reader) error {
		res.Files++
		ms, more := grepReader(r, kw, a.req.GrepCase, ctx, limit-total)
		for i := range ms {
			ms[i].File = name
		}
		per[name] = ms
		total += len(ms)
		if more || total >= limit {
			res.Truncated = res.Truncated || more
		}
		if total >= limit && src.TarGz == "" {
			res.Truncated = true
			return errStop
		}
		return nil
	})
	if err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	for _, f := range files {
		res.Matches = append(res.Matches, per[f.Path]...)
	}
	return a.result(proto.CodeOK, "", res)
}

// grepReader 找包含 kw 的行，最多 limit 条；more 为 true 表示后面还有。
func grepReader(r io.Reader, kw string, caseSensitive bool, ctx, limit int) (out []proto.LogMatch, more bool) {
	if limit <= 0 {
		return nil, grepAny(r, kw, caseSensitive) // 已经够了：只看还有没有
	}
	needle := kw
	if !caseSensitive {
		needle = strings.ToLower(kw)
	}
	var before []string
	var open []int // 还在收集「后面几行」的结果
	sc := newLineScanner(r)
	for i := 1; sc.Scan(); i++ {
		line := clipLine(sc.Text())
		for j := 0; j < len(open); {
			m := &out[open[j]]
			m.After = append(m.After, line)
			if len(m.After) >= ctx {
				open = append(open[:j], open[j+1:]...)
				continue
			}
			j++
		}
		hay := line
		if !caseSensitive {
			hay = strings.ToLower(line)
		}
		if strings.Contains(hay, needle) {
			if len(out) >= limit {
				return out, true
			}
			out = append(out, proto.LogMatch{Line: i, Text: line, Before: append([]string{}, before...)})
			if ctx > 0 {
				open = append(open, len(out)-1)
			}
		}
		if ctx > 0 {
			before = append(before, line)
			if len(before) > ctx {
				before = before[1:]
			}
		}
	}
	return out, false
}

func grepAny(r io.Reader, kw string, caseSensitive bool) bool {
	needle := kw
	if !caseSensitive {
		needle = strings.ToLower(kw)
	}
	sc := newLineScanner(r)
	for sc.Scan() {
		hay := sc.Text()
		if !caseSensitive {
			hay = strings.ToLower(hay)
		}
		if strings.Contains(hay, needle) {
			return true
		}
	}
	return false
}
