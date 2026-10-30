package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

// 每个应用的目录（0.12.18 起）：
//
//	apps/<app>/running/                    运行中的版本：程序（或静态文件）与它运行时写的日志 logs/（${logs}）；也是服务的工作目录
//	apps/<app>/versions/<id>/              其他版本：程序与它上一次运行时的日志 logs/
//	apps/<app>/data/                       不随版本变化的目录（sharedDirs，${data}），归运行用户；只在用到时才有
//	<root>/.ferry/apps/<app>/              Ferry 自己的文件（和锁、历史放在一起，不放在应用目录里）：
//	    running                              running/ 里是哪个版本
//	    run.json  legacy/                    当前运行的开始时间、接管前的单元文件
//	    nginx.conf                           Ferry 的 nginx 在用的配置（由版本保存的那份装上）
//	    versions/<id>/                       release.json、unit.service、nginx.conf、failed、logruns/、runs/<时间>/（这个版本以前运行的日志）
//
// 切换版本：running/ 里的版本放回 versions/<它>，要运行的版本从 versions/ 挪进 running/（同一分区内改名，瞬间完成；
// 没有服务要停的静态站点用 renameat2 一次交换两个目录，访问不中断）。由 Ferry 的 nginx 运行的静态站点：站点文件在 running/www/。
//
// 以前的结构（releases/、shared/、legacy/、.run.json，Ferry 的文件放在版本目录里；0.12.16 是 current 链接 + versions/；
// 0.12.17 是应用目录里的 .ferry/）在下次部署、回滚时自动换成上面的结构；换之前照常能看。

const (
	dirVersions = "versions"
	dirData     = "data"
	dirMeta     = ".ferry"
	dirRunning  = "running"
	dirWWW      = "www" // Ferry 的 nginx 的站点文件
)

