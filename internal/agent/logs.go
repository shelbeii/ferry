package agent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 运行日志和程序放在一起（目录结构见 layout.go）：
//
//	running/logs/…                            运行中的版本写的日志（${logs}）
//	versions/<id>/logs/…                      这个版本上一次运行时写的日志（切走时和程序一起放回 versions/）
//	.ferry/versions/<id>/runs/<时间>/logs/…    这个版本更早的运行（回滚回来再次运行时，上一次的移到这里）
//	.ferry/versions/<id>/logruns/<时间>.json   每次运行的开始、结束时间、所在目录与文件列表
//	.ferry/run.json                           当前运行：版本与开始时间
//
// 切换版本（部署、回滚、部署失败自动回滚）时，服务停下后把正在离开的版本的日志交给 root（只读）并记下这次运行；
// 要运行的版本从空的日志目录开始（它以前的日志移到 runs/ 下）。
//
// 日志写在 data（shared）下的配置（archiveLogs、以前的结构里的 shared/logs）：切换时把这些目录移进离开的版本目录，之后同上。
// 0.12.14 及以前压缩成的 <时间>.tar.gz（在 .ferry/versions/<id>/oldlogs/）仍然能看、能搜。

var reRunID = regexp.MustCompile(`^\d{8}-\d{6}$`)

type runMark struct {
	Release string    `json:"release"`
	Start   time.Time `json:"start"`
}

// logsDir 以前压缩的日志归档所在目录。
func (a *Agent) logsDir(app, id string) string { return a.verFile(app, id, "oldlogs") }

// markRun 记下当前运行的版本与开始时间（id 为空表示不再由 ferry 运行），并准备好这个版本的日志目录。
func (a *Agent) markRun(s *proto.AppSpec, id string) {
	p := a.runFilePath(s.Name)
	if id == "" {
		os.Remove(p)
		return
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.Marshal(runMark{Release: id, Start: a.Sys.Now()})
	writeFileAtomic(p, b, 0o644)
	a.beginRun(s, id)
}

func (a *Agent) readRun(app string) runMark {
	var m runMark
	if b, err := os.ReadFile(a.runFilePath(app)); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// runMetas 版本记下的各次运行（文件名 → 记录）。
func (a *Agent) runMetas(app, id string) map[string]proto.LogRun {
	out := map[string]proto.LogRun{}
	dir := a.verFile(app, id, "logruns")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		rid, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !reRunID.MatchString(rid) {
			continue
		}
		var r proto.LogRun
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && json.Unmarshal(b, &r) == nil {
			r.ID = rid
			out[filepath.Join(dir, e.Name())] = r
		}
	}
	return out
}

// writeRunMeta 写一次运行的记录。
func (a *Agent) writeRunMeta(app, id string, r proto.LogRun) {
	dir := a.verFile(app, id, "logruns")
	os.MkdirAll(dir, 0o750)
	b, _ := json.Marshal(r)
	writeFileAtomic(filepath.Join(dir, r.ID+".json"), b, 0o640)
}

// toRoot 归档的日志交给 root（运行用户所在的组能读）：服务进程不能再改。
func (a *Agent) toRoot(dir string, gid int) {
	if gid < 0 {
		return
	}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		a.Sys.Lchown(p, 0, gid)
		if fi.Mode().IsRegular() {
			os.Chmod(p, fi.Mode().Perm()&0o750)
		} else if fi.IsDir() {
			os.Chmod(p, 0o750)
		}
		return nil
	})
}

