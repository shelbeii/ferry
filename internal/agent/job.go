package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 定时任务（配置里的 job）：服务单元是 oneshot、不开机自启，由 <服务名>-schedule.timer 按时启动。
// 部署、回滚时换的是版本和单元（服务 + 定时器，都随版本保存），然后打开定时器——不启动服务；
// runOnDeploy 时马上运行一次，成功才算成功。在 Ferry 里「停止」= 关掉定时器，「启动」= 打开。

const jobTimerFile = "job.timer" // 版本记录里随版本保存的定时器单元

// isJob 定时任务。
func isJob(s *proto.AppSpec) bool { return s.Job != nil }

// jobTimerText 版本 id 的定时器单元；那个版本不是定时任务时为空。
func (a *Agent) jobTimerText(s *proto.AppSpec, id string) []byte {
	if b, err := os.ReadFile(a.verFile(s.Name, id, jobTimerFile)); err == nil {
		return b
	}
	return nil
}

// applyJob 版本 id 已经换上、服务单元已经写好之后：写它的定时器、打开（除非在 Ferry 里停着）。
// runOnDeploy 时马上运行一次，失败返回错误。
func (a *Agent) applyJob(s *proto.AppSpec, id string) error {
	text := a.jobTimerText(s, id)
	if text == nil {
		text = []byte(s.Job.TimerText)
	}
	if err := a.installUnit(s.Name, s.Job.Timer, text); err != nil {
		return err
	}
	a.Sys.Systemctl("disable", s.Unit) // 以前是常驻服务时设的开机自启
	a.removeBootTimer(s.Unit)
	if a.isStopped(s.Name) {
		a.info(proto.EvInfo, s.Name, "在 Ferry 里停着：定时器先不打开（「启动」后按时运行）")
		return nil
	}
	if err := a.Sys.Systemctl("enable", "--now", s.Job.Timer); err != nil {
		return fmt.Errorf("打开定时器 %s 失败：%v（看看 job.schedule 写得对不对）", s.Job.Timer, err)
	}
	a.Sys.Systemctl("restart", s.Job.Timer) // 时间表改了：按新的重新算下次
	if next := a.jobNext(s.Job.Timer); next != "" {
		a.info(proto.EvReady, s.Name, "定时器已打开，下次运行：%s", shortTime(sdTimeISO(next)))
	} else {
		a.info(proto.EvReady, s.Name, "定时器已打开")
	}
	if s.Job.RunOnDeploy {
		run := a.runJob(s)
		if !run.OK {
			return fmt.Errorf("部署后运行一次失败：%s", jobResultText(run))
		}
	}
	return nil
}

// removeJobTimer 不再是定时任务（回到常驻服务、回到接管前、卸载）：关掉、删掉定时器。
func (a *Agent) removeJobTimer(unit string) {
	t := strings.TrimSuffix(unit, ".service") + "-schedule.timer"
	b, err := os.ReadFile(a.unitPath(t))
	if err != nil || !strings.HasPrefix(string(b), ManagedMarker) {
		return
	}
	a.Sys.Systemctl("disable", "--now", t)
	os.Remove(a.unitPath(t))
	a.Sys.Systemctl("daemon-reload")
}

// stopJob 在 Ferry 里停止：关掉定时器；正在运行的这次也停掉。
func (a *Agent) stopJob(s *proto.AppSpec) error {
	err := a.Sys.Systemctl("disable", "--now", s.Job.Timer)
	if p := a.Sys.UnitProps(s.Unit); p.Active == "activating" || p.Active == "active" {
		a.info(proto.EvStop, s.Name, "正在运行的这次也停掉")
		a.Sys.Systemctl("stop", s.Unit)
	}
	return err
}

// startJob 在 Ferry 里启动：打开定时器（不马上运行；要马上运行用 run-job）。
func (a *Agent) startJob(s *proto.AppSpec) error {
	if _, err := os.Stat(a.unitPath(s.Job.Timer)); err != nil {
		return fmt.Errorf("定时器 %s 不存在：重新部署一次", s.Job.Timer)
	}
	if err := a.Sys.Systemctl("enable", "--now", s.Job.Timer); err != nil {
		return err
	}
	if next := a.jobNext(s.Job.Timer); next != "" {
		a.info(proto.EvReady, s.Name, "定时器已打开，下次运行：%s", shortTime(sdTimeISO(next)))
	}
	return nil
}

func (a *Agent) jobNext(timer string) string {
	out, err := a.Sys.Shell("systemctl show -p NextElapseUSecRealtime --value "+shellQuote(timer)+" 2>/dev/null", 10*time.Second)
	if err != nil {
		return ""
	}
	return sdNext(out)
}

// sdNext 定时器的下次运行时间（NextElapseUSecRealtime），没有时返回 ""。新的 systemd 写成日期（Fri 2026-11-13 10:15:00 CST）；
// 老的（CentOS 7 的 219）写成从 1970 年起算的时长（56y 10month 1w 4d 17h 15min），换成同样的日期写法（服务器的时区）。
func sdNext(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "n/a" || v == "0" {
		return ""
	}
	if us, ok := sdTimespan(v); ok {
		return time.UnixMicro(us).Format("Mon 2006-01-02 15:04:05 MST")
	}
	return v
}

