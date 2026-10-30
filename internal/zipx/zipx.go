// Package zipx：Spring Boot fat jar 的增量上传。
//
// fat jar 里 BOOT-INF/lib/*.jar 是未压缩（STORED）存放的，它们在 fat jar 中的数据字节就是依赖 jar 本身。
// 把 fat jar 拆成「依赖（按 sha256 复用服务器缓存）」和「其余原样字节」，服务器按计划拼回，
// 得到与本地逐字节一致的文件（最后用整个文件的 sha256 核对）。
//
// 计划文件每行（制表符分隔）：
//
//	RAW <在 pack 中的偏移> <长度>
//	LIB <sha256> <长度>
package zipx

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Entry ZIP 中的一个条目。
type Entry struct {
	Name      string
	Method    uint16
	CompSize  int64
	Offset    int64 // 本地文件头偏移
	DataStart int64 // 数据起始偏移
}

// Layout ZIP 文件布局（条目按文件内偏移排序）。
type Layout struct {
	Size    int64
	Entries []Entry
}

// ErrZip64 不支持 ZIP64（fat jar 远小于 4GB，遇到时退回全量上传）。
var ErrZip64 = errors.New("不支持 ZIP64")

// ReadLayout 读取中央目录与各本地文件头。
func ReadLayout(r io.ReaderAt, size int64) (*Layout, error) {
	tailLen := int64(65557)
	if size < tailLen {
		tailLen = size
	}
	if tailLen < 22 {
		return nil, errors.New("不是有效的 ZIP：文件太小")
	}
	tail := make([]byte, tailLen)
	if _, err := r.ReadAt(tail, size-tailLen); err != nil && err != io.EOF {
		return nil, err
	}
	eocd := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == 0x06054b50 {
			eocd = i
			break
		}
	}
	if eocd < 0 {
		return nil, errors.New("不是有效的 ZIP：找不到中央目录结尾")
	}
	total := binary.LittleEndian.Uint16(tail[eocd+10:])
	cdSize := int64(binary.LittleEndian.Uint32(tail[eocd+12:]))
	cdOffset := int64(binary.LittleEndian.Uint32(tail[eocd+16:]))
	if total == 0xffff || cdOffset == 0xffffffff || cdSize == 0xffffffff {
		return nil, ErrZip64
	}
	if cdOffset+cdSize > size {
		return nil, errors.New("中央目录越界")
	}
	cd := make([]byte, cdSize)
	if _, err := r.ReadAt(cd, cdOffset); err != nil && err != io.EOF {
		return nil, err
	}
	entries := make([]Entry, 0, total)
	p := 0
	for n := 0; n < int(total); n++ {
		if p+46 > len(cd) || binary.LittleEndian.Uint32(cd[p:]) != 0x02014b50 {
			return nil, errors.New("中央目录损坏")
		}
		method := binary.LittleEndian.Uint16(cd[p+10:])
		comp := binary.LittleEndian.Uint32(cd[p+20:])
		nameLen := int(binary.LittleEndian.Uint16(cd[p+28:]))
		extraLen := int(binary.LittleEndian.Uint16(cd[p+30:]))
		commentLen := int(binary.LittleEndian.Uint16(cd[p+32:]))
		off := binary.LittleEndian.Uint32(cd[p+42:])
		if comp == 0xffffffff || off == 0xffffffff {
			return nil, ErrZip64
		}
		if p+46+nameLen > len(cd) {
			return nil, errors.New("中央目录损坏")
		}
		entries = append(entries, Entry{
			Name: string(cd[p+46 : p+46+nameLen]), Method: method, CompSize: int64(comp), Offset: int64(off),
		})
		p += 46 + nameLen + extraLen + commentLen
	}
	lh := make([]byte, 30)
	for i := range entries {
		e := &entries[i]
		if _, err := r.ReadAt(lh, e.Offset); err != nil {
			return nil, fmt.Errorf("读取本地文件头 %s：%w", e.Name, err)
		}
		if binary.LittleEndian.Uint32(lh) != 0x04034b50 {
			return nil, fmt.Errorf("本地文件头损坏：%s", e.Name)
		}
		e.DataStart = e.Offset + 30 + int64(binary.LittleEndian.Uint16(lh[26:])) + int64(binary.LittleEndian.Uint16(lh[28:]))
		if e.DataStart+e.CompSize > size {
			return nil, fmt.Errorf("条目越界：%s", e.Name)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Offset < entries[j].Offset })
	return &Layout{Size: size, Entries: entries}, nil
}

// ReadLayoutFile 读取文件的布局。
func ReadLayoutFile(path string) (*Layout, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return ReadLayout(f, st.Size())
}

// IsStoredLib 是否是可复用的依赖：BOOT-INF/lib 下未压缩存放的 .jar。
func IsStoredLib(e Entry) bool {
	if e.Method != 0 || e.CompSize <= 0 {
		return false
	}
	if !strings.HasPrefix(e.Name, "BOOT-INF/lib/") || !strings.HasSuffix(e.Name, ".jar") {
		return false
	}
	return !strings.Contains(strings.TrimPrefix(e.Name, "BOOT-INF/lib/"), "/")
}

// Stats 增量拆分统计。
type Stats struct {
	RawBytes    int64
	ReusedBytes int64
	NewLibBytes int64
	Libs        int
	NewLibs     int
}

// UploadBytes 实际需要上传的字节数。
func (s Stats) UploadBytes() int64 { return s.RawBytes + s.NewLibBytes }

