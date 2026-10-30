package engine

import (
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"golang.org/x/crypto/ssh"
)

// specs 选中应用的 AppSpec。
func (e *Engine) specs(targetName string, names []string) (*config.Target, []proto.AppSpec, error) {
	t, err := e.File.Target(targetName)
	if err != nil {
		return nil, nil, err
	}
	apps, err := e.File.Select(t, names)
	if err != nil {
		return nil, nil, err
	}
	out := make([]proto.AppSpec, 0, len(apps))
	for _, a := range apps {
		s, err := Spec(e.File, t, a)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, s)
	}
	return t, out, nil
}

func (e *Engine) withSession(targetName string, names []string, fn func(*session, []proto.AppSpec) error) error {
	t, specs, err := e.specs(targetName, names)
	if err != nil {
		return err
	}
	s, err := e.connect(t)
	if err != nil {
		return err
	}
	defer s.close()
	return fn(s, specs)
}

// Status 应用状态。
func (e *Engine) Status(target string, names []string) ([]proto.AppStatus, error) {
	var out []proto.AppStatus
	err := e.withSession(target, names, func(s *session, specs []proto.AppSpec) error {
		return s.callData("status", &proto.Request{Apps: specs}, &out)
	})
	return out, err
}

// Preflight 服务器环境检查。
func (e *Engine) Preflight(target string, names []string) (*proto.Preflight, error) {
	var out proto.Preflight
	err := e.withSession(target, names, func(s *session, specs []proto.AppSpec) error {
		fix := agentFixPlan
		if e.Settings.Deploy.KeepPerms {
			fix = ""
		}
		if err := s.callData("preflight", &proto.Request{Apps: specs, FixPerms: fix}, &out); err != nil {
			return err
		}
		// 单独的预检只列出部署时会自动修正的权限，不修改服务器
		return nil
	})
	return &out, err
}

// Control 启动 / 停止 / 重启。
func (e *Engine) Control(target string, names []string, action string) ([]proto.AppOutcome, error) {
	var out proto.DeployOutcome
	err := e.withSession(target, names, func(s *session, specs []proto.AppSpec) error {
		if err := e.checkDeployer(s.t, map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}[action]); err != nil {
			return err
		}
		if s.t.Production && action != "start" && !e.Yes {
			var list []string
			for _, a := range specs {
				list = append(list, a.Name)
			}
			label := map[string]string{"stop": "停止", "restart": "重启"}[action]
			if !e.confirmProduction(fmt.Sprintf("%s %s 上的 %s？", label, s.t.Name, strings.Join(list, ", ")), []string{s.t.Name}) {
				return ErrCanceled
			}
		}
		return s.callData("control", &proto.Request{Apps: specs, Action: action, LockWaitSec: 300}, &out)
	})
	return out.Apps, err
}

// Releases 各应用的版本。
func (e *Engine) Releases(target string, names []string) ([]proto.AppReleases, error) {
	var out []proto.AppReleases
	err := e.withSession(target, names, func(s *session, specs []proto.AppSpec) error {
		return s.callData("releases", &proto.Request{Apps: specs}, &out)
	})
	return out, err
}

// Rollback 回滚一个应用到 to（空 = 上一个可用版本；legacy = 接管前）。
func (e *Engine) Rollback(target, app, to string) error {
	return e.withSession(target, []string{app}, func(s *session, specs []proto.AppSpec) error {
		if len(specs) != 1 {
			return fmt.Errorf("回滚一次只能指定一个应用")
		}
		if err := e.checkDeployer(s.t, "回滚"); err != nil {
			return err
		}
		if (s.t.NeedConfirm() || s.t.Production) && !e.Yes {
			dest := to
			if dest == "" {
				dest = "上一个可用版本"
			}
			prod := []string{}
			if s.t.Production {
				prod = append(prod, s.t.Name)
			}
			if !e.confirmProduction(fmt.Sprintf("回滚 %s@%s 到 %s？", specs[0].Name, s.t.Name, dest), prod) {
				return ErrCanceled
			}
		}
		e.Rep.Step("回滚 %s", specs[0].Name)
		start := time.Now()
		var out proto.AppOutcome
		err := s.callData("rollback", &proto.Request{Apps: specs, To: to}, &out)
		e.notifyRollback(s.t, specs[0].Name, out.Release, start, err)
		return err
	})
}

// History 服务器上的部署历史。
func (e *Engine) History(target string, names []string, limit int) ([]proto.HistoryEntry, error) {
	var out []proto.HistoryEntry
	err := e.withSession(target, names, func(s *session, specs []proto.AppSpec) error {
		if len(names) == 0 {
			specs = nil
		}
		return s.callData("history", &proto.Request{Apps: specs, Limit: limit}, &out)
	})
	return out, err
}

