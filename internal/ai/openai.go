package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OpenAI 兼容接口（/chat/completions）：DeepSeek、通义千问、Kimi、内网用 vLLM / Ollama 部署的模型等。
type openaiProvider struct {
	base, model, key, effort, thinking string
	images                             bool
	http                               *http.Client
}

func newOpenAI(c Config) *openaiProvider {
	return &openaiProvider{base: strings.TrimRight(c.BaseURL, "/"), model: c.Model, key: c.APIKey, effort: c.Effort,
		thinking: ThinkingStyle(c.Thinking, c.BaseURL, c.Model), images: c.Images,
		http: &http.Client{Timeout: 10 * time.Minute}}
}

type oaMessage struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
	// Parts 用户附了图片时代替 Content：[{type: text}, {type: image_url}]
	Parts []oaPart `json:"-"`
	// 思考模式（如 DeepSeek 的推理）返回的思考内容；带工具调用时下一轮要原样带回
	Reasoning  *string      `json:"reasoning_content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaPart struct {
	Type     string `json:"type"` // text | image_url
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// MarshalJSON 有 Parts 时 content 写成数组。
func (m oaMessage) MarshalJSON() ([]byte, error) {
	type plain oaMessage
	if len(m.Parts) == 0 {
		return json.Marshal(plain(m))
	}
	return json.Marshal(struct {
		plain
		Content []oaPart `json:"content"`
	}{plain(m), m.Parts})
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func str(s string) *string { return &s }

func (p *openaiProvider) Step(ctx context.Context, system string, msgs []Message, tools []Tool) (Message, error) {
	req := map[string]any{"model": p.model, "messages": openaiMessages(system, msgs, p.model, p.images)}
	p.thinkingParams(req)
	if len(tools) > 0 {
		var ts []map[string]any
		for _, t := range tools {
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Schema}})
		}
		req["tools"] = ts
	}
	// 流式：文字和思考内容边生成边交给界面；最后一段带用量
	req["stream"] = true
	req["stream_options"] = map[string]any{"include_usage": true}
	body, _ := json.Marshal(req)
	resp, err := p.post(ctx, body)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		// 不支持流式的服务：按一次性返回解析
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		return p.parseFull(raw)
	}
	var text, reasoning strings.Builder
	type pendingCall struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*pendingCall{}
	var order []int
	finish := ""
	var usage *oaUsage
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					Reasoning *string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *oaUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ch) != nil {
			continue
		}
		if ch.Error != nil {
			return Message{}, fmt.Errorf("模型接口出错：%s", ch.Error.Message)
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
		if len(ch.Choices) == 0 {
			continue
		}
		c := ch.Choices[0]
		if c.FinishReason != nil {
			finish = *c.FinishReason
		}
		if d := c.Delta.Reasoning; d != nil && *d != "" {
			reasoning.WriteString(*d)
			emitDelta(ctx, "thinking", *d)
		}
		if d := c.Delta.Content; d != nil && *d != "" {
			text.WriteString(*d)
			emitDelta(ctx, "delta", *d)
		}
		for _, tc := range c.Delta.ToolCalls {
			pc := calls[tc.Index]
			if pc == nil {
				pc = &pendingCall{}
				calls[tc.Index] = pc
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				pc.id = tc.ID
			}
			if tc.Function.Name != "" {
				pc.name = tc.Function.Name
			}
			pc.args.WriteString(tc.Function.Arguments)
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("读取模型的回复中断了：%v", err)
	}
	out := Message{Role: "assistant", Provider: OpenAI, Model: p.model, Text: text.String(), Usage: usage.toUsage()}
	if reasoning.Len() > 0 {
		out.Raw, _ = json.Marshal(reasoning.String())
	}
	for _, i := range order {
		pc := calls[i]
		in := json.RawMessage(pc.args.String())
		if !json.Valid(in) {
			in = json.RawMessage("{}")
		}
		out.Calls = append(out.Calls, ToolCall{ID: pc.id, Name: pc.name, Input: in})
	}
	if finish == "length" && len(out.Calls) == 0 {
		out.Text += "\n（回答太长被截断了）"
	}
	return out, nil
}

// retryWaits 请求失败（限流 429、服务器暂时出错 5xx、网络问题）时，隔多久重试；已经开始收回复后不再重试。
var retryWaits = []time.Duration{time.Second, 3 * time.Second}

// post 发请求，失败的可以重试的话重试几次。返回 2xx 的响应。
func (p *openaiProvider) post(ctx context.Context, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		hr, err := http.NewRequestWithContext(ctx, "POST", p.base+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("Authorization", "Bearer "+p.key)
		resp, err := p.http.Do(hr)
		var failure error
		wait := time.Duration(0)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failure = fmt.Errorf("请求模型失败：%v", err)
		case resp.StatusCode/100 == 2:
			return resp, nil
		default:
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			failure = fmt.Errorf("模型接口返回 %d：%s", resp.StatusCode, shortBody(string(raw)))
			retryable := resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504
			if !retryable {
				return nil, failure
			}
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 30 {
				wait = time.Duration(s) * time.Second
			}
		}
		if attempt >= len(retryWaits) {
			return nil, failure
		}
		if wait == 0 {
			wait = retryWaits[attempt]
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// thinkingParams 按服务商的方式传思考设置。effort 为空时什么都不传（用模型的默认）。
func (p *openaiProvider) thinkingParams(req map[string]any) {
	e := p.effort
	if e == "" {
		return
	}
	switch p.thinking {
	case "deepseek": // DeepSeek：只有开、关
		req["thinking"] = map[string]any{"type": map[bool]string{true: "disabled", false: "enabled"}[e == "off"]}
	case "qwen": // 通义千问：开关，低、中时限制思考的长度
		req["enable_thinking"] = e != "off"
		switch e {
		case "low":
			req["thinking_budget"] = 2048
		case "medium":
			req["thinking_budget"] = 8192
		}
	case "none":
	default: // reasoning_effort：low / medium / high
		switch e {
		case "off", "on":
		case "xhigh", "max":
			req["reasoning_effort"] = "high"
		default:
			req["reasoning_effort"] = e
		}
	}
}

// oaUsage OpenAI 兼容接口的用量。命中缓存的输入：OpenAI 在 prompt_tokens_details.cached_tokens，DeepSeek 在 prompt_cache_hit_tokens。
type oaUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
}

func (u *oaUsage) toUsage() *Usage {
	if u == nil {
		return nil
	}
	cached := u.PromptCacheHitTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > cached {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return &Usage{Input: max(u.PromptTokens-cached, 0), CacheRead: cached, Output: u.CompletionTokens}
}

// parseFull 一次性返回的回复（不支持流式的服务）。
func (p *openaiProvider) parseFull(raw []byte) (Message, error) {
	var r struct {
		Choices []struct {
			Message      oaMessage `json:"message"`
			FinishReason string    `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Choices) == 0 {
		return Message{}, fmt.Errorf("模型接口返回的内容看不懂：%s", Clip(string(raw), 400))
	}
	m := r.Choices[0].Message
	out := Message{Role: "assistant", Provider: OpenAI, Model: p.model, Usage: r.Usage.toUsage()}
	if m.Content != nil {
		out.Text = *m.Content
	}
	if m.Reasoning != nil && *m.Reasoning != "" {
		out.Raw, _ = json.Marshal(*m.Reasoning)
	}
	for _, c := range m.ToolCalls {
		in := json.RawMessage(c.Function.Arguments)
		if !json.Valid(in) {
			in = json.RawMessage("{}")
		}
		out.Calls = append(out.Calls, ToolCall{ID: c.ID, Name: c.Function.Name, Input: in})
	}
	if r.Choices[0].FinishReason == "length" && len(out.Calls) == 0 {
		out.Text += "\n（回答太长被截断了）"
	}
	return out, nil
}