// Split 生成拼装计划与 pack；have 里没有的依赖写到 libOut/<sha256>.jar 并加入 have（多个应用共用依赖只传一次）。
func Split(jarPath, planPath, packPath, libOut string, have map[string]bool) (Stats, error) {
	var st Stats
	layout, err := ReadLayoutFile(jarPath)
	if err != nil {
		return st, err
	}
	src, err := os.Open(jarPath)
	if err != nil {
		return st, err
	}
	defer src.Close()
	pack, err := os.Create(packPath)
	if err != nil {
		return st, err
	}
	defer pack.Close()
	pw := bufio.NewWriterSize(pack, 1<<20)
	var plan strings.Builder
	var packPos int64

	addRaw := func(from, to int64) error {
		if to <= from {
			return nil
		}
		n := to - from
		fmt.Fprintf(&plan, "RAW\t%d\t%d\n", packPos, n)
		if _, err := io.Copy(pw, io.NewSectionReader(src, from, n)); err != nil {
			return err
		}
		packPos += n
		st.RawBytes += n
		return nil
	}

	var cursor int64
	for _, e := range layout.Entries {
		if !IsStoredLib(e) {
			continue
		}
		if e.DataStart < cursor {
			return st, fmt.Errorf("条目重叠：%s", e.Name)
		}
		if err := addRaw(cursor, e.DataStart); err != nil {
			return st, err
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(src, e.DataStart, e.CompSize)); err != nil {
			return st, err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		fmt.Fprintf(&plan, "LIB\t%s\t%d\n", sum, e.CompSize)
		st.Libs++
		if have[sum] {
			st.ReusedBytes += e.CompSize
		} else {
			if err := copyRange(src, e.DataStart, e.CompSize, filepath.Join(libOut, sum+".jar")); err != nil {
				return st, err
			}
			have[sum] = true
			st.NewLibs++
			st.NewLibBytes += e.CompSize
		}
		cursor = e.DataStart + e.CompSize
	}
	if err := addRaw(cursor, layout.Size); err != nil {
		return st, err
	}
	if err := pw.Flush(); err != nil {
		return st, err
	}
	if err := pack.Close(); err != nil {
		return st, err
	}
	return st, os.WriteFile(planPath, []byte(plan.String()), 0o644)
}

func copyRange(src io.ReaderAt, off, n int64, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.NewSectionReader(src, off, n)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Step 拼装计划中的一步。
type Step struct {
	Raw    bool
	Offset int64  // RAW：pack 中的偏移
	SHA256 string // LIB
	Length int64
}

// ParsePlan 读取计划文件。
func ParsePlan(r io.Reader) ([]Step, error) {
	var steps []Step
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		if text == "" {
			continue
		}
		f := strings.Split(text, "\t")
		if len(f) != 3 {
			return nil, fmt.Errorf("计划第 %d 行格式错误", line)
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("计划第 %d 行长度错误", line)
		}
		switch f[0] {
		case "RAW":
			off, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || off < 0 {
				return nil, fmt.Errorf("计划第 %d 行偏移错误", line)
			}
			steps = append(steps, Step{Raw: true, Offset: off, Length: n})
		case "LIB":
			if !IsSHA256(f[1]) {
				return nil, fmt.Errorf("计划第 %d 行 sha256 错误", line)
			}
			steps = append(steps, Step{SHA256: f[1], Length: n})
		default:
			return nil, fmt.Errorf("计划第 %d 行未知类型 %q", line, f[0])
		}
	}
	return steps, sc.Err()
}

// Assemble 按计划用 pack 与依赖缓存拼出文件。libPath 返回缓存中某个依赖的路径。
func Assemble(planPath, packPath, outPath string, libPath func(sha string) string) error {
	pf, err := os.Open(planPath)
	if err != nil {
		return err
	}
	steps, err := ParsePlan(pf)
	pf.Close()
	if err != nil {
		return err
	}
	pack, err := os.Open(packPath)
	if err != nil {
		return err
	}
	defer pack.Close()
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 1<<20)
	fail := func(err error) error {
		out.Close()
		os.Remove(outPath)
		return err
	}
	for _, s := range steps {
		if s.Raw {
			n, err := io.Copy(w, io.NewSectionReader(pack, s.Offset, s.Length))
			if err != nil {
				return fail(err)
			}
			if n != s.Length {
				return fail(fmt.Errorf("pack 长度不足（偏移 %d）", s.Offset))
			}
			continue
		}
		lf, err := os.Open(libPath(s.SHA256))
		if err != nil {
			return fail(fmt.Errorf("缓存中缺少依赖 %s：%w", s.SHA256[:12], err))
		}
		n, err := io.Copy(w, lf)
		lf.Close()
		if err != nil {
			return fail(err)
		}
		if n != s.Length {
			return fail(fmt.Errorf("缓存依赖 %s 长度不符（%d ≠ %d）", s.SHA256[:12], n, s.Length))
		}
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	return out.Close()
}

// IsSHA256 是否是 64 位小写十六进制。
func IsSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// FileSHA256 计算文件 sha256 与大小。
func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ExtractLibs 从 fat jar 抽取未压缩依赖到缓存目录（已存在的跳过），返回新增数量。
// 文件先写临时文件，校验 sha256 后改名，避免半截文件进入缓存。
func ExtractLibs(jarPath, cacheDir string) (int, error) {
	layout, err := ReadLayoutFile(jarPath)
	if err != nil {
		return 0, err
	}
	src, err := os.Open(jarPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	added := 0
	for _, e := range layout.Entries {
		if !IsStoredLib(e) {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(src, e.DataStart, e.CompSize)); err != nil {
			return added, err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		dst := filepath.Join(cacheDir, sum+".jar")
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		tmp := dst + ".tmp"
		if err := copyRange(src, e.DataStart, e.CompSize, tmp); err != nil {
			os.Remove(tmp)
			return added, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return added, err
		}
		added++
	}
	return added, nil
}
