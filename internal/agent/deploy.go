package agent

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

const (
	staleIncoming = 12 * time.Hour
	cacheMaxAge   = 60 * 24 * time.Hour
)

// ---------------------------------------------------------------- prepare

// prepare 创建本次的上传目录（交给 SSH 登录用户，便于非 root 用 SFTP 上传），
// 按需从各 Java 应用的当前版本抽取依赖，返回已缓存的依赖列表。
func (a *Agent) prepare() int {
	if err := a.ensureMeta(); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	a.cleanIncoming()
	id := a.Sys.Now().Format("20060102-150405") + "-" + randHex(4)
	stage := filepath.Join(a.incomingDir(), id)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	// sudo 执行时把上传目录交给登录用户
	if uid, err := strconv.Atoi(os.Getenv("SUDO_UID")); err == nil && uid != 0 {
		gid, _ := strconv.Atoi(os.Getenv("SUDO_GID"))
		if err := a.Sys.Lchown(stage, uid, gid); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
	}
	out := proto.Prepared{Stage: stage}
	if a.req.Seed {
		for _, s := range a.req.Apps {
			if s.Type != proto.TypeJava {
				continue
			}
			out.Seeded += a.seedFrom(s)
		}
		if out.Seeded > 0 {
			a.info(proto.EvCache, "", "从服务器现有版本抽取了 %d 个依赖", out.Seeded)
		}
	}
	if a.req.Sites {
		for _, s := range a.req.Apps {
			if s.Type != proto.TypeStatic {
				continue
			}
			if files := a.siteFiles(s.Name); files != nil {
				if out.Sites == nil {
					out.Sites = map[string][]proto.SiteFile{}
				}
				out.Sites[s.Name] = files
			}
		}
	}
	ents, _ := os.ReadDir(a.cacheDir())
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".jar") && zipx.IsSHA256(strings.TrimSuffix(n, ".jar")) {
			out.Libs = append(out.Libs, strings.TrimSuffix(n, ".jar"))
		}
	}
	return a.result(proto.CodeOK, "", out)
}

// keepLibs 没部署成功（失败、跳过）的 Java 应用：把这次上传的 jar 里的依赖收进缓存。
// 改好配置重新部署时，即使重新构建了，也只需要上传变了的部分，不用再传一遍整个 jar。
func (a *Agent) keepLibs(s *proto.AppSpec, rel proto.ReleaseSpec, stage string) {
	if s.Type != proto.TypeJava {
		return
	}
	for _, p := range []string{filepath.Join(a.releaseDir(s.Name, rel.ID), s.FileName), filepath.Join(stage, rel.File)} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		n, err := zipx.ExtractLibs(p, a.cacheDir())
		if n > 0 {
			a.info(proto.EvCache, s.Name, "留下了这次上传的 %d 个依赖，下次部署不用再传", n)
		}
		if err == nil {
			return
		}
	}
}

// seedFrom 从应用的版本抽取依赖（每个版本只抽一次）：当前版本，以及还没切换过去的最新版本
// （比如第一次部署失败留下的：改好配置再部署时只传变了的部分）。
func (a *Agent) seedFrom(s proto.AppSpec) int {
	n := 0
	cur := a.current(s.Name)
	if cur != "" {
		n += a.seedRelease(s, cur)
	}
	if ids := a.listReleases(s.Name); len(ids) > 0 && ids[0] != cur {
		n += a.seedRelease(s, ids[0])
	}
	return n
}

func (a *Agent) seedRelease(s proto.AppSpec, cur string) int {
	dir := a.releaseDir(s.Name, cur)
	if _, err := os.Stat(filepath.Join(dir, s.FileName)); err != nil {
		return 0
	}
	marker := a.verFile(s.Name, cur, "libs-seeded")
	os.MkdirAll(filepath.Dir(marker), 0o750)
	if _, err := os.Stat(marker); err == nil {
		return 0
	}
	n, err := zipx.ExtractLibs(filepath.Join(dir, s.FileName), a.cacheDir())
	if err != nil {
		a.warn(proto.EvCache, s.Name, "抽取依赖失败（本次多传一些）：%v", err)
		return n
	}
	os.WriteFile(marker, nil, 0o644)
	return n
}

func (a *Agent) cleanIncoming() {
	ents, _ := os.ReadDir(a.incomingDir())
	for _, e := range ents {
		fi, err := e.Info()
		if err == nil && a.Sys.Now().Sub(fi.ModTime()) > staleIncoming {
			os.RemoveAll(filepath.Join(a.incomingDir(), e.Name()))
		}
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// ---------------------------------------------------------------- deploy

func (a *Agent) deploy() int {
	req := a.req
	stage := filepath.Clean(req.Stage)
	if filepath.Dir(stage) != a.incomingDir() {
		return a.result(proto.CodeBadInput, "上传目录不在 "+a.incomingDir()+" 下："+req.Stage, nil)
	}
	if len(req.Releases) != len(req.Apps) {
		return a.result(proto.CodeBadInput, "releases 与 apps 数量不一致", nil)
	}
	rels := map[string]proto.ReleaseSpec{}
	for _, r := range req.Releases {
		if !reID.MatchString(r.ID) || !reSafe.MatchString(r.File) || !zipx.IsSHA256(r.SHA256) {
			return a.result(proto.CodeBadInput, "版本信息无效："+r.App, nil)
		}
		if (r.Plan != "" && !reSafe.MatchString(r.Plan)) || (r.Pack != "" && !reSafe.MatchString(r.Pack)) {
			return a.result(proto.CodeBadInput, "增量文件名无效："+r.App, nil)
		}
		rels[r.App] = r
	}
	for _, s := range req.Apps {
		if _, ok := rels[s.Name]; !ok {
			return a.result(proto.CodeBadInput, "缺少版本信息："+s.Name, nil)
		}
	}
	if _, err := os.Stat(stage); err != nil {
		return a.result(proto.CodeBadInput, "上传目录不存在："+stage, nil)
	}

	defer os.RemoveAll(stage) // 本次的上传目录：无论成败都删除（拿不到锁也删，下次重新上传）
	unlock, err := a.lock(time.Duration(req.LockWaitSec) * time.Second)
	if errors.Is(err, ErrLocked) {
		return a.result(proto.CodeLocked, err.Error(), nil)
	}
	if err != nil {
		return a.result(proto.CodeFailed, "加锁失败："+err.Error(), nil)
	}
	defer unlock()

	if err := a.verifyStage(stage, req.Apps, rels); err != nil {
		a.fail(proto.EvAbort, "", "%v；未改动任何应用", err)
		return a.result(proto.CodeVerify, err.Error(), nil)
	}

	var out proto.DeployOutcome
	failed := ""
	var skipped []string
	for i := range req.Apps {
		s := &req.Apps[i]
		rel := rels[s.Name]
		if failed != "" {
			skipped = append(skipped, s.Name)
			out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Release: rel.ID, Result: "SKIPPED"})
			continue
		}
		o := a.deployOne(s, rel, stage)
		out.Apps = append(out.Apps, o)
		if o.Result != "OK" {
			failed = s.Name
		}
	}
	for i, o := range out.Apps {
		if o.Result != "OK" {
			a.keepLibs(&req.Apps[i], rels[o.App], stage)
		}
	}
	a.pruneCache()
	if failed != "" {
		if len(skipped) > 0 {
			a.fail(proto.EvAbort, strings.Join(skipped, ","), "因 %s 失败未部署，保持原版本", failed)
		}
		return a.result(proto.CodeFailed, failed+" 部署失败", out)
	}
	return a.result(proto.CodeOK, "", out)
}