// HostKey 取目标的主机公钥（ferry trust）。
func (e *Engine) HostKey(target string) (*config.Target, ssh.PublicKey, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, nil, err
	}
	k, err := remote.ScanHostKey(t.Host, t.Port)
	return t, k, err
}

// LogOptions 查看日志的选项。
type LogOptions struct {
	Lines  int
	Follow bool   // 持续输出（只用于当前运行）
	File   bool   // 看日志文件（Path，默认 diagLogs 的第一个）；否则看 journald
	Path   string // 相对 shared；可以有通配符（看最近修改的那个）
	Around int    // 看第 Around 行附近（搜索结果点进来）
	// 以往的运行：Release 为版本，Run 为归档名（空 = 该版本最近一次）。都为空 = 当前运行
	Release, Run string
	// Loaded 最后几行都输出了、开始持续输出前调用（桌面版据此不再显示「读取中」）
	Loaded func()
}

// Logs 输出应用日志。当前运行：journald 从这次运行开始的部分，或日志文件（可持续输出）；
// 以往的运行：journald 在那次运行期间的部分，或归档里的日志文件。
// 选运行、取最后几行都在服务器上一次做完（agent 的 logtail），网络慢时不用来回好几趟；持续输出时再接着看。
func (e *Engine) Logs(target, app string, o LogOptions, out io.Writer, stop <-chan struct{}) error {
	return e.withSession(target, []string{app}, func(s *session, specs []proto.AppSpec) error {
		sp := specs[0]
		if sp.Type == proto.TypeStatic && sp.NginxInstance == "" {
			return fmt.Errorf("%s 是静态应用，没有服务日志", sp.Name)
		}
		if sp.NginxInstance != "" {
			o.File = true
		}
		if o.Lines <= 0 {
			o.Lines = 200
		}
		req := &proto.Request{Apps: specs, LogRelease: o.Release, LogRun: o.Run, LogJournal: !o.File, LogFile: o.Path,
			Lines: o.Lines, Around: o.Around, LogFollow: o.Follow}
		want := o.Path // 要看的文件（可能有通配符）
		if o.File && o.Path == "" {
			if len(sp.DiagLogs) == 0 {
				return fmt.Errorf("%s 没有配置 diagLogs，请指定日志文件", sp.Name)
			}
			req.LogDiag, want = sp.DiagLogs[0], sp.DiagLogs[0]
		}
		var t proto.LogTail
		if err := s.callData("logtail", req, &t); err != nil {
			return err
		}
		if t.Stream { // diagLogs 不在应用目录下、要持续看的文件有通配符或还不存在：直接 tail
			fo := o
			fo.Follow = o.Follow && t.Run.Current && o.Around == 0 && !isGz(t.Path)
			return s.c.Stream(s.sudo+followCmd(fo, t.Path), out, fo.Follow, stop)
		}
		switch {
		case o.Around > 0: // 看某一行附近：告诉从第几行开始（界面据此标行号）
			fmt.Fprintf(out, "==> %s 第 %d 行起\n", t.Text.File, t.Text.First)
		case o.File && strings.ContainsAny(want, "*?[") && t.Text.File != "":
			fmt.Fprintf(out, "==> %s\n", t.Text.File) // 通配符匹配到的文件
		}
		for _, l := range t.Text.Lines {
			fmt.Fprintln(out, l)
		}
		if t.Follow == "" {
			return nil
		}
		if o.Loaded != nil {
			o.Loaded()
		}
		return s.c.Stream(s.sudo+t.Follow, out, true, stop)
	})
}

func isGz(p string) bool { return strings.HasSuffix(strings.ToLower(p), ".gz") }

