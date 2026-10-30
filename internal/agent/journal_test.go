package agent

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// journalLogs 照 ferry 看服务输出的做法：先取最后 n 条（journalTail，在 agent 里），持续输出时再接着看（journalFollow）。
func journalLogs(stream func(cmd string, out io.Writer, pty bool, stop <-chan struct{}) error, unit string, run proto.LogRun, n int, follow bool, out io.Writer, stop <-chan struct{}) error {
	sh := func(cmd string) (string, error) {
		var b strings.Builder
		err := stream(cmd, &b, false, nil)
		return b.String(), err
	}
	lines, cursor, err := journalTail(sh, unit, run, n)
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	if !follow || !run.End.IsZero() {
		return nil
	}
	return stream(journalFollow(unit, run, cursor), out, true, stop)
}

// fakeJournal 按 journalctl 的取法回答命令。old 为 true 时照老版本 systemd（219 的 journalctl.c）定位起点：
// 依次看游标、--since（不倒着取时）、--until（倒着取时）、-n（跳到倒数第 N 条，倒着取时再往前）、-r；
// 之后不倒着取时到 --until 为止，倒着取时到 --since 为止，最多 -n 条。
type fakeJournal struct {
	old     bool
	entries []fakeEntry
	cmds    []string
	follows []string // 跟着看的命令
}

type fakeEntry struct {
	t   int64 // 秒
	msg string
}

func (j *fakeJournal) cursor(i int) string { return "s=c" + strconv.Itoa(i) }

func (j *fakeJournal) line(i int) string {
	e := j.entries[i]
	ts := time.Unix(e.t, 0).UTC().Format("2006-01-02T15:04:05+0000")
	return ts + " host app[1]: " + strings.ReplaceAll(e.msg, "\n", "\n                               ")
}

func (j *fakeJournal) stream(cmd string, out io.Writer, pty bool, stop <-chan struct{}) error {
	j.cmds = append(j.cmds, cmd)
	f := strings.Fields(cmd)
	lines, reverse, follow, json := -1, false, false, false
	var since, until float64
	hasSince, hasUntil := false, false
	cur, after := -1, false
	for i := 0; i < len(f); i++ {
		switch f[i] {
		case "-n":
			i++
			if f[i] != "all" {
				lines, _ = strconv.Atoi(f[i])
			}
		case "-r":
			reverse = true
		case "-f":
			follow = true
		case "-o":
			i++
			json = f[i] == "json"
		case "--since", "--until":
			v, _ := strconv.ParseFloat(strings.TrimPrefix(f[i+1], "@"), 64)
			if f[i] == "--since" {
				since, hasSince = v, true
			} else {
				until, hasUntil = v, true
			}
			i++
		case "--cursor", "--after-cursor":
			after = f[i] == "--after-cursor"
			c := strings.Trim(f[i+1], "'")
			cur, _ = strconv.Atoi(strings.TrimPrefix(c, "s=c"))
			i++
		}
	}
	if !j.old && hasSince && cur >= 0 {
		return fmt.Errorf("退出码 1：Please specify only one of --since=, --cursor=, and --after-cursor=.")
	}
	if follow {
		j.follows = append(j.follows, cmd)
	}
	n := len(j.entries)
	at := func(i int) float64 { return float64(j.entries[i].t) }
	var pick []int // 按输出顺序
	if j.old {
		start, step := 0, 1
		switch {
		case cur >= 0:
			start = cur
			if reverse {
				step = -1
				if after {
					start--
				}
			} else if after {
				start++
			}
		case hasSince && !reverse:
			for start < n && at(start) < since {
				start++
			}
		case hasUntil && reverse:
			start, step = n-1, -1
			for start >= 0 && at(start) > until {
				start--
			}
		case lines >= 0: // 倒着取时也是先跳到倒数第 N 条，再往前输出（拿到的是倒数第 N～2N 条）
			start = max(n-lines, 0)
			if reverse {
				step = -1
			}
		case reverse:
			start, step = n-1, -1
		}
		for i := start; i >= 0 && i < n && (lines < 0 || len(pick) < lines); i += step {
			if step > 0 && hasUntil && at(i) > until {
				break
			}
			if step < 0 && hasSince && at(i) < since {
				break
			}
			pick = append(pick, i)
		}
	} else { // 新版本：先按起止时间、游标筛，再取最后 -n 条
		var in []int
		for i := range n {
			if (hasSince && at(i) < since) || (hasUntil && at(i) > until) {
				continue
			}
			if cur >= 0 && ((reverse && (i > cur || after && i == cur)) || (!reverse && (i < cur || after && i == cur))) {
				continue
			}
			in = append(in, i)
		}
		if lines >= 0 && len(in) > lines {
			if reverse || cur < 0 {
				in = in[len(in)-lines:]
			} else {
				in = in[:lines]
			}
		}
		if reverse {
			for i := len(in) - 1; i >= 0; i-- {
				pick = append(pick, in[i])
			}
		} else {
			pick = in
		}
	}
	for _, i := range pick {
		if json {
			fmt.Fprintf(out, "{\"__CURSOR\":%q,\"__REALTIME_TIMESTAMP\":\"%d\",\"MESSAGE\":%q}\n", j.cursor(i), j.entries[i].t*1e6, j.entries[i].msg)
		} else {
			fmt.Fprintln(out, j.line(i))
		}
	}
	return nil
}

