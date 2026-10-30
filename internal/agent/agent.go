// Package agent 是在服务器上以 root 运行的部署助手（ferry-agent）。
//
// 服务器目录（<root> 由目标配置决定）：
//
//	<root>/.ferry/lock                 部署/回滚/启停互斥（flock）
//	<root>/.ferry/history.jsonl        部署历史
//	<root>/.ferry/cache/lib/<sha>.jar  增量上传的依赖缓存
//	<root>/.ferry/incoming/<id>/       每次部署独立的上传目录
//	<root>/apps/<app>/                 一个应用：running/（运行中的版本）、versions/、data/、.ferry/（见 layout.go）
//
// 应用单独设置了部署根目录（AppSpec.Root）时，它的 apps/<app>/… 在那个目录下；.ferry（锁、历史、缓存、上传目录）仍在 <root>。
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// Version 不再在构建时注入：agent 以文件的 sha256 区分版本（代码没变时升级 Ferry 不用重新上传）。
var Version = "dev"

// ManagedMarker 写在 ferry 生成的单元文件第一行。
const ManagedMarker = "# managed by ferry"

var reID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-z]{4,12}$`)

// Agent 一次命令执行的上下文。
type Agent struct {
	instanceLookup func(string) (*proto.SoftwareInstance, error) // 可替换的实例存储，用于隔离的文件系统测试。
	Sys            System
	Out            io.Writer
	// Plain 输出给人看的文字（定时器里运行时进 journald），不是 JSON 事件
	Plain bool

	mu         sync.Mutex
	req        *proto.Request
	appRoots   map[string]string    // 应用 → 单独设置的部署根目录
	waitStart  map[string]time.Time // 单元 → 最近一次开始等待就绪的时间（诊断只看这之后的日志）
	readySec   int                  // 最近一次就绪用了几秒（部署时记下来，和上一次比）
	errorLines int                  // 最近一次启动期间服务输出里的错误行数
	cmd        string               // 正在执行的命令（deploy、rollback……）
}

// Run 执行命令，返回退出码。
func (a *Agent) Run(cmd string, req *proto.Request) int {
	a.req = req
	a.cmd = cmd
	if req.Proto != proto.Version {
		return a.result(proto.CodeBadInput, fmt.Sprintf("协议版本不一致（引擎 %d，agent %d）", req.Proto, proto.Version), nil)
	}
	if err := validateRoot(req.Root); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	a.appRoots = map[string]string{}
	for i := range req.Apps {
		if err := validateApp(&req.Apps[i]); err != nil {
			return a.result(proto.CodeBadInput, err.Error(), nil)
		}
		if r := req.Apps[i].Root; r != "" && r != req.Root {
			if err := validateRoot(r); err != nil {
				return a.result(proto.CodeBadInput, req.Apps[i].Name+"："+err.Error(), nil)
			}
			a.appRoots[req.Apps[i].Name] = r
		}
	}
	// 不停机发布的应用：单元、健康检查按正在用的槽位（状态、启停、日志都看它）
	for i := range req.Apps {
		a.applySlot(&req.Apps[i])
	}
	switch cmd {
	case "preflight":
		return a.preflight()
	case "prepare":
		return a.prepare()
	case "deploy":
		return a.deploy()
	case "rollback":
		return a.rollback()
	case "control":
		return a.control()
	case "status":
		return a.status()
	case "releases":
		return a.releases()
	case "history":
		return a.history()
	case "logruns":
		return a.logruns()
	case "runtime":
		return a.runtime()
	case "logread":
		return a.logread()
	case "logtail":
		return a.logtail()
	case "loggrep":
		return a.loggrep()
	case "software":
		return a.software()
	case "swconfig":
		return a.softwareConfig()
	case "swcheck":
		return a.swcheck()
	case "swinstall":
		return a.swinstall()
	case "repoprobe":
		return a.repoprobe()
	case "swcontrol":
		return a.swcontrol()
	case "swremove":
		return a.swremove()
	case "swlogs":
		return a.swlogs()
	case "swunit":
		return a.swunit()
	case "ports":
		return a.ports()
	case "portset":
		return a.portset()
	case "fwenable":
		return a.fwenable()
	case "fwconfirm":
		return a.fwconfirm()
	case "fwdisable":
		return a.fwdisable()
	case "monitor":
		return a.monitor()
	case "backup":
		return a.backup()
	}
	return a.result(proto.CodeBadInput, "未知命令："+cmd, nil)
}

func validateRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("部署根目录无效：%q", root)
	}
	if parent := filepath.Dir(root); parent == filepath.Dir(parent) {
		return fmt.Errorf("部署根目录太浅：%q", root)
	}
	return nil
}

var reSafe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validateApp(s *proto.AppSpec) error {
	if !reSafe.MatchString(s.Name) {
		return fmt.Errorf("应用名无效：%q", s.Name)
	}
	switch s.Type {
	case proto.TypeJava, proto.TypeBinary, proto.TypePython, proto.TypeNode, proto.TypeDotnet:
		if !proto.DirService(s.Type) && !reSafe.MatchString(s.FileName) {
			return fmt.Errorf("%s：文件名无效 %q", s.Name, s.FileName)
		}
		for _, x := range s.Executables {
			if x == "" || strings.HasPrefix(x, "/") || strings.Contains(x, "..") {
				return fmt.Errorf("%s：可执行文件必须是版本目录里的相对路径 %q", s.Name, x)
			}
		}
		if d := s.Deps; d != nil && (d.Kind != s.Type || strings.ContainsAny(d.Registry+d.Python, " '\"\n;&|$`")) {
			return fmt.Errorf("%s：deps 无效", s.Name)
		}
		if !strings.HasSuffix(s.Unit, ".service") || !reSafe.MatchString(strings.ReplaceAll(s.Unit, "@", "_")) {
			return fmt.Errorf("%s：单元名无效 %q", s.Name, s.Unit)
		}
		if !strings.HasPrefix(s.UnitText, ManagedMarker) {
			return fmt.Errorf("%s：单元文件缺少 ferry 标记", s.Name)
		}
		if s.User == "" || s.User == "root" {
			return fmt.Errorf("%s：需要非 root 的运行用户", s.Name)
		}
		for _, c := range s.Conflict {
			if !strings.HasSuffix(c, ".service") || strings.ContainsAny(c, "/ ") {
				return fmt.Errorf("%s：conflicts 无效 %q", s.Name, c)
			}
		}
		for _, d := range s.SharedDirs {
			if d == "" || strings.HasPrefix(d, "/") || strings.Contains(d, "..") {
				return fmt.Errorf("%s：sharedDirs 必须是相对路径 %q", s.Name, d)
			}
		}
		for _, d := range s.ArchiveLogs {
			if d == "" || d == "." || strings.HasPrefix(d, "/") || strings.Contains(d, "..") {
				return fmt.Errorf("%s：archiveLogs 必须是 data（shared）下的相对路径 %q", s.Name, d)
			}
		}
	case proto.TypeStatic:
		if s.NginxInstance != "" && ((!reInstName.MatchString(s.NginxInstance) && s.NginxInstance != proto.SystemNginxInstance) || s.Unit != "" || s.NginxConf == "" || s.SitePort == "") {
			return fmt.Errorf("%s：独立 nginx 站点信息无效", s.Name)
		}
		if s.Link != "" && !filepath.IsAbs(s.Link) {
			return fmt.Errorf("%s：link 必须是绝对路径", s.Name)
		}
		if s.Unit != "" { // 由 Ferry 的 nginx 运行
			if !strings.HasSuffix(s.Unit, ".service") || !reSafe.MatchString(s.Unit) {
				return fmt.Errorf("%s：单元名无效 %q", s.Name, s.Unit)
			}
			if !strings.HasPrefix(s.UnitText, ManagedMarker) {
				return fmt.Errorf("%s：单元文件缺少 ferry 标记", s.Name)
			}
			if s.User == "" || s.User == "root" {
				return fmt.Errorf("%s：需要非 root 的运行用户", s.Name)
			}
			if s.NginxConf == "" || !reNginxLink.MatchString(s.RuntimeNginx) {
				return fmt.Errorf("%s：缺少 nginx 配置", s.Name)
			}
			for _, d := range append(append([]string{}, s.SharedDirs...), s.ArchiveLogs...) {
				if d == "" || d == "." || strings.HasPrefix(d, "/") || strings.Contains(d, "..") {
					return fmt.Errorf("%s：sharedDirs 必须是相对路径 %q", s.Name, d)
				}
			}
		}
	default:
		return fmt.Errorf("%s：未知类型 %q", s.Name, s.Type)
	}
	if s.ZD != nil {
		if err := validateZD(s); err != nil {
			return err
		}
	}
	if s.KeepReleases < 2 {
		s.KeepReleases = 5
	}
	return nil
}

