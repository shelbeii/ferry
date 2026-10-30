package agent

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// LegacyID rollback 的特殊目标：回到 ferry 接管前的服务。
const LegacyID = "legacy"

// ---------------------------------------------------------------- rollback

func (a *Agent) rollback() int {
	if len(a.req.Apps) != 1 {
		return a.result(proto.CodeBadInput, "回滚一次只能指定一个应用", nil)
	}
	s := &a.req.Apps[0]
	unlock, err := a.lock(time.Duration(a.req.LockWaitSec) * time.Second)
	if errors.Is(err, ErrLocked) {
		return a.result(proto.CodeLocked, err.Error(), nil)
	}
	if err != nil {
		return a.result(proto.CodeFailed, "加锁失败："+err.Error(), nil)
	}
	defer unlock()

	cur := a.current(s.Name)
	to := a.req.To
	if to == "" {
		to = a.previousGood(s.Name, cur)
		if to == "" && a.hasLegacy(s) {
			to = LegacyID
		}
		if to == "" {
			a.fail(proto.EvAbort, s.Name, "没有可回滚的版本")
			return a.result(proto.CodeFailed, "没有可回滚的版本", nil)
		}
	}
	if to == cur {
		return a.result(proto.CodeBadInput, to+" 就是当前版本", nil)
	}
	if to != LegacyID {
		if !reID.MatchString(to) {
			return a.result(proto.CodeBadInput, "版本 ID 无效："+to, nil)
		}
		if _, err := os.Stat(a.releaseDir(s.Name, to)); err != nil {
			return a.result(proto.CodeBadInput, "没有版本 "+to, nil)
		}
	}
	a.info(proto.EvRollback, s.Name, "%s → %s", orNone(cur), to)

	var rerr error
	switch {
	case s.ZD != nil && to == LegacyID:
		rerr = errors.New("不停机发布的应用不能回到接管前：先在配置里去掉 zeroDowntime 部署一次")
	case s.ZD != nil:
		if res, msg := a.zdSwitch(s, to, cur, false); res != "OK" {
			rerr = errors.New(msg)
		}
		a.finishLogArchives(s)
	case to == LegacyID:
		a.leaveZD(s)
		rerr = a.toLegacy(s)
	case s.Type == proto.TypeStatic && !servedStatic(s):
		if res, msg := a.switchStatic(s, to, cur); res != "OK" {
			rerr = errors.New(msg)
		}
	default:
		a.leaveZD(s)
		rerr = a.rollbackService(s, to, cur)
		a.finishLogArchives(s)
	}
	meta := a.readMeta(s.Name, to)
	e := proto.HistoryEntry{Action: "rollback", App: s.Name, Release: to, Result: "OK", Commit: meta.Commit, SHA256: meta.SHA256,
		Msg: "从 " + orNone(cur) + " 回滚"}
	if rerr != nil {
		e.Result, e.Msg = "FAILED", e.Msg+"："+rerr.Error()
		a.addHistory(e)
		return a.result(proto.CodeFailed, rerr.Error(), nil)
	}
	a.addHistory(e)
	a.info(proto.EvRollback, s.Name, "已回滚到 %s", to)
	return a.result(proto.CodeOK, "", proto.AppOutcome{App: s.Name, Release: to, Result: "OK"})
}

