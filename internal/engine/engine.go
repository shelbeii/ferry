// Package engine 在本机编排一次部署：连接 → 预检 → 构建 → 检查产物 → 上传（增量）→ 服务器逐个切换 → 汇总。
package engine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/settings"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/artifact"
	"github.com/captain/ferry/internal/build"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/gitinfo"
	"github.com/captain/ferry/internal/jdk"
	"github.com/captain/ferry/internal/nginxrt"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/zipx"
)

// Reporter 过程输出（终端或 JSON）。
type Reporter interface {
	Step(format string, args ...any)
	Info(format string, args ...any)
	Warn(format string, args ...any)
	Error(format string, args ...any)
	Event(target string, ev proto.Event)
	Progress(label string, done, total int64)
	BuildOutput() io.Writer
	Confirm(prompt string) bool
	// Secret 询问密码（不回显）；不能交互时返回 false
	Secret(prompt string) (string, bool)
}

// Engine 一个配置文件上的操作。
type Engine struct {
	File             *config.File
	Rep              Reporter
	Yes              bool // 跳过确认
	AcceptNewHostKey bool
	Settings         settings.Settings
	// Pool 连接池（ferry serve 里）；为空时每次操作新建连接、用完关闭
	Pool *Pool
	// Ctx 本次请求的上下文：取消时中断正在执行的远程命令、上传与构建
	Ctx context.Context
	// Getenv 读环境变量（ferry serve 里每个请求带自己的环境）；为空时用 os.Getenv
	Getenv func(string) string
	// Background 桌面版的后台自动刷新：只用连接池里已有的连接（没有就返回 ErrNotConnected，不新建），
	// 用过也不算「使用」，不推迟空闲断开
	Background bool

	projectDir string
}

func (e *Engine) ctx() context.Context {
	if e.Ctx != nil {
		return e.Ctx
	}
	return context.Background()
}

func (e *Engine) getenv(k string) string {
	if e.Getenv != nil {
		return e.Getenv(k)
	}
	return os.Getenv(k)
}

// ProjectDir 本项目在数据目录下的目录（构建日志、本机部署历史）。第一次用到时把旧版本放在配置文件旁 .ferry/ 的数据搬过来。
func (e *Engine) ProjectDir() string {
	if e.projectDir == "" {
		if msg, err := appdir.MigrateProject(e.File.Project, e.File.Path); err != nil {
			e.Rep.Warn("迁移旧数据失败：%v", err)
		} else if msg != "" {
			e.Rep.Info("%s", msg)
		}
		e.projectDir = appdir.ProjectDir(e.File.Project, e.File.Path)
	}
	return e.projectDir
}

// DeployOptions 部署选项。
type DeployOptions struct {
	SkipBuild  bool
	Full       bool // 不用增量上传
	NoRollback bool
	// KeepPerms 不自动修正权限（默认会把运行用户访问不了的目录 / 文件、不安全的部署目录改好）
	KeepPerms bool
}

// DeployReport 部署结果。
type DeployReport struct {
	Target  string             `json:"target"`
	Release string             `json:"release"`
	Git     gitinfo.Info       `json:"git"`
	Apps    []proto.AppOutcome `json:"apps"`
	Upload  int64              `json:"upload"`  // 实际上传字节
	Total   int64              `json:"total"`   // 全量大小
	Elapsed time.Duration      `json:"elapsed"` // 纳秒
}

// ErrCanceled 用户取消。
var ErrCanceled = errors.New("已取消")

// ErrNotConnected 后台自动刷新时没有可用的连接（空闲断开了、手动断开了）。
var ErrNotConnected = errors.New("没有连着服务器，不自动连接")

// item 一个要部署的应用及其产物。
type item struct {
	app       *config.App
	spec      proto.AppSpec
	local     string // 本机产物路径（static 为目录）
	file      string // 要上传的文件（static 为打好的 tar.gz）
	sha       string
	size      int64
	notBefore time.Time
	info      *artifact.Info
	// 附带文件：本机路径与放到的位置；打好的 tar.gz 与它的 sha256、大小
	files     []artifact.ExtraFile
	filesTar  string
	filesSHA  string
	filesSize int64
	// site 静态站点各文件的 sha256（和服务器上的版本比，只传变化的）
	site []proto.SiteFile
}

// targetPlan 一个目标上的部署。
type targetPlan struct {
	t     *config.Target
	apps  []*config.App
	items []*item
	s     *session
	pf    *proto.Preflight
	// needRuntimes 服务器上还没有、部署时要安装的 JDK 大版本
	needRuntimes []int
	// needPins 锁定版本、服务器上还没有它自己链接的 JDK（部署时建链接，没装过的先上传）
	needPins []pinNeed
	// needNginx 部署根目录下还没有、部署时要安装的 nginx（链接名：nginx、nginx-1.30）
	needNginx []string
}

// Deploy 部署到一个或多个目标。先连接并预检全部目标、确认一次；然后逐个目标：
// 构建（与前面目标完全相同的构建不重复执行）→ 检查产物 → 上传 → 切换。某个目标失败后不再部署后面的目标。
func (e *Engine) Deploy(targets []string, names []string, o DeployOptions) ([]*DeployReport, error) {
	reports, err := e.deploy(targets, names, o)
	e.notifyDeploy(reports, err)
	return reports, err
}