func openaiMessages(system string, msgs []Message, model string, images bool) []oaMessage {
	out := []oaMessage{{Role: "system", Content: str(system)}}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			um := oaMessage{Role: "user", Content: str(m.Text)}
			if len(m.Images) > 0 && !images {
				um.Content = str(strings.TrimSpace(imagesNote(len(m.Images)) + "\n" + m.Text))
			} else if len(m.Images) > 0 {
				for _, im := range m.Images {
					p := oaPart{Type: "image_url"}
					p.ImageURL = &struct {
						URL string `json:"url"`
					}{"data:" + im.Media + ";base64," + im.Data}
					um.Parts = append(um.Parts, p)
				}
				if m.Text != "" {
					um.Parts = append(um.Parts, oaPart{Type: "text", Text: m.Text})
				}
			}
			out = append(out, um)
		case "tool":
			text := m.Text
			if m.IsError {
				text = "（出错）" + text
			}
			out = append(out, oaMessage{Role: "tool", ToolCallID: m.CallID, Content: str(text)})
		case "assistant":
			am := oaMessage{Role: "assistant", Content: str(m.Text)}
			var reasoning string
			if m.Provider == OpenAI && (m.Model == "" || m.Model == model) && json.Unmarshal(m.Raw, &reasoning) == nil && reasoning != "" {
				am.Reasoning = &reasoning
			}
			for _, c := range m.Calls {
				var tc oaToolCall
				tc.ID, tc.Type = c.ID, "function"
				tc.Function.Name, tc.Function.Arguments = c.Name, string(c.Input)
				am.ToolCalls = append(am.ToolCalls, tc)
			}
			out = append(out, am)
		}
	}
	return out
}