// rollbackService 切到版本 to（单元文件用该版本记录的），启动并等待；失败切回 cur。
func (a *Agent) rollbackService(s *proto.AppSpec, to, cur string) error {
	start := func(id string) error {
		text, err := os.ReadFile(a.verFile(s.Name, id, "unit.service"))
		if err != nil {
			text = []byte(s.UnitText)
		}
		text = []byte(rewriteOldPaths(a.appDir(s.Name), filepath.Join(a.appMetaDir(s.Name), "nginx.conf"), string(text), servedStatic(s))) // 以前的结构下部署的版本
		a.ensureDataFor(s, string(text))
		a.Sys.Systemctl("stop", s.Unit)
		a.rotateLogs(s, a.current(s.Name))
		if err := a.activate(s, id); err != nil {
			return err
		}
		a.info(proto.EvSwitch, s.Name, "running → %s", id)
		a.markRun(s, id)
		if err := a.syncNginxConf(s, id); err != nil {
			return err
		}
		if err := a.tlsPrepare(s); err != nil {
			return err
		}
		if err := a.installUnit(s.Name, s.Unit, text); err != nil {
			return err
		}
		a.Sys.Systemctl("enable", s.Unit)
		a.info(proto.EvStart, s.Name, "%s", s.Unit)
		if err := a.Sys.Systemctl("restart", s.Unit); err != nil {
			return err
		}
		if err := a.waitReady(s, s.Unit); err != nil {
			a.diag(s, s.Unit)
			return err
		}
		return nil
	}
	err := start(to)
	if err == nil {
		return nil
	}
	if cur == "" {
		return err
	}
	a.warn(proto.EvRollback, s.Name, "%s 无法启动，切回 %s", to, cur)
	if err2 := start(cur); err2 != nil {
		return fmt.Errorf("%v；切回 %s 也失败：%v", err, cur, err2)
	}
	return fmt.Errorf("%v（已切回 %s）", err, cur)
}

func (a *Agent) hasLegacy(s *proto.AppSpec) bool {
	if !isService(s) {
		return false
	}
	if _, err := os.Stat(filepath.Join(a.legacyDir(s.Name), s.Unit)); err == nil {
		return true
	}
	for _, c := range s.Conflict {
		if _, err := os.Stat(a.unitPath(c)); err == nil {
			return true
		}
	}
	return false
}

// toLegacy 回到 ferry 接管前：还原原单元文件（或启用冲突的旧服务），删除 current。
func (a *Agent) toLegacy(s *proto.AppSpec) error {
	if !a.hasLegacy(s) {
		return errors.New("没有接管前的服务可恢复")
	}
	a.Sys.Systemctl("stop", s.Unit)
	a.rotateLogs(s, a.current(s.Name))
	a.markRun(s, "")
	saved := filepath.Join(a.legacyDir(s.Name), s.Unit)
	if text, err := os.ReadFile(saved); err == nil {
		if err := a.installUnit(s.Name, s.Unit, text); err != nil {
			return err
		}
		a.deactivate(s.Name)
		a.Sys.Systemctl("enable", s.Unit)
		if err := a.Sys.Systemctl("restart", s.Unit); err != nil {
			return err
		}
		if err := a.waitReady(s, s.Unit); err != nil {
			a.diag(s, s.Unit)
			return err
		}
		return nil
	}
	a.Sys.Systemctl("disable", s.Unit)
	os.Remove(a.unitPath(s.Unit))
	a.Sys.Systemctl("daemon-reload")
	a.deactivate(s.Name)
	for _, c := range s.Conflict {
		if _, err := os.Stat(a.unitPath(c)); err != nil {
			continue
		}
		a.info(proto.EvStart, s.Name, "%s", c)
		a.Sys.Systemctl("enable", c)
		if err := a.Sys.Systemctl("restart", c); err != nil {
			return err
		}
		if err := a.waitReady(s, c); err != nil {
			a.diag(s, c)
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- control

func (a *Agent) control() int {
	action := a.req.Action
	if action != "start" && action != "stop" && action != "restart" {
		return a.result(proto.CodeBadInput, "action 只能是 start、stop、restart", nil)
	}
	wait := time.Duration(a.req.LockWaitSec) * time.Second
	unlock, err := a.lock(wait)
	if errors.Is(err, ErrLocked) {
		return a.result(proto.CodeLocked, err.Error(), nil)
	}
	if err != nil {
		return a.result(proto.CodeFailed, "加锁失败："+err.Error(), nil)
	}
	defer unlock()

	apps := a.req.Apps
	var out proto.DeployOutcome
	if action == "stop" {
		for i := len(apps) - 1; i >= 0; i-- {
			s := &apps[i]
			if !isService(s) {
				if s.NginxInstance != "" {
					res, msg := "OK", ""
					if err := a.controlNginxSite(s, action); err != nil {
						res, msg = "FAILED", err.Error()
					}
					out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Result: res, Msg: msg})
				}
				continue
			}
			if a.current(s.Name) == "" {
				a.info(proto.EvInfo, s.Name, "未通过 ferry 部署，跳过")
				out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Result: "NOT_DEPLOYED"})
				continue
			}
			a.info(proto.EvStop, s.Name, "%s", s.Unit)
			res := "OK"
			if err := a.Sys.Systemctl("stop", s.Unit); err != nil {
				a.fail(proto.EvFail, s.Name, "%v", err)
				res = "FAILED"
			}
			a.addHistory(proto.HistoryEntry{Action: "stop", App: s.Name, Release: a.current(s.Name), Result: res})
			out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Result: res})
		}
		return a.finishControl(out)
	}
	failed := ""
	for i := range apps {
		s := &apps[i]
		if failed != "" {
			out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Result: "SKIPPED"})
			continue
		}
		if !isService(s) {
			if s.NginxInstance != "" {
				res, msg := "OK", ""
				if err := a.controlNginxSite(s, action); err != nil {
					res, msg = "FAILED", err.Error()
					failed = s.Name
				}
				out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Result: res, Msg: msg})
			} else {
				a.info(proto.EvInfo, s.Name, "静态应用无需启停")
			}
			continue
		}
		if failed != "" {
			out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Result: "SKIPPED"})
			continue
		}
		if a.current(s.Name) == "" {
			a.info(proto.EvInfo, s.Name, "未通过 ferry 部署，跳过")
			out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Result: "NOT_DEPLOYED"})
			continue
		}
		res, msg := "OK", ""
		if err := a.startOne(s, action); err != nil {
			res, msg, failed = "FAILED", err.Error(), s.Name
		}
		a.addHistory(proto.HistoryEntry{Action: action, App: s.Name, Release: a.current(s.Name), Result: res, Msg: msg})
		out.Apps = append(out.Apps, proto.AppOutcome{App: s.Name, Result: res, Msg: msg})
	}
	if failed != "" {
		var skipped []string
		for _, o := range out.Apps {
			if o.Result == "SKIPPED" {
				skipped = append(skipped, o.App)
			}
		}
		if len(skipped) > 0 {
			a.fail(proto.EvAbort, strings.Join(skipped, ","), "因 %s 启动失败未执行", failed)
		}
	}
	return a.finishControl(out)
}

