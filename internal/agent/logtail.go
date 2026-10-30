package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// logtail 打开日志时要的东西一次给齐：选中的运行、服务输出或日志文件的最后几行、持续输出从哪里接着看。
// 以前是 ferry 一条一条地问（列运行、取最新一条、倒着取最后几行），网络慢时每条都要等一个来回。
func (a *Agent) logtail() int {
	if len(a.req.Apps) != 1 {
		return a.result(proto.CodeBadInput, "一次只能查看一个应用的日志", nil)
	}
	s := &a.req.Apps[0]
	n := a.req.Lines
	if n <= 0 {
		n = 200
	}
	n = min(n, maxLogLines)
	run, err := a.pickRun(s, a.req.LogRelease, a.req.LogRun)
	if err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	out := proto.LogTail{Run: run, Text: proto.LogText{Lines: []string{}}}
	follow := a.req.LogFollow && run.Current

	if a.req.LogJournal {
		sh := func(cmd string) (string, error) { return a.Sys.Shell(cmd+" 2>/dev/null", 2*time.Minute) }
		lines, cursor, err := journalTail(sh, s.Unit, run, n)
		if err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		out.Text.Lines, out.Text.Truncated = lines, len(lines) >= n
		if follow {
			out.Follow = journalFollow(s.Unit, run, cursor)
		}
		return a.result(proto.CodeOK, "", out)
	}

	rel := a.req.LogFile
	if rel == "" {
		if a.req.LogDiag == "" {
			return a.result(proto.CodeBadInput, s.Name+" 没有配置 diagLogs，请指定日志文件", nil)
		}
		r, ok := logRel(filepath.ToSlash(a.appDir(s.Name)), run, a.req.LogDiag)
		if !ok {
			if !run.Current {
				return a.result(proto.CodeFailed, fmt.Sprintf("diagLogs 的 %s 不在应用目录的 running/ 或 data/ 下，没有和版本放在一起", a.req.LogDiag), nil)
			}
			out.Stream, out.Path = true, a.req.LogDiag // 不在应用目录下：由 ferry 直接 tail
			return a.result(proto.CodeOK, "", out)
		}
		rel = r
	}
	if !validLogPath(rel) {
		return a.result(proto.CodeBadInput, "日志文件路径无效："+rel, nil)
	}
	if a.req.Around > 0 || strings.HasSuffix(strings.ToLower(rel), ".gz") {
		follow = false // 看某一行附近、压缩的旧日志：不会再有新内容
	}
	if follow && run.Dir != "" {
		p := path.Join(run.Dir, rel)
		if hasGlob(rel) {
			out.Stream, out.Path = true, p // 有通配符：按天切分的日志会换文件，由 ferry 的脚本跟着换
			return a.result(proto.CodeOK, "", out)
		}
		f, err := openRegular(run.Dir, filepath.FromSlash(rel))
		if err != nil {
			out.Stream, out.Path = true, p // 还没有这个文件：tail -F 等它出现
			return a.result(proto.CodeOK, "", out)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		out.Text = tailEnd(f, st.Size(), n)
		out.Text.File = rel
		// 从读到的地方接着看；换了新文件（按大小切分）时 -F 从新文件开头看
		out.Path, out.Follow = p, fmt.Sprintf("tail -c +%d -F -- %s", st.Size()+1, shellQuote(p))
		return a.result(proto.CodeOK, "", out)
	}
	src, err := a.logSource(s, run.Release, run.ID)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	if out.Text, err = src.tail(rel, n, a.req.Around); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	return a.result(proto.CodeOK, "", out)
}

// pickRun 要看的那次运行：release、id 都为空 = 当前运行；release 有、id 为空 = 这个版本最近一次归档的。
func (a *Agent) pickRun(s *proto.AppSpec, release, id string) (proto.LogRun, error) {
	for _, r := range a.logRuns(s) {
		if (release == "" && id == "" && r.Current) || (release != "" && r.Release == release && !r.Current && (id == "" || r.ID == id)) {
			return r, nil
		}
	}
	if release == "" && id == "" {
		return proto.LogRun{Current: true}, nil // 还没部署过：只看服务输出
	}
	if id != "" {
		return proto.LogRun{}, fmt.Errorf("没有版本 %s 以前运行的日志（%s）", release, id)
	}
	return proto.LogRun{}, fmt.Errorf("没有版本 %s 以前运行的日志", release)
}

// logRel diagLogs 里的绝对路径相对这次运行的日志目录的路径；不在应用目录的 running/、data/ 下时 ok 为 false。
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

// journalTail 服务输出（journald）在这次运行期间的最后 n 条，以及最新一条的游标（当前运行，持续输出从它之后接着看）。
// sh 在服务器上执行命令，返回标准输出。
//
// 老版本的 systemd（如 CentOS 7 的 219）定位起点时依次看：游标、--since（不倒着取时）、--until（倒着取时）、-n、-r。所以
//   - --since 加 -n：从 --since 往后取 N 条，不是最后 N 条；
//   - --until 加 -n（不倒着取）：先跳到最后 N 条再按 --until 筛，可能一条都不剩；
//   - -r 加 -n、没有 --until：先跳到倒数第 N 条再往前取，拿到的是倒数第 N～2N 条。
//
// 只有 -r 加 --until 在新老版本上都是从 --until 往前取 N 条、到 --since 为止，所以总是这样取，取回来再倒回正序。
// 当前运行没有结束时间：先取最新一条的游标和时间，倒着取的到那个时间为止
// （新版本不许 --since 和 --cursor 一起用，所以倒着取时用时间）。
func journalTail(sh func(cmd string) (string, error), unit string, run proto.LogRun, n int) ([]string, string, error) {
	if unit == "" {
		return nil, "", errors.New("没有服务单元")
	}
	journal := "journalctl -u " + shellQuote(unit) + " --no-pager"
	bounds := ""
	if !run.Start.IsZero() {
		bounds += " --since " + journalTime(run.Start)
	}
	until, cursor := run.End, ""
	if until.IsZero() {
		out, err := sh(journal + " -q -n 1 -o json")
		if err != nil {
			return nil, "", fmt.Errorf("读取服务输出失败：%v", err)
		}
		var last struct {
			Cursor string `json:"__CURSOR"`
			Time   string `json:"__REALTIME_TIMESTAMP"`
		}
		json.Unmarshal([]byte(strings.TrimSpace(out)), &last)
		usec, err := strconv.ParseInt(last.Time, 10, 64)
		if last.Cursor == "" || err != nil {
			return []string{}, "", nil // 这个服务还没有输出
		}
		cursor = last.Cursor
		until = time.UnixMicro(usec + 1) // 多给 1 微秒：不管到点算不算在内，最新一条都取到
	}
	bounds += " --until " + journalTime(until)
	out, err := sh(fmt.Sprintf("%s -o short-iso -q -r -n %d%s", journal, n, bounds))
	if err != nil {
		return nil, "", fmt.Errorf("读取服务输出失败：%v", err)
	}
	lines := splitLines(reverseEntries(out))
	if lines == nil {
		lines = []string{}
	}
	return lines, cursor, nil
}

// journalFollow 持续输出服务输出的命令：从最新一条（cursor）之后接着看；还没有输出时从这次运行开始看。
func journalFollow(unit string, run proto.LogRun, cursor string) string {
	cmd := "journalctl -u " + shellQuote(unit) + " --no-pager -o short-iso -q -n all -f"
	if cursor != "" {
		return cmd + " --after-cursor " + shellQuote(cursor)
	}
	if !run.Start.IsZero() {
		cmd += " --since " + journalTime(run.Start)
	}
	return cmd
}

// journalTime journalctl 的时间参数：精确到微秒的 @秒数（只到秒时，同一秒里相邻运行的输出会混进来）。
func journalTime(t time.Time) string {
	return fmt.Sprintf("@%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// reverseEntries 把 journalctl -r 的输出倒回正序。一条有多行时后面几行以空白开头，跟着它那条一起挪。
func reverseEntries(text string) string {
	var entries []string
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if l == "" {
			continue
		}
		if n := len(entries); n > 0 && (l[0] == ' ' || l[0] == '\t') {
			entries[n-1] += "\n" + l
			continue
		}
		entries = append(entries, l)
	}
	var b strings.Builder
	for i := len(entries) - 1; i >= 0; i-- {
		b.WriteString(entries[i])
		b.WriteByte('\n')
	}
	return b.String()
}
