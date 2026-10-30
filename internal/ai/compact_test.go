package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactForModel(t *testing.T) {
	big := strings.Repeat("日志行\n", 1000)
	turn := func(q string) []Message {
		return []Message{
			{Role: "user", Text: q},
			{Role: "assistant", Raw: json.RawMessage(`"思考"`), Calls: []ToolCall{{ID: q, Name: "cmd", Input: json.RawMessage(`{}`)}}},
			{Role: "tool", CallID: q, Text: big},
			{Role: "assistant", Text: "答：" + q},
		}
	}
	var msgs []Message
	for _, q := range []string{"一", "二", "三"} {
		msgs = append(msgs, turn(q)...)
	}
	out := compactForModel(msgs)
	if len(out) != len(msgs) || len(out[2].Text) > 1000 || out[1].Raw != nil {
		t.Fatalf("第一轮的输出应变短、不带思考：%d %d %v", len(out), len(out[2].Text), out[1].Raw)
	}
	if out[6].Text != big || out[10].Text != big || out[9].Raw == nil {
		t.Fatal("最近两轮应原样保留")
	}
	if msgs[2].Text != big {
		t.Fatal("不能改原来的对话")
	}
	// 很长的对话：省略最早的几轮，开头加上说明
	msgs = nil
	for i := 0; i < 40; i++ {
		ts := turn(strings.Repeat("问", i+1))
		ts[3].Text = strings.Repeat("长回答", 5000)
		msgs = append(msgs, ts...)
	}
	out = compactForModel(msgs)
	if size(out) > contextMax+200*1024 || out[0].Text != omittedNote || out[1].Role != "user" || out[len(out)-1].Text != msgs[len(msgs)-1].Text {
		t.Fatalf("应省略最早的几轮：%d 条，%d 字节，开头 %q", len(out), size(out), out[0].Text)
	}
}