func (a *Agent) finishControl(out proto.DeployOutcome) int {
	for _, o := range out.Apps {
		if o.Result != "OK" && o.Result != "SKIPPED" && o.Result != "NOT_DEPLOYED" {
			return a.result(proto.CodeFailed, o.App+" 失败", out)
		}
	}
	for _, o := range out.Apps {
		if o.Result == "SKIPPED" {
			return a.result(proto.CodeFailed, "部分应用未执行", out)
		}
	}
	// 一个都没执行（全是没通过 ferry 部署过的）：不能报成功，否则看起来像是重启了
	var notDeployed []string
	for _, o := range out.Apps {
		if o.Result == "NOT_DEPLOYED" {
			notDeployed = append(notDeployed, o.App)
		}
	}
	if len(out.Apps) > 0 && len(notDeployed) == len(out.Apps) {
		return a.result(proto.CodeFailed, strings.Join(notDeployed, "、")+" 还没有通过 ferry 部署过，ferry 不管理它们的启停（仍由原来的方式管理）", out)
	}
	return a.result(proto.CodeOK, "", out)
}

func (a *Agent) startOne(s *proto.AppSpec, action string) error {
	if a.current(s.Name) == "" {
		err := errors.New("还没有部署过")
		a.fail(proto.EvFail, s.Name, "%v", err)
		return err
	}
	if _, err := os.Stat(a.unitPath(s.Unit)); err != nil {
		err := fmt.Errorf("单元文件 %s 不存在", s.Unit)
		a.fail(proto.EvFail, s.Name, "%v", err)
		return err
	}
	if action == "start" && isRunning(a.Sys.UnitProps(s.Unit)) {
		a.info(proto.EvReady, s.Name, "已在运行")
		return nil
	}
	for _, c := range s.Conflict {
		if isRunning(a.Sys.UnitProps(c)) {
			a.info(proto.EvStop, s.Name, "停止冲突的服务 %s", c)
			a.Sys.Systemctl("stop", c)
		}
	}
	a.info(proto.EvStart, s.Name, "%s", s.Unit)
	if err := a.Sys.Systemctl(action, s.Unit); err != nil {
		a.fail(proto.EvFail, s.Name, "%v", err)
		a.diag(s, s.Unit)
		return err
	}
	if err := a.waitReady(s, s.Unit); err != nil {
		a.diag(s, s.Unit)
		return err
	}
	return nil
}

