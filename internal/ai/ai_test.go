package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 假模型服务：按顺序给出回复，记下收到的请求。请求要流式（stream: true）时，把准备好的完整回复拆成流式事件发出去。
type fakeModel struct {
	mu       sync.Mutex
	replies  []string
	got      []map[string]any
	headers  []http.Header
	noStream bool // 模拟不支持流式的服务
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	f.got = append(f.got, m)
	f.headers = append(f.headers, r.Header.Clone())
	if len(f.replies) == 0 {
		http.Error(w, `{"error":{"message":"没有更多回复"}}`, 500)
		return
	}
	reply := f.replies[0]
	f.replies = f.replies[1:]
	if m["stream"] != true || f.noStream {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	var full map[string]any
	json.Unmarshal([]byte(reply), &full)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	if full["type"] == "message" { // Anthropic
		content, _ := full["content"].([]any)
		usage, _ := full["usage"].(map[string]any)
		start := map[string]any{}
		for k, v := range full {
			start[k] = v
		}
		start["content"] = []any{}
		start["stop_reason"] = nil
		send("message_start", map[string]any{"type": "message_start", "message": start})
		for i, c := range content {
			blk := c.(map[string]any)
			switch blk["type"] {
			case "text":
				send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": blk["text"]}})
			case "thinking":
				send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": blk["thinking"]}})
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "signature_delta", "signature": blk["signature"]}})
			case "tool_use":
				in, _ := json.Marshal(blk["input"])
				send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": blk["id"], "name": blk["name"], "input": map[string]any{}}})
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(in)}})
			}
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		}
		send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": full["stop_reason"], "stop_sequence": nil}, "usage": usage})
		send("message_stop", map[string]any{"type": "message_stop"})
		return
	}
	// OpenAI 兼容：每样东西一个 chunk，最后一个带用量
	choices, _ := full["choices"].([]any)
	if len(choices) > 0 {
		ch := choices[0].(map[string]any)
		msg, _ := ch["message"].(map[string]any)
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			send("", map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": rc}}}})
		}
		if c, ok := msg["content"].(string); ok && c != "" {
			// 分两段发，验证拼接
			h := len([]rune(c)) / 2
			send("", map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": string([]rune(c)[:h])}}}})
			send("", map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": string([]rune(c)[h:])}}}})
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for i, tc := range tcs {
				t := tc.(map[string]any)
				fn := t["function"].(map[string]any)
				args, _ := fn["arguments"].(string)
				h := len(args) / 2
				send("", map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": t["id"], "type": "function", "function": map[string]any{"name": fn["name"], "arguments": args[:h]}}}}}}})
				send("", map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": args[h:]}}}}}}})
			}
		}
		send("", map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": ch["finish_reason"]}}})
	}
	usage := full["usage"]
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "prompt_cache_hit_tokens": 60}
	}
	send("", map[string]any{"choices": []any{}, "usage": usage})
	io.WriteString(w, "data: [DONE]\n\n")
}

func echoTool(calls *[]string) Tool {
	return Tool{
		Name: "run_command", Description: "执行命令",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}},
		Run: func(_ context.Context, in json.RawMessage) (string, error) {
			var a struct{ Command string }
			json.Unmarshal(in, &a)
			*calls = append(*calls, a.Command)
			return "Filesystem  Size  Used\n/dev/sda1  50G  45G", nil
		},
	}
}