func newFakeJournal(old bool) *fakeJournal {
	j := &fakeJournal{old: old}
	for i := range 10 {
		msg := fmt.Sprintf("m%d", i)
		if i == 8 {
			msg += "\n\tat Foo.bar"
		}
		j.entries = append(j.entries, fakeEntry{t: 1000 + int64(i)*10, msg: msg})
	}
	return j
}

func msgs(t *testing.T, text string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if _, m, ok := strings.Cut(l, "app[1]: "); ok {
			out = append(out, m)
		} else if strings.TrimSpace(l) != "" {
			out = append(out, "  "+strings.TrimSpace(l))
		}
	}
	return out
}

// 带起点（这次运行从某个时间开始）时，新老版本的 systemd 都取最后 N 条，按时间先后排。
func TestJournalLogsLastLinesSinceStart(t *testing.T) {
	for _, old := range []bool{true, false} {
		j := newFakeJournal(old)
		var b strings.Builder
		run := proto.LogRun{Current: true, Start: time.Unix(1025, 0)} // m3 以后是这次运行
		if err := journalLogs(j.stream, "app.service", run, 3, false, &b, nil); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(msgs(t, b.String()), "|")
		if want := "m7|m8|  at Foo.bar|m9"; got != want {
			t.Errorf("old=%v：得到 %q，应为 %q；命令 %q", old, got, want, j.cmds)
		}
		// 这次运行不到 N 条：只给这次运行的
		b.Reset()
		run.Start = time.Unix(1075, 0)
		journalLogs(j.stream, "app.service", run, 5, false, &b, nil)
		if got, want := strings.Join(msgs(t, b.String()), "|"), "m8|  at Foo.bar|m9"; got != want {
			t.Errorf("old=%v：得到 %q，应为 %q", old, got, want)
		}
	}
	// 老版本上以前的两种写法都取错：确认假 journal 照老版本的取法
	j := newFakeJournal(true)
	var b strings.Builder
	j.stream("journalctl -u 'app.service' -n 3 --since @1025.000000", &b, false, nil) // 0.12.39：从起点往后
	if got := strings.Join(msgs(t, b.String()), "|"); got != "m3|m4|m5" {
		t.Fatalf("假 journal 不像老版本：%q", got)
	}
	b.Reset()
	j.stream("journalctl -u 'app.service' -q -r -n 3 --since @1025.000000", &b, false, nil) // 0.12.40～41：倒数第 3～5 条
	if got := strings.Join(msgs(t, b.String()), "|"); got != "m7|m6|m5" {
		t.Fatalf("假 journal 不像老版本：%q", got)
	}
}