// beginRun 版本 id（已经在 running/ 里）要开始运行：它以前运行时留下的日志移到 .ferry/versions/<id>/runs/ 下，
// 日志目录交给运行用户、从空的开始。
func (a *Agent) beginRun(s *proto.AppSpec, id string) {
	top := runLogs(s)
	if top == "" {
		return
	}
	uid, gid, err := a.Sys.LookupUser(s.User)
	if err != nil {
		return
	}
	vdir := a.releaseDir(s.Name, id)
	if !realDir(vdir) {
		return
	}
	a.shelveRun(s, id, gid)
	logDir := filepath.Join(vdir, top)
	if fi, err := os.Lstat(logDir); err == nil && !fi.IsDir() {
		a.warn(proto.EvLogs, s.Name, "%s 不是目录，日志写不进去", logDir)
		return
	}
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		a.warn(proto.EvLogs, s.Name, "创建日志目录失败：%v", err)
		return
	}
	os.Chmod(logDir, 0o750)
	if err := a.Sys.Lchown(logDir, uid, gid); err != nil {
		a.warn(proto.EvLogs, s.Name, "日志目录交给 %s 失败：%v", s.User, err)
	}
}

// shelveRun 版本目录里放在原处的上一次运行的日志移到 .ferry/versions/<id>/runs/<那次结束的时间>/。
func (a *Agent) shelveRun(s *proto.AppSpec, id string, gid int) {
	top := runLogs(s)
	vdir := a.releaseDir(s.Name, id)
	var prev *proto.LogRun
	prevFile := ""
	for f, r := range a.runMetas(s.Name, id) {
		if normRunDir(vdir, r.Dir) == "." && (prev == nil || r.ID > prev.ID) {
			r := r
			prev, prevFile = &r, f
		}
	}
	tops := []string{top}
	if prev != nil {
		tops = runTops(*prev, top)
	}
	var have []string
	for _, t := range tops {
		if hasFiles(filepath.Join(vdir, t)) {
			have = append(have, t)
		}
	}
	if len(have) == 0 {
		return
	}
	now := a.Sys.Now()
	rid := now.Format("20060102-150405")
	if prev != nil {
		rid = prev.ID
	}
	dst := filepath.Join(a.verMetaDir(s.Name, id), "runs", rid)
	if exists(dst) {
		rid = now.Format("20060102-150405")
		dst = filepath.Join(a.verMetaDir(s.Name, id), "runs", rid)
	}
	if exists(dst) || os.MkdirAll(dst, 0o750) != nil {
		return
	}
	for _, t := range have {
		if err := moveTree(filepath.Join(vdir, t), filepath.Join(dst, t)); err != nil {
			a.warn(proto.EvLogs, s.Name, "移动 %s 上一次运行的日志失败：%v", id, err)
		}
	}
	for p := dst; p != a.appMetaDir(s.Name) && within(a.appMetaDir(s.Name), p); p = filepath.Dir(p) {
		os.Chmod(p, 0o750)
	}
	a.toRoot(dst, gid)
	dir := "meta/runs/" + rid
	if prev != nil {
		os.Remove(prevFile)
		prev.Dir = dir
		a.writeRunMeta(s.Name, id, *prev)
	} else {
		r := proto.LogRun{Release: id, ID: rid, End: now, Dir: dir}
		r.Files, r.Size = listFiles(dst, have)
		a.writeRunMeta(s.Name, id, r)
	}
	a.info(proto.EvLogs, s.Name, "%s 上一次运行的日志移到了 %s", id, dst)
}

// dataLogDirs data（shared）下要随版本归档的日志目录：archiveLogs；data/logs 不是 sharedDirs 时也算
// （以前工作目录是 data，程序按相对路径写的 logs/…；以前的结构里服务写在 shared/logs）。
func dataLogDirs(s *proto.AppSpec, old bool) []string {
	out := append([]string{}, s.ArchiveLogs...)
	if (old || !contains(s.SharedDirs, "logs")) && !contains(out, "logs") {
		out = append(out, "logs")
	}
	return out
}

