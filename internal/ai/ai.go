// Package ai 让大模型通过 Ferry 查看、操作服务器：用户说要做什么，模型调用工具（命令、应用状态、日志、安装软件……），
// Ferry 在服务器上执行后把结果交回模型，直到模型给出回答。
//
// 只读的工具（ReadOnly 检查通过的命令、查状态、看日志）自动执行；会改动服务器的工具（Tool.Approve 返回要确认的内容）
// 先停下来等用户确认：对话保存待确认的调用，用户确认或拒绝后（Loop.Decide）接着执行。
package ai

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Config 模型配置（设置 → AI）。APIKey 存在系统凭据库，不在设置文件里。
type Config struct {
	Provider string // anthropic | openai（OpenAI 兼容接口：DeepSeek、通义千问、内网部署的模型……）
	BaseURL  string // 空 = 服务商默认地址
	Model    string
	APIKey   string
	// Effort 思考强度：空 = 不设置 | off | on | low | medium | high | xhigh | max
	Effort string
	// Thinking OpenAI 兼容接口怎么传思考设置（见 ThinkingStyle）
	Thinking string
	// Images 模型能看图片：不能时对话里的图片换成一句说明再发
	Images bool
}

// SupportsImages 模型能不能看图片。setting 是设置里的选择：on | off | 空 = 按服务商和模型名识别
// （Anthropic 都能；OpenAI 兼容接口看模型名里有没有 vl、vision、4o 这类字样，认不出来的当作不能，可以在设置里改）。
func SupportsImages(setting, provider, model string) bool {
	switch setting {
	case "on":
		return true
	case "off":
		return false
	}
	if provider != OpenAI {
		return true
	}
	m := strings.ToLower(model)
	for _, k := range visionHints {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// visionHints 能看图片的模型名里常见的字样（和桌面版 ai.ts 里的一致）。
var visionHints = []string{"vl", "vision", "4o", "gpt-4.1", "gpt-5", "omni", "qvq", "gemini", "claude", "4v", "5v", "llava", "pixtral"}

// ThinkingStyle OpenAI 兼容接口传思考设置的方式：effort（reasoning_effort，OpenAI 等）、deepseek（thinking: {type}）、
// qwen（enable_thinking、thinking_budget，通义千问）、none（不传）。style 为空时按接口地址和模型名识别。
func ThinkingStyle(style, baseURL, model string) string {
	if style != "" {
		return style
	}
	b, m := strings.ToLower(baseURL), strings.ToLower(model)
	switch {
	case strings.Contains(b, "deepseek") || strings.HasPrefix(m, "deepseek"):
		return "deepseek"
	case strings.Contains(b, "dashscope") || strings.Contains(b, "aliyuncs") || strings.HasPrefix(m, "qwen") || strings.HasPrefix(m, "qwq"):
		return "qwen"
	}
	return "effort"
}

// 服务商
const (
	Anthropic = "anthropic"
	OpenAI    = "openai"
)

// DefaultModel 各服务商的默认模型。
func DefaultModel(provider string) string {
	if provider == Anthropic {
		return "claude-opus-5-5"
	}
	return ""
}

// Message 对话里的一条：用户的话、模型的回复（文字与工具调用）、工具的结果。
type Message struct {
	Role    string     `json:"role"` // user | assistant | tool
	Text    string     `json:"text,omitempty"`
	Images  []Image    `json:"images,omitempty"`  // user：附的图片
	Calls   []ToolCall `json:"calls,omitempty"`   // assistant
	CallID  string     `json:"callId,omitempty"`  // tool
	IsError bool       `json:"isError,omitempty"` // tool
	// Raw 服务商返回的原始回复（assistant）：同一服务商再请求时原样带回（含思考内容），换了服务商用上面的字段重建
	Raw      json.RawMessage `json:"raw,omitempty"`
	Provider string          `json:"provider,omitempty"`
	Model    string          `json:"model,omitempty"` // 哪个模型的回复：换了模型时不带回原始内容（思考内容只对原模型有效）
	Usage    *Usage          `json:"usage,omitempty"` // 这次请求用了多少 token（assistant）
}

// Usage 一次请求的 token 用量。Input 是没命中缓存的输入，CacheRead 命中缓存的输入（一般便宜很多），CacheWrite 写入缓存的输入。
type Usage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cacheRead,omitempty"`
	CacheWrite int64 `json:"cacheWrite,omitempty"`
	Output     int64 `json:"output"`
}

// Add 累加。
func (u *Usage) Add(o *Usage) {
	if o == nil {
		return
	}
	u.Input += o.Input
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Output += o.Output
}

// UsageByModel 对话里各模型一共用了多少 token（估算费用用）。
func (c *Conversation) UsageByModel() map[string]Usage {
	out := map[string]Usage{}
	for _, m := range c.Messages {
		if m.Usage != nil {
			u := out[m.Model]
			u.Add(m.Usage)
			out[m.Model] = u
		}
	}
	return out
}

type deltaKey struct{}

// withDeltas 模型回复的过程中，把逐段生成的文字（kind = delta）和思考内容（kind = thinking）交给 f。
func withDeltas(ctx context.Context, f func(kind, text string)) context.Context {
	return context.WithValue(ctx, deltaKey{}, f)
}

// emitDelta 服务商把收到的一小段回复交给界面。
func emitDelta(ctx context.Context, kind, text string) {
	if text == "" {
		return
	}
	if f, ok := ctx.Value(deltaKey{}).(func(string, string)); ok {
		f(kind, text)
	}
}

// Image 用户附的一张图片（截图等），原样存在对话里。
type Image struct {
	Media string `json:"media"` // image/png | image/jpeg | image/gif | image/webp
	Data  string `json:"data"`  // base64
}

// 图片的限制（Anthropic 的上限是单张 5 MB）
const (
	MaxImages     = 8
	MaxImageBytes = 5 << 20
)

// CheckImages 检查用户附的图片：格式、大小、张数。
func CheckImages(imgs []Image) error {
	if len(imgs) > MaxImages {
		return fmt.Errorf("一次最多附 %d 张图片", MaxImages)
	}
	for i, im := range imgs {
		switch im.Media {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return fmt.Errorf("第 %d 张图片的格式不支持：%s（只支持 PNG、JPEG、GIF、WebP）", i+1, im.Media)
		}
		b, err := base64.StdEncoding.DecodeString(im.Data)
		if err != nil || len(b) == 0 {
			return fmt.Errorf("第 %d 张图片的内容无效", i+1)
		}
		if len(b) > MaxImageBytes {
			return fmt.Errorf("第 %d 张图片太大：%.1f MB（最多 %d MB）", i+1, float64(len(b))/(1<<20), MaxImageBytes>>20)
		}
	}
	return nil
}

// imagesNote 模型看不了图片时，代替图片发过去的说明。
func imagesNote(n int) string {
	return fmt.Sprintf("（用户附了 %d 张图片，但当前模型看不了图片。如果需要图片里的内容，请用户用文字说明。）", n)
}

// ToolCall 模型要调用的工具。
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Tool 交给模型的工具。
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema（object）
	Run         func(ctx context.Context, input json.RawMessage) (string, error)
	// Approve 这次调用要不要用户确认：返回 nil 表示不用（只读）；没有设置也不用。
	Approve func(ctx context.Context, input json.RawMessage) *Approval
}