// 倒着取的命令都要带 --until（老版本没有 --until 时先跳到倒数第 N 条再往前）。
func TestJournalLogsReverseHasUntil(t *testing.T) {
	cases := []struct {
		run    proto.LogRun
		follow bool
	}{
		{proto.LogRun{Current: true, Start: time.Unix(1025, 0)}, false},
		{proto.LogRun{Current: true, Start: time.Unix(1025, 0)}, true},
		{proto.LogRun{Release: "r1", ID: "x", Start: time.Unix(1005, 0), End: time.Unix(1065, 0)}, false},
	}
	for _, c := range cases {
		j := newFakeJournal(true)
		journalLogs(j.stream, "app.service", c.run, 3, c.follow, io.Discard, nil)
		for _, cmd := range j.cmds {
			if strings.Contains(cmd, " -r ") && !strings.Contains(cmd, " --until @") {
				t.Errorf("倒着取没有 --until：%s", cmd)
			}
		}
	}
	// 当前运行还没有输出：不看的话什么都不给
	j := &fakeJournal{old: true}
	var b strings.Builder
	if err := journalLogs(j.stream, "app.service", proto.LogRun{Current: true, Start: time.Unix(1025, 0)}, 3, false, &b, nil); err != nil || b.Len() != 0 {
		t.Errorf("没有输出时：%v %q", err, b.String())
	}
}

// 以前的运行（有起止时间）：取结束前的最后 N 条。
func TestJournalLogsLastLinesOfPastRun(t *testing.T) {
	for _, old := range []bool{true, false} {
		j := newFakeJournal(old)
		var b strings.Builder
		run := proto.LogRun{Release: "r1", ID: "x", Start: time.Unix(1005, 0), End: time.Unix(1065, 0)} // m1～m6
		if err := journalLogs(j.stream, "app.service", run, 2, true, &b, nil); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join(msgs(t, b.String()), "|"), "m5|m6"; got != want {
			t.Errorf("old=%v：得到 %q，应为 %q", old, got, want)
		}
		if len(j.follows) != 0 {
			t.Errorf("以前的运行不该跟着看：%q", j.follows)
		}
	}
}

// 跟着看：先给这次运行的最后 N 条，再从最新一条之后接着看。
func TestJournalLogsFollow(t *testing.T) {
	for _, old := range []bool{true, false} {
		j := newFakeJournal(old)
		var b strings.Builder
		run := proto.LogRun{Current: true, Start: time.Unix(1025, 0)}
		if err := journalLogs(j.stream, "app.service", run, 2, true, &b, nil); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join(msgs(t, b.String()), "|"), "m8|  at Foo.bar|m9"; got != want {
			t.Errorf("old=%v：得到 %q，应为 %q", old, got, want)
		}
		if len(j.follows) != 1 || !strings.Contains(j.follows[0], "--after-cursor 's=c9'") {
			t.Errorf("old=%v：应从最新一条之后跟着看：%q", old, j.follows)
		}
	}
	// 服务还没有输出：从这次运行开始跟着看
	j := &fakeJournal{old: true}
	journalLogs(j.stream, "app.service", proto.LogRun{Current: true, Start: time.Unix(1025, 0)}, 2, true, io.Discard, nil)
	if len(j.follows) != 1 || !strings.Contains(j.follows[0], "--since @1025") {
		t.Errorf("没有输出时应从这次运行开始跟着看：%q", j.follows)
	}
	// 没有起止时间（不知道这次运行从什么时候开始）：直接取最后 N 条
	j = newFakeJournal(true)
	var b strings.Builder
	journalLogs(j.stream, "app.service", proto.LogRun{Current: true}, 2, false, &b, nil)
	if got := strings.Join(msgs(t, b.String()), "|"); got != "m8|  at Foo.bar|m9" {
		t.Errorf("得到 %q", got)
	}
}

func TestReverseEntries(t *testing.T) {
	in := "c\nb1\n  b2\n\tb3\n-- Reboot --\na\n"
	if got, want := reverseEntries(in), "a\n-- Reboot --\nb1\n  b2\n\tb3\nc\n"; got != want {
		t.Errorf("得到 %q，应为 %q", got, want)
	}
	if reverseEntries("") != "" {
		t.Error("空输出应为空")
	}
}