var sdSpanPart = regexp.MustCompile(`^(\d+(?:\.\d+)?)(y|month|w|d|h|min|s|ms|us)$`)

// sdSpanUnit systemd 时长里每个单位是多少微秒（年、月按 365.25 天、30.44 天算，和 systemd 一样）。
var sdSpanUnit = map[string]float64{
	"y": 31557600e6, "month": 2629800e6, "w": 604800e6, "d": 86400e6, "h": 3600e6, "min": 60e6, "s": 1e6, "ms": 1e3, "us": 1,
}

// sdTimespan systemd 的时长（56y 10month 1w 4d 17h 15min）换成微秒；不是这种写法时 ok 为 false。
func sdTimespan(v string) (int64, bool) {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0, false
	}
	var us float64
	for _, p := range f {
		m := sdSpanPart.FindStringSubmatch(p)
		if m == nil {
			return 0, false
		}
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, false
		}
		us += n * sdSpanUnit[m[2]]
	}
	return int64(us + 0.5), true
}

// jobStatus 定时器开没开、下次运行、上次运行的结果。
func (a *Agent) jobStatus(s *proto.AppSpec) *proto.JobStatus {
	st := &proto.JobStatus{Schedule: s.Job.Schedule}
	out, _ := a.Sys.Shell(jobShowCmd(s.Job.Timer, s.Unit), 10*time.Second)
	timer, svc, _ := strings.Cut(out, "---")
	tp, sp := showProps(timer), showProps(svc)
	st.TimerActive = tp["ActiveState"] == "active"
	if v := sdNext(tp["NextElapseUSecRealtime"]); v != "" {
		st.Next = sdTimeISO(v)
	}
	st.Running = sp["ActiveState"] == "activating"
	if start := sp["ExecMainStartTimestamp"]; start != "" && start != "n/a" {
		code, _ := strconv.Atoi(sp["ExecMainStatus"])
		run := &proto.JobRun{Start: start, Result: sp["Result"], ExitCode: code}
		if !st.Running {
			run.End = sp["ExecMainExitTimestamp"]
			run.OK = run.Result == "success"
			if t0, t1, ok := parseSDTimes(run.Start, run.End); ok {
				run.Sec = t1.Sub(t0).Seconds()
			}
			run.End = sdTimeISO(run.End)
			st.Last = run
		}
		run.Start = sdTimeISO(run.Start)
	}
	return st
}

// sdTimeISO systemd 的时间（Thu 2026-11-12 02:30:00 CST，服务器的时区）换成带时区的 RFC 3339，
// 桌面版再换成这台电脑的时间（服务器在别的时区时也对得上）；看不懂的原样返回。
func sdTimeISO(s string) string {
	f := strings.Fields(s)
	if len(f) < 3 {
		return s
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", f[1]+" "+f[2], time.Local)
	if err != nil {
		return s
	}
	return t.Format(time.RFC3339)
}

// shortTime 给人看的「11-12 02:30」（RFC 3339 或 systemd 的写法都行）。
func shortTime(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format("01-02 15:04")
	}
	return sinceShort(s)
}

// jobShowCmd 一次读出定时器和服务的状态（中间用 --- 隔开）。
func jobShowCmd(timer, unit string) string {
	return "systemctl show " + shellQuote(timer) + " -p ActiveState -p NextElapseUSecRealtime 2>/dev/null; echo ---; " +
		"systemctl show " + shellQuote(unit) + " -p ActiveState -p Result -p ExecMainStatus -p ExecMainStartTimestamp -p ExecMainExitTimestamp 2>/dev/null"
}

// showProps systemctl show 的 键=值。
func showProps(s string) map[string]string {
	m := map[string]string{}
	for _, l := range splitLines(s) {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			m[k] = v
		}
	}
	return m
}

// parseSDTimes systemd 的时间（Thu 2026-11-12 02:30:00 CST）：只用来算差，时区一样就行。
func parseSDTimes(a, b string) (time.Time, time.Time, bool) {
	p := func(s string) (time.Time, bool) {
		f := strings.Fields(s)
		if len(f) < 3 {
			return time.Time{}, false
		}
		t, err := time.Parse("2006-01-02 15:04:05", f[1]+" "+f[2])
		return t, err == nil
	}
	t0, ok0 := p(a)
	t1, ok1 := p(b)
	return t0, t1, ok0 && ok1
}

func jobResultText(r proto.JobRun) string {
	switch {
	case r.OK:
		return fmt.Sprintf("成功（%.0f 秒）", r.Sec)
	case r.Result == "timeout":
		return "超过最长运行时间（job.timeout），被停掉了"
	case r.Result == "exit-code":
		return fmt.Sprintf("退出码 %d", r.ExitCode)
	case r.Result == "signal" || r.Result == "core-dump":
		return "被信号杀掉了（" + r.Result + "）"
	case r.Result == "":
		return "没有运行"
	}
	return r.Result
}

