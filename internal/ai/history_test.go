package ai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 打开历史对话：用户的话、回答、工具调用和结果、用户的决定；停在确认上的显示为等确认
func TestHistoryItems(t *testing.T) {
	fm := &fakeModel{replies: []string{
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"先看看。","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"cmd","arguments":"{\"command\":\"df -h\"}"}},` +
			`{"id":"c2","type":"function","function":{"name":"cmd","arguments":"{\"command\":\"yum install -y a\"}"}},` +
			`{"id":"c3","type":"function","function":{"name":"cmd","arguments":"{\"command\":\"yum install -y b\"}"}}]}}]}`,
	}}
	p, _ := NewProvider(Config{Provider: OpenAI, BaseURL: newServer(t, fm), Model: "m", APIKey: "k"})
	tool := Tool{Name: "cmd", Schema: map[string]any{"type": "object"},
		Approve: func(_ context.Context, in json.RawMessage) *Approval {
			if strings.Contains(string(in), "yum") {
				return &Approval{Kind: "yum install", Title: "执行命令"}
			}
			return nil
		},
		Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	conv := &Conversation{ID: NewID(), Config: "/p/ferry.yaml", Target: "a"}
	l := &Loop{Provider: p, System: "s", Tools: []Tool{tool}, Emit: func(Event) {}}
	if err := l.Send(context.Background(), conv, "装 a 和 b，看看磁盘", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := conv.Save(dir); err != nil {
		t.Fatal(err)
	}
	conv, _ = Load(dir, conv.ID)
	items := conv.Items()
	var kinds []string
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	// c3 还没轮到，不显示
	if strings.Join(kinds, ",") != "user,text,tool,tool" {
		t.Fatalf("条目：%v", kinds)
	}
	if items[2].Result != "ok" || !items[2].Done || items[3].Call.ID != "c2" || !items[3].Waiting || items[3].Approval == nil {
		t.Fatalf("工具条目：%+v %+v", items[2], items[3])
	}

	// 接着说了别的：c2 记为没确认
	fm.replies = append(fm.replies, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好。"}}]}`)
	l.Send(context.Background(), conv, "算了", nil)
	items = conv.Items()
	if items[3].Decision != "skipped" || items[3].Waiting || items[3].Result == "" {
		t.Fatalf("没确认的：%+v", items[3])
	}

	list, err := List(dir)
	if err != nil || len(list) != 1 || list[0].Title != "装 a 和 b，看看磁盘" || list[0].Messages != 1 || list[0].Target != "a" {
		t.Fatalf("列表：%v %+v", err, list)
	}
}

func TestClean(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int, age time.Duration) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o600)
		tm := time.Now().Add(-age)
		os.Chtimes(p, tm, tm)
	}
	write("old-aaaaaaaa.json", 10, 40*24*time.Hour)
	write("mid-bbbbbbbb.json", 100, 5*24*time.Hour)
	write("new-cccccccc.json", 100, time.Hour)
	write("cur-dddddddd.json", 100, 50*24*time.Hour) // 正在用的，不删
	write("audit.jsonl", 1000, 400*24*time.Hour)     // 改动记录不动

	if n := Clean(dir, 30, 0, "cur-dddddddd"); n != 1 {
		t.Fatalf("按天数应删 1 个：%d", n)
	}
	// 总大小 300，限 250：从最旧的删起（跳过正在用的），删掉 mid
	if n := Clean(dir, 30, 250, "cur-dddddddd"); n != 1 {
		t.Fatalf("按大小应删 1 个：%d", n)
	}
	for _, name := range []string{"new-cccccccc.json", "cur-dddddddd.json", "audit.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 不应删", name)
		}
	}
	if c, b := DirUsage(dir); c != 2 || b != 200 {
		t.Fatalf("占用：%d %d", c, b)
	}
	if n := CleanAll(dir); n != 2 {
		t.Fatalf("全部删除：%d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl")); err != nil {
		t.Fatal("改动记录不应删")
	}
}