// followCmd 看日志文件末尾（持续输出时跟着看）。file 有通配符时看最近修改的那个；
// 持续输出时每 5 秒看一次有没有更新的文件（按天切分的日志到了第二天换新文件），有就换过去从头接着输出。
func followCmd(o LogOptions, file string) string {
	if !strings.ContainsAny(file, "*?[") {
		cmd := fmt.Sprintf("tail -n %d", o.Lines)
		if o.Follow {
			cmd += " -F"
		}
		if isGz(file) {
			return fmt.Sprintf("sh -c %s", shQuote(fmt.Sprintf("gzip -dc -- %s | tail -n %d", shQuote(file), o.Lines)))
		}
		return cmd + " -- " + shQuote(file)
	}
	newest := "ls -1t -- " + shGlob(file) + " 2>/dev/null | head -n 1"
	if !o.Follow {
		return "sh -c " + shQuote(fmt.Sprintf(`f=$(%s); [ -n "$f" ] || { echo "没有匹配 %s 的日志文件" >&2; exit 1; }; echo "==> $f"; tail -n %d -- "$f"`, newest, file, o.Lines))
	}
	script := fmt.Sprintf(`cur=; pid=
newest() { %s; }
cur=$(newest)
if [ -n "$cur" ]; then echo "==> $cur"; tail -n %d -F -- "$cur" & pid=$!; else echo "还没有匹配 %s 的日志文件，出现后自动显示"; fi
while sleep 5; do
  f=$(newest)
  if [ -n "$f" ] && [ "$f" != "$cur" ]; then
    [ -n "$pid" ] && kill $pid 2>/dev/null
    echo "==> 换到新的日志文件：$f"
    cur=$f; tail -n +1 -F -- "$cur" & pid=$!
  fi
done`, newest, o.Lines, file)
	return "sh -c " + shQuote(script)
}

// shGlob 给 sh 用的通配符路径：通配符以外的部分加引号。
func shGlob(p string) string {
	var b strings.Builder
	lit := ""
	flush := func() {
		if lit != "" {
			b.WriteString(shQuote(lit))
			lit = ""
		}
	}
	for _, r := range p {
		if r == '*' || r == '?' {
			flush()
			b.WriteRune(r)
			continue
		}
		lit += string(r)
	}
	flush()
	return b.String()
}

// SearchOptions 搜索日志。
type SearchOptions struct {
	Release, Run string // 哪次运行（都为空 = 当前运行）
	Journal      bool   // 搜服务输出（journald）；否则搜日志文件
	Path         string // 只搜这个文件（可以有通配符）；空 = 这次运行的全部日志文件
	DiagLog      bool   // 搜 diagLogs 的第一个文件
	Keyword      string
	Case         bool // 区分大小写
	Context      int  // 每条结果带前后几行
	Limit        int  // 最多几条
}

// SearchLogs 在应用的日志里找包含关键字的行。
func (e *Engine) SearchLogs(target, app string, o SearchOptions) (*proto.LogSearch, error) {
	var out proto.LogSearch
	err := e.withSession(target, []string{app}, func(s *session, specs []proto.AppSpec) error {
		sp := specs[0]
		if sp.Type == proto.TypeStatic {
			return fmt.Errorf("%s 是静态应用，没有服务日志", sp.Name)
		}
		if o.Limit <= 0 {
			o.Limit = 300
		}
		if o.DiagLog && o.Path == "" {
			if len(sp.DiagLogs) == 0 {
				return fmt.Errorf("%s 没有配置 diagLogs，请指定日志文件", sp.Name)
			}
			rel, ok := logRel(appDirOf(sp, s.t.Root), proto.LogRun{}, sp.DiagLogs[0])
			if !ok {
				return fmt.Errorf("diagLogs 的 %s 不在应用目录的 running/ 或 data/ 下：用 --log 指定日志文件", sp.DiagLogs[0])
			}
			o.Path = rel
		}
		if !o.Journal {
			return s.callData("loggrep", &proto.Request{Apps: specs, LogRelease: o.Release, LogRun: o.Run, LogFile: o.Path,
				Grep: o.Keyword, GrepCase: o.Case, GrepContext: o.Context, Lines: o.Limit}, &out)
		}
		// 服务输出：这次运行期间的 journald，交给服务器上的 grep
		var runs []proto.LogRun
		if err := s.callData("logruns", &proto.Request{Apps: specs}, &runs); err != nil {
			return err
		}
		var run *proto.LogRun
		for i, r := range runs {
			if (o.Release == "" && o.Run == "" && r.Current) || (o.Release != "" && r.Release == o.Release && !r.Current && (o.Run == "" || r.ID == o.Run)) {
				run = &runs[i]
				break
			}
		}
		cmd := fmt.Sprintf("journalctl -u %s --no-pager -o short-iso", shQuote(sp.Unit))
		if run != nil && !run.Start.IsZero() {
			cmd += " --since " + journalTime(run.Start)
		}
		if run != nil && !run.End.IsZero() {
			cmd += " --until " + journalTime(run.End)
		}
		flags := "-F"
		if !o.Case {
			flags += " -i"
		}
		ctx := min(max(o.Context, 0), 10)
		if ctx > 0 {
			flags += fmt.Sprintf(" -C %d", ctx)
		}
		cmd += fmt.Sprintf(" | grep %s -n -m %d -e %s; true", flags, o.Limit+1, shQuote(o.Keyword))
		var buf strings.Builder
		if err := s.c.Stream(s.sudo+"sh -c "+shQuote(cmd), &buf, false, nil); err != nil {
			return err
		}
		out = parseGrep(buf.String(), ctx)
		if len(out.Matches) > o.Limit {
			out.Matches, out.Truncated = out.Matches[:o.Limit], true
		}
		return nil
	})
	return &out, err
}

