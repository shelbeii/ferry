package mcpserver

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/dbx"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/flock"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/safe"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/taskhist"
)

func confirmChannelLabel(ch string) string {
	switch ch {
	case "mcp-app":
		return "编程软件卡片确认"
	case "elicitation":
		return "编程软件表单确认"
	case "two-step":
		return "编程软件两段式确认"
	default:
		return "客户端确认"
	}
}

// submitDirect 当 Ferry 桌面版未打开、且用户已在编程软件内确认允许离线执行的操作时，
// 由 ferry mcp 后台协程直接调用 engine 执行（同样支持进度推送、40 秒超时转 task_status 轮询、FERRY_VIA 审计与调用日志）。
func (s *server) submitDirect(r approve.Request, ev approve.ConfirmEvidence) *op {
	r.ClientConfirmed = true
	r.ConfirmChannel = ev.Channel
	r.ConfirmNote = ev.Note
	d := approve.Decision{
		Approve: true,
		Note:    cmp.Or(ev.Note, fmt.Sprintf("用户已在 %s 里确认（%s，离线直接执行）", orDash(r.Client), confirmChannelLabel(ev.Channel))),
	}
	now := time.Now()
	o := &op{req: r, started: now, state: "running", decision: &d, done: make(chan struct{})}
	s.mu.Lock()
	s.sweepOpsLocked(now)
	s.ops[r.ID] = o
	s.mu.Unlock()
	s.runDirectOnOp(o, r, ev)
	return o
}

func (s *server) runDirectOnOp(o *op, r approve.Request, ev approve.ConfirmEvidence) {
	safe.Go("客户端确认直接执行", func() {
		s.execDirectIntoOp(o, r, ev)
	})
}

func (s *server) execDirectIntoOp(o *op, r approve.Request, ev approve.ConfirmEvidence) {
	r.ClientConfirmed = true
	r.ConfirmChannel = ev.Channel
	r.ConfirmNote = ev.Note
	d := approve.Decision{
		Approve: true,
		Note:    cmp.Or(ev.Note, fmt.Sprintf("用户已在 %s 里确认（%s，离线直接执行）", orDash(r.Client), confirmChannelLabel(ev.Channel))),
	}
	execCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedMs := time.Now().UnixMilli()
	o.mu.Lock()
	o.req = r
	o.state = "running"
	o.decision = &d
	o.cancelFn = cancel
	o.mu.Unlock()

	taskID := directTaskHistID(r.ID)
	taskTitle := fmt.Sprintf("%s（%s 请求 · 离线执行）", opTitle(r), orDash(r.Client))
	saveDirectTaskHist(taskID, taskTitle, r, taskhist.Running, startedMs, 0, "", nil, nil)

	rep := newDirectReporter(o.addProgress)
	data, execErr := ExecuteRequest(execCtx, r, ExecOptions{
		Rep:           rep,
		Pool:          s.pool,
		Getenv:        s.directGetenv(r, ev),
		DesktopOnline: false,
	})
	var out approve.Outcome
	out.Log = rep.tail()
	if data != nil {
		out.Data, _ = json.Marshal(data)
	}
	if execErr != nil {
		out.OK = false
		if execCtx.Err() != nil {
			out.Error = "任务已停止"
		} else {
			out.Error = execErr.Error()
		}
	} else {
		out.OK = true
	}
	status := taskhist.OK
	if !out.OK {
		status = taskhist.Failed
	}
	saveDirectTaskHist(taskID, taskTitle, r, status, startedMs, time.Now().UnixMilli(), out.Error, out.Data, out.Log)
	s.finishOp(o, r, d, &out, nil)
}

func directTaskHistID(opID string) string {
	clean := strings.ToLower(strings.TrimSpace(opID))
	var b strings.Builder
	b.WriteString("mcp-")
	for _, r := range clean {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || r == '-' {
			b.WriteRune(r)
		}
	}
	id := b.String()
	if len(id) < 6 {
		id += newID()
	}
	if len(id) > 40 {
		id = id[:40]
	}
	return id
}

