package mcpserver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/settings"
)

// callEntry 一次工具调用的记录（mcp/calls.jsonl）。
type callEntry struct {
	Time     time.Time       `json:"time"`
	Client   string          `json:"client,omitempty"` // 编程软件的名称和版本（它自己报的）
	Tool     string          `json:"tool"`
	Op       string          `json:"op,omitempty"`       // 改动的 id（结果那条和请求那条对得上）
	Decision string          `json:"decision,omitempty"` // 同意 / 拒绝 / 按授权自动同意
	Args     json.RawMessage `json:"args,omitempty"`
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Ms       int64           `json:"ms"`
}

var (
	logMu     sync.Mutex
	lastClean time.Time // 这个进程上次按天数清理的时间
)

// LogFile 调用记录的位置。
func LogFile() string { return filepath.Join(appdir.SubDir("mcp"), "calls.jsonl") }

// oldLogFile 0.12.54 及以前超过 10 MB 时换下来的记录；清理时并回 calls.jsonl。
func oldLogFile() string { return filepath.Join(filepath.Dir(LogFile()), "calls.1.jsonl") }

func writeLog(e callEntry) {
	logMu.Lock()
	defer logMu.Unlock()
	p := LogFile()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	b, _ := json.Marshal(e)
	f.Write(append(b, '\n'))
	f.Close()
	// 超过最多占用，或这个进程一小时没按天数清理过：按设置（设置 → 存储）清理
	st, _ := settings.Load()
	fi, err := os.Stat(p)
	if (err == nil && fi.Size() > int64(st.MCP.LogMaxMB)<<20) || time.Since(lastClean) > time.Hour {
		cleanLocked(st.MCP.LogKeepDays, int64(st.MCP.LogMaxMB)<<20)
	}
}

// readLog 全部记录，旧的在前。
func readLog() [][]byte {
	var out [][]byte
	for _, p := range []string{oldLogFile(), LogFile()} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, l := range bytes.Split(b, []byte("\n")) {
			if l = bytes.TrimSpace(l); len(l) > 0 && json.Valid(l) {
				out = append(out, l)
			}
		}
	}
	return out
}

// ResultTool 改动结果那条记录在筛选里的「工具」（记录里是「deploy（结果）」这样的动作名）。
const ResultTool = "（结果）"

// changeTools 请求改动服务器的工具。
var changeTools = map[string]bool{"deploy": true, "app_control": true, "rollback": true, "software_control": true, "create_project": true}

func toolKey(tool string) string {
	if strings.HasSuffix(tool, ResultTool) {
		return ResultTool
	}
	return tool
}

// CallFilter 筛选调用记录；零值 = 全部。
type CallFilter struct {
	Tools   []string  // 只看这些工具（ResultTool = 改动的结果）
	Client  string    // 编程软件（记录里的名称和版本）
	Result  string    // ok = 只看成功，fail = 只看失败
	Changes bool      // 只看改动服务器的（请求和结果）
	Since   time.Time // 不早于
	Until   time.Time // 早于
	Keyword string    // 在参数、错误、编程软件、同意与否里找（不区分大小写）
}

func (f CallFilter) match(e *callEntry) bool {
	key := toolKey(e.Tool)
	switch {
	case len(f.Tools) > 0 && !slices.Contains(f.Tools, key),
		f.Client != "" && e.Client != f.Client,
		f.Result == "ok" && !e.OK,
		f.Result == "fail" && e.OK,
		f.Changes && !changeTools[key] && e.Op == "",
		!f.Since.IsZero() && e.Time.Before(f.Since),
		!f.Until.IsZero() && !e.Time.Before(f.Until):
		return false
	}
	if kw := strings.ToLower(strings.TrimSpace(f.Keyword)); kw != "" {
		hay := strings.ToLower(string(e.Args) + "\n" + e.Error + "\n" + e.Client + "\n" + e.Decision + "\n" + e.Tool)
		return strings.Contains(hay, kw)
	}
	return true
}

// CallPage 一页调用记录。
type CallPage struct {
	Items   []json.RawMessage `json:"items"`   // 新的在前
	Total   int               `json:"total"`   // 符合筛选的条数
	Clients []string          `json:"clients"` // 全部记录里出现过的编程软件（筛选下拉用）
	Tools   []string          `json:"tools"`   // 全部记录里出现过的工具
}

// Calls 符合筛选的调用记录，新的在前：跳过 offset 条后最多 limit 条。
func Calls(f CallFilter, offset, limit int) CallPage {
	logMu.Lock()
	all := readLog()
	logMu.Unlock()
	page := CallPage{Items: []json.RawMessage{}, Clients: []string{}, Tools: []string{}}
	clients, tools := map[string]bool{}, map[string]bool{}
	for i := len(all) - 1; i >= 0; i-- {
		var e callEntry
		if json.Unmarshal(all[i], &e) != nil {
			continue
		}
		if e.Client != "" && !clients[e.Client] {
			clients[e.Client] = true
			page.Clients = append(page.Clients, e.Client)
		}
		if k := toolKey(e.Tool); !tools[k] {
			tools[k] = true
			page.Tools = append(page.Tools, k)
		}
		if !f.match(&e) {
			continue
		}
		if page.Total >= max(offset, 0) && len(page.Items) < limit {
			page.Items = append(page.Items, json.RawMessage(all[i]))
		}
		page.Total++
	}
	sort.Strings(page.Clients)
	sort.Strings(page.Tools)
	return page
}

// LogUsage 记录条数和占用的字节数。
func LogUsage() (count int, size int64) {
	for _, p := range []string{oldLogFile(), LogFile()} {
		if fi, err := os.Stat(p); err == nil {
			size += fi.Size()
		}
	}
	logMu.Lock()
	defer logMu.Unlock()
	return len(readLog()), size
}

// CleanLog 删掉超过 days 天的记录；还超过 maxBytes 时从最旧的删起，删到 maxBytes 的九成（免得之后每记一条都要重写）。
// 返回删掉的条数。
func CleanLog(days int, maxBytes int64) int {
	logMu.Lock()
	defer logMu.Unlock()
	return cleanLocked(days, maxBytes)
}

func cleanLocked(days int, maxBytes int64) int {
	lastClean = time.Now()
	all := readLog()
	cutoff := time.Now().AddDate(0, 0, -days)
	keep := all[:0:0]
	var size int64
	for _, l := range all {
		var e struct {
			Time time.Time `json:"time"`
		}
		if json.Unmarshal(l, &e) == nil && days > 0 && e.Time.Before(cutoff) {
			continue
		}
		keep = append(keep, l)
		size += int64(len(l)) + 1
	}
	if maxBytes > 0 && size > maxBytes {
		for len(keep) > 0 && size > maxBytes*9/10 {
			size -= int64(len(keep[0])) + 1
			keep = keep[1:]
		}
	}
	_, oldErr := os.Stat(oldLogFile())
	if len(keep) == len(all) && oldErr != nil {
		return 0
	}
	var buf bytes.Buffer
	for _, l := range keep {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	if appdir.WriteFileAtomic(LogFile(), buf.Bytes(), 0o600) != nil {
		return 0
	}
	os.Remove(oldLogFile())
	return len(all) - len(keep)
}

// CleanAllLog 删除全部调用记录，返回删掉的条数。
func CleanAllLog() int {
	logMu.Lock()
	defer logMu.Unlock()
	n := len(readLog())
	os.Remove(LogFile())
	os.Remove(oldLogFile())
	return n
}