// parseGrep grep -n（-C）的输出：匹配行为「行号:内容」，上下文为「行号-内容」，各组之间是「--」。
func parseGrep(text string, ctx int) proto.LogSearch {
	out := proto.LogSearch{Matches: []proto.LogMatch{}, Files: 1}
	var pending []string // 还没归属的上下文行
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if l == "--" || l == "" {
			pending = nil
			continue
		}
		i := strings.IndexAny(l, ":-")
		if i <= 0 {
			continue
		}
		if _, err := strconv.Atoi(l[:i]); err != nil {
			continue
		}
		body := l[i+1:]
		if l[i] == ':' {
			m := proto.LogMatch{Text: body, Before: pending}
			pending = nil
			out.Matches = append(out.Matches, m)
			continue
		}
		// 上下文：属于上一条结果的「后面」还没满就放进去，否则是下一条的「前面」
		if n := len(out.Matches); n > 0 && len(out.Matches[n-1].After) < ctx && pending == nil {
			out.Matches[n-1].After = append(out.Matches[n-1].After, body)
			continue
		}
		pending = append(pending, body)
		if len(pending) > ctx {
			pending = pending[1:]
		}
	}
	return out
}

// appDirOf 应用在服务器上的目录。
// logRel 日志文件 p 相对这次运行的日志目录的路径。日志目录里的路径都以 logs/… 开头：
// 当前运行在 running（p 为 running/logs/…，以前叫 current）或 data（data/logs/…，以前的 shared/logs/…）；
// 以前的运行两种都可能（data 下的切换时移进了版本目录）。
func logRel(appDir string, run proto.LogRun, p string) (string, bool) {
	runDir, cur := path.Join(appDir, "running"), path.Join(appDir, "current")
	data, shared := path.Join(appDir, "data"), path.Join(appDir, "shared")
	// 还没换成新结构的应用改成新写法（${logs}）时，当前运行仍在 shared/logs：running/logs/… 也按 logs/… 找
	pres := []string{runDir, cur, data, shared}
	if run.Current && run.Dir != "" {
		pres = append([]string{run.Dir}, pres...)
	}
	for _, pre := range pres {
		if r, ok := strings.CutPrefix(p, pre+"/"); ok {
			return r, true
		}
	}
	return "", false
}

func appDirOf(sp proto.AppSpec, targetRoot string) string {
	root := sp.Root
	if root == "" {
		root = targetRoot
	}
	return path.Join(root, "apps", sp.Name)
}

// LogRuns 应用的运行日志：当前运行与各版本归档的。
func (e *Engine) LogRuns(target, app string) ([]proto.LogRun, error) {
	var out []proto.LogRun
	err := e.withSession(target, []string{app}, func(s *session, specs []proto.AppSpec) error {
		if specs[0].Type == proto.TypeStatic {
			return fmt.Errorf("%s 是静态应用，没有服务日志", specs[0].Name)
		}
		return s.callData("logruns", &proto.Request{Apps: specs}, &out)
	})
	return out, err
}

// journalTime journalctl 的时间参数：精确到微秒的 @秒数（只到秒时，同一秒里相邻运行的输出会混进来）。
func journalTime(t time.Time) string {
	return fmt.Sprintf("@%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// Software 服务器上已有的软件（只读查看）。
func (e *Engine) Software(target string) (*proto.SoftwareReport, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	s, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	var out proto.SoftwareReport
	if err := s.callData("software", &proto.Request{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// nameConfirmer 能要求输入文字确认的输出（终端）。
type nameConfirmer interface {
	ConfirmName(prompt, want string) bool
}

// confirmProduction 有生产服务器（prod 不为空）时要输入服务器名（多台用逗号连起来）才执行；没有时按普通的 y/n 确认。
// 输出不支持输入文字时（如 --json）退回 y/n。
func (e *Engine) confirmProduction(prompt string, prod []string) bool {
	if len(prod) == 0 {
		return e.Rep.Confirm(prompt)
	}
	nc, ok := e.Rep.(nameConfirmer)
	if !ok {
		return e.Rep.Confirm(prompt)
	}
	return nc.ConfirmName(prompt+"\n"+strings.Join(prod, "、")+" 是生产服务器，会影响线上业务。", strings.Join(prod, ","))
}