// ---------------------------------------------------------------- 输出

func (a *Agent) write(v any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, _ := json.Marshal(v)
	a.Out.Write(append(b, '\n'))
}

func (a *Agent) ev(level, code, app, format string, args ...any) {
	if a.Plain {
		a.mu.Lock()
		fmt.Fprintf(a.Out, "%s %s\n", app, fmt.Sprintf(format, args...))
		a.mu.Unlock()
		return
	}
	a.write(proto.Event{T: "ev", Time: a.Sys.Now().Format("15:04:05"), Level: level, Code: code, App: app,
		Msg: fmt.Sprintf(format, args...)})
}

func (a *Agent) info(code, app, format string, args ...any) { a.ev("info", code, app, format, args...) }
func (a *Agent) warn(code, app, format string, args ...any) { a.ev("warn", code, app, format, args...) }
func (a *Agent) fail(code, app, format string, args ...any) {
	a.ev("error", code, app, format, args...)
}

func (a *Agent) result(code int, msg string, data any) int {
	r := proto.Result{T: "result", RC: code, Msg: msg}
	if data != nil {
		b, err := json.Marshal(data)
		if err == nil {
			r.Data = b
		}
	}
	a.write(r)
	return code
}

// ---------------------------------------------------------------- 路径

func (a *Agent) meta() string                { return filepath.Join(a.req.Root, ".ferry") }
func (a *Agent) cacheDir() string            { return filepath.Join(a.meta(), "cache", "lib") }
func (a *Agent) incomingDir() string         { return filepath.Join(a.meta(), "incoming") }
func (a *Agent) appDir(app string) string    { return filepath.Join(a.appRoot(app), "apps", app) }
func (a *Agent) unitPath(unit string) string { return filepath.Join(a.Sys.UnitDir(), unit) }