// verifyStage 收下新依赖、拼装增量文件、核对全部文件的 sha256 与大小。任何一步失败都不动应用。
func (a *Agent) verifyStage(stage string, apps []proto.AppSpec, rels map[string]proto.ReleaseSpec) error {
	libs := filepath.Join(stage, "libs")
	if ents, err := os.ReadDir(libs); err == nil {
		for _, e := range ents {
			sha := strings.TrimSuffix(e.Name(), ".jar")
			if !zipx.IsSHA256(sha) || !strings.HasSuffix(e.Name(), ".jar") {
				return fmt.Errorf("上传的依赖文件名无效：%s", e.Name())
			}
			src := filepath.Join(libs, e.Name())
			got, _, err := zipx.FileSHA256(src)
			if err != nil || got != sha {
				return fmt.Errorf("上传的依赖 %s 校验失败", sha[:12])
			}
			dst := filepath.Join(a.cacheDir(), e.Name())
			if _, err := os.Stat(dst); err == nil {
				os.Remove(src)
				continue
			}
			if err := moveFile(src, dst); err != nil {
				return err
			}
			os.Chmod(dst, 0o644)
		}
	}
	for _, s := range apps {
		r := rels[s.Name]
		if r.Plan == "" {
			continue
		}
		a.info(proto.EvAssemble, s.Name, "%s", r.File)
		plan := filepath.Join(stage, r.Plan)
		pack := filepath.Join(stage, r.Pack)
		if err := zipx.Assemble(plan, pack, filepath.Join(stage, r.File), a.cachedLib); err != nil {
			a.purgeBadLibs(plan)
			return fmt.Errorf("%s 拼装失败：%v", s.Name, err)
		}
		a.touchLibs(plan)
		if got, _, err := zipx.FileSHA256(filepath.Join(stage, r.File)); err == nil && got != r.SHA256 {
			a.purgeBadLibs(plan)
		}
		os.Remove(plan)
		os.Remove(pack)
	}
	for _, s := range apps {
		r := rels[s.Name]
		p := filepath.Join(stage, r.File)
		got, size, err := zipx.FileSHA256(p)
		if err != nil {
			return fmt.Errorf("%s 缺少上传文件 %s", s.Name, r.File)
		}
		if size != r.Size {
			return fmt.Errorf("%s 文件大小不一致（%d ≠ %d）", s.Name, size, r.Size)
		}
		if got != r.SHA256 {
			return fmt.Errorf("%s sha256 不一致，文件在传输中损坏", s.Name)
		}
		a.info(proto.EvVerify, s.Name, "%s", got[:12])
		if r.Files == "" {
			continue
		}
		if s.Type != proto.TypeJava && s.Type != proto.TypeBinary {
			return fmt.Errorf("%s：附带文件只用于 java、binary", s.Name)
		}
		got, size, err = zipx.FileSHA256(filepath.Join(stage, r.Files))
		if err != nil {
			return fmt.Errorf("%s 缺少上传文件 %s", s.Name, r.Files)
		}
		if size != r.FilesSize || got != r.FilesSHA256 {
			return fmt.Errorf("%s 的附带文件校验失败，文件在传输中损坏", s.Name)
		}
	}
	// 静态站点只传了变化的文件：在上传目录里拼出完整站点并逐个核对
	for _, s := range apps {
		r := rels[s.Name]
		if r.Site == "" {
			continue
		}
		if s.Type != proto.TypeStatic {
			return fmt.Errorf("%s：文件清单只用于静态站点", s.Name)
		}
		if got, _, err := zipx.FileSHA256(filepath.Join(stage, r.Site)); err != nil || got != r.SiteSHA256 {
			return fmt.Errorf("%s 的文件清单校验失败，文件在传输中损坏", s.Name)
		}
		if r.SiteDelta {
			if err := a.assembleSite(s, stage, r); err != nil {
				return fmt.Errorf("%s 拼装站点失败：%v", s.Name, err)
			}
		}
	}
	return nil
}

func (a *Agent) cachedLib(sha string) string { return filepath.Join(a.cacheDir(), sha+".jar") }