// saveDirectTaskHist 把离线直接执行的 MCP 改动写入桌面端任务记录（data/tasks/），下次打开 Ferry 桌面版即可查看。
func saveDirectTaskHist(id, title string, r approve.Request, status string, startedAt, endedAt int64, errMsg string, data json.RawMessage, logLines []string) {
	if r.Action == "create" || r.Action == "config-edit" || r.Action == "secret" {
		return
	}
	type ferryLine struct {
		Kind string `json:"kind"`
		Msg  string `json:"msg"`
	}
	lines := make([]ferryLine, 0, len(logLines))
	for _, ln := range logLines {
		kind := "info"
		msg := ln
		switch {
		case strings.HasPrefix(ln, "==> "):
			kind, msg = "step", strings.TrimPrefix(ln, "==> ")
		case strings.HasPrefix(ln, "✗ "):
			kind, msg = "error", strings.TrimPrefix(ln, "✗ ")
		case strings.HasPrefix(ln, "! "):
			kind, msg = "warn", strings.TrimPrefix(ln, "! ")
		}
		lines = append(lines, ferryLine{Kind: kind, Msg: msg})
	}
	linesJSON, _ := json.Marshal(lines)
	rec := &taskhist.Record{
		ID:        id,
		Title:     title,
		Status:    status,
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Error:     errMsg,
		Config:    r.Config,
		Target:    r.Target,
		Data:      data,
		Lines:     linesJSON,
	}
	if r.Action == "install" {
		rec.InstallID = r.ID
	}
	dir := appdir.SubDir("tasks")
	_ = taskhist.Save(dir, rec)
	if status != taskhist.Running {
		st, _ := loadSettingsCached()
		taskhist.Clean(dir, st.UI.TaskKeepDays, int64(st.UI.TaskMaxMB)<<20, st.UI.TaskKeep, id)
	}
}

func (s *server) directGetenv(r approve.Request, ev approve.ConfirmEvidence) func(string) string {
	via := fmt.Sprintf("%s · MCP（%s）", orDash(r.Client), confirmChannelLabel(ev.Channel))
	base := s.o.Getenv
	if base == nil {
		base = os.Getenv
	}
	return func(k string) string {
		switch k {
		case "FERRY_VIA":
			return via
		case "FERRY_TASK_ID":
			if r.Action == "install" {
				return r.ID
			}
		}
		return base(k)
	}
}

// ExecOptions 统一执行已确认 MCP 改动请求的参数（桌面端 ferry mcp-exec 与离线直连执行共用）。
type ExecOptions struct {
	Rep           engine.Reporter
	Pool          *engine.Pool
	Getenv        func(string) string
	DesktopOnline bool // true = 由运行中的桌面端执行（允许需要桌面端交互/凭据的操作；create 由桌面端 addProject 写入项目列表）
}

// ExecuteRequest 执行一条已经用户确认的 MCP 改动请求（桌面端 ferry mcp-exec 与离线直接执行共用同一套校验与引擎调用）。
func ExecuteRequest(ctx context.Context, r approve.Request, o ExecOptions) (any, error) {
	if !o.DesktopOnline {
		if reason := desktopOnlyExecReason(r); reason != "" {
			return nil, desktopRequiredError(reason)
		}
	}
	rep := o.Rep
	if rep == nil {
		rep = quiet{}
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	st, _ := settings.Load()

	switch r.Action {
	case "create":
		return executeCreate(r, !o.DesktopOnline)
	case "config-edit":
		return executeConfigEdit(st, r)
	case "db-exec":
		return executeDB(ctx, st, r, rep, o.Pool, getenv)
	}

	f, err := config.LoadWithLocal(r.Config)
	if err != nil {
		return nil, err
	}
	t, err := f.Target(r.Target)
	if err != nil {
		return nil, err
	}
	if err := ChangeAllowed(st, f, t); err != nil {
		return nil, err
	}
	e := &engine.Engine{
		File:             f,
		Rep:              rep,
		Yes:              true,
		Settings:         st,
		AcceptNewHostKey: st.Connect.AcceptNewHostKey,
		Pool:             o.Pool,
		Ctx:              ctx,
		Getenv:           getenv,
	}

	if tool, ok := aiActionTool[r.Action]; ok {
		if r.Tool != tool {
			return nil, fmt.Errorf("操作和工具对不上：%s / %s", r.Action, r.Tool)
		}
		out, err := e.AIExec(t.Name, engine.MCPScope(r.Session), tool, r.Input, r.AIState, r.AIScope, func(text string) {
			for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
				if strings.TrimSpace(ln) != "" {
					rep.Info("%s", ln)
				}
			}
		})
		if err != nil {
			if out != "" {
				return nil, fmt.Errorf("%s\n%v", out, err)
			}
			return nil, err
		}
		return map[string]string{"output": out}, nil
	}

	switch r.Action {
	case "deploy":
		if err := checkTargets(st, f, &r); err != nil {
			return nil, err
		}
		targets := []string{t.Name}
		if len(r.Targets) > 0 {
			targets = r.Targets
		}
		var keep map[string][]*config.KeepItem
		if len(r.Keep) > 0 {
			if err := json.Unmarshal(r.Keep, &keep); err != nil {
				return nil, fmt.Errorf("keep 参数格式不对：%v", err)
			}
		}
		opts := engine.DeployOptions{
			SkipBuild:  r.SkipBuild,
			Full:       r.Full,
			NoRollback: r.NoRollback,
			KeepPerms:  r.KeepPerms,
			Keep:       keep,
			Rollout:    rolloutOpts(r),
		}
		return e.Deploy(targets, r.Apps, opts)

	case "start", "stop", "restart":
		return e.Control(t.Name, r.Apps, r.Action)

	case "rollback":
		if len(r.Apps) != 1 {
			return nil, errors.New("回滚一次只能一个应用")
		}
		if err := e.Rollback(t.Name, r.Apps[0], r.To); err != nil {
			return nil, err
		}
		return map[string]any{"target": t.Name, "app": r.Apps[0], "to": r.To, "result": "OK"}, nil

	case "sw-start", "sw-stop", "sw-restart":
		if err := e.SoftwareControl(t.Name, r.Software, r.Action[3:]); err != nil {
			return nil, err
		}
		return map[string]any{"target": t.Name, "software": r.Software, "action": r.Action[3:], "result": "OK"}, nil

	case "port-open", "port-close":
		m := rePortSpec.FindStringSubmatch(r.Port)
		if m == nil {
			return nil, fmt.Errorf("端口不对：%q", r.Port)
		}
		portNum, _ := strconv.Atoi(m[1])
		return e.PortSet(t.Name, portNum, m[2], r.Action[5:])

	case "fw-enable":
		return e.FirewallEnable(t.Name, r.Allow, r.RestartDocker)

	case "ssh-kick":
		return e.SSHKick(t.Name, r.Pids)

	case "backup-run":
		return e.Backup(t.Name, "run", engine.BackupOptions{Name: r.Backup})

	default:
		return nil, fmt.Errorf("不支持的操作：%s", r.Action)
	}
}