func TestRunOpenAICompatible(t *testing.T) {
	fm := &fakeModel{replies: []string{
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"先看看磁盘。","reasoning_content":"用户问磁盘，先 df。","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"df -h\"}"}}]}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"根目录已用 90%。"}}]}`,
	}}
	srv := httptest.NewServer(fm)
	defer srv.Close()
	p, err := NewProvider(Config{Provider: OpenAI, BaseURL: srv.URL + "/v1", Model: "deepseek-chat", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	var evs []Event
	conv := &Conversation{ID: NewID()}
	if err := Run(context.Background(), p, "系统提示", conv, "磁盘满了吗", []Tool{echoTool(&calls)}, func(e Event) { evs = append(evs, e) }); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "df -h" {
		t.Fatalf("应执行 df -h：%v", calls)
	}
	var types []string
	var streamed strings.Builder
	var usage Usage
	for _, e := range evs {
		switch e.Type {
		case "delta":
			streamed.WriteString(e.Text)
			continue
		case "thinking":
			continue
		case "usage":
			usage.Add(e.Usage)
			continue
		}
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "text,call,result,text,done" {
		t.Fatalf("事件顺序：%v", types)
	}
	// 流式：逐段收到的文字拼起来就是完整的回答；用量里命中缓存的另算
	if streamed.String() != "先看看磁盘。根目录已用 90%。" || usage.Input != 80 || usage.CacheRead != 120 || usage.Output != 40 {
		t.Fatalf("流式文字 %q，用量 %+v", streamed.String(), usage)
	}
	if fm.got[0]["stream"] != true || conv.Messages[1].Usage == nil {
		t.Fatalf("应请求流式并记下用量")
	}
	// 第二次请求带上了工具结果，并且对上 tool_call_id
	msgs := fm.got[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "call_1" || !strings.Contains(last["content"].(string), "/dev/sda1") {
		t.Fatalf("工具结果：%v", last)
	}
	// 思考模式的思考内容随工具调用原样带回
	if am := msgs[len(msgs)-2].(map[string]any); am["role"] != "assistant" || am["reasoning_content"] != "用户问磁盘，先 df。" {
		t.Fatalf("思考内容没带回：%v", am)
	}
	if fm.headers[0].Get("Authorization") != "Bearer k" || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("请求头或系统提示不对")
	}
	// 对话保存后能接着用
	dir := t.TempDir()
	if err := conv.Save(dir); err != nil {
		t.Fatal(err)
	}
	again, err := Load(dir, conv.ID)
	if err != nil || len(again.Messages) != 4 {
		t.Fatalf("读回的对话：%v %+v", err, again)
	}
}

func TestRunAnthropic(t *testing.T) {
	fm := &fakeModel{replies: []string{
		`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"tool_use",
		  "content":[{"type":"thinking","thinking":"","signature":"sig1"},{"type":"text","text":"先看看磁盘。"},{"type":"tool_use","id":"toolu_1","name":"run_command","input":{"command":"df -h"}}],
		  "usage":{"input_tokens":10,"output_tokens":5}}`,
		`{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn",
		  "content":[{"type":"text","text":"根目录已用 90%。"}],"usage":{"input_tokens":10,"output_tokens":5}}`,
	}}
	srv := httptest.NewServer(fm)
	defer srv.Close()
	p, err := NewProvider(Config{Provider: Anthropic, BaseURL: srv.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	conv := &Conversation{ID: NewID()}
	var final string
	err = Run(context.Background(), p, "系统提示", conv, "磁盘满了吗", []Tool{echoTool(&calls)}, func(e Event) {
		if e.Type == "text" {
			final = e.Text
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || final != "根目录已用 90%。" {
		t.Fatalf("calls=%v final=%q", calls, final)
	}
	first := fm.got[0]
	if first["model"] != "claude-opus-5-5" || first["fallbacks"] != "default" {
		t.Fatalf("默认模型与拒绝回退：%v %v", first["model"], first["fallbacks"])
	}
	if !strings.Contains(fm.headers[0].Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") || fm.headers[0].Get("X-Api-Key") != "sk-test" {
		t.Fatalf("请求头：%v", fm.headers[0])
	}
	// 第二次请求：上一轮的回复原样带回（含思考块），工具结果对上 tool_use_id
	msgs := fm.got[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("消息数：%d", len(msgs))
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["type"] != "thinking" || asst[2].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("模型的回复应原样带回：%v", asst)
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "toolu_1" {
		t.Fatalf("工具结果：%v", res)
	}
}

func TestToolErrorsGoBackToModel(t *testing.T) {
	fm := &fakeModel{replies: []string{
		`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"nope","arguments":"{}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"好的"}}]}`,
	}}
	srv := httptest.NewServer(fm)
	defer srv.Close()
	p, _ := NewProvider(Config{Provider: OpenAI, BaseURL: srv.URL, Model: "m", APIKey: "k"})
	conv := &Conversation{ID: NewID()}
	var errs int
	if err := Run(context.Background(), p, "s", conv, "hi", nil, func(e Event) {
		if e.Type == "result" && e.IsError {
			errs++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if errs != 1 || !conv.Messages[2].IsError {
		t.Fatalf("不存在的工具应作为出错的结果交回模型：%+v", conv.Messages)
	}
}

func TestProviderErrors(t *testing.T) {
	if _, err := NewProvider(Config{Provider: Anthropic}); err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("没有 Key：%v", err)
	}
	if _, err := NewProvider(Config{Provider: OpenAI, APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("OpenAI 兼容接口要填地址")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Provider: OpenAI, BaseURL: srv.URL, Model: "m", APIKey: "bad"})
	if _, err := p.Step(context.Background(), "s", []Message{{Role: "user", Text: "hi"}}, nil); err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("应报出接口的错误：%v", err)
	}
}

func TestClip(t *testing.T) {
	s := strings.Repeat("日志", 10000)
	c := Clip(s, 1000)
	if len(c) > 1200 || !strings.Contains(c, "中间省略") || !strings.HasPrefix(c, "日志") || !strings.HasSuffix(c, "日志") {
		t.Fatalf("%d %q", len(c), c[:50])
	}
}

// 不支持流式的服务（直接返回完整 JSON）也能用
func TestNoStreamFallback(t *testing.T) {
	fm := &fakeModel{noStream: true, replies: []string{`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`}}
	p, _ := NewProvider(Config{Provider: OpenAI, BaseURL: newServer(t, fm), Model: "m", APIKey: "k"})
	m, err := p.Step(context.Background(), "s", []Message{{Role: "user", Text: "hi"}}, nil)
	if err != nil || m.Text != "好" || m.Usage == nil || m.Usage.Input != 10 || m.Usage.Output != 2 {
		t.Fatalf("%v %+v", err, m)
	}
}

// 限流、服务器暂时出错时重试；请求本身有问题（400）不重试
func TestOpenAIRetry(t *testing.T) {
	old := retryWaits
	retryWaits = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryWaits = old }()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n <= 2 {
			http.Error(w, `{"error":{"message":"rate limited"}}`, 429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]}`)
	}))
	defer srv.Close()
	p, _ := NewProvider(Config{Provider: OpenAI, BaseURL: srv.URL, Model: "m", APIKey: "k"})
	m, err := p.Step(context.Background(), "s", []Message{{Role: "user", Text: "hi"}}, nil)
	if err != nil || m.Text != "好" || n != 3 {
		t.Fatalf("应重试两次后成功：%v %q %d", err, m.Text, n)
	}
	n = 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Error(w, `{"error":{"message":"bad request"}}`, 400)
	}))
	defer bad.Close()
	p, _ = NewProvider(Config{Provider: OpenAI, BaseURL: bad.URL, Model: "m", APIKey: "k"})
	if _, err := p.Step(context.Background(), "s", []Message{{Role: "user", Text: "hi"}}, nil); err == nil || n != 1 {
		t.Fatalf("400 不重试：%v %d", err, n)
	}
}