// appRoot 应用所在的部署根目录：应用单独设置的，否则目标的。
func (a *Agent) appRoot(app string) string {
	if r := a.appRoots[app]; r != "" {
		return r
	}
	return a.req.Root
}

// ensureAppRoot 应用单独设置了部署根目录时，创建它与其下的 apps（不能是符号链接）。
func (a *Agent) ensureAppRoot(app string) error {
	r := a.appRoots[app]
	if r == "" {
		return nil
	}
	for _, d := range []string{r, filepath.Join(r, "apps")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s 是符号链接，拒绝使用", d)
		}
	}
	return nil
}

// ensureMeta 创建 .ferry 与 apps 目录（root 所有、不能是符号链接）。
func (a *Agent) ensureMeta() error {
	for _, d := range []string{a.req.Root, a.meta(), a.cacheDir(), a.incomingDir(), filepath.Join(a.req.Root, "apps")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s 是符号链接，拒绝使用", d)
		}
	}
	return nil
}

// lock 部署锁：同一个部署目录同时只能有一个部署、回滚、启停。拿到锁后记下是谁、在做什么（lock.who），
// 别人拿不到锁时告诉他「张三正在部署」；要等的话先说一声在等谁。
func (a *Agent) lock(wait time.Duration) (func(), error) {
	if err := a.ensureMeta(); err != nil {
		return nil, err
	}
	p := filepath.Join(a.meta(), "lock")
	unlock, err := a.Sys.Lock(p, 0)
	if errors.Is(err, ErrLocked) {
		h := a.lockHolder()
		if wait <= 0 {
			return nil, lockedErr(h)
		}
		if h != nil {
			a.info(proto.EvInfo, "", "%s，等对方做完…", holderText(h, a.Sys.Now()))
		}
		if unlock, err = a.Sys.Lock(p, wait); errors.Is(err, ErrLocked) {
			return nil, lockedErr(a.lockHolder())
		}
	}
	if err != nil {
		return nil, err
	}
	h := proto.LockHolder{Actor: a.req.Actor, Action: a.lockAction(), Since: a.Sys.Now(), PID: os.Getpid()}
	for _, s := range a.req.Apps {
		h.Apps = append(h.Apps, s.Name)
	}
	who := filepath.Join(a.meta(), "lock.who")
	if b, err := json.Marshal(h); err == nil {
		writeFileAtomic(who, b, 0o644)
	}
	return func() {
		os.Remove(who)
		unlock()
	}, nil
}