// 版本的 Ferry 文件：新结构放在 .ferry/versions/<id>/ 下的名字 → 以前直接放在版本目录下的名字
var oldVerFiles = map[string]string{
	"release.json": "release.json",
	"unit.service": "unit.service",
	"nginx.conf":   "nginx.conf",
	"failed":       ".failed",
	"libs-seeded":  ".libs-seeded",
	"logruns":      ".logruns",
	"oldlogs":      "logs", // 0.12.14 及以前压缩的日志归档
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// realDir 是目录（不是符号链接）。
func realDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

func (a *Agent) runningDir(app string) string  { return filepath.Join(a.appDir(app), dirRunning) }
func (a *Agent) versionsDir(app string) string { return filepath.Join(a.appDir(app), dirVersions) }
func (a *Agent) appMetaDir(app string) string  { return filepath.Join(a.meta(), "apps", app) }

// oldAppMetaDir 0.12.17 放在应用目录里的 .ferry。
func (a *Agent) oldAppMetaDir(app string) string { return filepath.Join(a.appDir(app), dirMeta) }

// verMetaDir 版本的 Ferry 文件所在目录（0.12.17 部署、还没换过来的在应用目录的 .ferry 里）。
func (a *Agent) verMetaDir(app, id string) string {
	p := filepath.Join(a.appMetaDir(app), "versions", id)
	if !exists(p) {
		if old := filepath.Join(a.oldAppMetaDir(app), "versions", id); realDir(old) {
			return old
		}
	}
	return p
}

// appMetaFile 应用的 Ferry 文件：0.12.17 的在应用目录的 .ferry 里，以前的在应用目录下（old 为以前的名字）。
func (a *Agent) appMetaFile(app, name, old string) string {
	if old != "" {
		if p := filepath.Join(a.appDir(app), old); exists(p) {
			return p
		}
	}
	if p := filepath.Join(a.oldAppMetaDir(app), name); exists(p) {
		return p
	}
	return filepath.Join(a.appMetaDir(app), name)
}

// oldCurrentLink 0.12.16 及以前的 current 链接。
func (a *Agent) oldCurrentLink(app string) string { return filepath.Join(a.appDir(app), "current") }

// current 运行中的版本 ID（没有返回空）。
func (a *Agent) current(app string) string {
	if b, err := os.ReadFile(a.appMetaFile(app, "running", "")); err == nil {
		if id := strings.TrimSpace(string(b)); reID.MatchString(id) {
			return id
		}
	}
	// 还没换成新结构：current 链接指向的版本
	t, err := a.Sys.ReadLink(a.oldCurrentLink(app))
	if err != nil {
		return ""
	}
	id := filepath.Base(filepath.FromSlash(t))
	if !reID.MatchString(id) {
		return ""
	}
	return id
}

func (a *Agent) writeRunning(app, id string) error {
	p := filepath.Join(a.appMetaDir(app), "running")
	os.Remove(filepath.Join(a.oldAppMetaDir(app), "running"))
	if id == "" {
		os.Remove(p)
		return nil
	}
	if err := os.MkdirAll(a.appMetaDir(app), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(p, []byte(id+"\n"), 0o644)
}

// releaseDir 版本的目录：运行中的在 running/，其他的在 versions/（以前的结构在 releases/）。
func (a *Agent) releaseDir(app, id string) string {
	if realDir(a.runningDir(app)) && id == a.current(app) {
		return a.runningDir(app)
	}
	for _, d := range []string{dirVersions, "releases"} {
		if p := filepath.Join(a.appDir(app), d, id); realDir(p) {
			return p
		}
	}
	return filepath.Join(a.versionsDir(app), id)
}

// listReleases 版本 ID，新 → 旧。
func (a *Agent) listReleases(app string) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if reID.MatchString(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, d := range []string{dirVersions, "releases"} {
		ents, _ := os.ReadDir(filepath.Join(a.appDir(app), d))
		for _, e := range ents {
			if e.IsDir() {
				add(e.Name())
			}
		}
	}
	add(a.current(app))
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

// sharedDir 不随版本变化的目录：data（还没换成新结构的为 shared）。
func (a *Agent) sharedDir(app string) string {
	d := a.appDir(app)
	if !exists(filepath.Join(d, dirData)) && realDir(filepath.Join(d, "shared")) {
		return filepath.Join(d, "shared")
	}
	return filepath.Join(d, dirData)
}

// legacyDir 接管前的单元文件。
func (a *Agent) legacyDir(app string) string { return a.appMetaFile(app, "legacy", "legacy") }

// runFilePath 当前运行的记录。
func (a *Agent) runFilePath(app string) string { return a.appMetaFile(app, "run.json", ".run.json") }

// verFile 版本的 Ferry 文件（name 用新结构的名字）。没换成新结构的版本，文件还在以前的位置。
func (a *Agent) verFile(app, id, name string) string {
	p := filepath.Join(a.verMetaDir(app, id), name)
	if exists(p) {
		return p
	}
	dir := a.releaseDir(app, id)
	for _, c := range []string{filepath.Join(dir, dirMeta, name), filepath.Join(dir, oldVerFiles[name])} {
		if exists(c) {
			return c
		}
	}
	return p
}

// runLogs 版本运行时写日志的目录名（在版本目录里）；不是服务的为空。
func runLogs(s *proto.AppSpec) string {
	if isService(s) {
		return "logs"
	}
	return ""
}

// oldLayoutDir 版本目录还在 releases/ 下（以前的结构）。
func oldLayoutDir(vdir string) bool { return filepath.Base(filepath.Dir(vdir)) == "releases" }

// rewriteOldPaths 以前的版本保存的单元文件、nginx 配置里的路径换成现在的：
// current → running（静态站点的 root 为 running/www），shared → data，nginx 配置文件 → conf（.ferry/apps/<app>/nginx.conf）。
func rewriteOldPaths(appDir, conf, text string, static bool) string {
	p := filepath.ToSlash(appDir)
	conf = filepath.ToSlash(conf)
	r := strings.NewReplacer(
		p+"/current/.ferry/nginx.conf", conf,
		p+"/current/nginx.conf", conf,
		p+"/.ferry/nginx.conf", conf,
		p+"/current/.ferry/logs", p+"/running/logs",
		p+"/shared", p+"/data",
	)
	text = r.Replace(text)
	if static {
		return strings.ReplaceAll(text, p+"/current", p+"/running/www")
	}
	return strings.ReplaceAll(text, p+"/current", p+"/running")
}

// ---------------------------------------------------------------- 切换

// activate 让版本 to 成为运行中的版本（running/）：现在 running/ 里的放回 versions/。
// 服务应当已经停下；也负责把以前的结构换成新的（见 migrateLayout）。
func (a *Agent) activate(s *proto.AppSpec, to string) error {
	app := s.Name
	dir := a.appDir(app)
	running := a.runningDir(app)
	versions := a.versionsDir(app)
	from := a.current(app)
	hasRunning := realDir(running)
	if hasRunning && from == to {
		return nil
	}
	if err := a.migrateLayout(s); err != nil {
		return fmt.Errorf("换成新的目录结构失败：%v", err)
	}
	toDir := ""
	for _, d := range []string{dirVersions, "releases"} {
		if p := filepath.Join(dir, d, to); realDir(p) {
			toDir = p
			break
		}
	}
	if toDir == "" {
		return fmt.Errorf("没有版本 %s", to)
	}
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return err
	}
	// 以前结构里版本放在 releases/ 下：先挪进 versions/
	if oldLayoutDir(toDir) {
		if err := os.Rename(toDir, filepath.Join(versions, to)); err != nil {
			return err
		}
		toDir = filepath.Join(versions, to)
	}
	if hasRunning {
		if from == "" {
			from = a.unrecordedID(app)
		}
		store := filepath.Join(versions, from)
		if exists(store) {
			return fmt.Errorf("%s 已存在，无法把 %s 放回去", store, from)
		}
		// 一次交换 running 与 versions/<to>：running 立刻是新版本，不会有一刻没有 running
		if exchangeDirs(running, toDir) == nil {
			if err := os.Rename(toDir, store); err != nil {
				exchangeDirs(running, toDir)
				return err
			}
		} else {
			if err := os.Rename(running, store); err != nil {
				return err
			}
			if err := os.Rename(toDir, running); err != nil {
				os.Rename(store, running)
				return err
			}
		}
	} else if err := os.Rename(toDir, running); err != nil {
		return err
	}
	if err := a.writeRunning(app, to); err != nil {
		return err
	}
	a.Sys.RemoveLink(a.oldCurrentLink(app)) // 以前的 current 链接
	for _, id := range []string{from, to} {
		if id != "" {
			a.relocateMeta(s, id)
		}
	}
	return nil
}

// unrecordedID running/ 里有程序，却没有记录是哪个版本（例如目录是从别的应用挪过来的）：放回 versions/ 时起一个新名字。
func (a *Agent) unrecordedID(app string) string {
	base := a.Sys.Now().Format("20060102-150405") + "-unknown"
	id := base
	for i := 2; exists(filepath.Join(a.versionsDir(app), id)); i++ {
		id = fmt.Sprintf("%s%d", base, i)
	}
	a.warn(proto.EvWarn, app, "running/ 里的程序没有版本记录（可能是从别处挪过来的），放到 versions/%s", id)
	return id
}

// deactivate 没有运行中的版本了（首次部署失败、回到接管前）：running/ 里的放回 versions/。
func (a *Agent) deactivate(app string) error {
	running := a.runningDir(app)
	from := a.current(app)
	if realDir(running) {
		if from == "" {
			from = a.unrecordedID(app)
		}
		if err := os.MkdirAll(a.versionsDir(app), 0o755); err != nil {
			return err
		}
		store := filepath.Join(a.versionsDir(app), from)
		if exists(store) {
			return fmt.Errorf("%s 已存在，无法把 %s 放回去", store, from)
		}
		if err := os.Rename(running, store); err != nil {
			return err
		}
	}
	a.Sys.RemoveLink(a.oldCurrentLink(app))
	return a.writeRunning(app, "")
}

// ---------------------------------------------------------------- 迁移

// migrateLayout 把以前的目录结构换成新的（服务已停下）。每一步都可以重复执行，中途失败下次接着做。
func (a *Agent) migrateLayout(s *proto.AppSpec) error {
	app := s.Name
	dir := a.appDir(app)
	if !realDir(dir) {
		return nil
	}
	var done []string
	if err := os.MkdirAll(a.appMetaDir(app), 0o755); err != nil {
		return err
	}
	if old := a.oldAppMetaDir(app); realDir(old) { // 0.12.17：应用目录里的 .ferry
		if _, err := mergeTree(old, a.appMetaDir(app)); err != nil {
			return err
		}
		os.RemoveAll(old) // 只剩下和新位置重复的（不会有）
		done = append(done, "apps/"+app+"/.ferry → .ferry/apps/"+app)
	}
	if realDir(filepath.Join(dir, "releases")) {
		if err := os.MkdirAll(a.versionsDir(app), 0o755); err != nil {
			return err
		}
		ents, _ := os.ReadDir(filepath.Join(dir, "releases"))
		for _, e := range ents {
			if e.IsDir() && reID.MatchString(e.Name()) && !exists(filepath.Join(a.versionsDir(app), e.Name())) {
				if err := os.Rename(filepath.Join(dir, "releases", e.Name()), filepath.Join(a.versionsDir(app), e.Name())); err != nil {
					return err
				}
			}
		}
		os.Remove(filepath.Join(dir, "releases")) // 空了才删得掉
		done = append(done, "releases → versions")
	}
	if shared := filepath.Join(dir, "shared"); realDir(shared) && !exists(filepath.Join(dir, dirData)) {
		if err := os.Rename(shared, filepath.Join(dir, dirData)); err != nil {
			return err
		}
		done = append(done, "shared → data")
	}
	if old := filepath.Join(dir, "legacy"); realDir(old) {
		dst := filepath.Join(a.appMetaDir(app), "legacy")
		if exists(dst) {
			return fmt.Errorf("%s 已存在，无法迁移 %s", dst, old)
		}
		if err := moveTree(old, dst); err != nil {
			return err
		}
		done = append(done, "legacy → .ferry/apps/"+app+"/legacy")
	}
	if old := filepath.Join(dir, ".run.json"); exists(old) {
		if err := moveTree(old, filepath.Join(a.appMetaDir(app), "run.json")); err != nil {
			return err
		}
	}
	// 没在运行的版本：Ferry 的文件放进 .ferry/versions/<id>/（运行中的在 activate 里放，它的路径还有人在用）
	cur := a.current(app)
	for _, id := range a.listReleases(app) {
		if id != cur {
			a.relocateMeta(s, id)
		}
	}
	if len(done) > 0 {
		a.info(proto.EvInfo, app, "目录换成新的结构（%s；Ferry 的文件放进了部署目录的 .ferry/apps/%s，运行中的版本在 running/）", strings.Join(done, "，"), app)
	}
	return nil
}

// relocateMeta 版本里 Ferry 的文件放进 .ferry/versions/<id>/（以前放在版本目录里），日志记录里的目录改成现在的写法。
func (a *Agent) relocateMeta(s *proto.AppSpec, id string) {
	dir := a.releaseDir(s.Name, id)
	if !realDir(dir) {
		return
	}
	dst := a.verMetaDir(s.Name, id)
	mv := func(src, to string) bool {
		if !exists(src) || exists(to) {
			return false
		}
		if os.MkdirAll(filepath.Dir(to), 0o750) != nil {
			return false
		}
		return moveTree(src, to) == nil
	}
	if bm := filepath.Join(dir, dirMeta); realDir(bm) { // 0.12.16
		ents, _ := os.ReadDir(bm)
		for _, e := range ents {
			if e.Name() == "logs" { // Ferry 的 nginx 的日志：和程序放在一起
				os.Rename(filepath.Join(bm, "logs"), filepath.Join(dir, "logs"))
				continue
			}
			mv(filepath.Join(bm, e.Name()), filepath.Join(dst, e.Name()))
		}
		os.Remove(bm)
	}
	for name, old := range oldVerFiles { // 0.12.15 及以前
		if name != "oldlogs" {
			mv(filepath.Join(dir, old), filepath.Join(dst, name))
		}
	}
	// 0.12.14 及以前压缩的日志归档：logs/<时间>.tar.gz、.json、.pending-<时间>/
	if ents, err := os.ReadDir(filepath.Join(dir, "logs")); err == nil {
		for _, e := range ents {
			n := e.Name()
			if reRunID.MatchString(strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(n, ".pending-"), ".tar.gz"), ".json")) {
				mv(filepath.Join(dir, "logs", n), filepath.Join(dst, "oldlogs", n))
			}
		}
	}
	os.Remove(filepath.Join(dir, "logs"))                      // 压缩的归档都挪走了才删得掉
	mv(filepath.Join(dir, "runs"), filepath.Join(dst, "runs")) // 以前运行的日志
	// 日志记录里的目录
	for f, r := range a.runMetas(s.Name, id) {
		d := normRunDir(dir, r.Dir)
		if d == r.Dir {
			continue
		}
		r.Dir = d
		b, _ := json.Marshal(r)
		writeFileAtomic(f, b, 0o640)
	}
	if servedStatic(s) {
		a.reshapeStatic(dir)
	}
}

