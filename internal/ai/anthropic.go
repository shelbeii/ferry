package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

type anthropicProvider struct {
	client anthropic.Client
	model  string
	effort string
	images bool
}

func newAnthropic(c Config) *anthropicProvider {
	opts := []option.RequestOption{option.WithAPIKey(c.APIKey)}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	return &anthropicProvider{client: anthropic.NewClient(opts...), model: c.Model, effort: c.Effort, images: c.Images}
}

// 这些模型支持服务端的拒绝回退（被安全策略拒绝时由服务端换一个模型接着答）
func supportsFallback(model string) bool {
	for _, p := range []string{"claude-opus-5", "claude-fable-5", "claude-sonnet-5-5"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

func (p *anthropicProvider) Step(ctx context.Context, system string, msgs []Message, tools []Tool) (Message, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: system, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
		Messages:  anthropicMessages(msgs, p.model, p.images),
	}
	if p.effort != "" {
		params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(p.effort)
	}
	for _, t := range tools {
		props, _ := t.Schema["properties"]
		var req []string
		if r, ok := t.Schema["required"].([]string); ok {
			req = r
		}
		tp := anthropic.BetaToolParam{
			Name:        t.Name,
			Description: param.NewOpt(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: props, Required: req},
		}
		params.Tools = append(params.Tools, anthropic.BetaToolUnionParam{OfTool: &tp})
	}
	if supportsFallback(p.model) {
		params.Betas = append(params.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	// 流式：文字和思考内容边生成边交给界面，最后拼成完整的回复
	stream := p.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var resp anthropic.BetaMessage
	for stream.Next() {
		ev := stream.Current()
		if err := resp.Accumulate(ev); err != nil {
			return Message{}, fmt.Errorf("模型返回的内容不完整：%v", err)
		}
		if ev.Type == "content_block_delta" {
			switch ev.Delta.Type {
			case "text_delta":
				emitDelta(ctx, "delta", ev.Delta.Text)
			case "thinking_delta":
				emitDelta(ctx, "thinking", ev.Delta.Thinking)
			}
		}
	}
	if err := stream.Err(); err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return Message{}, fmt.Errorf("模型接口返回 %d：%s", apiErr.StatusCode, shortBody(apiErr.RawJSON()))
		}
		return Message{}, fmt.Errorf("请求模型失败：%v", err)
	}
	out := Message{Role: "assistant", Raw: json.RawMessage(resp.RawJSON()), Provider: Anthropic, Model: p.model,
		Usage: &Usage{Input: resp.Usage.InputTokens, CacheRead: resp.Usage.CacheReadInputTokens,
			CacheWrite: resp.Usage.CacheCreationInputTokens, Output: resp.Usage.OutputTokens}}
	var texts []string
	for _, b := range resp.Content {
		switch v := b.AsAny().(type) {
		case anthropic.BetaTextBlock:
			texts = append(texts, v.Text)
		case anthropic.BetaToolUseBlock:
			out.Calls = append(out.Calls, ToolCall{ID: v.ID, Name: v.Name, Input: json.RawMessage(v.JSON.Input.Raw())})
		}
	}
	out.Text = strings.Join(texts, "\n")
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		out.Text = strings.TrimSpace(out.Text + "\n（模型拒绝回答这个请求：" + string(resp.StopDetails.Category) + "）")
		out.Calls = nil
	case anthropic.BetaStopReasonMaxTokens:
		if len(out.Calls) == 0 {
			out.Text += "\n（回答太长被截断了）"
		}
	}
	return out, nil
}

// anthropicMessages 对话 → Anthropic 的消息。模型自己的回复用原始内容带回（思考内容要原样保留）；
// 连续的工具结果合成一条 user 消息。用户附的图片放在文字前面；images 为 false（模型看不了图片）时换成一句说明。
func anthropicMessages(msgs []Message, model string, images bool) []anthropic.BetaMessageParam {
	var out []anthropic.BetaMessageParam
	var results []anthropic.BetaContentBlockParamUnion
	flush := func() {
		if len(results) > 0 {
			out = append(out, anthropic.NewBetaUserMessage(results...))
			results = nil
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			text := m.Text
			if text == "" {
				text = "（没有输出）"
			}
			results = append(results, anthropic.NewBetaToolResultBlock(m.CallID, text, m.IsError))
		case "user":
			flush()
			var blocks []anthropic.BetaContentBlockParamUnion
			text := m.Text
			if len(m.Images) > 0 && !images {
				text = strings.TrimSpace(imagesNote(len(m.Images)) + "\n" + text)
			} else {
				for _, im := range m.Images {
					blocks = append(blocks, anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
						Data: im.Data, MediaType: anthropic.BetaBase64ImageSourceMediaType(im.Media)}))
				}
			}
			if text != "" || len(blocks) == 0 {
				blocks = append(blocks, anthropic.NewBetaTextBlock(text))
			}
			out = append(out, anthropic.NewBetaUserMessage(blocks...))
		case "assistant":
			flush()
			if m.Provider == Anthropic && len(m.Raw) > 0 && (m.Model == "" || m.Model == model) {
				var bm anthropic.BetaMessage
				if json.Unmarshal(m.Raw, &bm) == nil {
					out = append(out, bm.ToParam())
					continue
				}
			}
			var blocks []anthropic.BetaContentBlockParamUnion
			if strings.TrimSpace(m.Text) != "" {
				blocks = append(blocks, anthropic.NewBetaTextBlock(m.Text))
			}
			for _, c := range m.Calls {
				var in any
				json.Unmarshal(c.Input, &in)
				blocks = append(blocks, anthropic.NewBetaToolUseBlock(c.ID, in, c.Name))
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: blocks})
			}
		}
	}
	flush()
	return out
}

func shortBody(s string) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(s), &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return Clip(strings.TrimSpace(s), 400)
}