// runJob 现在运行一次：启动服务，边跑边把它的输出发出去，等它结束，返回结果。
func (a *Agent) runJob(s *proto.AppSpec) proto.JobRun {
	if p := a.Sys.UnitProps(s.Unit); p.Active == "activating" {
		a.warn(proto.EvWarn, s.Name, "上一次还在运行：等它结束")
	}
	start := a.Sys.Now()
	a.info(proto.EvStart, s.Name, "运行 %s", s.Unit)
	if err := a.Sys.Systemctl("start", "--no-block", s.Unit); err != nil {
		return proto.JobRun{Result: "start-failed: " + err.Error()}
	}
	limit := time.Duration(max(s.Job.TimeoutSec, 1))*time.Second + time.Minute
	seen := 0
	emit := func() {
		lines := a.Sys.Journal(s.Unit, 2000, start.Add(-time.Second))
		for _, l := range lines[min(seen, len(lines)):] {
			a.info(proto.EvLogs, s.Name, "%s", l)
		}
		seen = max(seen, len(lines))
	}
	for began := false; ; {
		a.Sys.Sleep(time.Second)
		emit()
		p := a.Sys.UnitProps(s.Unit)
		if p.Active == "activating" || p.Active == "active" {
			began = true
		} else if began || a.Sys.Now().Sub(start) > 5*time.Second {
			break // 跑完了（或者太快，一秒内就结束了）
		}
		if a.Sys.Now().Sub(start) > limit {
			a.warn(proto.EvWarn, s.Name, "超过 %s 还没结束：不等了（它还在服务器上运行）", limit)
			break
		}
	}
	emit()
	st := a.jobStatus(s)
	run := proto.JobRun{Result: "unknown"}
	if st.Last != nil {
		run = *st.Last
	}
	if run.Sec == 0 {
		run.Sec = a.Sys.Now().Sub(start).Seconds()
	}
	if run.OK {
		a.info(proto.EvReady, s.Name, "运行完了：%s", jobResultText(run))
	} else {
		a.fail(proto.EvFail, s.Name, "运行失败：%s", jobResultText(run))
	}
	return run
}

// runjob 命令：现在运行一次定时任务。
func (a *Agent) runjobCmd() int {
	if len(a.req.Apps) != 1 || !isJob(&a.req.Apps[0]) {
		return a.result(proto.CodeBadInput, "只能运行一个定时任务（配置里写了 job 的应用）", nil)
	}
	s := &a.req.Apps[0]
	if a.current(s.Name) == "" {
		return a.result(proto.CodeFailed, s.Name+" 还没有部署过", nil)
	}
	run := a.runJob(s)
	a.addHistory(proto.HistoryEntry{Action: "run", App: s.Name, Release: a.current(s.Name), Result: map[bool]string{true: "OK", false: "FAILED"}[run.OK], Msg: jobResultText(run)})
	if !run.OK {
		return a.result(proto.CodeFailed, s.Name+" 运行失败："+jobResultText(run), run)
	}
	return a.result(proto.CodeOK, "", run)
}

// saveJobTimer 部署时把定时器单元随版本保存（回滚时用那个版本的时间表）。
func saveJobTimer(meta string, s *proto.AppSpec) error {
	if s.Job == nil {
		return nil
	}
	return os.WriteFile(filepath.Join(meta, jobTimerFile), []byte(s.Job.TimerText), 0o644)
}

// unitIsJob 单元文件第二行带 job=yes（定时任务）。
func unitIsJob(path string) bool {
	b, _ := os.ReadFile(path)
	for i, l := range strings.Split(string(b), "\n") {
		if i > 3 {
			break
		}
		if strings.HasPrefix(l, "# project=") {
			for _, f := range strings.Fields(l) {
				if f == "job=yes" {
					return true
				}
			}
		}
	}
	return false
}

// jobFindings 服务器告警：定时任务的定时器被关了（到点不会运行）、上次运行失败了。
func (a *Agent) jobFindings(unit, app, entry string) []finding {
	s := &proto.AppSpec{Name: app, Unit: unit, Job: &proto.JobSpec{Timer: strings.TrimSuffix(unit, ".service") + "-schedule.timer"}}
	st := a.jobStatus(s)
	var out []finding
	if !st.TimerActive {
		out = append(out, finding{proto.MonitorProblem{Key: "job-timer:" + unit, Level: "bad",
			Text: fmt.Sprintf("定时任务 %s 的定时器没开（%s）：到点不会运行。在 Ferry 里「启动」它，或重新部署", app, s.Job.Timer)}, entry})
	}
	if r := st.Last; r != nil && !r.OK && !st.Running {
		out = append(out, finding{proto.MonitorProblem{Key: "job:" + unit, Level: "bad",
			Text: fmt.Sprintf("定时任务 %s 上次运行失败（%s 开始）：%s，看看它的日志", app, shortTime(r.Start), jobResultText(*r))}, entry})
	}
	return out
}
