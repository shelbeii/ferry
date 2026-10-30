// Package ui 终端输出与 JSON 输出（GUI 以 sidecar 方式调用 ferry --json 时使用）。
package ui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/captain/ferry/internal/proto"
)

const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
)

// Console 终端输出。
type Console struct {
	Out     io.Writer
	Verbose bool // 实时显示构建输出

	mu       sync.Mutex
	color    bool
	tty      bool
	progress bool // 当前行是进度行
	lastPct  int
	in       *bufio.Reader
}

// NewConsole 输出到 stderr（stdout 留给 --json 与表格）。
func NewConsole(verbose bool) *Console {
	tty := term.IsTerminal(int(os.Stderr.Fd()))
	color := tty && os.Getenv("NO_COLOR") == "" && enableVT()
	return &Console{Out: os.Stderr, Verbose: verbose, color: color, tty: tty, lastPct: -1, in: bufio.NewReader(os.Stdin)}
}

// NewConsoleTo 输出到 w、不能交互（ferry serve 里查看日志时用）：需要确认或密码时按非交互处理。
func NewConsoleTo(w io.Writer, verbose bool) *Console {
	return &Console{Out: w, Verbose: verbose, lastPct: -1}
}

func (c *Console) paint(code, s string) string {
	if !c.color {
		return s
	}
	return code + s + reset
}

func (c *Console) println(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.progress {
		fmt.Fprint(c.Out, "\r\033[K")
		c.progress = false
	}
	fmt.Fprintln(c.Out, s)
}

// Step 阶段标题。
func (c *Console) Step(format string, args ...any) {
	c.println("\n" + c.paint(bold+cyan, "==> "+fmt.Sprintf(format, args...)))
}

// Info 普通信息。
func (c *Console) Info(format string, args ...any) { c.println("    " + fmt.Sprintf(format, args...)) }

// Warn 提示。
func (c *Console) Warn(format string, args ...any) {
	c.println(c.paint(yellow, "  ! "+indentRest(fmt.Sprintf(format, args...))))
}

// Error 错误。
func (c *Console) Error(format string, args ...any) {
	c.println(c.paint(red, "  ✗ "+indentRest(fmt.Sprintf(format, args...))))
}

// indentRest 多行消息的后续行与第一行的文字对齐。
func indentRest(s string) string { return strings.ReplaceAll(s, "\n", "\n    ") }

// Event agent 事件。
func (c *Console) Event(_ string, ev proto.Event) {
	col := ""
	switch {
	case ev.Level == "error":
		col = red
	case ev.Level == "warn":
		col = yellow
	case ev.Code == proto.EvReady:
		col = green
	case ev.Code == proto.EvRollback:
		col = magenta
	case ev.Code == proto.EvSwitch || ev.Code == proto.EvStart:
		col = blue
	}
	app := ev.App
	if app == "" {
		app = "-"
	}
	// 用本机时间显示（服务器时区可能不同）
	now := time.Now().Format("15:04:05")
	label := padRight(proto.EvLabel(ev.Code), 8)
	line := fmt.Sprintf("    %s %s %s %s", c.paint(dim, now), label, padRight(app, 12), ev.Msg)
	if col != "" {
		line = fmt.Sprintf("    %s %s %s %s", c.paint(dim, now), c.paint(col, label), padRight(app, 12), c.paint(col, ev.Msg))
	}
	c.println(line)
}

// Progress 上传进度（终端里原地刷新；非终端每 25% 一行）。
func (c *Console) Progress(label string, done, total int64) {
	if total <= 0 {
		return
	}
	pct := int(done * 100 / total)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tty {
		if label == "" && done >= total {
			if c.progress {
				fmt.Fprint(c.Out, "\r\033[K")
				c.progress = false
			}
			return
		}
		fmt.Fprintf(c.Out, "\r\033[K    %3d%%  %s / %s  %s", pct, human(done), human(total), truncate(label, 48))
		c.progress = true
		return
	}
	if label == "" {
		c.lastPct = -1 // 一组上传结束
		return
	}
	if pct/25 != c.lastPct/25 || (done >= total && c.lastPct != 100) {
		c.lastPct = pct
		fmt.Fprintf(c.Out, "    %3d%%  %s / %s\n", pct, human(done), human(total))
	}
}

// BuildOutput 构建输出：-v 时实时显示，否则只在失败时显示最后几行。
func (c *Console) BuildOutput() io.Writer {
	if !c.Verbose {
		return nil
	}
	return &prefixWriter{c: c}
}

