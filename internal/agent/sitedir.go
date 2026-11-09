package agent

// 已有站点的网站目录（static.siteDir）：服务器上原来就有的 nginx / OpenResty（容器里的也行）在用这个目录，
// Ferry 不改它的配置，每次部署、回滚只把那一版的文件换进这个目录。
//
// 换的办法：一个文件一个文件地放（先写到同一个目录里的临时文件再改名，访问不到写了一半的文件），网页（*.html）最后放，
// 新的都放好了再删掉这一版没有的。目录本身不换（容器挂载的、别的程序指着的目录都还是它），属主和原来的目录一样。
// 第一次换之前，把目录里原来的文件整个复制一份到 <应用目录>/site-before-ferry/：第一次部署失败时自动换回去，
// 以后也能在「版本」里「回到接管前」。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

const siteBackupDir = "site-before-ferry"

// siteDirRecord 记下接管的是哪个目录、原来的文件备份在哪。
type siteDirRecord struct {
	Dir    string `json:"dir"`
	Backup string `json:"backup,omitempty"` // 空 = 接管时目录是空的（或没有）
	Time   string `json:"time"`
}

// siteKeepNames 网站目录最上层这些不是站点的文件：不删（证书验证、面板的配置）。
var siteKeepNames = map[string]bool{".well-known": true, ".user.ini": true, ".htaccess": true}

func (a *Agent) siteDirRecordFile(app string) string {
	return filepath.Join(a.appMetaDir(app), "site-dir.json")
}

func (a *Agent) readSiteDirRecord(app string) (siteDirRecord, bool) {
	var r siteDirRecord
	raw, err := os.ReadFile(a.siteDirRecordFile(app))
	if err != nil || json.Unmarshal(raw, &r) != nil || r.Dir == "" {
		return r, false
	}
	return r, true
}