// reshapeStatic Ferry 的 nginx 的站点：站点文件放在 www/ 里（以前直接放在版本目录下，logs/ 是日志）。
func (a *Agent) reshapeStatic(dir string) {
	www := filepath.Join(dir, dirWWW)
	if exists(www) {
		return
	}
	ents, _ := os.ReadDir(dir)
	if os.Mkdir(www, 0o755) != nil {
		return
	}
	for _, e := range ents {
		switch e.Name() {
		case "logs", "runs", dirMeta, dirWWW:
			continue
		}
		os.Rename(filepath.Join(dir, e.Name()), filepath.Join(www, e.Name()))
	}
}

// normRunDir 日志记录里的目录（0.12.15 为绝对路径，0.12.16 相对版本目录）→ 现在的写法：
// "." 版本目录本身，"meta/…" .ferry/versions/<id>/ 下，其余相对版本目录。
func normRunDir(verDir, d string) string {
	if d == "meta" || strings.HasPrefix(d, "meta/") {
		return d
	}
	rel := relRunDir(verDir, d)
	switch {
	case rel == dirMeta:
		return "."
	case strings.HasPrefix(rel, dirMeta+"/runs/"):
		return "meta/" + strings.TrimPrefix(rel, dirMeta+"/")
	case strings.HasPrefix(rel, "runs/"):
		return "meta/" + rel
	}
	return rel
}