// purgeBadLibs 拼装失败时逐个核对计划用到的缓存依赖，删掉内容与文件名（sha256）不符的，下次不再复用。
func (a *Agent) purgeBadLibs(plan string) {
	f, err := os.Open(plan)
	if err != nil {
		return
	}
	steps, _ := zipx.ParsePlan(f)
	f.Close()
	for _, s := range steps {
		if s.Raw {
			continue
		}
		p := a.cachedLib(s.SHA256)
		got, _, err := zipx.FileSHA256(p)
		if err == nil && got != s.SHA256 {
			os.Remove(p)
			a.warn(proto.EvCache, "", "缓存依赖 %s 已损坏，已删除", s.SHA256[:12])
		}
	}
}

// touchLibs 更新用到的缓存依赖的修改时间（缓存按修改时间清理）。
func (a *Agent) touchLibs(plan string) {
	f, err := os.Open(plan)
	if err != nil {
		return
	}
	steps, _ := zipx.ParsePlan(f)
	f.Close()
	now := a.Sys.Now()
	for _, s := range steps {
		if !s.Raw {
			os.Chtimes(a.cachedLib(s.SHA256), now, now)
		}
	}
}

func (a *Agent) pruneCache() {
	ents, _ := os.ReadDir(a.cacheDir())
	removed := 0
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tmp") || a.Sys.Now().Sub(fi.ModTime()) > cacheMaxAge {
			if os.Remove(filepath.Join(a.cacheDir(), e.Name())) == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		a.info(proto.EvPrune, "", "清理了 %d 个 60 天未用的缓存依赖", removed)
	}
}

// ---------------------------------------------------------------- 单个应用

// isService 由 systemd 运行的应用：java、binary，以及由 Ferry 的 nginx 运行的静态站点。
func isService(s *proto.AppSpec) bool {
	return s.Type == proto.TypeJava || s.Type == proto.TypeBinary || proto.DirService(s.Type) || servedStatic(s)
}

// servedStatic 由 Ferry 提供的 nginx 运行的静态站点（static.port）。
func servedStatic(s *proto.AppSpec) bool { return s.Type == proto.TypeStatic && s.Unit != "" }

func isRunning(p UnitProps) bool {
	return p.Active == "active" || p.Active == "activating" || p.Active == "reloading"
}

// deployOne 放置新版本 → 切换 → 启动 → 等待就绪；失败时回滚该应用。
func (a *Agent) deployOne(s *proto.AppSpec, rel proto.ReleaseSpec, stage string) proto.AppOutcome {
	o := proto.AppOutcome{App: s.Name, Release: rel.ID}
	failNow := func(err error) proto.AppOutcome {
		a.fail(proto.EvFail, s.Name, "%v", err)
		o.Result, o.Msg = "FAILED", err.Error()
		a.addHistory(proto.HistoryEntry{Action: "deploy", App: s.Name, Release: rel.ID, Result: o.Result,
			Commit: rel.Meta.Commit, SHA256: rel.SHA256, Msg: o.Msg})
		return o
	}
	if err := a.ensureAppRoot(s.Name); err != nil {
		return failNow(err)
	}
	gid := 0
	if isService(s) {
		uid, g, err := a.Sys.LookupUser(s.User)
		if err != nil {
			return failNow(err)
		}
		gid = g
		if err := a.ensureShared(s, uid, gid); err != nil {
			return failNow(err)
		}
	}
	if err := a.placeRelease(s, rel, stage, gid); err != nil {
		return failNow(err)
	}
	prev := a.current(s.Name)
	a.info(proto.EvRelease, s.Name, "%s（上一版本 %s）", rel.ID, orNone(prev))
	if s.Hooks != nil {
		// 切换前的命令（如数据库迁移）：失败时不切换，线上照旧
		ph := hookPhase{name: "beforeSwitch", label: "切换前的命令", dir: a.releaseDir(s.Name, rel.ID)}
		if err := a.runHooks(s, ph, s.Hooks.BeforeSwitch, rel.ID, prev); err != nil {
			a.markFailed(s.Name, rel.ID)
			if prev != "" {
				return failNow(fmt.Errorf("%v；没有切换，线上还是 %s", err, prev))
			}
			return failNow(fmt.Errorf("%v；没有切换", err))
		}
	}

	var res, msg string
	a.readySec = 0
	if isService(s) {
		if s.ZD != nil {
			res, msg = a.zdSwitch(s, rel.ID, prev, true)
		} else {
			a.leaveZD(s)
			res, msg = a.switchService(s, rel.ID, prev)
		}
		a.finishLogArchives(s)
	} else {
		res, msg = a.switchStatic(s, rel.ID, prev)
	}
	o.Result, o.Msg = res, msg
	var errLines *int
	if res == "OK" && isService(s) && a.readySec > 0 {
		o.ReadySec, o.PrevReadySec = a.readySec, a.lastReadySec(s.Name)
		a.compareReady(s.Name, o.ReadySec, o.PrevReadySec)
		n := a.errorLines
		errLines = &n
		o.ErrorLines, o.PrevErrorLines = n, a.lastErrorLines(s.Name)
		a.compareErrors(s.Name, o.ErrorLines, o.PrevErrorLines)
	}
	a.addHistory(proto.HistoryEntry{Action: "deploy", App: s.Name, Release: rel.ID, Result: res,
		Commit: rel.Meta.Commit, SHA256: rel.SHA256, Msg: msg, ReadySec: o.ReadySec, ErrorLines: errLines})
	if res == "OK" {
		a.pruneReleases(s)
		a.dropEmptyData(s)
	}
	return o
}

func orNone(s string) string {
	if s == "" {
		return "无"
	}
	return s
}