// checkSiteDirSafe 网站目录不能是 Ferry 自己的目录（或包含它）。
func (a *Agent) checkSiteDirSafe(s *proto.AppSpec) error {
	dir := s.SiteDir
	if err := proto.CheckSiteDir(dir); err != nil {
		return err
	}
	for _, own := range []string{a.req.Root, a.appRoot(s.Name), a.req.FerryDir} {
		if own == "" {
			continue
		}
		if inDir(own, dir) || dir == own || inDir(dir, own) {
			return fmt.Errorf("网站目录 %s 和 Ferry 的目录 %s 重叠", dir, own)
		}
	}
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("网站目录 %s 是符号链接：请填它指向的目录", dir)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s 不是目录", dir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// backupSiteDir 第一次接管时把网站目录里原来的文件复制一份。已经备份过（同一个目录）就不再备份。
func (a *Agent) backupSiteDir(s *proto.AppSpec) error {
	if r, ok := a.readSiteDirRecord(s.Name); ok && r.Dir == s.SiteDir {
		return nil
	}
	rec := siteDirRecord{Dir: s.SiteDir, Time: a.Sys.Now().Format("2006-01-02 15:04:05")}
	entries, err := os.ReadDir(s.SiteDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(entries) > 0 {
		backup := filepath.Join(a.appDir(s.Name), siteBackupDir)
		if exists(backup) {
			// 以前接管别的目录时的备份：改个名留着
			old := backup + "-" + a.Sys.Now().Format("20060102-150405")
			if err := os.Rename(backup, old); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
			return err
		}
		tmp := backup + ".tmp"
		os.RemoveAll(tmp)
		if err := copyTreeOwned(s.SiteDir, tmp); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("备份网站目录原来的文件失败：%v", err)
		}
		if err := os.Rename(tmp, backup); err != nil {
			return err
		}
		rec.Backup = backup
		a.info(proto.EvInfo, s.Name, "网站目录 %s 原来的文件备份在 %s（可以「回到接管前」）", s.SiteDir, backup)
	}
	if err := os.MkdirAll(a.appMetaDir(s.Name), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(rec, "", "  ")
	return writeFileAtomic(a.siteDirRecordFile(s.Name), raw, 0o644)
}

// publishSiteDir 把 src（运行中的版本）换进网站目录。
func (a *Agent) publishSiteDir(s *proto.AppSpec, src string) error {
	if err := a.checkSiteDirSafe(s); err != nil {
		return err
	}
	if err := a.backupSiteDir(s); err != nil {
		return err
	}
	n, err := syncSiteFiles(src, s.SiteDir, s.Keep)
	if err != nil {
		return fmt.Errorf("把文件放进网站目录 %s 失败：%v", s.SiteDir, err)
	}
	a.info(proto.EvSwitch, s.Name, "网站目录 %s：换了 %d 个文件", s.SiteDir, n)
	return nil
}

// hasSiteBackup 接管已有站点时备份过原来的文件。
func (a *Agent) hasSiteBackup(s *proto.AppSpec) bool {
	r, ok := a.readSiteDirRecord(s.Name)
	return ok && s.SiteDir != "" && r.Dir == s.SiteDir && r.Backup != "" && realDir(r.Backup)
}

// restoreSiteDir 网站目录换回接管前的文件。
func (a *Agent) restoreSiteDir(s *proto.AppSpec) error {
	r, ok := a.readSiteDirRecord(s.Name)
	if !ok || r.Dir != s.SiteDir {
		return errors.New("没有接管前的网站文件可恢复")
	}
	if err := a.checkSiteDirSafe(s); err != nil {
		return err
	}
	if r.Backup == "" {
		// 接管时是空的：清空
		_, err := syncSiteFiles(emptyDir(), s.SiteDir, s.Keep)
		return err
	}
	if _, err := syncSiteFiles(r.Backup, s.SiteDir, s.Keep); err != nil {
		return err
	}
	a.info(proto.EvRollback, s.Name, "网站目录 %s 换回了接管前的文件", s.SiteDir)
	return nil
}

func emptyDir() string {
	d, err := os.MkdirTemp("", "ferry-empty-")
	if err != nil {
		return os.TempDir() + "/ferry-empty-none"
	}
	return d
}

// syncSiteFiles 让 dst 的内容和 src 一样（keep 里的、最上层的 .well-known 等不动）。返回放了几个文件。
func syncSiteFiles(src, dst string, keep []proto.KeepItem) (int, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, err
	}
	uid, gid := -1, -1
	if fi, err := os.Stat(dst); err == nil {
		if u, g, ok := fileOwner(fi); ok && os.Geteuid() == 0 {
			uid, gid = u, g
		}
	}
	kept := func(rel string) bool {
		first := strings.SplitN(rel, "/", 2)[0]
		if siteKeepNames[first] {
			return true
		}
		for _, k := range keep {
			p := strings.Trim(filepath.ToSlash(k.Path), "/")
			if rel == p || strings.HasPrefix(rel, p+"/") {
				return true
			}
		}
		return false
	}
	type entry struct {
		rel  string
		mode fs.FileMode
	}
	var dirs, files, pages []entry
	want := map[string]bool{}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if kept(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want[rel] = true
		switch {
		case d.IsDir():
			dirs = append(dirs, entry{rel, info.Mode()})
		case strings.HasSuffix(strings.ToLower(rel), ".html") || strings.HasSuffix(strings.ToLower(rel), ".htm"):
			pages = append(pages, entry{rel, info.Mode()})
		default:
			files = append(files, entry{rel, info.Mode()})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	chown := func(p string) {
		if uid >= 0 {
			os.Lchown(p, uid, gid)
		}
	}
	for _, d := range dirs {
		p := filepath.Join(dst, filepath.FromSlash(d.rel))
		if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
			if err := os.Remove(p); err != nil {
				return 0, err
			}
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return 0, err
		}
		os.Chmod(p, 0o755)
		chown(p)
	}
	placed := 0
	put := func(e entry) error {
		from := filepath.Join(src, filepath.FromSlash(e.rel))
		to := filepath.Join(dst, filepath.FromSlash(e.rel))
		if fi, err := os.Lstat(to); err == nil && fi.IsDir() {
			if err := os.RemoveAll(to); err != nil {
				return err
			}
		}
		tmp := filepath.Join(filepath.Dir(to), ".ferry-tmp-"+filepath.Base(to))
		os.Remove(tmp)
		if e.mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(from)
			if err != nil {
				return err
			}
			if cur, err := os.Readlink(to); err == nil && cur == target {
				return nil
			}
			if err := os.Symlink(target, tmp); err != nil {
				return err
			}
		} else {
			if !e.mode.IsRegular() {
				return nil
			}
			if same, _ := sameSiteFile(from, to); same {
				return nil
			}
			if err := copyFile(from, tmp, e.mode.Perm()|0o444); err != nil {
				os.Remove(tmp)
				return err
			}
			os.Chmod(tmp, e.mode.Perm()|0o444)
		}
		chown(tmp)
		if err := os.Rename(tmp, to); err != nil {
			os.Remove(tmp)
			return err
		}
		placed++
		return nil
	}
	for _, list := range [][]entry{files, pages} {
		for _, e := range list {
			if err := put(e); err != nil {
				return placed, err
			}
		}
	}
	// 删掉这一版没有的（深的先删）
	var stale []string
	filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dst, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if kept(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !want[rel] {
			stale = append(stale, p)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(stale)))
	for _, p := range stale {
		if err := os.RemoveAll(p); err != nil {
			return placed, fmt.Errorf("删除 %s 失败：%v", p, err)
		}
	}
	return placed, nil
}

// sameSiteFile 两个普通文件内容一样（大文件分块比）。
func sameSiteFile(a, b string) (bool, error) {
	fa, err := os.Lstat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Lstat(b)
	if err != nil || !fb.Mode().IsRegular() || fa.Size() != fb.Size() {
		return false, err
	}
	x, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer x.Close()
	y, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer y.Close()
	bx, by := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		nx, ex := io.ReadFull(x, bx)
		ny, ey := io.ReadFull(y, by)
		if nx != ny || !bytes.Equal(bx[:nx], by[:ny]) {
			return false, nil
		}
		if ex == io.EOF || ex == io.ErrUnexpectedEOF {
			return ey == io.EOF || ey == io.ErrUnexpectedEOF, nil
		}
		if ex != nil {
			return false, ex
		}
	}
}

// copyTreeOwned 整个目录复制（权限、属主、符号链接照原样）。
func copyTreeOwned(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(to, info.Mode().Perm()|0o700); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, to); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyFile(p, to, info.Mode().Perm()); err != nil {
				return err
			}
			os.Chmod(to, info.Mode().Perm())
		default:
			return nil
		}
		if u, g, ok := fileOwner(info); ok && os.Geteuid() == 0 {
			os.Lchown(to, u, g)
		}
		return nil
	})
}