func (a *Agent) lockAction() string {
	switch a.cmd {
	case "deploy":
		return "部署"
	case "rollback":
		return "回滚"
	case "control":
		return map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}[a.req.Action]
	}
	return a.cmd
}

// pidAlive 进程还在不在（测试里换掉）。
var pidAlive = func(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// lockHolder 正拿着部署锁的是谁；没有人（或记录是崩溃留下的）时为 nil。
func (a *Agent) lockHolder() *proto.LockHolder {
	var h proto.LockHolder
	if !readJSON(filepath.Join(a.meta(), "lock.who"), &h) || h.PID == 0 || !pidAlive(h.PID) {
		return nil
	}
	return &h
}

func holderText(h *proto.LockHolder, now time.Time) string {
	who := h.Actor
	if who == "" {
		who = "有人"
	}
	what := h.Action
	if len(h.Apps) > 0 {
		what += " " + strings.Join(h.Apps, "、")
	}
	return fmt.Sprintf("%s正在%s（%s开始）", who, what, agoText(now.Sub(h.Since)))
}

func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	}
	return fmt.Sprintf("%d 小时前", int(d.Hours()))
}

func lockedErr(h *proto.LockHolder) error {
	if h == nil {
		return ErrLocked
	}
	return fmt.Errorf("%w：%s", ErrLocked, holderText(h, time.Now()))
}

func (a *Agent) readMeta(app, id string) *proto.ReleaseMeta {
	b, err := os.ReadFile(a.verFile(app, id, "release.json"))
	if err != nil {
		return &proto.ReleaseMeta{ID: id, App: app}
	}
	var m proto.ReleaseMeta
	if json.Unmarshal(b, &m) != nil {
		return &proto.ReleaseMeta{ID: id, App: app}
	}
	m.ID = id
	return &m
}

func (a *Agent) isFailed(app, id string) bool {
	_, err := os.Stat(a.verFile(app, id, "failed"))
	return err == nil
}

func (a *Agent) markFailed(app, id string) {
	p := a.verFile(app, id, "failed")
	os.MkdirAll(filepath.Dir(p), 0o750)
	os.WriteFile(p, []byte(a.Sys.Now().Format(time.RFC3339)+"\n"), 0o644)
}

// previousGood 比 id 旧、且没有标记失败的最近版本。
func (a *Agent) previousGood(app, id string) string {
	for _, r := range a.listReleases(app) {
		if (id == "" || r < id) && !a.isFailed(app, r) {
			return r
		}
	}
	return ""
}

func (a *Agent) addHistory(e proto.HistoryEntry) {
	e.Time = a.Sys.Now()
	if e.Actor == "" {
		e.Actor = a.req.Actor
	}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(filepath.Join(a.meta(), "history.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		a.warn(proto.EvWarn, e.App, "写部署历史失败：%v", err)
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

func (a *Agent) history() int {
	limit := a.req.Limit
	if limit <= 0 {
		limit = 50
	}
	lines, _ := tailFile(filepath.Join(a.meta(), "history.jsonl"), limit*4)
	want := map[string]bool{}
	for _, s := range a.req.Apps {
		want[s.Name] = true
	}
	var out []proto.HistoryEntry
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		var e proto.HistoryEntry
		if json.Unmarshal([]byte(lines[i]), &e) != nil {
			continue
		}
		if len(want) > 0 && !want[e.App] {
			continue
		}
		out = append(out, e)
	}
	return a.result(proto.CodeOK, "", out)
}

// chownTree 把目录树（不跟随符号链接）交给运行用户。
func (a *Agent) chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return a.Sys.Lchown(p, uid, gid)
	})
}