// Approval 要用户确认的一个改动。
type Approval struct {
	Kind   string `json:"kind"`             // 同类操作的标识：用户选「本次对话同类不再询问」后按它放行
	Title  string `json:"title"`            // 一句话说明，如「执行命令」「重启应用 System」
	Detail string `json:"detail,omitempty"` // 完整的命令、要写的文件内容（或差异）等
	Danger string `json:"danger,omitempty"` // 危险的原因：非空时每次都要确认
	Once   string `json:"once,omitempty"`   // 每次都要确认的原因（执行别的命令或代码、访问外部地址……），不能「同类不再询问」
	AsRoot bool   `json:"asRoot,omitempty"` // 以 root 执行
	// Fields 要用户填的密码、账号（ask_secret）：界面上显示输入框，用户填好了才执行
	Fields []SecretField `json:"fields,omitempty"`
}

// 用户的决定
const (
	Approve = "approve" // 执行
	Always  = "always"  // 执行，并且本次对话里同类的不再询问
	Reject  = "reject"  // 不执行
)

// Pending 等用户确认的工具调用：第一个要确认，后面的是模型同一轮里要的其余调用。
type Pending struct {
	Calls    []ToolCall `json:"calls"`
	Approval Approval   `json:"approval"`
}

// AuditEntry 一条改动记录（要确认的工具调用，不管执行没执行）。
type AuditEntry struct {
	Time     time.Time `json:"time"`
	Config   string    `json:"config"`
	Target   string    `json:"target"`
	Conv     string    `json:"conv"`
	Tool     string    `json:"tool"`
	Input    string    `json:"input"` // 调用的参数（遮住了密码）
	Kind     string    `json:"kind"`
	Danger   string    `json:"danger,omitempty"`
	Decision string    `json:"decision"` // approve | always | allowed（同类已放行）| reject | skipped（没确认就说了别的）
	Result   string    `json:"result,omitempty"`
	IsError  bool      `json:"isError,omitempty"`
}