func executeCreate(r approve.Request, registerProject bool) (any, error) {
	content, err := generateProject(r.Spec, r.Config)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(r.Config), 0o755); err != nil {
		return nil, err
	}
	// 原子创建，绝不覆盖已有 ferry.yaml
	f, err := os.OpenFile(r.Config, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	_, werr := f.WriteString(content)
	cerr := f.Close()
	if werr != nil {
		return nil, werr
	}
	if cerr != nil {
		return nil, cerr
	}
	if registerProject {
		// 加入 Ferry 项目列表（跨进程加锁，防止与桌面端并发改设置丢数据），下次打开桌面版即可看到
		if unlock, lerr := flock.Lock(filepath.Join(appdir.Dir(), "settings.lock"), 3*time.Second, nil); lerr == nil {
			defer unlock()
		}
		if st, err := settings.Load(); err == nil {
			exists := false
			for _, p := range st.Desktop.Projects {
				if samePath(p.Path, r.Config) {
					exists = true
					break
				}
			}
			if !exists {
				st.Desktop.Projects = append(st.Desktop.Projects, settings.Project{Path: r.Config, Name: r.Project})
				_ = settings.Save(st)
			}
		}
	}
	return map[string]any{
		"path":    filepath.ToSlash(r.Config),
		"project": r.Project,
		"note":    "已生成配置文件并加进 Ferry 的项目列表；新服务器第一次连接前要在 Ferry 里核对主机公钥",
	}, nil
}

func executeConfigEdit(st settings.Settings, r approve.Request) (any, error) {
	if !st.MCP.AllowChanges {
		return nil, errors.New("Ferry 没有允许编程软件改动")
	}
	var in engine.ConfigEditInput
	if err := json.Unmarshal(r.Input, &in); err != nil {
		return nil, fmt.Errorf("参数格式不对：%v", err)
	}
	plan, err := engine.PlanConfigEdit(r.Config, in)
	if err != nil {
		return nil, err
	}
	if err := engine.ConfigPlanProblem(plan); err != nil {
		return nil, err
	}
	if in.Base != "" && in.Base != plan.Base {
		return nil, fmt.Errorf("%s 在确认之后被改过了，没有写入：请重新发起", plan.File)
	}
	if err := plan.Write(); err != nil {
		return nil, err
	}
	return engine.ConfigPlanResult(plan, true), nil
}