// relRunDir 日志记录里的目录 → 相对版本目录的路径。
func relRunDir(verDir, d string) string {
	if d == "" {
		return "."
	}
	if !strings.HasPrefix(d, "/") && !filepath.IsAbs(d) {
		return path.Clean(filepath.ToSlash(d))
	}
	// 0.12.15 的绝对路径：…/releases/<id>/<rel>（版本目录改了位置也认得）
	slash := filepath.ToSlash(d)
	id := filepath.Base(verDir)
	for _, p := range []string{"/releases/" + id, "/" + dirVersions + "/" + id, "/" + dirRunning} {
		if i := strings.LastIndex(slash, p); i >= 0 {
			rest := strings.TrimPrefix(slash[i+len(p):], "/")
			if rest == "" {
				return "."
			}
			return path.Clean(rest)
		}
	}
	return "."
}

// runDirAbs 日志记录里的目录 → 服务器上的目录。
func (a *Agent) runDirAbs(app, id, d string) string {
	if d == "meta" || strings.HasPrefix(d, "meta/") {
		return filepath.Join(a.verMetaDir(app, id), filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(d, "meta"), "/")))
	}
	vdir := a.releaseDir(app, id)
	return filepath.Join(vdir, filepath.FromSlash(relRunDir(vdir, d)))
}

