package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 历史对话：列出、打开（转成界面显示的条目）、删除，按保留天数和总大小清理。

// Decided 用户对一个改动的决定（打开历史对话时显示「已同意」「没有执行」等）。
type Decided struct {
	Approval Approval `json:"approval"`
	Decision string   `json:"decision"` // approve | always | allowed | reject | skipped
}

// Summary 一个对话的概要（历史列表用）。
type Summary struct {
	ID       string    `json:"id"`
	Config   string    `json:"config"`
	Target   string    `json:"target"`
	Title    string    `json:"title"` // 第一句话
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Messages int       `json:"messages"` // 用户说了几句
	Bytes    int64     `json:"bytes"`
	Pending  bool      `json:"pending,omitempty"`
}

// List 列出对话（最近的在前）。读不了的文件跳过。
func List(dir string) ([]Summary, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !reConvID.MatchString(id) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		c, err := Load(dir, id)
		if err != nil || len(c.Messages) == 0 {
			continue
		}
		s := Summary{ID: id, Config: c.Config, Target: c.Target, Created: c.Created, Updated: c.Updated, Bytes: fi.Size(), Pending: c.Pending != nil}
		if s.Updated.IsZero() {
			s.Updated = fi.ModTime()
		}
		for _, m := range c.Messages {
			if m.Role == "user" {
				if s.Title == "" {
					s.Title = shorten(m.Text, 60)
					if s.Title == "" && len(m.Images) > 0 {
						s.Title = "（图片）"
					}
				}
				s.Messages++
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// Delete 删除一个对话。
func Delete(dir, id string) error {
	if !reConvID.MatchString(id) {
		return fmt.Errorf("对话 ID 无效：%q", id)
	}
	err := os.Remove(filepath.Join(dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// DirUsage 对话记录一共几个、占多少字节（不含改动记录）。
func DirUsage(dir string) (count int, bytes int64) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			count++
			bytes += fi.Size()
		}
	}
	return
}

// Clean 按设置清理对话记录：先删 days 天没有再用过的，再从最旧的删起，直到总大小不超过 maxBytes。
// days、maxBytes 为 0 时不按它清理；keep 是正在用的对话，不删。返回删掉几个。改动记录（audit*.jsonl）不动。
func Clean(dir string, days int, maxBytes int64, keep string) int {
	ents, _ := os.ReadDir(dir)
	type f struct {
		name string
		mod  time.Time
		size int64
	}
	var files []f
	var total int64
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{e.Name(), fi.ModTime(), fi.Size()})
		total += fi.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) }) // 最旧的在前
	removed := 0
	for _, x := range files {
		if keep != "" && x.name == keep+".json" {
			continue
		}
		old := days > 0 && time.Since(x.mod) > time.Duration(days)*24*time.Hour
		big := maxBytes > 0 && total > maxBytes
		if !old && !big {
			continue
		}
		if os.Remove(filepath.Join(dir, x.name)) == nil {
			removed++
			total -= x.size
		}
	}
	return removed
}