// Provider 一种模型接口：发一次请求，返回模型这一轮的回复。
type Provider interface {
	Step(ctx context.Context, system string, msgs []Message, tools []Tool) (Message, error)
}

// Event 对话的进展，逐条交给界面。
type Event struct {
	Type    string    `json:"type"` // delta | thinking | text | usage | call | output | confirm | result | done
	Text    string    `json:"text,omitempty"`
	Call    *ToolCall `json:"call,omitempty"`
	Result  string    `json:"result,omitempty"`
	IsError bool      `json:"isError,omitempty"`
	// Approval confirm：要用户确认的内容；call：同类已放行、直接执行的改动（Allowed 为 true）
	Approval *Approval `json:"approval,omitempty"`
	Allowed  bool      `json:"allowed,omitempty"`
	// usage：这次请求的用量和模型
	Usage *Usage `json:"usage,omitempty"`
	Model string `json:"model,omitempty"`
}

// Conversation 一次对话，保存在数据目录的 ai/<id>.json：下一句话接着这段上下文。
type Conversation struct {
	ID       string    `json:"id"`
	Config   string    `json:"config"` // 项目配置文件（或连接列表）
	Target   string    `json:"target"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Messages []Message `json:"messages"`
	// Pending 等用户确认的调用（确认前对话停在这里）
	Pending *Pending `json:"pending,omitempty"`
	// Allowed 本次对话里用户放行的同类改动（Approval.Kind）
	Allowed map[string]bool `json:"allowed,omitempty"`
	// Decisions 各个改动（按工具调用 ID）用户的决定
	Decisions map[string]Decided `json:"decisions,omitempty"`
	// Executing 正在执行的调用（第一个）和同一轮里后面还没执行的：执行前就保存，
	// Ferry 中途退出时据此把它标成「结果未知」，不会再显示成等确认、被重复执行
	Executing []ToolCall `json:"executing,omitempty"`
}

// Recover 上次执行到一半就退出了（Executing 不为空）：正在执行的那个记为结果未知，后面的记为没有执行。返回是否有这种情况。
func (c *Conversation) Recover() bool {
	if len(c.Executing) == 0 {
		return false
	}
	for i, call := range c.Executing {
		text := "没有执行：上一个操作执行时中断了。"
		if i == 0 {
			text = "结果未知：执行时 Ferry 退出或连接断开了，不知道有没有执行完。先查看服务器上的实际情况，不要直接重做。"
		}
		c.Messages = append(c.Messages, Message{Role: "tool", CallID: call.ID, Text: text, IsError: true})
	}
	c.Executing = nil
	return true
}

var reConvID = regexp.MustCompile(`^[0-9a-z-]{8,40}$`)

// NewID 新对话的 ID。
func NewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// Load 读对话；不存在时返回新的空对话。
func Load(dir, id string) (*Conversation, error) {
	if !reConvID.MatchString(id) {
		return nil, fmt.Errorf("对话 ID 无效：%q", id)
	}
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return &Conversation{ID: id, Created: time.Now()}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Conversation
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("对话记录损坏：%v", err)
	}
	return &c, nil
}

// Save 保存对话（只有本人能读：里面有服务器上的命令输出）。
func (c *Conversation) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	makePrivate(dir)
	c.Updated = time.Now()
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, c.ID+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, c.ID+".json"))
}

// MaxSteps 一句话里模型最多连续调用几轮工具。
const MaxSteps = 30

// ResultLimit 交给模型的工具结果最多多少字节（太长的保留开头和结尾）。
const ResultLimit = 16 << 10

// Loop 一段对话的执行：模型、系统提示、工具。
type Loop struct {
	Provider Provider
	System   string
	Tools    []Tool
	Emit     func(Event)
	// Audit 记下每个要确认的调用（可以为空）
	Audit func(AuditEntry)
	// Save 保存对话（可以为空）：每执行一步就存一次，中途退出也不会丢掉已经做了的决定
	Save func()
}

func (l *Loop) save() {
	if l.Save != nil {
		l.Save()
	}
}

// Run 简写：把用户的话交给模型，直到它给出回答。
func Run(ctx context.Context, p Provider, system string, conv *Conversation, user string, tools []Tool, emit func(Event)) error {
	return (&Loop{Provider: p, System: system, Tools: tools, Emit: emit}).Send(ctx, conv, user, nil)
}

// Send 把用户的话（和附的图片）加进对话，循环：请求模型 → 执行它要的工具 → 把结果交回去，直到模型不再调用工具，
// 或者遇到要用户确认的改动（对话的 Pending 不为空，等 Decide）。
func (l *Loop) Send(ctx context.Context, conv *Conversation, user string, images []Image) error {
	if pd := conv.Pending; pd != nil {
		// 没有确认就接着说了别的：待确认的都不执行，告诉模型
		l.audit(conv, pd.Calls[0], &pd.Approval, "skipped", "", false)
		for _, c := range pd.Calls {
			conv.Messages = append(conv.Messages, Message{Role: "tool", CallID: c.ID, IsError: true,
				Text: "没有执行：用户没有确认这个操作，接着说了下面的话。"})
		}
		conv.Pending = nil
	}
	// 用户又在对话里直接打了填过的值：也遮住
	conv.Messages = append(conv.Messages, Message{Role: "user", Text: MaskSecrets(conv.ID, user), Images: images})
	l.save()
	return l.loop(ctx, conv)
}

// Decide 用户对待确认的改动做了决定（Approve / Always / Reject），接着执行。
func (l *Loop) Decide(ctx context.Context, conv *Conversation, decision string) error {
	pd := conv.Pending
	if pd == nil || len(pd.Calls) == 0 {
		return errors.New("没有等待确认的操作（可能已经处理过了）")
	}
	if decision != Approve && decision != Always && decision != Reject {
		return fmt.Errorf("不认识的决定：%s", decision)
	}
	conv.Pending = nil
	paused, err := l.runCalls(ctx, conv, pd.Calls, &pd.Approval, decision)
	if err != nil || paused {
		return err
	}
	return l.loop(ctx, conv)
}

func (l *Loop) loop(ctx context.Context, conv *Conversation) error {
	for step := 0; step < MaxSteps; step++ {
		sctx := withDeltas(ctx, func(kind, text string) { l.Emit(Event{Type: kind, Text: text}) })
		reply, err := l.Provider.Step(sctx, l.System, compactForModel(conv.Messages), l.Tools)
		if err != nil {
			return err
		}
		conv.Messages = append(conv.Messages, reply)
		if strings.TrimSpace(reply.Text) != "" {
			l.Emit(Event{Type: "text", Text: reply.Text})
		}
		if reply.Usage != nil {
			l.Emit(Event{Type: "usage", Usage: reply.Usage, Model: reply.Model})
		}
		if len(reply.Calls) == 0 {
			l.Emit(Event{Type: "done"})
			return nil
		}
		paused, err := l.runCalls(ctx, conv, reply.Calls, nil, "")
		if err != nil || paused {
			return err
		}
	}
	return fmt.Errorf("连续调用了 %d 轮工具还没有结论，先停下；可以换个更具体的问法", MaxSteps)
}

// runCalls 依次执行模型这一轮要的调用。first 不为空时，第一个调用是用户刚做了决定（decision）的。
// 遇到要确认的调用时停下（保存到 conv.Pending），返回 paused。
func (l *Loop) runCalls(ctx context.Context, conv *Conversation, calls []ToolCall, first *Approval, decision string) (paused bool, err error) {
	byName := map[string]Tool{}
	for _, t := range l.Tools {
		byName[t.Name] = t
	}
	for i, c := range calls {
		c := c
		var ap *Approval
		how := ""
		if i == 0 && first != nil {
			ap, how = first, decision
			if decision == Reject {
				out := "没有执行：用户不同意这个操作。不要换个方式去做同样的事；需要的话先问用户。"
				if len(ap.Fields) > 0 {
					out = "用户没有填写，取消了。不要让用户在对话里直接发密码；需要的话先问用户想怎么办。"
				}
				l.audit(conv, c, ap, Reject, "", false)
				l.Emit(Event{Type: "result", Call: &c, Result: out, IsError: true})
				conv.Messages = append(conv.Messages, Message{Role: "tool", CallID: c.ID, Text: out, IsError: true})
				l.save()
				continue
			}
			if decision == Always && ap.Danger == "" && ap.Once == "" {
				if conv.Allowed == nil {
					conv.Allowed = map[string]bool{}
				}
				conv.Allowed[ap.Kind] = true
			}
		} else {
			if t, ok := byName[c.Name]; ok && t.Approve != nil {
				ap = t.Approve(ctx, c.Input)
			}
			switch {
			case ap == nil:
				l.Emit(Event{Type: "call", Call: &c})
			case ap.Danger == "" && ap.Once == "" && conv.Allowed[ap.Kind]:
				how = "allowed"
				l.Emit(Event{Type: "call", Call: &c, Approval: ap, Allowed: true})
			default:
				conv.Pending = &Pending{Calls: calls[i:], Approval: *ap}
				l.save()
				l.Emit(Event{Type: "call", Call: &c})
				l.Emit(Event{Type: "confirm", Call: &c, Approval: ap})
				return true, nil
			}
		}
		// 先记下「正在执行」再执行：中途退出时不会被当成还没确认
		record(conv, c, ap, how)
		conv.Executing = calls[i:]
		l.save()
		out, isErr := runTool(withProgress(ctx, l.Emit, c), byName, c)
		conv.Executing = nil
		if ctx.Err() != nil {
			// 停止了：这个调用和后面的都没有结果，补上，免得对话接不下去
			for _, rest := range calls[i:] {
				conv.Messages = append(conv.Messages, Message{Role: "tool", CallID: rest.ID, IsError: true, Text: "用户停止了执行。"})
			}
			l.audit(conv, c, ap, how, "用户停止了执行", true)
			return false, ctx.Err()
		}
		l.audit(conv, c, ap, how, out, isErr)
		l.Emit(Event{Type: "result", Call: &c, Result: out, IsError: isErr})
		// 交给模型（也存进对话记录）的输出遮住密码、密钥和用户填过的值；界面上实时显示的是完整的
		conv.Messages = append(conv.Messages, Message{Role: "tool", CallID: c.ID, Text: Clip(Redact(MaskSecrets(conv.ID, out)), ResultLimit), IsError: isErr})
		l.save()
	}
	return false, nil
}

// record 把用户对这个改动的决定记进对话。
func record(conv *Conversation, c ToolCall, ap *Approval, decision string) {
	if ap == nil {
		return
	}
	if conv.Decisions == nil {
		conv.Decisions = map[string]Decided{}
	}
	conv.Decisions[c.ID] = Decided{Approval: *ap, Decision: decision}
}

func (l *Loop) audit(conv *Conversation, c ToolCall, ap *Approval, decision, result string, isErr bool) {
	record(conv, c, ap, decision)
	if ap == nil || l.Audit == nil {
		return
	}
	l.Audit(AuditEntry{Time: time.Now(), Config: conv.Config, Target: conv.Target, Conv: conv.ID, Tool: c.Name, Input: Redact(string(c.Input)),
		Kind: ap.Kind, Danger: ap.Danger, Decision: decision, Result: Clip(Redact(MaskSecrets(conv.ID, result)), 2000), IsError: isErr})
}

type progressKey struct{}

func withProgress(ctx context.Context, emit func(Event), c ToolCall) context.Context {
	return context.WithValue(ctx, progressKey{}, func(text string) { emit(Event{Type: "output", Call: &c, Text: text}) })
}

// WithProgress 不在对话里执行工具时（编程软件经 MCP 请求的），把工具的实时输出交给 f。
func WithProgress(ctx context.Context, f func(string)) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

// Progress 工具执行中的实时输出（如安装命令的输出），交给界面显示。
func Progress(ctx context.Context, text string) {
	if f, ok := ctx.Value(progressKey{}).(func(string)); ok {
		f(text)
	}
}

func runTool(ctx context.Context, byName map[string]Tool, c ToolCall) (string, bool) {
	t, ok := byName[c.Name]
	if !ok {
		return "没有这个工具：" + c.Name, true
	}
	out, err := t.Run(ctx, c.Input)
	if err != nil {
		if out != "" {
			out += "\n"
		}
		return out + err.Error(), true
	}
	return out, false
}

// AppendAudit 把一条改动记录追加到文件（每行一条 JSON，只有本人能读）。
func AppendAudit(file string, e AuditEntry) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	makePrivate(filepath.Dir(file))
	rotateAudit(file)
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, err = f.Write(append(b, '\n'))
	return err
}

// Clip 太长的文本保留开头和结尾。
func Clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head, tail := max*2/5, max*3/5
	h := s[:head]
	for !utf8.ValidString(h) && len(h) > 0 {
		h = h[:len(h)-1]
	}
	t := s[len(s)-tail:]
	for !utf8.ValidString(t) && len(t) > 0 {
		t = t[1:]
	}
	return fmt.Sprintf("%s\n…（中间省略 %d 字节）…\n%s", h, len(s)-len(h)-len(t), t)
}

// NewProvider 按配置创建模型接口。
func NewProvider(c Config) (Provider, error) {
	if c.APIKey == "" {
		return nil, errors.New("还没有设置 AI 模型的 API Key：在「设置 → AI」里填写")
	}
	if c.Model == "" {
		c.Model = DefaultModel(c.Provider)
	}
	if c.Model == "" {
		return nil, errors.New("还没有设置模型名：在「设置 → AI」里填写")
	}
	switch c.Provider {
	case Anthropic, "":
		return newAnthropic(c), nil
	case OpenAI:
		if c.BaseURL == "" {
			return nil, errors.New("OpenAI 兼容接口要填接口地址（如 https://api.deepseek.com/v1）")
		}
		return newOpenAI(c), nil
	}
	return nil, fmt.Errorf("不认识的服务商：%s", c.Provider)
}