// runTops 一次运行的日志在哪几个顶层目录（相对它的目录）；always 总是算上。
func runTops(r proto.LogRun, always string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if t != "" && t != "." && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	add(always)
	for _, f := range r.Files {
		add(strings.SplitN(f.Path, "/", 2)[0])
	}
	return out
}

// syncNginxConf Ferry 的 nginx 在用的配置（.ferry/nginx.conf）换成版本 id 保存的那份。
func (a *Agent) syncNginxConf(s *proto.AppSpec, id string) error {
	if !servedStatic(s) {
		return nil
	}
	text, err := os.ReadFile(a.verFile(s.Name, id, "nginx.conf"))
	if err != nil {
		text = []byte(s.NginxConf)
	}
	p := filepath.Join(a.appMetaDir(s.Name), "nginx.conf")
	conf := []byte(rewriteOldPaths(a.appDir(s.Name), p, string(text), true)) // 以前的结构下部署的版本
	// 以前的结构下部署的版本：日志写在 data/logs（nginx 不会自己建目录，这里要先有）
	if dl := filepath.ToSlash(a.appDir(s.Name)) + "/data/logs"; strings.Contains(string(conf), dl+"/") {
		if uid, gid, err := a.Sys.LookupUser(s.User); err == nil {
			if err := os.MkdirAll(filepath.FromSlash(dl), 0o750); err == nil {
				a.Sys.Lchown(filepath.FromSlash(dl), uid, gid)
			}
		}
	}
	if sameFile(p, conf) {
		return nil
	}
	if err := os.MkdirAll(a.appMetaDir(s.Name), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(p, conf, 0o644); err != nil {
		return fmt.Errorf("写 nginx 配置失败：%v", err)
	}
	return nil
}

// ensureDataFor 单元文件里用到 data（以前的版本的工作目录、日志）时，data 要在（归运行用户）。
func (a *Agent) ensureDataFor(s *proto.AppSpec, text string) {
	data := filepath.Join(a.appDir(s.Name), dirData)
	if exists(data) || !strings.Contains(text, filepath.ToSlash(data)) {
		return
	}
	if uid, gid, err := a.Sys.LookupUser(s.User); err == nil && os.Mkdir(data, 0o750) == nil {
		a.Sys.Lchown(data, uid, gid)
	}
}

// dropEmptyData data 里没有文件、现在的配置也用不到它（工作目录、sharedDirs）时删掉：日志都和程序在一起了。
func (a *Agent) dropEmptyData(s *proto.AppSpec) {
	dir := a.appDir(s.Name)
	data := filepath.Join(dir, dirData)
	if !realDir(data) || len(s.SharedDirs) > 0 || len(s.ArchiveLogs) > 0 || hasFiles(data) {
		return
	}
	p := filepath.ToSlash(dir)
	uses := func(b []byte) bool {
		return strings.Contains(string(b), p+"/data") || strings.Contains(string(b), p+"/shared")
	}
	if uses([]byte(s.UnitText)) {
		return
	}
	if s.Unit != "" {
		if b, err := os.ReadFile(a.unitPath(s.Unit)); err == nil && uses(b) {
			return
		}
	}
	// 以前的版本的单元文件用到 data 的：回滚到它时再建（ensureDataFor）
	if os.RemoveAll(data) == nil {
		a.info(proto.EvPrune, s.Name, "%s 里没有文件、也没有版本用到，已删除（日志都和程序在一起）", data)
	}
}