// ensureShared 创建 data 目录与 sharedDirs（归运行用户）；用不到 data 时不建。
func (a *Agent) ensureShared(s *proto.AppSpec, uid, gid int) error {
	if err := os.MkdirAll(a.versionsDir(s.Name), 0o755); err != nil {
		return err
	}
	dirs := append(append([]string{}, s.SharedDirs...), s.ArchiveLogs...)
	// data 只在用到时才建：sharedDirs、或单元文件里写了它（如工作目录）
	if len(dirs) == 0 && !strings.Contains(s.UnitText, filepath.ToSlash(a.sharedDir(s.Name))) {
		return nil
	}
	for _, d := range append([]string{""}, dirs...) {
		p := filepath.Join(a.sharedDir(s.Name), filepath.FromSlash(d))
		if !within(a.appDir(s.Name), p) {
			return fmt.Errorf("sharedDirs 越界：%s", d)
		}
		if err := os.MkdirAll(p, 0o750); err != nil {
			return err
		}
		if err := a.Sys.Lchown(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// placeRelease 在 versions/.tmp-<id> 里放好程序，Ferry 的文件（release.json 等）放进 .ferry/versions/<id>/，再改名为正式版本目录。
// 版本目录归 root：服务进程只能读（组 = 运行用户的组），不能改，root 写 failed 等标记也不会被符号链接劫持；
// 只有运行时的日志目录（logs/）归运行用户，由开始运行时建。
func (a *Agent) placeRelease(s *proto.AppSpec, rel proto.ReleaseSpec, stage string, gid int) error {
	if err := os.MkdirAll(a.versionsDir(s.Name), 0o755); err != nil {
		return err
	}
	final := a.releaseDir(s.Name, rel.ID)
	if exists(final) || exists(a.verMetaDir(s.Name, rel.ID)) {
		return fmt.Errorf("版本 %s 已存在", rel.ID)
	}
	tmp := filepath.Join(a.versionsDir(s.Name), ".tmp-"+rel.ID)
	os.RemoveAll(tmp)
	dirMode := os.FileMode(0o755)
	if isService(s) {
		dirMode = 0o750
	}
	if err := os.Mkdir(tmp, dirMode); err != nil {
		return err
	}
	meta := a.verMetaDir(s.Name, rel.ID)
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
			os.RemoveAll(meta)
		}
	}()
	// .ferry 本身运行用户要能进（Ferry 的 nginx 读 .ferry/nginx.conf），里面各版本的文件只有 root 能看
	if err := os.MkdirAll(a.appMetaDir(s.Name), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(meta, 0o750); err != nil {
		return err
	}
	src := filepath.Join(stage, rel.File)
	var fileSums map[string]string
	switch s.Type {
	case proto.TypeStatic:
		site := tmp // 站点文件直接放在版本目录里；Ferry 的 nginx 的放在 www/（日志 logs/ 与它并列）
		if servedStatic(s) {
			site = filepath.Join(tmp, dirWWW)
			if err := os.Mkdir(site, 0o755); err != nil {
				return err
			}
		}
		if rel.SiteDelta {
			if err := placeSite(stage, s.Name, site); err != nil {
				return fmt.Errorf("放置站点文件失败：%v", err)
			}
		} else if err := extractTarGz(src, site); err != nil {
			return fmt.Errorf("解压失败：%v", err)
		}
		if rel.Site != "" { // 文件清单随版本保存：下次部署据此只传变化的文件
			if b, err := os.ReadFile(filepath.Join(stage, rel.Site)); err == nil {
				os.WriteFile(filepath.Join(meta, siteManifest), b, 0o644)
			}
		}
		if servedStatic(s) || s.NginxInstance != "" {
			// 单元与 nginx 配置随版本保存（回滚时一起回滚）
			if err := os.WriteFile(filepath.Join(meta, "unit.service"), []byte(s.UnitText), 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(meta, "nginx.conf"), []byte(s.NginxConf), 0o644); err != nil {
				return err
			}
		}
	case proto.TypePython, proto.TypeNode, proto.TypeDotnet:
		if err := a.placeDirApp(s, src, tmp, gid); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(meta, "unit.service"), []byte(s.UnitText), 0o644); err != nil {
			return err
		}
	default:
		dst := filepath.Join(tmp, s.FileName)
		if err := moveFile(src, dst); err != nil {
			return err
		}
		mode := os.FileMode(0o640)
		if s.Type == proto.TypeBinary {
			mode = 0o750
		}
		if err := os.Chmod(dst, mode); err != nil {
			return err
		}
		if err := a.Sys.Lchown(dst, 0, gid); err != nil {
			return err
		}
		if rel.Files != "" {
			sums, err := a.placeFiles(filepath.Join(stage, rel.Files), tmp, s.FileName, gid)
			if err != nil {
				return fmt.Errorf("放附带文件失败：%v", err)
			}
			fileSums = sums
		}
		if err := os.WriteFile(filepath.Join(meta, "unit.service"), []byte(s.UnitText), 0o644); err != nil {
			return err
		}
	}
	m := rel.Meta
	m.ID, m.App, m.SHA256, m.Size = rel.ID, s.Name, rel.SHA256, rel.Size
	m.FileSums = fileSums
	m.DeployedAt = a.Sys.Now()
	if m.DeployedBy == "" {
		m.DeployedBy = a.req.Actor
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(meta, "release.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	if isService(s) {
		if err := a.Sys.Lchown(tmp, 0, gid); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	ok = true
	if s.Deps != nil {
		a.pruneDeps(s.Name)
	}
	return nil
}

// switchService 停止 → 切换 → 写单元 → 启动 → 等待就绪；失败回滚。返回结果与说明。
func (a *Agent) switchService(s *proto.AppSpec, id, prev string) (string, string) {
	// 站点端口被别的程序占着：新版本起不来，先不停旧版本
	for _, p := range sitePorts(s) {
		if msg := a.portBusy(p, append([]string{s.Unit}, s.Conflict...)...); msg != "" {
			a.fail(proto.EvFail, s.Name, "%s", msg)
			a.markFailed(s.Name, id)
			if prev != "" {
				return "FAILED", msg + "；没有切换，线上还是 " + prev
			}
			return "FAILED", msg + "；没有切换"
		}
	}
	unitFile := a.unitPath(s.Unit)
	before, err := os.ReadFile(unitFile)
	if err != nil {
		before = nil
	} else if !strings.HasPrefix(string(before), ManagedMarker) {
		if err := a.saveLegacy(s.Unit, before, s.Name); err != nil {
			a.fail(proto.EvFail, s.Name, "保存接管前的单元文件失败：%v", err)
			a.markFailed(s.Name, id)
			return "FAILED", err.Error()
		}
	}
	wasActive := isRunning(a.Sys.UnitProps(s.Unit))

	var stopped []string
	for _, c := range s.Conflict {
		if isRunning(a.Sys.UnitProps(c)) {
			a.info(proto.EvStop, s.Name, "停止冲突的服务 %s", c)
			if err := a.Sys.Systemctl("stop", c); err != nil {
				a.warn(proto.EvWarn, s.Name, "%v", err)
			}
			stopped = append(stopped, c)
		}
	}
	if wasActive {
		a.info(proto.EvStop, s.Name, "%s", s.Unit)
		if err := a.Sys.Systemctl("stop", s.Unit); err != nil {
			a.warn(proto.EvWarn, s.Name, "%v", err)
		}
	}
	a.rotateLogs(s, prev)
	// 以前的目录结构这时才换（服务已停下，没有人在用旧路径）；失败也能退回去，因为切换前的单元文件会还原
	fail := func(msg string) (string, string) {
		a.markFailed(s.Name, id)
		if a.req.NoRollback {
			a.warn(proto.EvWarn, s.Name, "按 --no-rollback 保留失败版本")
			return "FAILED", msg
		}
		if err := a.restoreService(s, id, prev, wasActive, before, stopped); err != nil {
			a.fail(proto.EvRollback, s.Name, "回滚失败：%v；请人工处理", err)
			return "ROLLBACK_FAILED", msg + "；回滚失败：" + err.Error()
		}
		return "ROLLED_BACK", msg
	}

	if err := a.activate(s, id); err != nil {
		return fail("切换版本失败：" + err.Error())
	}
	a.info(proto.EvSwitch, s.Name, "running → %s", id)
	a.markRun(s, id)
	if err := a.syncNginxConf(s, id); err != nil {
		return fail(err.Error())
	}
	if err := a.tlsPrepare(s); err != nil {
		return fail("准备 HTTPS 证书失败：" + err.Error())
	}
	if err := a.installUnit(s.Name, s.Unit, []byte(s.UnitText)); err != nil {
		return fail(err.Error())
	}
	if err := a.Sys.Systemctl("enable", s.Unit); err != nil {
		a.warn(proto.EvWarn, s.Name, "设置开机自启失败：%v", err)
	}
	a.info(proto.EvStart, s.Name, "%s", s.Unit)
	if err := a.Sys.Systemctl("restart", s.Unit); err != nil {
		a.diag(s, s.Unit)
		return fail(err.Error())
	}
	if err := a.waitReady(s, s.Unit); err != nil {
		a.diag(s, s.Unit)
		return fail(err.Error())
	}
	a.tlsAfterStart(s) // 站点起来了：需要时申请 HTTPS 证书（失败只提示）
	if s.Hooks != nil {
		ph := hookPhase{name: "afterSwitch", label: "切换后的命令", dir: a.runningDir(s.Name)}
		if err := a.runHooks(s, ph, s.Hooks.AfterSwitch, id, prev); err != nil {
			a.fail(proto.EvFail, s.Name, "%v", err)
			return fail(err.Error())
		}
	}
	// 新版本稳定后，冲突的旧服务不再开机自启（回滚时还原）
	for _, c := range stopped {
		if err := a.Sys.Systemctl("disable", c); err == nil {
			a.info(proto.EvInfo, s.Name, "已取消 %s 的开机自启", c)
		}
	}
	return "OK", ""
}

// installUnit 写单元文件（内容变化时）并 daemon-reload。
func (a *Agent) installUnit(app, unit string, text []byte) error {
	p := a.unitPath(unit)
	if sameFile(p, text) {
		return nil
	}
	if err := writeFileAtomic(p, text, 0o644); err != nil {
		return fmt.Errorf("写单元文件失败：%v", err)
	}
	if err := a.Sys.Systemctl("daemon-reload"); err != nil {
		return err
	}
	a.info(proto.EvUnit, app, "已更新 %s", p)
	return nil
}

func (a *Agent) saveLegacy(unit string, text []byte, app string) error {
	dir := a.legacyDir(app)
	p := filepath.Join(dir, unit)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	a.info(proto.EvUnit, app, "接管 %s，原单元文件保存在 %s", unit, p)
	return os.WriteFile(p, text, 0o644)
}

// restoreService 回到部署前：切回上一版本（或删掉 current）、还原单元文件、按部署前的状态拉起。
// 失败版本 failed 这次的日志归档到它名下，便于排查。
func (a *Agent) restoreService(s *proto.AppSpec, failed, prev string, wasActive bool, before []byte, stopped []string) error {
	a.Sys.Systemctl("stop", s.Unit)
	a.rotateLogs(s, failed)
	if prev != "" {
		if err := a.activate(s, prev); err != nil {
			return err
		}
		a.markRun(s, prev)
		if err := a.syncNginxConf(s, prev); err != nil {
			return err
		}
	} else {
		if err := a.deactivate(s.Name); err != nil {
			return err
		}
		a.markRun(s, "")
	}
	if before != nil && strings.HasPrefix(string(before), ManagedMarker) {
		before = []byte(rewriteOldPaths(a.appDir(s.Name), filepath.Join(a.appMetaDir(s.Name), "nginx.conf"), string(before), servedStatic(s)))
		a.ensureDataFor(s, string(before))
	}
	if before == nil {
		a.Sys.Systemctl("disable", s.Unit)
		os.Remove(a.unitPath(s.Unit))
		a.Sys.Systemctl("daemon-reload")
	} else if err := a.installUnit(s.Name, s.Unit, before); err != nil {
		return err
	}
	if wasActive && before != nil {
		if err := a.Sys.Systemctl("restart", s.Unit); err != nil {
			return err
		}
		if err := a.waitReady(s, s.Unit); err != nil {
			a.diag(s, s.Unit)
			return err
		}
	}
	for _, c := range stopped {
		a.info(proto.EvStart, s.Name, "恢复 %s", c)
		if err := a.Sys.Systemctl("start", c); err != nil {
			return err
		}
		if err := a.waitReady(s, c); err != nil {
			a.diag(s, c)
			return err
		}
	}
	switch {
	case prev != "" && wasActive:
		a.info(proto.EvRollback, s.Name, "已回滚到 %s 并恢复运行", prev)
	case prev != "":
		a.info(proto.EvRollback, s.Name, "已切回 %s；部署前未运行，保持停止", prev)
	case len(stopped) > 0 || (wasActive && before != nil):
		a.info(proto.EvRollback, s.Name, "已恢复到接管前的服务")
	default:
		a.info(proto.EvRollback, s.Name, "首次部署失败，保持停止")
	}
	return nil
}

// switchStatic 切换 current、维护额外链接、reload、可选健康检查；失败切回。
func (a *Agent) switchStatic(s *proto.AppSpec, id, prev string) (string, string) {
	apply := func(target string) error {
		if target == "" {
			if err := a.deactivate(s.Name); err != nil {
				return err
			}
		} else if err := a.activate(s, target); err != nil {
			return err
		}
		if s.Link != "" && target != "" {
			want := a.runningDir(s.Name)
			if got, err := a.Sys.ReadLink(s.Link); err != nil || got != want {
				if err := os.MkdirAll(filepath.Dir(s.Link), 0o755); err != nil {
					return err
				}
				if err := a.Sys.SwitchLink(s.Link, want); err != nil {
					return err
				}
				a.info(proto.EvSwitch, s.Name, "%s → %s", s.Link, want)
			}
		}
		if s.Reload != "" {
			out, err := a.Sys.Shell(s.Reload, 60*time.Second)
			if err != nil {
				for _, l := range lastLines(out, 8) {
					a.fail(proto.EvDiag, s.Name, "%s", l)
				}
				return fmt.Errorf("reload 失败：%v", err)
			}
			a.info(proto.EvReload, s.Name, "%s", s.Reload)
		}
		if s.NginxInstance != "" {
			return a.publishNginxSite(s, target)
		}
		return nil
	}
	// 以前自带 nginx、现在改用 nginx 实例：先停掉原来的服务（它占着同一个端口），成功后删掉，失败时恢复
	var old, stopped []string
	if s.NginxInstance != "" {
		old = a.standaloneUnits(s)
		for _, u := range old {
			if isRunning(a.Sys.UnitProps(u)) {
				a.info(proto.EvStop, s.Name, "停止原来自带 nginx 的服务 %s（改用 nginx 实例 %s）", u, s.NginxInstance)
				if err := a.Sys.Systemctl("stop", u); err != nil {
					a.warn(proto.EvWarn, s.Name, "%v", err)
				}
				stopped = append(stopped, u)
			}
		}
	}
	err := apply(id)
	if err == nil {
		a.info(proto.EvSwitch, s.Name, "running → %s", id)
		if s.Health.TCP != "" || s.Health.HTTP != "" {
			err = a.waitProbe(s)
		}
	}
	if err == nil && s.Hooks != nil {
		ph := hookPhase{name: "afterSwitch", label: "切换后的命令", dir: a.runningDir(s.Name)}
		err = a.runHooks(s, ph, s.Hooks.AfterSwitch, id, prev)
	}
	if err == nil {
		a.retireUnits(s, old)
		return "OK", ""
	}
	a.fail(proto.EvFail, s.Name, "%v", err)
	a.markFailed(s.Name, id)
	if a.req.NoRollback {
		return "FAILED", err.Error()
	}
	if len(old) > 0 {
		// 旧版本是自带 nginx 运行的：撤下实例里的站点，换回旧版本，再启动原来的服务
		a.publishNginxSite(s, "")
		if prev != "" {
			if rerr := a.activate(s, prev); rerr != nil {
				a.fail(proto.EvRollback, s.Name, "切回 %s 失败：%v", prev, rerr)
				return "ROLLBACK_FAILED", err.Error()
			}
		}
		for _, u := range stopped {
			if rerr := a.Sys.Systemctl("start", u); rerr != nil {
				a.fail(proto.EvRollback, s.Name, "启动原来的服务 %s 失败：%v", u, rerr)
				return "ROLLBACK_FAILED", err.Error()
			}
		}
		a.info(proto.EvRollback, s.Name, "已切回 %s，仍由原来自带的 nginx 运行", orNone(prev))
		return "ROLLED_BACK", err.Error()
	}
	if rerr := apply(prev); rerr != nil {
		a.fail(proto.EvRollback, s.Name, "切回 %s 失败：%v", orNone(prev), rerr)
		return "ROLLBACK_FAILED", err.Error()
	}
	a.info(proto.EvRollback, s.Name, "已切回 %s", orNone(prev))
	return "ROLLED_BACK", err.Error()
}

func lastLines(s string, n int) []string {
	lines := splitLines(strings.TrimSpace(s))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// ---------------------------------------------------------------- 就绪与诊断

// waitReady 等健康检查通过（并稳定 StableSec 秒）。进程退出（failed / inactive / auto-restart / 主进程号变化）立即判失败。
func (a *Agent) waitReady(s *proto.AppSpec, unit string) error {
	h := s.Health
	timeout := time.Duration(h.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	stable := time.Duration(h.StableSec) * time.Second
	probe := h.TCP != "" || h.HTTP != ""
	start := a.Sys.Now()
	if a.waitStart == nil {
		a.waitStart = map[string]time.Time{}
	}
	a.waitStart[unit] = start
	deadline := start.Add(timeout)
	what := h.TCP
	if h.HTTP != "" {
		what = h.HTTP
	}
	if what == "" {
		what = fmt.Sprintf("稳定运行 %ds", h.StableSec)
	}
	a.info(proto.EvWait, s.Name, "%s（最多 %ds）", what, int(timeout.Seconds()))
	pid0 := 0
	var okSince time.Time
	var lastErr error
	for {
		now := a.Sys.Now()
		p := a.Sys.UnitProps(unit)
		crashed := p.Active == "failed" || p.Active == "inactive" || p.Sub == "auto-restart" ||
			(pid0 != 0 && p.PID != pid0)
		if crashed {
			a.fail(proto.EvCrash, s.Name, "进程已退出（%s/%s）", p.Active, p.Sub)
			return fmt.Errorf("%s 启动后退出", s.Name)
		}
		if pid0 == 0 && p.PID != 0 {
			pid0 = p.PID
		}
		healthy := p.Active == "active"
		if healthy && probe {
			if err := a.Sys.Probe(h); err != nil {
				healthy, lastErr = false, err
			}
		}
		if healthy {
			if okSince.IsZero() {
				okSince = now
			}
			if now.Sub(okSince) >= stable {
				a.readySec = max(int(now.Sub(start).Seconds()), 1) // 1 秒内就绪的也记下（0 是没有记录）
				a.errorLines = countErrorLines(a.Sys.Journal(unit, 5000, start.Add(-3*time.Second)))
				a.info(proto.EvReady, s.Name, "%s（%ds）", what, int(now.Sub(start).Seconds()))
				return nil
			}
		} else {
			okSince = time.Time{}
		}
		if !now.Before(deadline) {
			msg := fmt.Sprintf("等待 %ds 仍未就绪", int(timeout.Seconds()))
			if lastErr != nil {
				msg += "：" + lastErr.Error()
			}
			a.fail(proto.EvFail, s.Name, "%s", msg)
			return errors.New(msg)
		}
		a.Sys.Sleep(2 * time.Second)
	}
}

// waitProbe 只做健康检查（static）。
func (a *Agent) waitProbe(s *proto.AppSpec) error {
	timeout := time.Duration(s.Health.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := a.Sys.Now().Add(timeout)
	for {
		err := a.Sys.Probe(s.Health)
		if err == nil {
			a.info(proto.EvReady, s.Name, "健康检查通过")
			return nil
		}
		if !a.Sys.Now().Before(deadline) {
			return fmt.Errorf("健康检查未通过：%v", err)
		}
		a.Sys.Sleep(2 * time.Second)
	}
}

// diag 输出启动失败原因：配置的日志文件（去掉堆栈）最后几行；没有就看 journald。
func (a *Agent) diag(s *proto.AppSpec, unit string) {
	var lines []string
	for _, p := range s.DiagLogs {
		if hasGlob(p) { // 按天命名的日志：看最近修改的那个
			if p = newestGlob(p); p == "" {
				continue
			}
		}
		ls, err := tailFile(p, 200)
		if err != nil {
			continue
		}
		for _, l := range ls {
			if !isStackLine(l) && strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) > 0 {
			break
		}
	}
	if len(lines) == 0 {
		since := a.Sys.Now().Add(-time.Minute)
		if t, ok := a.waitStart[unit]; ok {
			since = t.Add(-3 * time.Second) // 启动命令在开始等待前一刻发出
		}
		for _, l := range a.Sys.Journal(unit, 60, since) {
			if !isStackLine(l) && strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
	}
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	lines = append(lines, exitHints(lines, s.UnitText)...)
	lines = append(lines, a.inUseHints(s, lines, unit)...)
	for _, l := range lines {
		if len(l) > 400 {
			l = l[:400] + "…"
		}
		a.fail(proto.EvDiag, s.Name, "%s", l)
	}
	hint := "journalctl -u " + unit
	if len(s.DiagLogs) > 0 {
		hint = strings.Join(s.DiagLogs, " ") + " ；" + hint
	}
	a.info(proto.EvDiag, s.Name, "完整日志：%s", hint)
}

// exitHints 日志里常见的启动失败退出码：用直白的话说明，找得到的话直接指出缺的文件。
func exitHints(lines []string, unit string) []string {
	has := func(s string) bool {
		for _, l := range lines {
			if strings.Contains(l, s) {
				return true
			}
		}
		return false
	}
	var out []string
	switch {
	case has("226/NAMESPACE"):
		out = append(out, "（226/NAMESPACE：systemd 准备服务的隔离环境失败，常见原因是单元里 ReadWritePaths 等写的目录在服务器上不存在；"+
			"建好目录，或改配置 unitExtra 里的路径，不一定存在的路径前面加 -）")
	case has("status=127"):
		out = append(out, "（status=127：找不到要执行的命令。启动命令里的脚本，或脚本里要执行的程序不在服务器上）")
	case has("203/EXEC"):
		out = append(out, "（203/EXEC：启动命令的程序不存在或不能执行）")
	case has("200/CHDIR"):
		out = append(out, "（200/CHDIR：进不去工作目录：目录不存在，或运行用户没有权限）")
	case has("217/USER"):
		out = append(out, "（217/USER：运行用户在服务器上不存在）")
	case has("Failed to load environment files"):
		out = append(out, "（环境变量文件不存在：检查配置里的 service.envFile）")
	}
	if has("status=127") || has("203/EXEC") {
		for _, v := range append(unitValues(unit, "ExecStartPre"), unitValues(unit, "ExecStart")...) {
			for _, n := range execNeeds("ExecStart", v) {
				if n.reason != "program" && n.must == "" {
					continue
				}
				if _, err := os.Stat(n.path); err != nil {
					out = append(out, "服务器上没有 "+n.path)
				}
			}
		}
	}
	return out
}

// pruneReleases 保留最近 KeepReleases 个版本；当前版本与上一个可回滚版本永远保留。
func (a *Agent) pruneReleases(s *proto.AppSpec) {
	cur := a.current(s.Name)
	keep := map[string]bool{cur: true, a.previousGood(s.Name, cur): true}
	ids := a.listReleases(s.Name)
	var removed []string
	for i, id := range ids {
		if i < s.KeepReleases || keep[id] {
			continue
		}
		if os.RemoveAll(a.releaseDir(s.Name, id)) == nil {
			os.RemoveAll(a.verMetaDir(s.Name, id))
			removed = append(removed, id)
		}
	}
	ents, _ := os.ReadDir(a.versionsDir(s.Name))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			os.RemoveAll(filepath.Join(a.versionsDir(s.Name), e.Name()))
		}
	}
	if len(removed) > 0 {
		a.info(proto.EvPrune, s.Name, "清理旧版本 %s", strings.Join(removed, ", "))
	}
}

// placeFiles 把附带文件解到版本目录 dir 里（和程序放在一起）：归 root、组为运行用户的组，程序能读、不能改
// （目录 0750，文件 0640，可执行的 0750）。不能占用程序文件名和 Ferry 用的名字，已有的文件不覆盖。
func (a *Agent) placeFiles(src, dir, program string, gid int) (map[string]string, error) {
	exec := map[string]bool{}
	sums := map[string]string{}
	var tops []string
	err := extractTar(src, dir, func(name string, h *tar.Header) error {
		if err := proto.CheckExtraFile(name, program); err != nil {
			return err
		}
		top, _, _ := strings.Cut(name, "/")
		if !slices.Contains(tops, top) {
			tops = append(tops, top)
		}
		if h.Typeflag == tar.TypeReg && h.Mode&0o100 != 0 {
			exec[name] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, top := range tops {
		err := filepath.WalkDir(filepath.Join(dir, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			mode := os.FileMode(0o640)
			if rel, _ := filepath.Rel(dir, p); d.IsDir() || exec[filepath.ToSlash(rel)] {
				mode = 0o750
			}
			if err := os.Chmod(p, mode); err != nil {
				return err
			}
			if !d.IsDir() {
				if sum, _, err := zipx.FileSHA256(p); err == nil {
					rel, _ := filepath.Rel(dir, p)
					sums[filepath.ToSlash(rel)] = sum
				}
			}
			return a.Sys.Lchown(p, 0, gid)
		})
		if err != nil {
			return nil, err
		}
	}
	return sums, nil
}

// ---------------------------------------------------------------- tar.gz

// extractTarGz 安全解压：只接受普通文件和目录，拒绝绝对路径、..、链接与设备文件。
func extractTarGz(src, dst string) error { return extractTar(src, dst, nil) }

// extractTar 同 extractTarGz；check 不为空时先用它检查每个条目（name 为整理过的相对路径，用 /）。
func extractTar(src, dst string, check func(name string, h *tar.Header) error) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("压缩包里有越界路径：%s", h.Name)
		}
		p := filepath.Join(dst, filepath.FromSlash(name))
		if !within(dst, p) {
			return fmt.Errorf("压缩包里有越界路径：%s", h.Name)
		}
		if check != nil {
			if err := check(name, h); err != nil {
				return err
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("压缩包里有不支持的条目类型（%c）：%s", h.Typeflag, h.Name)
		}
	}
}

// lastReadySec 上一次成功部署这个应用时，从启动到就绪用了几秒（部署历史里最近的一条；没有记录为 0）。
func (a *Agent) lastReadySec(app string) int {
	lines, _ := tailFile(filepath.Join(a.meta(), "history.jsonl"), 400)
	for i := len(lines) - 1; i >= 0; i-- {
		var e proto.HistoryEntry
		if json.Unmarshal([]byte(lines[i]), &e) == nil && e.App == app && e.Action == "deploy" && e.Result == "OK" && e.ReadySec > 0 {
			return e.ReadySec
		}
	}
	return 0
}

// compareReady 新版本启动得比上一次慢很多时提醒（慢一倍以上且多了 10 秒以上：多半是启动时多做了事、连不上依赖在重试）。
func (a *Agent) compareReady(app string, now, prev int) {
	switch {
	case prev == 0:
		a.info(proto.EvReady, app, "这次从启动到就绪用了 %d 秒", now)
	case now >= prev*2 && now-prev >= 10:
		a.warn(proto.EvReady, app, "这次启动用了 %d 秒，上次只要 %d 秒：看看日志里有没有在重试连接、初始化变慢", now, prev)
	default:
		a.info(proto.EvReady, app, "这次从启动到就绪用了 %d 秒（上次 %d 秒）", now, prev)
	}
}

// reErrorLine 服务输出里算作错误的行：ERROR 级别的日志、异常（Java 的 Exception、Go 的 panic）。
var reErrorLine = regexp.MustCompile(`(?i)\b(error|fatal|panic)\b|exception|错误|异常`)

// countErrorLines 启动期间的输出里有几行错误（堆栈的 at ... 行不算，一个异常只算一次）。
func countErrorLines(lines []string) int {
	n := 0
	for _, l := range lines {
		if !isStackLine(l) && reErrorLine.MatchString(l) {
			n++
		}
	}
	return n
}

// lastErrorLines 上一次成功部署这个应用时，启动期间的错误行数（没有记录为 -1）。
func (a *Agent) lastErrorLines(app string) int {
	lines, _ := tailFile(filepath.Join(a.meta(), "history.jsonl"), 400)
	for i := len(lines) - 1; i >= 0; i-- {
		var e proto.HistoryEntry
		if json.Unmarshal([]byte(lines[i]), &e) == nil && e.App == app && e.Action == "deploy" && e.Result == "OK" && e.ErrorLines != nil {
			return *e.ErrorLines
		}
	}
	return -1
}

// compareErrors 启动期间的错误比上一次多了时提醒（多 5 行以上，或者以前没有、这次有）。
func (a *Agent) compareErrors(app string, now, prev int) {
	switch {
	case now == 0:
		return
	case prev < 0:
		a.warn(proto.EvReady, app, "启动期间服务输出里有 %d 行错误（error、exception），看看日志", now)
	case now > prev+5 || (prev == 0 && now > 0):
		a.warn(proto.EvReady, app, "启动期间有 %d 行错误，上次是 %d 行：看看日志里多出来的是什么", now, prev)
	default:
		a.info(proto.EvReady, app, "启动期间有 %d 行错误（上次 %d 行）", now, prev)
	}
}
