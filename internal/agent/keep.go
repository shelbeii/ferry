package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// 部署时保留服务器上的（artifact.keep）：新版本放好之后、切换之前，把运行中的版本里的这些带进新版本。
//   - 不共用的：复制过去（属主、权限照旧），跟着版本走
//   - 共用的：第一次放进 data/<路径>（运行中的版本里有就复制它，没有就用包里的，都没有就建空目录），
//     以后每个版本里的 <路径> 都是指向它的链接
//
// 运行中的版本里没有的，用包里的（第一次部署）。

const keepListMax = 500 // keepscan 列目录时最多列几个

// keepSource 运行中的版本里放站点文件 / 程序的目录与版本 ID；没有运行中的版本时为空。
func (a *Agent) keepSource(s *proto.AppSpec) (string, string) {
	cur := a.current(s.Name)
	if cur == "" {
		return "", ""
	}
	if s.Type == proto.TypeStatic {
		return a.siteDir(s.Name, cur), cur
	}
	return a.releaseDir(s.Name, cur), cur
}

// noLinkUnder base 下的 rel（用 /）一路上的目录都是真的目录，不是符号链接（rel 本身可以是）；rel 不越出 base。
func noLinkUnder(base, rel string) bool {
	if rel == "" || rel == "." {
		return true
	}
	c := path.Clean(rel)
	if c != rel || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return false
	}
	segs := strings.Split(c, "/")
	p := base
	for _, s := range segs[:len(segs)-1] {
		p = filepath.Join(p, s)
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return true // 还没有（要新建的）
		}
		if err != nil || !fi.IsDir() { // 链接的 Lstat 不是目录
			return false
		}
	}
	return true
}

// lexists 有这个文件、目录或链接（不跟链接）。
func lexists(p string) bool { _, err := os.Lstat(p); return err == nil }