// rotateLogs 服务停下之后、切换到别的版本之前调用：正在离开的版本 from 这次运行的日志交给 root、记下来，
// 它们留在它的目录里（切走后和程序一起在 versions/<from>/）；写在 data 下的日志目录移进它的目录。
func (a *Agent) rotateLogs(s *proto.AppSpec, from string) {
	if from == "" || from == LegacyID || !isService(s) {
		return
	}
	vdir := a.releaseDir(s.Name, from)
	if !realDir(vdir) {
		return
	}
	gid := -1
	if _, g, err := a.Sys.LookupUser(s.User); err == nil {
		gid = g
	}
	now := a.Sys.Now()
	id := now.Format("20060102-150405")
	old := oldLayoutDir(vdir) // 以前的结构：版本目录里的 logs/ 是更早的运行（压缩的归档、或移过来的日志），不是这次的
	// 这个版本的单元文件里还写着 shared/…（以前的结构下部署的，回滚回来运行的）：它的日志写在 data/logs
	oldData := old
	if b, err := os.ReadFile(a.verFile(s.Name, from, "unit.service")); err == nil && strings.Contains(string(b), filepath.ToSlash(a.appDir(s.Name))+"/shared") {
		oldData = true
	}
	top := runLogs(s)
	var tops []string
	if !old && top != "" && hasFiles(filepath.Join(vdir, top)) {
		tops = append(tops, top)
	}
	// 写在 data 下的日志目录：移进版本目录（同一分区内改名，瞬间完成，不延长停机）
	data := a.sharedDir(s.Name)
	var srcs []string
	for _, d := range dataLogDirs(s, oldData) {
		src := filepath.Join(data, filepath.FromSlash(d))
		if within(data, src) && realDir(src) && hasFiles(src) {
			srcs = append(srcs, d)
		}
	}
	target, dirRec := vdir, "."
	for _, d := range srcs {
		if t := filepath.Join(vdir, strings.SplitN(d, "/", 2)[0]); old && exists(t) { // 以前的结构：版本目录里的 logs/ 是更早的运行
			target, dirRec = filepath.Join(a.verMetaDir(s.Name, from), "runs", id), "meta/runs/"+id
			break
		}
	}
	movedData := false
	for _, d := range srcs {
		dst := filepath.Join(target, filepath.FromSlash(d))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			a.warn(proto.EvLogs, s.Name, "移动 %s 失败（日志留在原处）：%v", d, err)
			continue
		}
		// 和版本目录里的日志合并（同一次运行，有的写在这里、有的写在 data 下）
		left, err := mergeTree(filepath.Join(data, filepath.FromSlash(d)), dst)
		if err != nil {
			a.warn(proto.EvLogs, s.Name, "移动 %s 失败（日志留在原处）：%v", d, err)
			continue
		}
		if left > 0 {
			a.warn(proto.EvLogs, s.Name, "%s 下有 %d 个文件和版本目录里的重名，留在原处", filepath.Join(data, filepath.FromSlash(d)), left)
		}
		movedData = true
		if t := strings.SplitN(d, "/", 2)[0]; !contains(tops, t) {
			tops = append(tops, t)
		}
	}
	if len(tops) == 0 {
		return
	}
	for _, t := range tops {
		a.toRoot(filepath.Join(target, t), gid)
	}
	if target != vdir {
		for p := target; p != a.appMetaDir(s.Name) && within(a.appMetaDir(s.Name), p); p = filepath.Dir(p) {
			os.Chmod(p, 0o750)
		}
	}
	meta := proto.LogRun{Release: from, ID: id, End: now, Dir: dirRec}
	if run := a.readRun(s.Name); run.Release == from {
		meta.Start = run.Start
	}
	meta.Files, meta.Size = listFiles(target, tops)
	a.writeRunMeta(s.Name, from, meta)
	if movedData {
		// data 下的日志目录从空的开始
		if uid, g, err := a.Sys.LookupUser(s.User); err == nil {
			if err := a.ensureShared(s, uid, g); err != nil {
				a.warn(proto.EvLogs, s.Name, "重建日志目录失败：%v", err)
			}
		}
	}
	where := target
	if len(tops) == 1 {
		where = filepath.Join(target, tops[0])
	}
	a.info(proto.EvLogs, s.Name, "%s 这次运行的日志（%d 个文件，%s）和程序一起留在 %s", from, len(meta.Files), sizeText(meta.Size), where)
}