// ---------------------------------------------------------------- status / releases

func (a *Agent) status() int {
	var out []proto.AppStatus
	busy := a.lockHolder()
	// nginx 配置只读一次，多个静态站点共用
	var ngText string
	var ngErr error
	ngRead := false
	nginx := func() (string, error) {
		if !ngRead {
			ngText, ngErr = a.Sys.NginxDump()
			ngRead = true
		}
		return ngText, ngErr
	}
	for i := range a.req.Apps {
		s := &a.req.Apps[i]
		st := proto.AppStatus{Name: s.Name, Type: s.Type, Unit: s.Unit, Releases: len(a.listReleases(s.Name))}
		if cur := a.current(s.Name); cur != "" {
			st.Current = a.readMeta(s.Name, cur)
		} else {
			st.Problem = "未部署"
		}
		if s.NginxInstance != "" && st.Current != nil {
			a.nginxSiteStatus(s, &st)
		} else if isService(s) {
			p := a.Sys.UnitProps(s.Unit)
			st.Active, st.Sub, st.PID, st.Since = p.Active, p.Sub, p.PID, p.Since
			st.Port = healthPort(s.Health)
			if p.PID > 0 {
				st.Listen, st.Extra = a.classifyPorts(s, st.Port, a.Sys.Listening(p.PID), p.PID)
			}
			if p.Load == "not-found" {
				st.Problem = "单元不存在"
			}
			if p.Active == "active" && (s.Health.TCP != "" || s.Health.HTTP != "") {
				ok := a.Sys.Probe(s.Health) == nil
				st.Healthy = &ok
			}
			if st.Current == nil {
				for _, c := range s.Conflict {
					if cp := a.Sys.UnitProps(c); isRunning(cp) {
						st.Problem = "未接管，旧服务 " + c + " 在运行"
					}
				}
			}
		} else if st.Current != nil {
			if s.Health.TCP != "" || s.Health.HTTP != "" {
				ok := a.Sys.Probe(s.Health) == nil
				st.Healthy = &ok
			}
			st.Sites, st.Listen, st.Serving = a.staticServing(s, nginx)
			st.Port = healthPort(s.Health)
		}
		st.Busy = busy
		out = append(out, st)
	}
	return a.result(proto.CodeOK, "", out)
}

// healthPort 健康检查里的端口（tcp 的 host:port，或 http 地址里的端口）。
func healthPort(h proto.Health) string {
	if h.TCP != "" {
		if _, p, err := net.SplitHostPort(h.TCP); err == nil {
			return p
		}
	}
	if h.HTTP != "" {
		if u, err := url.Parse(h.HTTP); err == nil {
			if p := u.Port(); p != "" {
				return p
			}
		}
	}
	return ""
}

func (a *Agent) releases() int {
	var out []proto.AppReleases
	for i := range a.req.Apps {
		s := &a.req.Apps[i]
		cur := a.current(s.Name)
		ar := proto.AppReleases{App: s.Name, Legacy: a.hasLegacy(s)}
		if a.req.Detail && cur != "" {
			if b, err := os.ReadFile(a.verFile(s.Name, cur, "unit.service")); err == nil {
				ar.Unit = string(b)
			}
		}
		for _, id := range a.listReleases(s.Name) {
			ar.Releases = append(ar.Releases, proto.ReleaseInfo{ReleaseMeta: *a.readMeta(s.Name, id), Current: id == cur,
				Failed: a.isFailed(s.Name, id), LogRuns: len(a.archivedRuns(s.Name, id))})
		}
		out = append(out, ar)
	}
	return a.result(proto.CodeOK, "", out)
}

