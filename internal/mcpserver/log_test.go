package mcpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
)

func logHome(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
}

func writeEntries(t *testing.T, p string, from, n int, at time.Time) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := from; i < from+n; i++ {
		b, _ := json.Marshal(callEntry{Time: at, Tool: fmt.Sprintf("t%d", i), OK: true})
		f.Write(append(b, '\n'))
	}
}

func toolOf(raw json.RawMessage) string {
	var e callEntry
	json.Unmarshal(raw, &e)
	return e.Tool
}

// 分页：新的在前；旧版本换下来的 calls.1.jsonl 也算在里面，排在更旧的位置。
func TestCallsPaging(t *testing.T) {
	logHome(t)
	writeEntries(t, oldLogFile(), 0, 3, time.Now())
	writeEntries(t, LogFile(), 3, 5, time.Now())
	pg := Calls(CallFilter{}, 0, 3)
	items, total := pg.Items, pg.Total
	if total != 8 || len(items) != 3 || toolOf(items[0]) != "t7" || toolOf(items[2]) != "t5" {
		t.Fatalf("第一页：%d %s", total, items)
	}
	items = Calls(CallFilter{}, 6, 3).Items
	if len(items) != 2 || toolOf(items[0]) != "t1" || toolOf(items[1]) != "t0" {
		t.Fatalf("最后一页：%s", items)
	}
	if items := Calls(CallFilter{}, 20, 3).Items; len(items) != 0 {
		t.Fatalf("超出范围：%s", items)
	}
	if n, size := LogUsage(); n != 8 || size == 0 {
		t.Fatalf("占用：%d %d", n, size)
	}
}

// 按天数清理：旧的删掉，calls.1.jsonl 并回 calls.jsonl。
func TestCleanLogDays(t *testing.T) {
	logHome(t)
	writeEntries(t, oldLogFile(), 0, 2, time.Now().AddDate(0, 0, -40))
	writeEntries(t, LogFile(), 2, 3, time.Now())
	if n := CleanLog(30, 20<<20); n != 2 {
		t.Fatalf("删掉 %d 条", n)
	}
	if _, err := os.Stat(oldLogFile()); err == nil {
		t.Fatal("calls.1.jsonl 应并回 calls.jsonl")
	}
	pg := Calls(CallFilter{}, 0, 10)
	items, total := pg.Items, pg.Total
	if total != 3 || toolOf(items[2]) != "t2" {
		t.Fatalf("剩下：%s", items)
	}
	if n := CleanLog(30, 20<<20); n != 0 {
		t.Fatalf("没有要删的时候删了 %d 条", n)
	}
}

// 超过最多占用：从最旧的删起，删到九成。
func TestCleanLogSize(t *testing.T) {
	logHome(t)
	writeEntries(t, LogFile(), 0, 100, time.Now())
	fi, _ := os.Stat(LogFile())
	max := fi.Size() / 2
	n := CleanLog(30, max)
	fi, _ = os.Stat(LogFile())
	if n == 0 || fi.Size() > max*9/10 {
		t.Fatalf("删了 %d 条，还有 %d 字节（最多 %d）", n, fi.Size(), max)
	}
	pg := Calls(CallFilter{}, 0, 1)
	items, total := pg.Items, pg.Total
	if total != 100-n || toolOf(items[0]) != "t99" {
		t.Fatalf("留下的应是最新的：%d %s", total, items)
	}
	if CleanAllLog() != 100-n {
		t.Fatal("全部删除")
	}
	if Calls(CallFilter{}, 0, 1).Total != 0 {
		t.Fatal("全部删除后还有记录")
	}
}

// 记一条时顺带按设置清理（这个进程第一次记的时候也按天数清理一遍）。
func TestWriteLogCleans(t *testing.T) {
	logHome(t)
	os.WriteFile(appdir.SettingsFile(), []byte(`{"mcp":{"logKeepDays":7}}`), 0o600)
	writeEntries(t, LogFile(), 0, 2, time.Now().AddDate(0, 0, -10))
	lastClean = time.Time{}
	writeLog(callEntry{Time: time.Now(), Tool: "new", OK: true})
	pg := Calls(CallFilter{}, 0, 10)
	items, total := pg.Items, pg.Total
	if total != 1 || !strings.Contains(string(items[0]), `"new"`) {
		t.Fatalf("应只剩新记的：%s", items)
	}
}

// 筛选：工具（改动的结果算一类）、编程软件、成功 / 失败、只看改动、时间、关键字；下拉用的编程软件、工具来自全部记录。
func TestCallsFilter(t *testing.T) {
	logHome(t)
	now := time.Now()
	f, _ := os.Create(LogFile())
	for _, e := range []callEntry{
		{Time: now.Add(-3 * time.Hour), Client: "claude-code 2", Tool: "app_status", Args: json.RawMessage(`{"app":"api"}`), OK: true},
		{Time: now.Add(-2 * time.Hour), Client: "cursor 1", Tool: "app_logs", Args: json.RawMessage(`{"app":"web","keyword":"OOM"}`), OK: false, Error: "连不上服务器"},
		{Time: now.Add(-time.Hour), Client: "claude-code 2", Tool: "deploy", Op: "op1", Decision: "同意", OK: true},
		{Time: now, Client: "claude-code 2", Tool: "deploy（结果）", Op: "op1", OK: false, Error: "构建失败"},
	} {
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
	}
	f.Close()
	tools := func(pg CallPage) string {
		var out []string
		for _, it := range pg.Items {
			out = append(out, toolOf(it))
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		name string
		f    CallFilter
		want string
	}{
		{"全部", CallFilter{}, "deploy（结果）,deploy,app_logs,app_status"},
		{"工具", CallFilter{Tools: []string{"app_status", "app_logs"}}, "app_logs,app_status"},
		{"改动的结果", CallFilter{Tools: []string{ResultTool}}, "deploy（结果）"},
		{"编程软件", CallFilter{Client: "cursor 1"}, "app_logs"},
		{"失败", CallFilter{Result: "fail"}, "deploy（结果）,app_logs"},
		{"只看改动", CallFilter{Changes: true}, "deploy（结果）,deploy"},
		{"时间", CallFilter{Since: now.Add(-150 * time.Minute), Until: now.Add(-30 * time.Minute)}, "deploy,app_logs"},
		{"关键字在参数里", CallFilter{Keyword: "oom"}, "app_logs"},
		{"关键字在错误里", CallFilter{Keyword: "构建失败"}, "deploy（结果）"},
		{"组合", CallFilter{Client: "claude-code 2", Result: "ok"}, "deploy,app_status"},
	} {
		if got := tools(Calls(c.f, 0, 10)); got != c.want {
			t.Errorf("%s：%s，应为 %s", c.name, got, c.want)
		}
	}
	pg := Calls(CallFilter{Result: "fail"}, 1, 10)
	if pg.Total != 2 || len(pg.Items) != 1 || toolOf(pg.Items[0]) != "app_logs" {
		t.Errorf("筛选后分页：%d %s", pg.Total, pg.Items)
	}
	if strings.Join(pg.Clients, ",") != "claude-code 2,cursor 1" || strings.Join(pg.Tools, ",") != "app_logs,app_status,deploy,"+ResultTool {
		t.Errorf("下拉：%v %v", pg.Clients, pg.Tools)
	}
}
