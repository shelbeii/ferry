package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

var pngData = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n假装是图片"))

// 附了图片：Anthropic 图片块在文字前面，OpenAI 兼容接口的 content 变成数组；模型看不了图片时换成一句说明
func TestImagesInRequest(t *testing.T) {
	msgs := []Message{{Role: "user", Text: "这个报错是什么意思", Images: []Image{{Media: "image/png", Data: pngData}}}}

	b, _ := json.Marshal(anthropicMessages(msgs, "claude-a", true)[0])
	s := string(b)
	if !strings.Contains(s, `"type":"image"`) || !strings.Contains(s, `"media_type":"image/png"`) || strings.Index(s, "image") > strings.Index(s, "这个报错") {
		t.Fatalf("Anthropic：%s", s)
	}
	b, _ = json.Marshal(anthropicMessages(msgs, "claude-a", false)[0])
	if strings.Contains(string(b), `"type":"image"`) || !strings.Contains(string(b), "看不了图片") {
		t.Fatalf("Anthropic 不支持图片：%s", b)
	}

	b, _ = json.Marshal(openaiMessages("s", msgs, "qwen-vl", true)[1])
	s = string(b)
	if !strings.Contains(s, `"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,`) || !strings.Contains(s, `{"type":"text","text":"这个报错是什么意思"}`) {
		t.Fatalf("OpenAI：%s", s)
	}
	b, _ = json.Marshal(openaiMessages("s", msgs, "deepseek-chat", false)[1])
	if strings.Contains(string(b), "image_url") || !strings.Contains(string(b), `"content":"（用户附了 1 张图片`) {
		t.Fatalf("OpenAI 不支持图片：%s", b)
	}
	// 没有图片的消息还是普通的字符串 content
	b, _ = json.Marshal(openaiMessages("s", []Message{{Role: "user", Text: "你好"}}, "x", true)[1])
	if string(b) != `{"role":"user","content":"你好"}` {
		t.Fatalf("普通消息：%s", b)
	}
}

// 只有图片没有文字也能发
func TestImageOnly(t *testing.T) {
	msgs := []Message{{Role: "user", Images: []Image{{Media: "image/png", Data: pngData}}}}
	b, _ := json.Marshal(anthropicMessages(msgs, "claude-a", true)[0])
	if strings.Contains(string(b), `"type":"text"`) {
		t.Fatalf("只有图片时不加空文字块：%s", b)
	}
	b, _ = json.Marshal(openaiMessages("s", msgs, "x", true)[1])
	if strings.Contains(string(b), `"type":"text"`) || !strings.Contains(string(b), "image_url") {
		t.Fatalf("OpenAI 只有图片：%s", b)
	}
}

// 更早几轮的图片发给模型时省略，最近的原样保留；存下来的对话不变
func TestCompactImages(t *testing.T) {
	img := []Image{{Media: "image/png", Data: pngData}}
	msgs := []Message{
		{Role: "user", Text: "一", Images: img}, {Role: "assistant", Text: "答一"},
		{Role: "user", Text: "二"}, {Role: "assistant", Text: "答二"},
		{Role: "user", Text: "三", Images: img}, {Role: "assistant", Text: "答三"},
	}
	out := compactForModel(msgs)
	if out[0].Images != nil || !strings.Contains(out[0].Text, "附过 1 张图片") || !strings.HasSuffix(out[0].Text, "一") {
		t.Fatalf("早先的图片：%+v", out[0])
	}
	if len(out[4].Images) != 1 || out[4].Text != "三" {
		t.Fatalf("最近的图片要保留：%+v", out[4])
	}
	if len(msgs[0].Images) != 1 {
		t.Fatal("不能改动原来的对话")
	}
}

func TestCheckImages(t *testing.T) {
	ok := Image{Media: "image/jpeg", Data: pngData}
	if err := CheckImages([]Image{ok}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]Image{
		{{Media: "image/bmp", Data: pngData}},
		{{Media: "image/png", Data: "不是 base64"}},
		{{Media: "image/png", Data: base64.StdEncoding.EncodeToString(make([]byte, MaxImageBytes+1))}},
		make([]Image, MaxImages+1),
	} {
		if CheckImages(bad) == nil {
			t.Errorf("应该不通过：%.60v", bad)
		}
	}
}

func TestSupportsImages(t *testing.T) {
	for _, c := range []struct {
		setting, provider, model string
		want                     bool
	}{
		{"", Anthropic, "claude-opus-5-5", true},
		{"", OpenAI, "deepseek-chat", false},
		{"", OpenAI, "qwen-vl-max", true},
		{"", OpenAI, "gpt-4o-mini", true},
		{"", OpenAI, "glm-4v-plus", true},
		{"on", OpenAI, "deepseek-chat", true},
		{"off", Anthropic, "claude-opus-5-5", false},
	} {
		if got := SupportsImages(c.setting, c.provider, c.model); got != c.want {
			t.Errorf("%+v：%v", c, got)
		}
	}
}

// 通过接口真的发出去：请求里带着图片
func TestImagesOverHTTP(t *testing.T) {
	ant := `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"end_turn","content":[{"type":"text","text":"看到了"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	oa := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"看到了"}}]}`
	msgs := []Message{{Role: "user", Text: "看图", Images: []Image{{Media: "image/png", Data: pngData}}}}
	for _, c := range []struct{ provider, reply, want string }{
		{Anthropic, ant, `"type":"image"`},
		{OpenAI, oa, `"type":"image_url"`},
	} {
		fm := &fakeModel{replies: []string{c.reply}}
		p, _ := NewProvider(Config{Provider: c.provider, BaseURL: newServer(t, fm), Model: "x", APIKey: "k", Images: true})
		if _, err := p.Step(context.Background(), "s", msgs, nil); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(fm.got[0]["messages"])
		if !strings.Contains(string(b), c.want) || !strings.Contains(string(b), pngData) {
			t.Errorf("%s 请求里没有图片：%s", c.provider, b)
		}
	}
}