// 确认后执行到一半 Ferry 退出了：重新打开时那个操作记为结果未知，不会再等确认、被重复执行
func TestRecoverAfterCrash(t *testing.T) {
	fm := &fakeModel{replies: []string{
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"cmd","arguments":"{\"command\":\"yum install -y a\"}"}},` +
			`{"id":"c2","type":"function","function":{"name":"cmd","arguments":"{\"command\":\"yum install -y b\"}"}}]}}]}`,
	}}
	p, _ := NewProvider(Config{Provider: OpenAI, BaseURL: newServer(t, fm), Model: "m", APIKey: "k"})
	dir := t.TempDir()
	conv := &Conversation{ID: NewID()}
	var snapshot []byte // 执行时磁盘上的对话（模拟这时 Ferry 被关掉）
	tool := Tool{Name: "cmd", Schema: map[string]any{"type": "object"},
		Approve: func(context.Context, json.RawMessage) *Approval {
			return &Approval{Kind: "yum install", Title: "执行命令"}
		},
		Run: func(context.Context, json.RawMessage) (string, error) {
			if snapshot == nil {
				snapshot, _ = os.ReadFile(filepath.Join(dir, conv.ID+".json"))
			}
			return "ok", nil
		}}
	l := &Loop{Provider: p, System: "s", Tools: []Tool{tool}, Emit: func(Event) {}, Save: func() { conv.Save(dir) }}
	l.Send(context.Background(), conv, "装 a 和 b", nil)
	if conv.Pending == nil {
		t.Fatal("应停在确认上")
	}
	fm.replies = append(fm.replies, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好了"}}]}`)
	if err := l.Decide(context.Background(), conv, Always); err != nil {
		t.Fatal(err)
	}
	// 用执行时的那份对话（相当于退出后重新打开）
	var crashed Conversation
	if err := json.Unmarshal(snapshot, &crashed); err != nil {
		t.Fatal(err)
	}
	if crashed.Pending != nil || len(crashed.Executing) != 2 || crashed.Decisions["c1"].Decision != Always {
		t.Fatalf("执行时应已记下：不再等确认、正在执行、用户的决定：%+v", crashed)
	}
	if !crashed.Recover() {
		t.Fatal("应恢复")
	}
	items := crashed.Items()
	last := items[len(items)-1]
	first := items[len(items)-2]
	if first.Waiting || !strings.Contains(first.Result, "结果未知") || first.Decision != Always || !strings.Contains(last.Result, "没有执行") {
		t.Fatalf("恢复后的条目：%+v / %+v", first, last)
	}
}

// 改了第几句话重新发：退回到那句之前；导出成 Markdown
func TestRewindAndMarkdown(t *testing.T) {
	c := &Conversation{ID: NewID(), Target: "web", Messages: []Message{
		{Role: "user", Text: "第一句"}, {Role: "assistant", Text: "答一"},
		{Role: "user", Text: "第二句"}, {Role: "assistant", Text: "", Calls: []ToolCall{{ID: "c1", Name: "run_command", Input: json.RawMessage(`{"command":"df -h"}`)}}},
		{Role: "tool", CallID: "c1", Text: "/dev/sda1 50G"}, {Role: "assistant", Text: "答二", Usage: &Usage{Input: 10, Output: 5}, Model: "m"},
	}}
	md := c.Markdown()
	for _, want := range []string{"# AI 助手对话：web", "## 问\n\n第一句", "df -h", "/dev/sda1 50G", "答二", "输入 10"} {
		if !strings.Contains(md, want) {
			t.Errorf("导出里应有 %q：\n%s", want, md)
		}
	}
	c.Decisions = map[string]Decided{"c1": {Decision: Approve}}
	if err := c.Rewind(1); err != nil || len(c.Messages) != 2 || c.Messages[1].Text != "答一" || len(c.Decisions) != 0 {
		t.Fatalf("退回到第二句之前：%v %+v", err, c.Messages)
	}
	if err := c.Rewind(5); err == nil {
		t.Fatal("没有第六句")
	}
}