// finishLogArchives 把刚移过来的日志压缩成 tar.gz（新版本就绪之后调用，不占停机时间）。
func (a *Agent) finishLogArchives(s *proto.AppSpec) {
	for _, rel := range a.listReleases(s.Name) {
		dir := a.logsDir(s.Name, rel)
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			id, ok := strings.CutPrefix(e.Name(), ".pending-")
			if !ok || !e.IsDir() || !reRunID.MatchString(id) {
				continue
			}
			pending := filepath.Join(dir, e.Name())
			meta := proto.LogRun{Release: rel, ID: id}
			if b, err := os.ReadFile(filepath.Join(pending, ".meta.json")); err == nil {
				json.Unmarshal(b, &meta)
			}
			files, raw, err := tarGzDir(pending, filepath.Join(dir, id+".tar.gz"))
			if err != nil {
				a.warn(proto.EvLogs, s.Name, "压缩归档的日志失败（保留未压缩的）：%v", err)
				continue
			}
			meta.Files = files
			if fi, err := os.Stat(filepath.Join(dir, id+".tar.gz")); err == nil {
				meta.Size = fi.Size()
			}
			b, _ := json.Marshal(meta)
			if err := writeFileAtomic(filepath.Join(dir, id+".json"), b, 0o640); err != nil {
				continue
			}
			os.RemoveAll(pending)
			a.info(proto.EvLogs, s.Name, "%s 的日志已压缩：%d 个文件，%s → %s", rel, len(files), sizeText(raw), sizeText(meta.Size))
		}
	}
}

// tarGzDir 把目录里的普通文件打包（跳过符号链接与 .meta.json），返回文件列表与原始总大小。
func tarGzDir(dir, out string) ([]proto.LogFile, int64, error) {
	tmp := out + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, 0, err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	var files []proto.LogFile
	var total int64
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == ".meta.json" {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o640, Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		n, err := io.CopyN(tw, src, fi.Size())
		if err != nil {
			return err
		}
		files = append(files, proto.LogFile{Path: rel, Size: n, MTime: fi.ModTime()})
		total += n
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return nil, 0, err
	}
	sortLogFiles(files)
	return files, total, os.Rename(tmp, out)
}

// hasFiles 目录里（含子目录）有没有普通文件。
func hasFiles(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// listFiles 目录里的普通文件（路径相对 base）。
func listFiles(base string, dirs []string) ([]proto.LogFile, int64) {
	var files []proto.LogFile
	var total int64
	for _, d := range dirs {
		root := filepath.Join(base, filepath.FromSlash(d))
		filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || !e.Type().IsRegular() {
				return nil
			}
			fi, err := e.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			if rel = filepath.ToSlash(rel); rel != ".meta.json" {
				files = append(files, proto.LogFile{Path: rel, Size: fi.Size(), MTime: fi.ModTime()})
				total += fi.Size()
			}
			return nil
		})
	}
	sortLogFiles(files)
	return files, total
}

// currentLogs 当前运行的日志在哪：目录与其下的日志目录。
// 日志写在 data 下的配置（archiveLogs）与还没换成新结构的应用为 data；其余为运行中版本目录里的 logs/。
func (a *Agent) currentLogs(s *proto.AppSpec, cur string) (string, []string) {
	vdir := a.releaseDir(s.Name, cur)
	if oldLayoutDir(vdir) || len(s.ArchiveLogs) > 0 {
		dirs := s.ArchiveLogs
		if len(dirs) == 0 {
			dirs = []string{"logs"}
		}
		return a.sharedDir(s.Name), dirs
	}
	top := runLogs(s)
	if top == "" {
		return a.sharedDir(s.Name), nil
	}
	if servedStatic(s) && realDir(filepath.Join(vdir, dirMeta, "logs")) { // 0.12.16：Ferry 的 nginx 的日志在版本的 .ferry/logs
		return filepath.Join(vdir, dirMeta), []string{top}
	}
	return vdir, []string{top}
}