// keepMatches 运行中的版本 src 里匹配 k 的（相对路径，用 /，排好序）。
func keepMatches(src string, k proto.KeepItem) []string {
	if src == "" {
		return nil
	}
	if !strings.ContainsAny(k.Path, "*?[") {
		if noLinkUnder(src, k.Path) && lexists(filepath.Join(src, filepath.FromSlash(k.Path))) {
			return []string{k.Path}
		}
		return nil
	}
	ms, _ := filepath.Glob(filepath.Join(src, filepath.FromSlash(k.Path)))
	var out []string
	for _, m := range ms {
		rel, err := filepath.Rel(src, m)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if noLinkUnder(src, rel) {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// countFiles 目录里的普通文件个数（不跟链接）；是文件时为 1。
func countFiles(p string) int {
	n := 0
	filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

// keepFiles 把要保留的带进新版本：dst 是新版本里站点文件 / 程序所在的目录（包里的已经放好）。返回保留了的（记进版本记录）。
func (a *Agent) keepFiles(s *proto.AppSpec, dst string) ([]string, error) {
	if len(s.Keep) == 0 {
		return nil, nil
	}
	src, cur := a.keepSource(s)
	uid, gid := 0, 0
	if isService(s) {
		u, g, err := a.Sys.LookupUser(s.User)
		if err != nil {
			return nil, err
		}
		uid, gid = u, g
	}
	var kept []string
	for _, k := range s.Keep {
		if err := proto.CheckKeep(k, s.FileName, s.Type == proto.TypeStatic); err != nil {
			return kept, err
		}
		if k.Shared {
			if err := a.keepShared(s, k, src, dst, uid, gid); err != nil {
				return kept, fmt.Errorf("共用 %s 失败：%v", k.Path, err)
			}
			kept = append(kept, k.Path+"（共用）")
			continue
		}
		ms := keepMatches(src, k)
		if len(ms) == 0 {
			switch {
			case cur == "":
				// 第一次部署：用包里的，不用说
			case strings.ContainsAny(k.Path, "*?["):
				a.info(proto.EvKeep, s.Name, "%s：运行中的版本里没有匹配的，用包里的", k.Path)
			case lexists(filepath.Join(dst, filepath.FromSlash(k.Path))):
				a.info(proto.EvKeep, s.Name, "%s：运行中的版本里没有，用包里的", k.Path)
			default:
				a.info(proto.EvKeep, s.Name, "%s：运行中的版本里和包里都没有", k.Path)
			}
			continue
		}
		for _, rel := range ms {
			note, err := keepCopy(filepath.Join(src, filepath.FromSlash(rel)), dst, rel)
			if err != nil {
				return kept, fmt.Errorf("保留 %s 失败：%v", rel, err)
			}
			a.info(proto.EvKeep, s.Name, "%s：用服务器上的（%s）", rel, note)
			kept = append(kept, rel)
		}
	}
	return kept, nil
}

// keepCopy 用运行中的版本里的 from 换掉新版本 dst 里的 rel。返回给人看的说明（和包里的比）。
func keepCopy(from, dst, rel string) (string, error) {
	if !noLinkUnder(dst, rel) {
		return "", fmt.Errorf("新版本里 %s 的上层不是目录", rel)
	}
	to := filepath.Join(dst, filepath.FromSlash(rel))
	fi, err := os.Lstat(from)
	if err != nil {
		return "", err
	}
	note := ""
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		note = "链接"
	case fi.IsDir():
		note = fmt.Sprintf("目录，%d 个文件", countFiles(from))
	case fi.Mode().IsRegular():
		pkg, perr := os.Lstat(to)
		switch {
		case perr != nil:
			note = "包里没有"
		case pkg.Mode().IsRegular():
			a, _, _ := zipx.FileSHA256(from)
			b, _, _ := zipx.FileSHA256(to)
			if a == b {
				note = "和包里的一样"
			} else {
				note = "和包里的不同"
			}
		default:
			note = "包里的不是文件"
		}
	default:
		return "", fmt.Errorf("不是普通文件或目录")
	}
	if err := os.RemoveAll(to); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	if err := copyTree(from, to); err != nil {
		os.RemoveAll(to)
		return "", err
	}
	return note, nil
}

// keepShared 共用的 k：data/<路径> 没有就先放好（运行中的版本里的 → 包里的 → 空目录），新版本里的换成指向它的链接。
func (a *Agent) keepShared(s *proto.AppSpec, k proto.KeepItem, src, dst string, uid, gid int) error {
	root := a.sharedDir(s.Name)
	if !lexists(root) {
		mode := os.FileMode(0o755) // 不是服务的静态站点：服务器上的 nginx（别的用户）要能进
		if isService(s) {
			mode = 0o750
		}
		if err := os.MkdirAll(root, mode); err != nil {
			return err
		}
		if isService(s) {
			a.Sys.Lchown(root, uid, gid)
		}
	}
	if !noLinkUnder(root, k.Path) || !noLinkUnder(dst, k.Path) {
		return fmt.Errorf("上层目录不能是链接")
	}
	data := filepath.Join(root, filepath.FromSlash(k.Path))
	pkg := filepath.Join(dst, filepath.FromSlash(k.Path))
	rel := strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(data, a.appDir(s.Name))), "/")
	if !lexists(data) {
		if err := a.mkdirsOwned(root, path.Dir(k.Path), uid, gid, isService(s)); err != nil {
			return err
		}
		from := ""
		if src != "" && noLinkUnder(src, k.Path) {
			from = filepath.Join(src, filepath.FromSlash(k.Path))
		}
		fi, ferr := os.Lstat(from)
		switch {
		case from != "" && ferr == nil && fi.Mode()&os.ModeSymlink == 0 && (fi.IsDir() || fi.Mode().IsRegular()):
			// 第一次共用：复制运行中的版本里的（它自己的留着，回滚到它时还能用）
			if err := copyTree(from, data); err != nil {
				os.RemoveAll(data)
				return err
			}
			a.info(proto.EvKeep, s.Name, "%s：放进共用目录 %s（复制运行中的版本里的 %d 个文件）", k.Path, rel, countFiles(data))
		case lexists(pkg):
			if err := moveTree(pkg, data); err != nil {
				return err
			}
			a.info(proto.EvKeep, s.Name, "%s：放进共用目录 %s（用包里的 %d 个文件）", k.Path, rel, countFiles(data))
		default:
			if err := os.Mkdir(data, 0o755); err != nil {
				return err
			}
			a.info(proto.EvKeep, s.Name, "%s：建了空的共用目录 %s", k.Path, rel)
		}
		if isService(s) {
			// 程序要能写（上传的文件等）：归运行用户
			filepath.WalkDir(data, func(p string, _ fs.DirEntry, err error) error {
				if err == nil {
					a.Sys.Lchown(p, uid, gid)
				}
				return nil
			})
		}
	} else {
		a.info(proto.EvKeep, s.Name, "%s：用共用目录 %s", k.Path, rel)
	}
	if err := os.RemoveAll(pkg); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pkg), 0o755); err != nil {
		return err
	}
	return os.Symlink(data, pkg)
}

