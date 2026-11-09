package dbx

// 这台电脑上按连接、库存的东西：保存的查询（<数据目录>/db/queries/<连接>/<库>/<名称>.sql）
// 和备份（<数据目录>/db/backups/<连接>/<库>/<时间>.sql，就是「导出结构和数据」的文件）。

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/captain/ferry/internal/appdir"
)

// FileItem 保存的查询或备份文件。
type FileItem struct {
	Name     string `json:"name"` // 不带 .sql
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	Preview  string `json:"preview,omitempty"` // 查询：开头的一段
}

// badName 文件名里不能有的字符（Windows 最严）
const badName = `\/:*?"<>|`

// CheckFileName 查询的名称能不能当文件名。
func CheckFileName(name string) (string, error) {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return "", errors.New("请填名称")
	case utf8.RuneCountInString(n) > 100:
		return "", errors.New("名称太长（最多 100 个字）")
	case strings.ContainsAny(n, badName):
		return "", fmt.Errorf("名称里不能有 %s 这些字符", strings.Join(strings.Split(badName, ""), " "))
	case strings.HasSuffix(n, "."), n == "." || n == "..":
		return "", errors.New("名称不能以点结尾")
	}
	for _, r := range n {
		if r < 32 {
			return "", errors.New("名称里不能有控制字符")
		}
	}
	return n, nil
}

// dirPart 连接 ID、库名当目录名：能直接用就直接用，否则换成名称的摘要
func dirPart(s string) string {
	if s == "" {
		return "_"
	}
	if n, err := CheckFileName(s); err == nil && n == s && !strings.HasPrefix(s, " ") {
		return s
	}
	return fmt.Sprintf("x%x", sha1.Sum([]byte(s)))[:17]
}

func savedDir(what, connID, db string) string {
	return filepath.Join(appdir.Dir(), "db", what, dirPart(connID), dirPart(db))
}

// QueriesDir 保存的查询放在哪里。
func QueriesDir(connID, db string) string { return savedDir("queries", connID, db) }

// BackupsDir 备份放在哪里。
func BackupsDir(connID, db string) string { return savedDir("backups", connID, db) }

func listSQL(dir string, preview bool) ([]FileItem, error) {
	out := []FileItem{}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		it := FileItem{Name: strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())), Path: filepath.Join(dir, e.Name()), Size: info.Size(),
			Modified: info.ModTime().Format("2006-01-02 15:04:05")}
		if preview {
			if b, err := readHead(it.Path, 300); err == nil {
				it.Preview = strings.Join(strings.Fields(b), " ")
			}
		}
		out = append(out, it)
	}
	return out, nil
}

func readHead(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, n)
	k, _ := f.Read(b)
	s := string(b[:k])
	for !utf8.ValidString(s) && len(s) > 0 { // 别截断半个字
		s = s[:len(s)-1]
	}
	return s, nil
}

// ListQueries 保存的查询，按名称排。
func ListQueries(connID, db string) ([]FileItem, error) {
	items, err := listSQL(QueriesDir(connID, db), true)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	return items, nil
}

func queryPath(connID, db, name string) (string, error) {
	n, err := CheckFileName(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(QueriesDir(connID, db), n+".sql"), nil
}

// ReadQuery 读一个保存的查询。
func ReadQuery(connID, db, name string) (string, error) {
	p, err := queryPath(connID, db, name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("没有叫 %s 的查询（可能已经删了）", name)
	}
	return string(b), err
}

// SaveQuery 保存查询。old 是原来的名称（改名时）；overwrite 为 false 时不覆盖别的同名查询。
func SaveQuery(connID, db, name, old, text string, overwrite bool) (string, error) {
	p, err := queryPath(connID, db, name)
	if err != nil {
		return "", err
	}
	name = strings.TrimSuffix(filepath.Base(p), ".sql")
	same := old != "" && strings.EqualFold(old, name) // 只改大小写也算同一个
	if !overwrite && !same {
		if _, err := os.Stat(p); err == nil {
			return "", fmt.Errorf("已经有叫 %s 的查询了", name)
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	op := ""
	if old != "" && old != name {
		op, _ = queryPath(connID, db, old)
	}
	if same && op != "" {
		os.Rename(op, p) // 只改了大小写：Windows 上是同一个文件，先改名让名称跟着变
		op = ""
	}
	if err := writeFileAtomic(p, []byte(text)); err != nil {
		return "", err
	}
	if op != "" {
		os.Remove(op)
	}
	return name, nil
}

// RenameQuery 改名（不改内容）。
func RenameQuery(connID, db, old, name string) (string, error) {
	text, err := ReadQuery(connID, db, old)
	if err != nil {
		return "", err
	}
	return SaveQuery(connID, db, name, old, text, false)
}

// DeleteQuery 删掉一个保存的查询。
func DeleteQuery(connID, db, name string) error {
	p, err := queryPath(connID, db, name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ListBackups 备份，新的在前。
func ListBackups(connID, db string) ([]FileItem, error) {
	items, err := listSQL(BackupsDir(connID, db), false)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name > items[j].Name })
	return items, nil
}

// NewBackupPath 新备份的文件（按时间起名）。
func NewBackupPath(connID, db string, now time.Time) (string, error) {
	dir := BackupsDir(connID, db)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := now.Format("2006-01-02_150405")
	for i := 0; ; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s_%d", base, i+1)
		}
		p := filepath.Join(dir, name+".sql")
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
	}
}

// BackupPath 一个备份文件的路径（名称来自列表，检查过不会跑到目录外面）。
func BackupPath(connID, db, name string) (string, error) {
	n, err := CheckFileName(name)
	if err != nil {
		return "", err
	}
	p := filepath.Join(BackupsDir(connID, db), n+".sql")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("没有这个备份：%s（可能已经删了）", name)
	}
	return p, nil
}

// DeleteBackup 删掉一个备份。
func DeleteBackup(connID, db, name string) error {
	p, err := BackupPath(connID, db, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}