// logRuns 应用的运行日志：当前运行在前，归档的按时间从新到旧。
func (a *Agent) logRuns(s *proto.AppSpec) []proto.LogRun {
	out := []proto.LogRun{}
	if cur := a.current(s.Name); cur != "" {
		r := proto.LogRun{Release: cur, Current: true}
		if run := a.readRun(s.Name); run.Release == cur {
			r.Start = run.Start
		}
		base, dirs := a.currentLogs(s, cur)
		r.Dir = base
		r.Files, r.Size = listFiles(base, dirs)
		if r.Files == nil {
			r.Files = []proto.LogFile{}
		}
		out = append(out, r)
	}
	var archived []proto.LogRun
	for _, rel := range a.listReleases(s.Name) {
		archived = append(archived, a.archivedRuns(s.Name, rel)...)
	}
	sort.SliceStable(archived, func(i, j int) bool { return archived[i].ID > archived[j].ID })
	return append(out, archived...)
}

// archivedRuns 某个版本归档的运行日志：放在版本目录里的，以及以前压缩的、还没压缩的。
func (a *Agent) archivedRuns(app, rel string) []proto.LogRun {
	var out []proto.LogRun
	relDir := a.releaseDir(app, rel)
	metaDir := a.verMetaDir(app, rel)
	for _, r := range a.runMetas(app, rel) {
		d := a.runDirAbs(app, rel, r.Dir)
		if d != relDir && !within(relDir, d) && d != metaDir && !within(metaDir, d) {
			continue
		}
		// 文件以磁盘上为准（只看记录里的那几个顶层目录，不把程序文件算进来）
		dirs := runTops(r, "")
		r.Release, r.Dir = rel, d
		r.Files, r.Size = listFiles(d, dirs)
		if r.Files == nil {
			r.Files = []proto.LogFile{}
		}
		out = append(out, r)
	}
	dir := a.logsDir(app, rel)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		name := e.Name()
		if id, ok := strings.CutSuffix(name, ".json"); ok && reRunID.MatchString(id) {
			var r proto.LogRun
			if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil && json.Unmarshal(b, &r) == nil {
				r.Release, r.ID = rel, id
				if r.Files == nil {
					r.Files = []proto.LogFile{}
				}
				out = append(out, r)
			}
		} else if id, ok := strings.CutPrefix(name, ".pending-"); ok && e.IsDir() && reRunID.MatchString(id) {
			r := proto.LogRun{Release: rel, ID: id}
			p := filepath.Join(dir, name)
			if b, err := os.ReadFile(filepath.Join(p, ".meta.json")); err == nil {
				json.Unmarshal(b, &r)
			}
			r.Files, r.Size = listFiles(p, []string{"."})
			if r.Files == nil {
				r.Files = []proto.LogFile{}
			}
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// ---------------------------------------------------------------- agent 命令

func (a *Agent) logruns() int {
	if len(a.req.Apps) != 1 {
		return a.result(proto.CodeBadInput, "一次只能查看一个应用的日志", nil)
	}
	return a.result(proto.CodeOK, "", a.logRuns(&a.req.Apps[0]))
}

// tailLines 读到结尾，保留最后 n 行（单行过长时截断）。
func tailLines(r io.Reader, n int) proto.LogText {
	const maxLine = 16 << 10
	ring := make([]string, 0, n)
	start, total := 0, 0
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if len(line) > maxLine {
				line = line[:maxLine] + " …（这一行太长，已截断）"
			}
			if len(ring) < n {
				ring = append(ring, line)
			} else {
				ring[start] = line
				start = (start + 1) % n
			}
			total++
		}
		if err != nil {
			break
		}
	}
	out := append(append([]string{}, ring[start:]...), ring[:start]...)
	return proto.LogText{Lines: out, Truncated: total > n}
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