// ---------------------------------------------------------------- preflight

func (a *Agent) preflight() int {
	p := proto.Preflight{
		Arch: runtime.GOARCH, UID: os.Getuid(), Users: map[string]bool{}, Commands: map[string]string{},
		ExistingUnits: map[string]string{}, AgentVersion: Version, ProtoVersion: proto.Version,
	}
	p.OS = osRelease()
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		p.Kernel = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("systemctl", "--version").Output(); err == nil {
		f := strings.Fields(string(out))
		if len(f) >= 2 {
			p.Systemd, _ = strconv.Atoi(f[1])
		}
	}
	if out, err := exec.Command("getenforce").Output(); err == nil {
		p.SELinux = strings.TrimSpace(string(out))
	}
	need := map[string]bool{}
	hasService := false
	access := map[string]error{} // 各应用共用的权限测试结果
	for i := range a.req.Apps {
		s := &a.req.Apps[i]
		if s.NginxInstance != "" {
			if _, err := a.hostingInstance(s); err != nil {
				p.Problems = append(p.Problems, s.Name+"："+err.Error())
			}
			hasService = true
		}
		if isService(s) {
			hasService = true
			if _, ok := p.Users[s.User]; !ok {
				_, _, err := a.Sys.LookupUser(s.User)
				switch {
				case err == nil:
				case a.req.FixPerms == FixPlan: // 自动修正时一并处理：plan 只列出，apply 时创建
					p.Fixable = append(p.Fixable, "创建运行用户 "+s.User)
				case a.req.FixPerms == FixApply:
					if err = a.createRunUser(s.User); err == nil {
						p.Fixed = append(p.Fixed, "创建运行用户 "+s.User)
					} else {
						p.Problems = append(p.Problems, err.Error())
					}
				default:
					p.Problems = append(p.Problems, fmt.Sprintf("运行用户 %s 不存在（可执行：useradd -r -m -s /sbin/nologin %s）", s.User, s.User))
				}
				p.Users[s.User] = err == nil
			}
			if p.Users[s.User] && p.UID == 0 {
				a.checkServiceAccess(s, &p, access)
			}
			if b, err := os.ReadFile(a.unitPath(s.Unit)); err == nil {
				if strings.HasPrefix(string(b), ManagedMarker) {
					p.ExistingUnits[s.Unit] = "managed"
				} else {
					p.ExistingUnits[s.Unit] = "foreign"
					p.Warnings = append(p.Warnings, fmt.Sprintf("%s 已存在且不是 ferry 生成的：首次部署会接管它（原文件保存在 apps/%s/legacy/，可 rollback --to legacy）", s.Unit, s.Name))
				}
			}
			for _, c := range s.Conflict {
				if _, err := os.Stat(a.unitPath(c)); err == nil {
					p.ExistingUnits[c] = "conflict"
				}
			}
		}
		if s.Type == proto.TypeStatic && !servedStatic(s) && s.NginxInstance == "" {
			a.checkStaticServing(s, &p)
		}
		if s.Type == proto.TypeStatic && s.Reload != "" {
			if f := strings.Fields(s.Reload); len(f) > 0 {
				need[f[0]] = true
			}
			if s.Link != "" {
				if fi, err := os.Lstat(s.Link); err == nil && fi.Mode()&os.ModeSymlink == 0 {
					p.Problems = append(p.Problems, fmt.Sprintf("%s 已存在且不是符号链接：先移走（ferry 会把它变成指向 current 的链接）", s.Link))
				}
			}
		}
	}
	if hasService {
		need["systemctl"], need["journalctl"] = true, true
		if p.Systemd == 0 {
			p.Problems = append(p.Problems, "没有 systemd")
		}
	}
	for c := range need {
		path, _ := exec.LookPath(c)
		p.Commands[c] = path
		if path == "" {
			p.Problems = append(p.Problems, "缺少命令 "+c)
		}
	}
	if p.UID != 0 {
		p.Problems = append(p.Problems, "agent 需要以 root 运行（非 root 登录请在目标上设置 sudo: true）")
	}
	if p.SELinux == "Enforcing" {
		p.Warnings = append(p.Warnings, "SELinux 为 Enforcing：若服务因权限无法启动，检查部署目录的安全上下文")
	}

	a.checkJavas(&p)
	a.checkDirRuntimes(&p)
	p.Runtimes = a.installedRuntimes()
	var pinLinks []string
	for _, s := range a.req.Apps {
		if s.RuntimeVersion != "" {
			pinLinks = append(pinLinks, proto.JavaPinLink(s.RuntimeVersion))
		}
	}
	p.JDKs, p.JavaPins = a.installedJDKs(pinLinks)
	p.Nginx = a.installedNginx()
	free, owner, used := a.checkRootDir(a.req.Root, &p)
	p.RootFreeMB, p.RootOwner, p.DeployRootUsed = free, owner, used
	// 应用单独设置的部署根目录：同样检查；同一个目录的应用合在一起
	var order []string
	byRoot := map[string]*proto.RootInfo{}
	for _, s := range a.req.Apps {
		r := a.appRoots[s.Name]
		if r == "" {
			continue
		}
		if ri := byRoot[r]; ri != nil {
			ri.Apps = append(ri.Apps, s.Name)
		} else {
			free, _, _ := a.checkRootDir(r, &p)
			byRoot[r] = &proto.RootInfo{Path: r, Apps: []string{s.Name}, FreeMB: free}
			order = append(order, r)
		}
		// 以前部署在目标的部署根目录下：不会搬过来
		old := filepath.Join(a.req.Root, "apps", s.Name, dirVersions)
		if !realDir(old) {
			old = filepath.Join(a.req.Root, "apps", s.Name, "releases")
		}
		if run := filepath.Join(a.req.Root, "apps", s.Name, dirRunning); realDir(run) {
			old = run
		}
		if ents, err := os.ReadDir(old); err == nil && len(ents) > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s 改到 %s 部署：%s 下原有的 %d 个版本不会搬过去，也不能回滚到它们（确认后可手动删除）", s.Name, r, old, len(ents)))
		}
	}
	for _, r := range order {
		p.AppRoots = append(p.AppRoots, *byRoot[r])
	}
	return a.result(proto.CodeOK, "", p)
}