// mkdirsOwned 在 root 下建 rel（用 /，"." = 不用建）的各层目录；service 为真时归运行用户。
func (a *Agent) mkdirsOwned(root, rel string, uid, gid int, service bool) error {
	if rel == "." || rel == "" {
		return nil
	}
	p := root
	for _, seg := range strings.Split(rel, "/") {
		p = filepath.Join(p, seg)
		if lexists(p) {
			continue
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			return err
		}
		if service {
			a.Sys.Lchown(p, uid, gid)
		}
	}
	return nil
}

// keepShares 有没有共用的保留（data 目录要留着）。
func keepShares(s *proto.AppSpec) bool {
	for _, k := range s.Keep {
		if k.Shared {
			return true
		}
	}
	return false
}

// dropKept 版本记录里的站点文件清单去掉保留的（它们用的是服务器上的，下次部署总是重新比）。
func dropKept(files []proto.SiteFile, keep []proto.KeepItem) []proto.SiteFile {
	if len(keep) == 0 {
		return files
	}
	out := files[:0:0]
	for _, f := range files {
		if !proto.KeepMatch(keep, f.Path) {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------- keepscan

// keepscan 各应用要保留的在服务器上运行中的版本里的情况；KeepDir 不为空时还列出这个目录下的（从服务器上选要保留的）。只读。
func (a *Agent) keepscan() int {
	var out []proto.KeepScan
	for i := range a.req.Apps {
		s := &a.req.Apps[i]
		src, cur := a.keepSource(s)
		ks := proto.KeepScan{App: s.Name, Release: cur}
		for _, k := range s.Keep {
			ks.States = append(ks.States, a.keepState(s, src, k))
		}
		if d := a.req.KeepDir; d != "" && src != "" {
			entries, more, err := listKeepDir(src, d)
			if err != nil {
				return a.result(proto.CodeBadInput, fmt.Sprintf("%s：%v", s.Name, err), nil)
			}
			ks.Entries, ks.More = entries, more
		}
		out = append(out, ks)
	}
	return a.result(proto.CodeOK, "", out)
}

func (a *Agent) keepState(s *proto.AppSpec, src string, k proto.KeepItem) proto.KeepState {
	st := proto.KeepState{KeepItem: k, Server: "none"}
	if k.Shared {
		root := a.sharedDir(s.Name)
		st.Data = noLinkUnder(root, k.Path) && lexists(filepath.Join(root, filepath.FromSlash(k.Path)))
	}
	ms := keepMatches(src, k)
	st.Matches = len(ms)
	if len(ms) != 1 {
		if len(ms) > 1 {
			st.Server = "many"
		}
		return st
	}
	p := filepath.Join(src, filepath.FromSlash(ms[0]))
	fi, err := os.Lstat(p)
	switch {
	case err != nil:
	case fi.Mode()&os.ModeSymlink != 0:
		st.Server = "link"
	case fi.IsDir():
		st.Server = "dir"
		st.Size = int64(countFiles(p))
	case fi.Mode().IsRegular():
		st.Server = "file"
		st.Size = fi.Size()
		if fi.Size() <= 64<<20 {
			st.SHA256, _, _ = zipx.FileSHA256(p)
		}
	}
	return st
}

// listKeepDir 列出 src 下的目录 dir（"." = 最上层）里的文件和目录：目录在前，按名字排。
func listKeepDir(src, dir string) ([]proto.KeepEntry, bool, error) {
	if dir != "." {
		if c := path.Clean(dir); c != dir || !noLinkUnder(src, dir+"/x") {
			return nil, false, fmt.Errorf("目录无效：%q", dir)
		}
	}
	base := filepath.Join(src, filepath.FromSlash(dir))
	fi, err := os.Lstat(base)
	if err != nil || !fi.IsDir() {
		return nil, false, fmt.Errorf("运行中的版本里没有目录 %s", dir)
	}
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil, false, err
	}
	var out []proto.KeepEntry
	for _, e := range ents {
		rel := e.Name()
		if dir != "." {
			rel = dir + "/" + e.Name()
		}
		if dir == "." && e.Name() == dirMeta {
			continue
		}
		x := proto.KeepEntry{Name: e.Name(), Path: rel, Dir: e.IsDir(), Link: e.Type()&fs.ModeSymlink != 0}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			x.Size = info.Size()
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	more := false
	if len(out) > keepListMax {
		out, more = out[:keepListMax], true
	}
	return out, more, nil
}