// CleanAll 删除全部对话记录（改动记录不动）。
func CleanAll(dir string) int {
	ents, _ := os.ReadDir(dir)
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") && os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// Item 界面上显示的一条（和 AI 助手抽屉里的条目一致）。
type Item struct {
	Kind     string    `json:"kind"` // user | text | tool
	Text     string    `json:"text,omitempty"`
	Images   []Image   `json:"images,omitempty"` // user
	Call     *ToolCall `json:"call,omitempty"`
	Result   string    `json:"result,omitempty"`
	IsError  bool      `json:"isError,omitempty"`
	Done     bool      `json:"done,omitempty"`
	Approval *Approval `json:"approval,omitempty"`
	Waiting  bool      `json:"waiting,omitempty"`
	Decision string    `json:"decision,omitempty"`
}

// Items 把对话转成界面显示的条目：用户的话、模型的回答、每个工具调用和它的结果；等确认的操作排在最后。
func (c *Conversation) Items() []Item {
	var out []Item
	idx := map[string]int{}
	for _, m := range c.Messages {
		switch m.Role {
		case "user":
			out = append(out, Item{Kind: "user", Text: m.Text, Images: m.Images})
		case "assistant":
			if strings.TrimSpace(m.Text) != "" {
				out = append(out, Item{Kind: "text", Text: m.Text})
			}
			for _, call := range m.Calls {
				call := call
				it := Item{Kind: "tool", Call: &call, Done: true}
				if d, ok := c.Decisions[call.ID]; ok {
					ap := d.Approval
					it.Approval, it.Decision = &ap, d.Decision
				}
				idx[call.ID] = len(out)
				out = append(out, it)
			}
		case "tool":
			if i, ok := idx[m.CallID]; ok {
				out[i].Result, out[i].IsError = m.Text, m.IsError
			}
		}
	}
	if pd := c.Pending; pd != nil && len(pd.Calls) > 0 {
		// 第一个是等确认的；同一轮里后面的调用还没轮到，不显示
		for k, call := range pd.Calls {
			i, ok := idx[call.ID]
			if !ok {
				continue
			}
			if k == 0 {
				ap := pd.Approval
				out[i].Approval, out[i].Waiting, out[i].Done, out[i].Decision = &ap, true, false, ""
			} else {
				out[i].Done = false
				out[i].Call = nil // 删掉
			}
		}
		var keep []Item
		for _, it := range out {
			if it.Kind == "tool" && it.Call == nil {
				continue
			}
			keep = append(keep, it)
		}
		out = keep
	}
	return out
}

// auditMax 改动记录超过这么大时换一个文件（旧的改名为 audit-1.jsonl，再旧的删掉）。
const auditMax = 10 << 20

func rotateAudit(file string) {
	if fi, err := os.Stat(file); err == nil && fi.Size() > auditMax {
		os.Rename(file, strings.TrimSuffix(file, ".jsonl")+"-1.jsonl")
	}
}

// MarshalItems 方便命令行输出。
func MarshalItems(items []Item) json.RawMessage {
	b, _ := json.Marshal(items)
	return b
}

// UserCount 用户说了几句。
func (c *Conversation) UserCount() int {
	n := 0
	for _, m := range c.Messages {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

// Rewind 退回到第 n 句用户的话之前（0 起），后面的都去掉：用户改了这句话重新发。
// 已经在服务器上执行的改动不会撤销。
func (c *Conversation) Rewind(n int) error {
	k := 0
	for i, m := range c.Messages {
		if m.Role != "user" {
			continue
		}
		if k == n {
			c.Messages = c.Messages[:i]
			c.Pending, c.Executing = nil, nil
			// 去掉的那些调用的决定也去掉（新的调用可能用同样的 ID）
			keep := map[string]bool{}
			for _, m := range c.Messages {
				for _, call := range m.Calls {
					keep[call.ID] = true
				}
			}
			for id := range c.Decisions {
				if !keep[id] {
					delete(c.Decisions, id)
				}
			}
			return nil
		}
		k++
	}
	return fmt.Errorf("对话里没有第 %d 句话", n+1)
}

// Markdown 导出成 Markdown：用户的话、回答、每个操作（命令、用户的决定、输出）。
func (c *Conversation) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# AI 助手对话：%s\n\n", c.Target)
	fmt.Fprintf(&b, "- 配置：%s\n- 开始：%s\n- 最后：%s\n", c.Config, c.Created.Local().Format("2006-01-02 15:04"), c.Updated.Local().Format("2006-01-02 15:04"))
	for model, u := range c.UsageByModel() {
		fmt.Fprintf(&b, "- 用量（%s）：输入 %d、缓存 %d、输出 %d tokens\n", model, u.Input, u.CacheRead+u.CacheWrite, u.Output)
	}
	decision := map[string]string{"approve": "已同意执行", "always": "已同意，本次对话同类不再询问", "allowed": "同类已允许，直接执行",
		"reject": "没有执行（拒绝了）", "skipped": "没有执行（接着说了别的）"}
	for _, it := range c.Items() {
		switch it.Kind {
		case "user":
			fmt.Fprintf(&b, "\n## 问\n\n%s\n", it.Text)
			if n := len(it.Images); n > 0 {
				fmt.Fprintf(&b, "\n（附了 %d 张图片）\n", n)
			}
		case "text":
			fmt.Fprintf(&b, "\n%s\n", it.Text)
		case "tool":
			input := string(it.Call.Input)
			var a struct{ Command string }
			if json.Unmarshal(it.Call.Input, &a) == nil && a.Command != "" {
				input = a.Command
			}
			fmt.Fprintf(&b, "\n**%s**\n\n```\n%s\n```\n", it.Call.Name, input)
			if it.Approval != nil {
				d := decision[it.Decision]
				if it.Waiting {
					d = "等确认"
				}
				fmt.Fprintf(&b, "\n> %s：%s\n", it.Approval.Title, d)
			}
			if it.Result != "" {
				fmt.Fprintf(&b, "\n<details><summary>输出</summary>\n\n```\n%s\n```\n\n</details>\n", strings.TrimRight(it.Result, "\n"))
			}
		}
	}
	return b.String()
}

// MakePrivate 让目录只有当前用户能读（Windows 上设权限；其他系统靠 0700）。任务记录等也用它。
func MakePrivate(dir string) { makePrivate(dir) }

// RelinkConfig 配置文件移动了：把 from 的对话改记成 to（不改「最近更新」的时间）。返回改了几个。
func RelinkConfig(dir, from, to string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !reConvID.MatchString(id) {
			continue
		}
		c, err := Load(dir, id)
		if err != nil || !samePath(c.Config, from) {
			continue
		}
		c.Config = to
		b, err := json.Marshal(c)
		if err != nil {
			continue
		}
		p := filepath.Join(dir, id+".json")
		if os.WriteFile(p+".tmp", b, 0o600) == nil && os.Rename(p+".tmp", p) == nil {
			n++
		}
	}
	return n
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || (runtime.GOOS == "windows" && strings.EqualFold(a, b))
}