func (e *Engine) deploy(targets []string, names []string, o DeployOptions) ([]*DeployReport, error) {
	f := e.File
	if len(targets) == 0 {
		return nil, errors.New("没有指定目标")
	}
	var plans []*targetPlan
	seen := map[string]bool{}
	for _, tn := range targets {
		t, err := f.Target(tn)
		if err != nil {
			return nil, err
		}
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		apps, err := f.Select(t, names)
		if err != nil {
			return nil, err
		}
		p := &targetPlan{t: t, apps: apps}
		for _, a := range apps {
			spec, err := Spec(f, t, a)
			if err != nil {
				return nil, err
			}
			if spec.TLS != nil && spec.TLS.DNS != "" {
				// DNS 验证的密钥：服务器上存一份，每天续期时用
				if spec.TLS.DNSCred = e.DNSCred(spec.TLS.DNS); spec.TLS.DNSCred == nil {
					e.Rep.Warn("%s：这台电脑上没有 %s 的 DNS 密钥，用服务器上以前存的（没有的话证书申请不下来，在设置 → 部署里填上）", a.Name, spec.TLS.DNS)
				}
			}
			local, err := LocalArtifact(f, t, a)
			if err != nil {
				return nil, err
			}
			files, err := LocalFiles(f, t, a)
			if err != nil {
				return nil, err
			}
			p.items = append(p.items, &item{app: a, spec: spec, local: local, files: files})
		}
		plans = append(plans, p)
	}
	git, fallback := deployGit(f, plans)
	if fallback {
		e.Rep.Info("配置文件所在目录不是 git 仓库，提交信息取自 %s（%s %s）", git.Top, git.Branch, git.Short())
	}
	if git.Dirty {
		if f.Git.RequireClean {
			return nil, fmt.Errorf("工作区有未提交的修改（git.requireClean）：%s", strings.Join(git.Files, ", "))
		}
		e.Rep.Warn("工作区有未提交的修改，会记入部署记录（%s）", strings.Join(git.Files, ", "))
	}
	if note := localPathVarsNote(f, targets); note != "" {
		e.Rep.Warn("%s", note)
	}

	// 先连接、预检全部目标：任何一台有问题都不开始构建
	defer func() {
		for _, p := range plans {
			if p.s != nil {
				p.s.close()
			}
		}
	}()
	for _, p := range plans {
		s, err := e.connect(p.t)
		if err != nil {
			return nil, err
		}
		p.s = s
		mode := ""
		if !o.KeepPerms && !e.Settings.Deploy.KeepPerms {
			mode = agentFixPlan
		}
		pf, err := e.preflight(s, p.items, mode)
		if err != nil {
			return nil, err
		}
		p.pf = pf
		e.planRuntimes(p)
		e.planNginx(p)
	}

	// 要上线的改动：和线上运行的版本比（读不到不影响部署）
	dir := git.Top
	if dir == "" {
		dir = f.Dir
	}
	for _, p := range plans {
		specs := make([]proto.AppSpec, 0, len(p.items))
		for _, it := range p.items {
			specs = append(specs, it.spec)
		}
		var rels []proto.AppReleases
		if err := p.s.callData("releases", &proto.Request{Apps: specs, Detail: true}, &rels); err == nil {
			if len(plans) > 1 {
				e.Rep.Step("%s 要上线的改动", p.t.Name)
			} else {
				e.Rep.Step("要上线的改动")
			}
			local := map[string]localApp{}
			for _, it := range p.items {
				la := localApp{unit: it.spec.UnitText, zd: it.spec.ZD}
				if len(it.files) > 0 {
					la.files = artifact.FileSums(it.files)
				}
				local[it.app.Name] = la
			}
			e.ReportChanges(p.t.Name, changesFrom(dir, git, rels, local))
		}
	}

	id := releaseID()
	confirm := false
	var tnames, prod []string
	for _, p := range plans {
		if err := e.checkDeployer(p.t, "部署"); err != nil {
			return nil, err
		}
		confirm = confirm || p.t.NeedConfirm()
		tnames = append(tnames, p.t.Name)
		if p.t.Production {
			prod = append(prod, p.t.Name)
		}
	}
	if (confirm || len(prod) > 0) && !e.Yes {
		var list []string
		for _, a := range plans[0].apps {
			list = append(list, a.Name)
		}
		q := fmt.Sprintf("部署 %s 到 %s（版本 %s，提交 %s）？", strings.Join(list, ", "), strings.Join(tnames, "、"), id, git.Short())
		if !e.confirmProduction(q, prod) {
			return nil, ErrCanceled
		}
	}

	// 确认之后、构建之前：真正修正权限并重新检查
	for _, p := range plans {
		if len(p.pf.Fixable) == 0 {
			continue
		}
		e.Rep.Step("自动修正 %s 上的问题（权限、运行用户）", p.t.Name)
		pf, err := e.preflight(p.s, p.items, agentFixApply)
		if err != nil {
			return nil, err
		}
		p.pf = pf
	}

	// 服务器上还没有的运行环境：本机没有就下载，再上传安装
	for _, p := range plans {
		if err := e.installRuntimes(p); err != nil {
			return nil, fmt.Errorf("%s：%w", p.t.Name, err)
		}
		if err := e.installNginx(p); err != nil {
			return nil, fmt.Errorf("%s：%w", p.t.Name, err)
		}
	}

	tmp, err := os.MkdirTemp(appdir.TempDir(), "deploy-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	built := map[string]time.Time{}
	// 1. 依次构建、检查产物、打包（各目标的变量可能不同；同样的构建只执行一次）
	for _, p := range plans {
		if err := e.readyTarget(p, filepath.Join(tmp, p.t.Name), o, built); err != nil {
			return nil, fmt.Errorf("%s：%w", p.t.Name, err)
		}
	}
	// 2. 同时上传到各目标；有一台没传成功就都不切换
	ups := e.uploadAll(plans, id, git, tmp, !o.Full)
	var failed []string
	for i, u := range ups {
		if u.err != nil {
			failed = append(failed, plans[i].t.Name)
		}
	}
	if len(failed) > 0 {
		e.dropStages(plans, ups)
		for i, u := range ups {
			if u.err != nil {
				return nil, fmt.Errorf("%s 上传失败，各服务器都没有改动：%w", plans[i].t.Name, u.err)
			}
		}
	}
	// 3. 依次切换：一台失败就停，后面的不切换（已上传的文件删掉）
	var reports []*DeployReport
	for i, p := range plans {
		r, err := e.switchTarget(p, ups[i], id, git, filepath.Join(tmp, p.t.Name), o)
		if r != nil {
			reports = append(reports, r)
		}
		if err != nil {
			if rest := tnames[i+1:]; len(rest) > 0 {
				e.Rep.Error("%s 失败，未部署：%s", p.t.Name, strings.Join(rest, "、"))
				e.dropStages(plans[i+1:], ups[i+1:])
			}
			return reports, fmt.Errorf("%s：%w", p.t.Name, err)
		}
	}
	return reports, nil
}

// readyTarget 一个目标上：构建、检查产物、打包（还没连服务器做改动）。
func (e *Engine) readyTarget(p *targetPlan, tmp string, o DeployOptions, built map[string]time.Time) error {
	t, s, items := p.t, p.s, p.items
	if len(e.File.Targets) > 1 {
		e.Rep.Step("目标 %s", t.Name)
	}
	if !o.SkipBuild {
		if err := e.runBuilds(t, p.apps, items, built); err != nil {
			return err
		}
	}
	e.Rep.Step("检查产物")
	for _, it := range items {
		info, err := artifact.Check(it.app, it.local, it.notBefore, s.arch)
		if err != nil {
			return err
		}
		it.info = info
		if err := e.checkJava(p, it); err != nil {
			return err
		}
		note := ""
		if len(info.Notes) > 0 {
			note = "，" + strings.Join(info.Notes, "，")
		}
		if info.Files > 0 {
			e.Rep.Info("%-12s %s（%d 个文件，%s）", it.app.Name, it.local, info.Files, humanBytes(info.Size))
		} else {
			e.Rep.Info("%-12s %s（%s%s）", it.app.Name, filepath.Base(it.local), humanBytes(info.Size), note)
		}
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	var err error
	for _, it := range items {
		it.file = it.local
		if proto.DirService(it.app.Type) {
			it.file = filepath.Join(tmp, it.app.Name+".tar.gz")
			exec := map[string]bool{}
			for _, x := range it.spec.Executables {
				exec[x] = true
			}
			if err := artifact.PackDirExclude(it.local, it.file, artifact.Excludes(it.app), exec); err != nil {
				return fmt.Errorf("%s 打包失败：%w", it.app.Name, err)
			}
		}
		if it.app.Type == proto.TypeStatic {
			it.file = filepath.Join(tmp, it.app.Name+".tar.gz")
			if err := artifact.PackDir(it.local, it.file); err != nil {
				return fmt.Errorf("%s 打包失败：%w", it.app.Name, err)
			}
			if it.site, err = artifact.SiteManifest(it.local); err != nil {
				return fmt.Errorf("%s 读取站点文件失败：%w", it.app.Name, err)
			}
		}
		if it.sha, it.size, err = zipx.FileSHA256(it.file); err != nil {
			return err
		}
		if err := e.packFiles(it, tmp); err != nil {
			return err
		}
	}
	return nil
}

// switchTarget 一个目标上：让 agent 部署已上传的文件（切换版本、启动、等待就绪）；增量拼装或校验不过时全量上传重试一次。
func (e *Engine) switchTarget(p *targetPlan, u uploaded, id string, git gitinfo.Info, tmp string, o DeployOptions) (*DeployReport, error) {
	started := u.started
	t, s, items := p.t, p.s, p.items
	rep := &DeployReport{Target: t.Name, Release: id, Git: git}
	delta := u.delta
	for attempt := 0; ; attempt++ {
		req, up, total, err := u.req, u.up, u.total, error(nil)
		if attempt > 0 {
			if req, up, total, err = e.upload(s, items, id, git, tmp, delta); err != nil {
				return nil, err
			}
		}
		rep.Upload += up
		rep.Total = total
		req.NoRollback = o.NoRollback
		if attempt == 0 {
			if err := e.backupBefore(s); err != nil {
				return nil, err
			}
		}
		e.Rep.Step("部署到 %s", t.Name)
		r, err := s.call("deploy", req)
		if err != nil {
			return nil, err
		}
		if r.RC == proto.CodeVerify && delta && attempt == 0 {
			e.Rep.Warn("增量拼装或校验未通过（服务器未改动），改为全量上传重试")
			delta = false
			continue
		}
		var out proto.DeployOutcome
		if len(r.Data) > 0 {
			json.Unmarshal(r.Data, &out)
		}
		rep.Apps = out.Apps
		rep.Elapsed = time.Since(started)
		e.localHistory(rep)
		switch r.RC {
		case proto.CodeOK:
			return rep, nil
		case proto.CodeLocked:
			return rep, errors.New("另一个部署/回滚/启停正在进行，稍后再试")
		case proto.CodeVerify:
			return rep, fmt.Errorf("上传文件校验未通过，未改动任何应用：%s", r.Msg)
		default:
			return rep, fmt.Errorf("部署失败：%s", r.Msg)
		}
	}
}

// packFiles 把应用的附带文件打成 tar.gz（放进版本目录、和程序放在一起）。
func (e *Engine) packFiles(it *item, tmp string) error {
	if len(it.files) == 0 {
		return nil
	}
	it.filesTar = filepath.Join(tmp, it.app.Name+".files.tar.gz")
	n, size, err := artifact.PackFiles(it.files, it.filesTar)
	if err != nil {
		return fmt.Errorf("%s 的附带文件：%w", it.app.Name, err)
	}
	if it.filesSHA, it.filesSize, err = zipx.FileSHA256(it.filesTar); err != nil {
		return err
	}
	e.Rep.Info("%-12s 附带文件 %s（%d 个文件，%s）", it.app.Name, strings.Join(extraNames(it.files), "、"), n, humanBytes(size))
	return nil
}

func extraNames(files []artifact.ExtraFile) []string {
	names := make([]string, len(files))
	for i, x := range files {
		names[i] = x.To
	}
	return names
}

func releaseID() string {
	b := make([]byte, 4)
	rand.Read(b)
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return time.Now().Format("20060102-150405") + "-" + string(b)
}

const (
	agentFixPlan  = "plan"
	agentFixApply = "apply"
)

// preflight 预检。fix 为 plan 时列出部署时会自动修正的权限；apply 时修正后重新检查。
func (e *Engine) preflight(s *session, items []*item, fix string) (*proto.Preflight, error) {
	specs := make([]proto.AppSpec, len(items))
	for i, it := range items {
		specs[i] = it.spec
	}
	var p proto.Preflight
	if err := s.callData("preflight", &proto.Request{Apps: specs, FixPerms: fix}, &p); err != nil {
		return nil, err
	}
	if fix != agentFixApply {
		e.Rep.Info("%s，%s，systemd %d", p.OS, p.Arch, p.Systemd)
		for _, w := range p.Warnings {
			e.Rep.Warn("%s", w)
		}
		for _, f := range p.Fixable {
			e.Rep.Info("部署时会自动修正：%s", f)
		}
	}
	for _, f := range p.Fixed {
		e.Rep.Info("已修正：%s", f)
	}
	if len(p.Problems) > 0 {
		for _, pr := range withVarHints(e.File, s.t, items, p.Problems) {
			e.Rep.Error("%s", pr)
		}
		what := "服务器上没有做任何改动"
		if len(p.Fixed) > 0 {
			what = fmt.Sprintf("已自动修正了 %d 处权限，其余的需要手动处理", len(p.Fixed))
		}
		return nil, fmt.Errorf("目标 %s 预检未通过：%d 个问题（见上方说明），处理后重新部署；%s", s.t.Name, len(p.Problems), what)
	}
	return &p, nil
}

// checkJava 构建之后、上传之前：服务器上的 java 版本够不够运行这个 jar。不够时直接停下，服务器上什么都不动。
func (e *Engine) checkJava(p *targetPlan, it *item) error {
	if it.spec.Type != proto.TypeJava || it.info == nil || p.pf == nil {
		return nil
	}
	need := artifact.JavaOf(it.info.ClassMajor)
	if need > 0 && it.spec.RuntimeJava > 0 {
		if it.spec.RuntimeJava >= need {
			return nil
		}
		e.Rep.Error("%s 需要 Java %d 或更高（jar 的 class 文件版本 %d），但配置的 runtime.java 是 %d\n修复：把配置里 %s 的 service.runtime 改成 {java: %d}，Ferry 会自动提供 JDK %d",
			it.app.Name, need, it.info.ClassMajor, it.spec.RuntimeJava, it.app.Name, need, need)
		return &JavaMismatchError{App: it.app.Name, Need: need, Have: it.spec.RuntimeJava}
	}
	j, ok := p.pf.Javas[it.spec.Java]
	if need == 0 || !ok {
		return nil
	}
	if j.Major == 0 {
		e.Rep.Warn("%s：读不出服务器上 %s 的版本（%s），无法确认它能运行需要 Java %d 的 jar", it.app.Name, j.Path, j.Error, need)
		return nil
	}
	if j.Major >= need {
		return nil
	}
	var ok17 []string
	for _, c := range p.pf.JavaFound {
		if c.Major >= need {
			ok17 = append(ok17, fmt.Sprintf("%s（Java %d）", c.Path, c.Major))
		}
	}
	msg := fmt.Sprintf("%s 需要 Java %d 或更高（jar 的 class 文件版本 %d），但 %s 上的 %s 是 Java %d（%s），启动会报 UnsupportedClassVersionError",
		it.app.Name, need, it.info.ClassMajor, p.t.Name, j.Path, j.Major, j.Version)
	fix := fmt.Sprintf("改用 Ferry 提供的 JDK：在配置里 %s 的 service 下写 runtime: {java: %d}（并去掉 java），部署时自动下载并上传；或在服务器上安装 JDK %d+ 后把 service.java 改成它的路径", it.app.Name, need, need)
	if len(ok17) > 0 {
		fix = fmt.Sprintf("服务器上已有：%s，把配置里 %s 的 service.java 改成其中一个；或改用 Ferry 提供的 JDK（service.runtime: {java: %d}）", strings.Join(ok17, "、"), it.app.Name, need)
	}
	e.Rep.Error("%s\n修复：%s", msg, fix)
	return &JavaMismatchError{App: it.app.Name, Need: need, Have: j.Major}
}

// JavaMismatchError 服务器上的 java 不够新（桌面版据此提供「改用 Ferry 提供的 JDK」）。
type JavaMismatchError struct {
	App        string
	Need, Have int
}

func (e *JavaMismatchError) Error() string {
	return fmt.Sprintf("%s 的 java 版本不够（需要 %d，现在是 %d），没有上传，服务器上没有做任何改动", e.App, e.Need, e.Have)
}

// JSONDetail --json 输出的附加信息。
func (e *JavaMismatchError) JSONDetail() map[string]any {
	return map[string]any{"kind": "java-mismatch", "app": e.App, "need": e.Need, "have": e.Have}
}

// pinNeed 一个锁定版本的 JDK：版本、大版本，服务器上已经装过的话是它的 ID（只建链接，不用上传）。
type pinNeed struct {
	version string
	java    int
	have    string
}

// planRuntimes 预检之后：服务器上还没有的 JDK（部署时安装）。
func (e *Engine) planRuntimes(p *targetPlan) {
	seen := map[int]bool{}
	pinned := map[string]bool{}
	for _, it := range p.items {
		n := it.spec.RuntimeJava
		if v := it.spec.RuntimeVersion; n > 0 && v != "" {
			if !pinned[proto.JavaPinLink(v)] {
				pinned[proto.JavaPinLink(v)] = true
				e.planPin(p, it.app.Name, n, v)
			}
			continue
		}
		if n == 0 || seen[n] {
			continue
		}
		seen[n] = true
		if r, ok := p.pf.Runtimes[n]; ok {
			e.Rep.Info("%s 使用服务器上 Ferry 提供的 JDK %d（%s）", it.app.Name, n, r.ID)
			continue
		}
		p.needRuntimes = append(p.needRuntimes, n)
		arch := jdk.ArchOf(p.s.arch)
		if m := jdk.Find(e.Settings.RuntimeDir(), n, arch); m != nil {
			e.Rep.Info("部署时会把 JDK %d 上传到 %s（服务器上还没有；本机已有 %s，%s）", n, p.s.ferryDir+"/runtimes", m.Version, humanBytes(m.Size))
		} else {
			e.Rep.Info("部署时会下载 JDK %d（linux/%s）并上传到 %s（服务器上还没有，本机也没有缓存）", n, arch, p.s.ferryDir+"/runtimes")
		}
	}
}

// planPin 锁定版本的 JDK：服务器上已有它的链接就直接用；装过这个版本（比如别的项目用 java-N 装的）只建链接；
// 都没有时部署时上传（本机没有就按版本下载）。
func (e *Engine) planPin(p *targetPlan, app string, n int, version string) {
	link, arch := proto.JavaPinLink(version), jdk.ArchOf(p.s.arch)
	if id, ok := p.pf.JavaPins[link]; ok {
		e.Rep.Info("%s 使用锁定的 JDK %s（%s）", app, version, id)
		return
	}
	for _, r := range p.pf.JDKs {
		if jdk.MatchID(r.ID, version, arch) {
			e.Rep.Info("%s 锁定 JDK %s：服务器上已经装过（%s），部署时只建链接 %s", app, version, r.ID, link)
			p.needPins = append(p.needPins, pinNeed{version: version, java: n, have: r.ID})
			return
		}
	}
	p.needPins = append(p.needPins, pinNeed{version: version, java: n})
	if m := jdk.FindVersion(e.Settings.RuntimeDir(), version, arch); m != nil {
		e.Rep.Info("部署时会把 JDK %s 上传到 %s（服务器上还没有；本机已有，%s）", version, p.s.ferryDir+"/runtimes", humanBytes(m.Size))
	} else {
		e.Rep.Info("部署时会下载 JDK %s（linux/%s）并上传到 %s（服务器上还没有，本机也没有这个版本）", version, arch, p.s.ferryDir+"/runtimes")
	}
}

// planNginx 预检之后：部署根目录下还没有的 nginx（部署时安装）。
func (e *Engine) planNginx(p *targetPlan) {
	seen := map[string]bool{}
	for _, it := range p.items {
		l := it.spec.RuntimeNginx
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		if r, ok := p.pf.Nginx[l]; ok {
			e.Rep.Info("%s 使用部署目录里 Ferry 提供的 nginx %s", it.app.Name, r.Version)
			continue
		}
		p.needNginx = append(p.needNginx, l)
		series := strings.TrimPrefix(strings.TrimPrefix(l, "nginx"), "-")
		arch := nginxrt.ArchOf(p.s.arch)
		where := p.t.Root + "/runtimes"
		if m := nginxrt.Find(e.Settings.NginxDir(), series, arch); m != nil {
			e.Rep.Info("部署时会把 nginx %s 上传到 %s（服务器上还没有；本机已有，%s）", m.Version, where, humanBytes(m.Size))
		} else {
			e.Rep.Info("部署时会下载 nginx %s（linux/%s）并上传到 %s（服务器上还没有，本机也没有）", orStable(series), arch, where)
		}
	}
}

func orStable(series string) string {
	if series == "" {
		return "最新稳定版"
	}
	return series
}

// installNginx 安装部署根目录下还没有的 nginx：本机库里没有就下载，上传后由 agent 校验、放好、更新链接。
func (e *Engine) installNginx(p *targetPlan) error {
	for _, l := range p.needNginx {
		s := p.s
		series := strings.TrimPrefix(strings.TrimPrefix(l, "nginx"), "-")
		arch := nginxrt.ArchOf(s.arch)
		dir := e.Settings.NginxDir()
		m := nginxrt.Find(dir, series, arch)
		if m == nil {
			src := e.Settings.Runtime.NginxSource
			if src == "" {
				src = nginxrt.DefaultSource
			}
			e.Rep.Step("下载 nginx %s（linux/%s，%s）", orStable(series), arch, src)
			var err error
			m, err = nginxrt.Download(e.ctx(), src, series, arch, dir, func(done, total int64) { e.Rep.Progress("nginx", done, total) })
			if err != nil {
				return fmt.Errorf("下载 nginx 失败：%w（可以在设置 → 运行环境里换下载源，或手动导入下载好的 nginx）", err)
			}
			e.Rep.Info("已下载 nginx %s（%s），存放在 %s", m.Version, humanBytes(m.Size), dir)
		}
		e.Rep.Step("上传 nginx %s 到 %s 的 %s/runtimes", m.Version, p.t.Name, p.t.Root)
		var prep proto.Prepared
		if err := s.callData("prepare", &proto.Request{}, &prep); err != nil {
			return err
		}
		item := remote.UploadItem{Local: filepath.Join(dir, m.File), Remote: remote.Join(prep.Stage, m.File), Mode: 0o640, Size: m.Size}
		if err := e.uploadItems(s, []remote.UploadItem{item}, m.Size, m.Size); err != nil {
			return fmt.Errorf("上传 nginx 失败：%w", err)
		}
		var r proto.Runtime
		spec := &proto.RuntimeSpec{Kind: "nginx", ID: m.ID, Link: l, Version: m.Version, File: m.File, SHA256: m.SHA256, Size: m.Size}
		if err := s.callData("runtime", &proto.Request{Root: p.t.Root, Stage: prep.Stage, Runtime: spec}, &r); err != nil {
			return fmt.Errorf("安装 nginx 失败：%w", err)
		}
		if p.pf.Nginx == nil {
			p.pf.Nginx = map[string]proto.Runtime{}
		}
		p.pf.Nginx[l] = r
	}
	return nil
}

// installRuntimes 安装服务器上还没有的 JDK：本机缓存没有就下载，上传后由 agent 校验、解开、更新 java-N 链接；
// 锁定版本的建它自己的链接。
func (e *Engine) installRuntimes(p *targetPlan) error {
	for _, pn := range p.needPins {
		if err := e.installPin(p.s, pn); err != nil {
			return err
		}
	}
	for _, n := range p.needRuntimes {
		r, err := e.installJDK(p.s, n, "")
		if err != nil {
			return err
		}
		if p.pf.Runtimes == nil {
			p.pf.Runtimes = map[int]proto.Runtime{}
		}
		p.pf.Runtimes[n] = r
	}
	return nil
}

// installJDK 把 JDK n 装到服务器上（部署、安装软件都用）。
// dir 不为空时装到这个目录（装软件时用户指定的），否则装到 Ferry 的公共目录。
func (e *Engine) installJDK(s *session, n int, dir string) (proto.Runtime, error) {
	arch := jdk.ArchOf(s.arch)
	lib := e.Settings.RuntimeDir()
	m := jdk.Find(lib, n, arch)
	if m == nil {
		src := e.Settings.Runtime.Source
		if src == "" {
			src = jdk.DefaultSource
		}
		e.Rep.Step("下载 JDK %d（linux/%s，%s）", n, arch, src)
		var err error
		m, err = jdk.Download(e.ctx(), src, n, arch, lib, func(done, total int64) { e.Rep.Progress("JDK "+fmt.Sprint(n), done, total) })
		if err != nil {
			return proto.Runtime{}, fmt.Errorf("下载 JDK %d 失败：%w（可以在设置 → 运行环境里换下载源，或手动导入下载好的压缩包）", n, err)
		}
		e.Rep.Info("已下载 %s（%s），存放在 %s", m.Version, humanBytes(m.Size), lib)
	}
	e.Rep.Step("上传 JDK %s 到 %s", m.Version, s.t.Name)
	var prep proto.Prepared
	if err := s.callData("prepare", &proto.Request{}, &prep); err != nil {
		return proto.Runtime{}, err
	}
	item := remote.UploadItem{Local: filepath.Join(lib, m.File), Remote: remote.Join(prep.Stage, m.File), Mode: 0o640, Size: m.Size}
	if err := e.uploadItems(s, []remote.UploadItem{item}, m.Size, m.Size); err != nil {
		return proto.Runtime{}, fmt.Errorf("上传 JDK 失败：%w", err)
	}
	var r proto.Runtime
	spec := &proto.RuntimeSpec{ID: m.ID, Java: n, Version: m.Version, File: m.File, SHA256: m.SHA256, Size: m.Size, Dir: dir}
	if err := s.callData("runtime", &proto.Request{Stage: prep.Stage, Runtime: spec}, &r); err != nil {
		return proto.Runtime{}, fmt.Errorf("安装 JDK 失败：%w", err)
	}
	return r, nil
}

// installPin 锁定版本的 JDK：服务器上装过就只建链接，否则上传这个版本（本机没有就按版本下载）。
func (e *Engine) installPin(s *session, pn pinNeed) error {
	link := proto.JavaPinLink(pn.version)
	if pn.have != "" {
		spec := &proto.RuntimeSpec{ID: pn.have, Java: pn.java, Version: pn.version, Pin: link}
		if err := s.callData("runtime", &proto.Request{Runtime: spec}, nil); err != nil {
			return fmt.Errorf("锁定 JDK %s 失败：%w", pn.version, err)
		}
		return nil
	}
	arch := jdk.ArchOf(s.arch)
	lib := e.Settings.RuntimeDir()
	m := jdk.FindVersion(lib, pn.version, arch)
	if m == nil {
		src := e.Settings.Runtime.Source
		if src == "" {
			src = jdk.DefaultSource
		}
		e.Rep.Step("下载 JDK %s（linux/%s，%s）", pn.version, arch, src)
		var err error
		m, err = jdk.DownloadVersion(e.ctx(), src, pn.version, arch, lib, func(done, total int64) { e.Rep.Progress("JDK "+pn.version, done, total) })
		if err != nil {
			return fmt.Errorf("下载 JDK %s 失败：%w（可以在设置 → 运行环境里换下载源，或手动导入这个版本的压缩包）", pn.version, err)
		}
		e.Rep.Info("已下载 %s（%s），存放在 %s", m.Version, humanBytes(m.Size), lib)
	}
	e.Rep.Step("上传 JDK %s 到 %s", m.Version, s.t.Name)
	var prep proto.Prepared
	if err := s.callData("prepare", &proto.Request{}, &prep); err != nil {
		return err
	}
	item := remote.UploadItem{Local: filepath.Join(lib, m.File), Remote: remote.Join(prep.Stage, m.File), Mode: 0o640, Size: m.Size}
	if err := e.uploadItems(s, []remote.UploadItem{item}, m.Size, m.Size); err != nil {
		return fmt.Errorf("上传 JDK 失败：%w", err)
	}
	spec := &proto.RuntimeSpec{ID: m.ID, Java: pn.java, Version: m.Version, Pin: link, File: m.File, SHA256: m.SHA256, Size: m.Size}
	if err := s.callData("runtime", &proto.Request{Stage: prep.Stage, Runtime: spec}, nil); err != nil {
		return fmt.Errorf("安装 JDK %s 失败：%w", pn.version, err)
	}
	return nil
}

// runBuilds 执行构建。built 记录本次已执行过的构建（命令、目录、环境完全相同的不再执行），值为开始时间。
func (e *Engine) runBuilds(t *config.Target, apps []*config.App, items []*item, built map[string]time.Time) error {
	steps, err := build.Plan(e.File, t, apps)
	if err != nil {
		return err
	}
	runner := &build.Runner{
		LogDir:  filepath.Join(e.ProjectDir(), "logs"),
		LockDir: appdir.LocksDir(),
		Env:     e.Settings.BuildEnv(),
		Out:     e.Rep.BuildOutput(),
		OnWait:  func(lock string) { e.Rep.Warn("另一个构建正在使用构建锁 %s，排队等待……", lock) },
		Ctx:     e.Ctx,
	}
	for _, st := range steps {
		key := st.Key()
		start, done := built[key]
		if done {
			e.Rep.Info("构建 %s 本次已执行过，复用产物", st.Name)
		} else {
			e.Rep.Step("构建 %s（%s）：%s", st.Name, st.Workdir, st.Command)
			if err := e.checkBusyArtifacts(t, st); err != nil {
				return err
			}
			start, err = runner.Run(st)
			if err != nil {
				var be *build.Error
				if errors.As(err, &be) {
					for _, l := range be.Tail {
						e.Rep.Error("%s", l)
					}
				}
				return err
			}
			built[key] = start
			e.Settings.CleanBuildLogs(runner.LogDir)
			e.Rep.Info("构建完成（%s）", time.Since(start).Round(time.Second))
		}
		for _, a := range st.Apps {
			for _, it := range items {
				if it.app == a {
					it.notBefore = start
				}
			}
		}
	}
	return nil
}

// upload 准备上传目录并上传；delta 时 Java 应用只传服务器没有的依赖和其余字节。
func (e *Engine) upload(s *session, items []*item, id string, git gitinfo.Info, tmp string, delta bool) (*proto.Request, int64, int64, error) {
	specs := make([]proto.AppSpec, len(items))
	hasJava, hasStatic := false, false
	for i, it := range items {
		specs[i] = it.spec
		hasJava = hasJava || it.app.Type == proto.TypeJava
		hasStatic = hasStatic || it.app.Type == proto.TypeStatic
	}
	var prep proto.Prepared
	if err := s.callData("prepare", &proto.Request{Apps: specs, Seed: delta && hasJava, Sites: delta && hasStatic}, &prep); err != nil {
		return nil, 0, 0, err
	}
	have := map[string]bool{}
	for _, l := range prep.Libs {
		have[l] = true
	}
	dtmp := filepath.Join(tmp, fmt.Sprintf("delta-%d", time.Now().UnixNano()))
	libDir := filepath.Join(dtmp, "libs")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return nil, 0, 0, err
	}
	var files []upFile
	var upBytes, total int64
	req := &proto.Request{Apps: specs, Stage: prep.Stage}
	for _, it := range items {
		rel := proto.ReleaseSpec{App: it.app.Name, ID: id, SHA256: it.sha, Size: it.size, Archive: it.app.Type == proto.TypeStatic || proto.DirService(it.app.Type),
			Meta: proto.ReleaseMeta{Commit: git.Commit, Branch: git.Branch, Dirty: git.Dirty, DeployedBy: e.actor(), Source: it.local}}
		if it.notBefore.IsZero() {
			if st, err := os.Stat(it.file); err == nil {
				rel.Meta.BuiltAt = st.ModTime()
			}
		} else {
			rel.Meta.BuiltAt = it.notBefore
		}
		rel.File = it.spec.FileName
		if it.app.Type == proto.TypeStatic || proto.DirService(it.app.Type) {
			rel.File = it.app.Name + ".tar.gz"
		}
		total += it.size
		usedDelta := false
		if delta && it.app.Type == proto.TypeJava {
			plan := filepath.Join(dtmp, rel.File+".plan")
			pack := filepath.Join(dtmp, rel.File+".pack")
			before := len(have)
			snapshot := make(map[string]bool, len(have))
			for k := range have {
				snapshot[k] = true
			}
			st, err := zipx.Split(it.file, plan, pack, libDir, have)
			switch {
			case err != nil:
				e.Rep.Warn("%s 无法增量拆分（%v），全量上传", it.app.Name, err)
				have = snapshot
			case st.UploadBytes() > it.size*8/10:
				// 几乎没有可复用的依赖：直接全量，省得服务器拼装
				have = snapshot
				removeNewLibs(libDir, snapshot)
			default:
				usedDelta = true
				rel.Plan, rel.Pack = rel.File+".plan", rel.File+".pack"
				files = append(files, upFile{plan, rel.Plan}, upFile{pack, rel.Pack})
				upBytes += st.RawBytes
				e.Rep.Info("%-12s 增量：上传 %s（新依赖 %d 个），复用缓存 %s，全量 %s", it.app.Name,
					humanBytes(st.UploadBytes()), len(have)-before, humanBytes(st.ReusedBytes), humanBytes(it.size))
			}
		}
		if it.app.Type == proto.TypeStatic {
			manifest, pack, err := e.siteDelta(it, &rel, prep.Sites[it.app.Name], delta, dtmp)
			if err != nil {
				return nil, 0, 0, err
			}
			files = append(files, manifest)
			if fi, err := os.Stat(manifest.local); err == nil {
				upBytes += fi.Size()
			}
			if pack != nil {
				files = append(files, *pack)
				upBytes += rel.Size
				usedDelta = true
			}
		}
		if !usedDelta {
			files = append(files, upFile{it.file, rel.File})
			upBytes += it.size
		}
		if it.filesTar != "" {
			rel.Files, rel.FilesSHA256, rel.FilesSize = it.app.Name+".files.tar.gz", it.filesSHA, it.filesSize
			rel.Meta.Files = extraNames(it.files)
			files = append(files, upFile{it.filesTar, rel.Files})
			upBytes += it.filesSize
			total += it.filesSize
		}
		req.Releases = append(req.Releases, rel)
	}
	libs, _ := os.ReadDir(libDir)
	if len(libs) > 0 {
		if err := s.c.MkdirAll(remote.Join(prep.Stage, "libs")); err != nil {
			return nil, 0, 0, err
		}
		for _, l := range libs {
			files = append(files, upFile{filepath.Join(libDir, l.Name()), "libs/" + l.Name()})
			if fi, err := l.Info(); err == nil {
				upBytes += fi.Size()
			}
		}
	}
	uploads := make([]remote.UploadItem, 0, len(files))
	for _, uf := range files {
		var size int64
		if fi, err := os.Stat(uf.local); err == nil {
			size = fi.Size()
		}
		uploads = append(uploads, remote.UploadItem{Local: uf.local, Remote: remote.Join(prep.Stage, uf.remote), Mode: 0o640, Size: size})
	}
	if err := e.uploadItems(s, uploads, upBytes, total); err != nil {
		if !errors.Is(err, remote.ErrCanceled) {
			s.broken.Store(true)
		}
		return nil, 0, 0, err
	}
	return req, upBytes, total, nil
}

// upFile 要上传的一个文件：本机路径、上传目录里的名字。
type upFile struct{ local, remote string }

// siteDelta 静态站点：写好文件清单 manifest（总要上传，服务器存进版本记录，下次据此比较）；
// 服务器上有当前版本、变化的不多时，只把变化的文件打成增量包 pack（rel 改成增量），否则 pack 为空，照常全量上传。
func (e *Engine) siteDelta(it *item, rel *proto.ReleaseSpec, remote []proto.SiteFile, delta bool, dir string) (manifest upFile, pack *upFile, err error) {
	b, err := json.Marshal(it.site)
	if err != nil {
		return manifest, nil, err
	}
	manifest = upFile{filepath.Join(dir, it.app.Name+".site.json"), it.app.Name + ".site.json"}
	if err := os.WriteFile(manifest.local, b, 0o644); err != nil {
		return manifest, nil, err
	}
	if rel.SiteSHA256, _, err = zipx.FileSHA256(manifest.local); err != nil {
		return manifest, nil, err
	}
	rel.Site = manifest.remote
	if !delta || remote == nil {
		return manifest, nil, nil
	}
	var total int64
	for _, f := range it.site {
		total += f.Size
	}
	changed, bytes := artifact.SiteChanges(it.site, remote)
	if bytes > total*8/10 { // 几乎都变了：直接全量，省得服务器拼装
		return manifest, nil, nil
	}
	p := upFile{filepath.Join(dir, it.app.Name+".delta.tar.gz"), it.app.Name + ".delta.tar.gz"}
	if err := artifact.PackDirOnly(it.local, p.local, changed); err != nil {
		e.Rep.Warn("%s 无法增量打包（%v），全量上传", it.app.Name, err)
		return manifest, nil, nil
	}
	sha, size, err := zipx.FileSHA256(p.local)
	if err != nil {
		return manifest, nil, err
	}
	rel.File, rel.SHA256, rel.Size, rel.SiteDelta = p.remote, sha, size, true
	e.Rep.Info("%-12s 增量：上传 %d 个变化的文件（%s），%d 个沿用服务器上的，全部 %s", it.app.Name,
		len(changed), humanBytes(size), len(it.site)-len(changed), humanBytes(it.size))
	return manifest, &p, nil
}

// parallelMin 小于这个大小时不开额外连接（建连接的开销比省下的时间多）。
const parallelMin = 8 << 20

// uploadStreams 目标的上传并行连接数：目标配置优先，其次设置。
func (e *Engine) uploadStreams(t *config.Target) int {
	if t.UploadStreams > 0 {
		return t.UploadStreams
	}
	if e.Settings.Connect.UploadStreams > 0 {
		return e.Settings.Connect.UploadStreams
	}
	return 1
}

// uploadRetries 上传时连接断了、卡住了，最多重新连接几次（每次接着传剩下的）。
const uploadRetries = 5

// uploadItems 上传：总量大时切片多连接并行（应对 VPN 等对单条连接的限速），否则用一条连接逐个上传。
// 连接断了、卡住了时重新连接，只传还没传完的片（大文件）或重传（小文件）；断过一次就少用几路，网络撑不住那么多连接。
func (e *Engine) uploadItems(s *session, items []remote.UploadItem, upBytes, total int64) error {
	// 实际传输的是各文件的大小（增量时含拼装计划等），进度按它算，不会超过 100%
	var sent int64
	for _, it := range items {
		sent += it.Size
	}
	if sent > 0 {
		upBytes = sent
	}
	streams := e.uploadStreams(s.t)
	state := &remote.UploadState{}
	for attempt := 1; ; attempt++ {
		err := e.uploadOnce(s, items, upBytes, total, streams, state, attempt == 1)
		if err == nil {
			e.Rep.Progress("", upBytes, upBytes)
			return nil
		}
		if e.ctx().Err() != nil || errors.Is(err, remote.ErrCanceled) || !remote.ConnLost(err) || attempt > uploadRetries {
			return err
		}
		if streams > 2 {
			streams = (streams + 1) / 2
		}
		wait := time.Duration(attempt*attempt) * 3 * time.Second // 3、12、27…… 秒：给隧道、VPN 恢复的时间
		left := upBytes - state.Done()
		e.Rep.Warn("上传时连接断了（%v）：%d 秒后重新连接，接着传剩下的 %s（已传完 %s），改用 %d 路（第 %d / %d 次重试）",
			err, int(wait.Seconds()), humanBytes(left), humanBytes(state.Done()), streams, attempt, uploadRetries)
		select {
		case <-time.After(wait):
		case <-e.ctx().Done():
			return ErrCanceled
		}
		// 断的、卡住的可能只是额外的几路：主连接还好就接着用（连接池里别的操作也在用它）
		if !s.c.Alive() || s.c.Ping(10*time.Second) != nil {
			if err := s.reconnect(); err != nil {
				e.Rep.Warn("重新连接失败：%v", err)
			}
		} else {
			s.c.ResetSFTP() // SSH 连接还在，SFTP 可能断了
		}
	}
}

// uploadOnce 上传一次：大文件用 UploadParallel（记在 state 里，可以接着传），小文件一个个传。
func (e *Engine) uploadOnce(s *session, items []remote.UploadItem, upBytes, total int64, streams int, state *remote.UploadState, first bool) error {
	if upBytes < parallelMin {
		if first {
			e.Rep.Step("上传 %d 个文件，共 %s（全量 %s）", len(items), humanBytes(upBytes), humanBytes(total))
		}
		var done int64
		for _, it := range items {
			label := path.Base(it.Remote)
			base := done
			err := s.c.UploadContext(e.ctx(), it.Local, it.Remote, it.Mode, func(d, _ int64) {
				e.Rep.Progress(label, base+d, upBytes)
			})
			if err != nil {
				return err
			}
			done += it.Size
		}
		return nil
	}
	// 逐条建立额外连接（同时握手太多会被 sshd 的 MaxStartups 随机拒绝）；建不起来就用已有的
	var extra []*remote.Client
	opts := s.opts
	opts.OnNewHostKey = nil // 主连接已经记录过主机公钥
	for i := 1; i < streams; i++ {
		if e.ctx().Err() != nil {
			break
		}
		c, err := remote.Dial(opts)
		if err != nil {
			e.Rep.Warn("第 %d 路连接建立失败（%v），用 %d 路上传", i+1, err, i)
			break
		}
		extra = append(extra, c)
	}
	defer func() {
		for _, c := range extra {
			c.Close()
		}
	}()
	if first {
		if len(extra) > 0 {
			e.Rep.Step("上传 %d 个文件，共 %s（全量 %s，%d 路并行）", len(items), humanBytes(upBytes), humanBytes(total), len(extra)+1)
		} else {
			e.Rep.Step("上传 %d 个文件，共 %s（全量 %s）", len(items), humanBytes(upBytes), humanBytes(total))
		}
	}
	var mu sync.Mutex
	var last time.Time
	return remote.UploadParallel(e.ctx(), s.c, extra, items, state, func(done int64, label string) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) < 150*time.Millisecond && done < upBytes {
			return
		}
		last = time.Now()
		e.Rep.Progress(path.Base(label), done, upBytes)
	})
}

// removeNewLibs 删掉本次拆分新写出的依赖（不在 keep 里的）。
func removeNewLibs(dir string, keep map[string]bool) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		sha := strings.TrimSuffix(e.Name(), ".jar")
		if !keep[sha] {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// localHistory 本机部署历史：<项目目录>/history.jsonl
func (e *Engine) localHistory(r *DeployReport) {
	dir := e.ProjectDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	fh, err := os.OpenFile(filepath.Join(dir, "history.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	for _, a := range r.Apps {
		b, _ := json.Marshal(map[string]any{
			"time": time.Now().Format(time.RFC3339), "target": r.Target, "app": a.App, "release": r.Release,
			"result": a.Result, "commit": r.Git.Commit, "dirty": r.Git.Dirty, "actor": e.actor(), "msg": a.Msg,
		})
		fh.Write(append(b, '\n'))
	}
}