func executeDB(ctx context.Context, st settings.Settings, r approve.Request, rep engine.Reporter, pool *engine.Pool, getenv func(string) string) (any, error) {
	cn, err := dbx.Get(r.DB)
	if err != nil {
		return nil, err
	}
	if r.DBCheck != "" && cn.ApprovalKey() != r.DBCheck {
		return nil, errors.New("数据库连接配置在确认之后被改过了，没有执行：请重新发起")
	}
	if r.DBKind != "" && r.DBKind != cn.Kind {
		return nil, errors.New("数据库连接类型已改动，请重新发起")
	}
	f, t, err := DBChangeAllowed(st, cn)
	if err != nil {
		return nil, err
	}
	if err := dbx.CheckAIWriteStmts(cn.Kind, r.Database, r.Stmts); err != nil {
		return nil, err
	}
	e := &engine.Engine{Rep: rep, Settings: st, AcceptNewHostKey: st.Connect.AcceptNewHostKey, Pool: pool, Ctx: ctx, Getenv: getenv}
	cl, release, err := e.OpenAIDB(cn)
	if err != nil {
		return nil, err
	}
	defer release()
	unit := "语句"
	if cn.Kind == dbx.Redis {
		unit = "命令"
	} else if cn.Kind == dbx.MongoDB {
		unit = "操作"
	}
	rep.Step("在 %s 执行 %d 条%s", cn.Name, len(r.Stmts), unit)
	res, err := cl.AIExec(ctx, r.Database, r.Stmts)
	if err == nil && !res.OK {
		err = engine.AIExecError(res)
	}
	// 记录审计（与桌面版 cmd/ferry/db.go 保持一致）
	kind, obj := "db-exec", cn.Name
	if cn.Kind == dbx.Redis {
		kind = "redis-exec"
	}
	if r.Database != "" {
		obj += " / " + r.Database
	}
	detail := strings.Join(r.Stmts, ";\n")
	if f != nil && t != nil {
		ae := &engine.Engine{File: f, Rep: quiet{}, Settings: st, AcceptNewHostKey: st.Connect.AcceptNewHostKey, Pool: pool, Ctx: ctx, Getenv: getenv}
		ae.Audit(t.Name, kind, obj, detail, err)
	} else {
		engine.AuditLocal(e.Actor(), kind, obj, detail, err)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ---------------------------------------------------------------- directReporter

// directReporter 收集直接执行时的最近 60 行日志，并实时推送进展（供 MCP NotifyProgress 和 task_status 使用）。
type directReporter struct {
	mu       sync.Mutex
	lines    []string
	onLine   func(string)
	buildBuf *lineWriter
}

func newDirectReporter(onLine func(string)) *directReporter {
	dr := &directReporter{onLine: onLine}
	dr.buildBuf = &lineWriter{onLine: func(s string) { dr.append(s, false) }}
	return dr
}

func (d *directReporter) append(line string, notify bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	d.mu.Lock()
	d.lines = append(d.lines, line)
	if len(d.lines) > 60 {
		d.lines = d.lines[len(d.lines)-60:]
	}
	fn := d.onLine
	d.mu.Unlock()
	if notify && fn != nil {
		fn(line)
	}
}

func (d *directReporter) tail() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.lines))
	copy(out, d.lines)
	return out
}

func (d *directReporter) Step(format string, args ...any) {
	d.append("==> "+fmt.Sprintf(format, args...), true)
}
func (d *directReporter) Info(format string, args ...any) {
	d.append(fmt.Sprintf(format, args...), true)
}
func (d *directReporter) Warn(format string, args ...any) {
	d.append("! "+fmt.Sprintf(format, args...), true)
}
func (d *directReporter) Error(format string, args ...any) {
	d.append("✗ "+fmt.Sprintf(format, args...), true)
}
func (d *directReporter) Event(app string, ev proto.Event) {
	msg := strings.TrimSpace(ev.Code + " " + app + " " + ev.Msg)
	d.append(msg, true)
}
func (d *directReporter) Progress(string, int64, int64) {}
func (d *directReporter) BuildOutput() io.Writer        { return d.buildBuf }
func (d *directReporter) Confirm(string) bool           { return false }
func (d *directReporter) Secret(string) (string, bool)  { return "", false }

type lineWriter struct {
	mu     sync.Mutex
	buf    string
	onLine func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf += string(p)
	sc := bufio.NewScanner(strings.NewReader(w.buf))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if strings.HasSuffix(w.buf, "\n") {
		w.buf = ""
	} else if len(lines) > 0 {
		w.buf = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	}
	for _, ln := range lines {
		w.onLine(ln)
	}
	return len(p), nil
}