// Confirm 询问 y/N；不是终端时拒绝（请用 --yes）。
func (c *Console) Confirm(prompt string) bool {
	if c.in == nil || !term.IsTerminal(int(os.Stdin.Fd())) {
		c.Error("%s 需要确认：不是交互终端，请加 --yes", prompt)
		return false
	}
	c.mu.Lock()
	fmt.Fprintf(c.Out, "\n%s [y/N] ", c.paint(bold+yellow, prompt))
	c.mu.Unlock()
	line, _ := c.in.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// ConfirmName 要输入 want 才算确认（生产服务器上的部署、回滚等）；不是交互终端时返回 false。
func (c *Console) ConfirmName(prompt, want string) bool {
	if c.in == nil || !term.IsTerminal(int(os.Stdin.Fd())) {
		c.Error("%s 需要确认：不是交互终端，请加 --yes", prompt)
		return false
	}
	c.mu.Lock()
	fmt.Fprintf(c.Out, "\n%s\n输入 %s 确认：", c.paint(bold+yellow, prompt), c.paint(bold, want))
	c.mu.Unlock()
	line, _ := c.in.ReadString('\n')
	if strings.TrimSpace(line) == want {
		return true
	}
	c.Error("输入的不是 %s，没有执行", want)
	return false
}

// Secret 在终端里询问密码（不回显）；不是交互终端时返回 false。
func (c *Console) Secret(prompt string) (string, bool) {
	fd := int(os.Stdin.Fd())
	if c.in == nil || !term.IsTerminal(fd) {
		return "", false
	}
	c.mu.Lock()
	if c.progress {
		fmt.Fprint(c.Out, "\r\033[K")
		c.progress = false
	}
	fmt.Fprint(c.Out, c.paint(bold+yellow, prompt))
	c.mu.Unlock()
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(c.Out)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

// OK 成功结论。
func (c *Console) OK(format string, args ...any) {
	c.println("\n" + c.paint(bold+green, "✔ "+fmt.Sprintf(format, args...)))
}

// Fail 失败结论。
func (c *Console) Fail(format string, args ...any) {
	c.println("\n" + c.paint(bold+red, "✗ "+fmt.Sprintf(format, args...)))
}

// Paint 给外部用的着色。
func (c *Console) Paint(kind, s string) string {
	switch kind {
	case "ok":
		return c.paint(green, s)
	case "bad":
		return c.paint(red, s)
	case "warn":
		return c.paint(yellow, s)
	case "dim":
		return c.paint(dim, s)
	}
	return s
}

type prefixWriter struct {
	c   *Console
	buf []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := strings.IndexByte(string(p.buf), '\n')
		if i < 0 {
			break
		}
		p.c.println(p.c.paint(dim, "    │ "+strings.TrimRight(string(p.buf[:i]), "\r")))
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---------------------------------------------------------------- JSON

// JSON 每个事件一行 JSON 输出到 stdout，供 GUI 解析。
type JSON struct {
	mu  sync.Mutex
	enc *json.Encoder
	// Verbose 时构建输出逐行以 kind=build 输出
	Verbose bool
}

// NewJSON 输出到 w。
func NewJSON(w io.Writer) *JSON { return &JSON{enc: json.NewEncoder(w)} }

func (j *JSON) emit(kind string, fields map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fields["kind"] = kind
	fields["time"] = time.Now().Format(time.RFC3339)
	j.enc.Encode(fields)
}

// Emit 输出一行自定义 kind 的事件（如终端已打开）。
func (j *JSON) Emit(kind string, fields map[string]any) { j.emit(kind, fields) }

func (j *JSON) Step(format string, args ...any) {
	j.emit("step", map[string]any{"msg": fmt.Sprintf(format, args...)})
}
func (j *JSON) Info(format string, args ...any) {
	j.emit("info", map[string]any{"msg": fmt.Sprintf(format, args...)})
}
func (j *JSON) Warn(format string, args ...any) {
	j.emit("warn", map[string]any{"msg": fmt.Sprintf(format, args...)})
}
func (j *JSON) Error(format string, args ...any) {
	j.emit("error", map[string]any{"msg": fmt.Sprintf(format, args...)})
}
func (j *JSON) Event(target string, ev proto.Event) {
	j.emit("event", map[string]any{"target": target, "level": ev.Level, "code": ev.Code, "app": ev.App, "msg": ev.Msg})
}
func (j *JSON) Progress(label string, done, total int64) {
	j.emit("progress", map[string]any{"label": label, "done": done, "total": total})
}
func (j *JSON) BuildOutput() io.Writer {
	if !j.Verbose {
		return nil
	}
	return &jsonLineWriter{j: j}
}

type jsonLineWriter struct {
	j   *JSON
	buf []byte
}

func (w *jsonLineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.j.emit("build", map[string]any{"msg": strings.TrimRight(string(w.buf[:i]), "\r")})
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

// Confirm JSON 模式下不交互：需要确认时请带 --yes。
func (j *JSON) Confirm(prompt string) bool {
	j.emit("confirm-required", map[string]any{"msg": prompt})
	return false
}

// Secret JSON 模式下不交互：返回需要凭据的错误，由 GUI 询问后通过环境变量传入重试。
func (j *JSON) Secret(string) (string, bool) { return "", false }

// Result 最终结果。错误带结构化信息（需要密码、未知主机……）时放在 errorDetail 里。
func (j *JSON) Result(ok bool, data any, err error) {
	f := map[string]any{"ok": ok}
	if data != nil {
		f["data"] = data
	}
	if err != nil {
		f["error"] = err.Error()
		var d interface{ JSONDetail() map[string]any }
		if errors.As(err, &d) {
			f["errorDetail"] = d.JSONDetail()
		}
	}
	j.emit("result", f)
}