// checkRootDir 检查部署根目录：它及其上级必须归 root 且其他用户不可写，否则 root 运行的 agent 可能被符号链接劫持。
// 问题与提示记到 p 里；返回所在分区的剩余空间、（最近一级已存在的目录的）属主、目录是否已存在。
func (a *Agent) checkRootDir(root string, p *proto.Preflight) (freeMB int64, owner string, used bool) {
	existing := root
	if _, err := os.Stat(existing); err == nil {
		used = true
	}
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	freeMB = diskFreeMB(existing)
	if freeMB >= 0 && freeMB < 1024 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("部署目录 %s 所在分区只剩 %d MB", root, freeMB))
	}
	for d := existing; ; d = filepath.Dir(d) {
		uid, mode, err := ownerOf(d)
		if err == nil {
			if d == existing {
				owner = strconv.Itoa(uid)
			}
			if mode&os.ModeSymlink != 0 && d == root {
				p.Problems = append(p.Problems, d+" 是符号链接")
			}
			if uid != 0 || mode.Perm()&0o022 != 0 && mode&os.ModeSticky == 0 {
				if a.req.FixPerms != "" {
					if desc, ok := a.fixRootDir(root, d, a.req.FixPerms == FixApply); ok {
						if a.req.FixPerms == FixApply {
							p.Fixed = append(p.Fixed, desc)
						} else {
							p.Fixable = append(p.Fixable, desc)
						}
						if filepath.Dir(d) == d {
							break
						}
						continue
					}
				}
				p.Problems = append(p.Problems, fmt.Sprintf("%s 不安全（属主 uid %d，权限 %s）：部署根目录及其上级必须归 root 且其他用户不可写，建议用 /opt/<项目>", d, uid, mode.Perm()))
				break
			}
			if mode&os.ModeSticky != 0 {
				p.Problems = append(p.Problems, d+" 是公共可写目录，不能作为部署根目录的上级")
				break
			}
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return freeMB, owner, used
}

func osRelease() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}
