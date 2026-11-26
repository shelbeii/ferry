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
	"github.com/captain/ferry/internal/flock"
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

func logLockFile() string { return filepath.Join(filepath.Dir(LogFile()), "calls.lock") }

// withLogLock 进程内互斥 + 跨进程文件锁，避免多个 ferry mcp 与桌面端并发追加/清理 calls.jsonl 丢记录或 rename 冲突。
func withLogLock(fn func()) {
	logMu.Lock()
	defer logMu.Unlock()
	unlock, err := flock.Lock(logLockFile(), 3*time.Second, nil)
	if err == nil {
		defer unlock()
	}
	fn()
}

func logLimitsLocked() (days int, maxBytes int64) {
	st, _ := loadSettingsCached()
	return st.MCP.LogKeepDays, int64(st.MCP.LogMaxMB) << 20
}

// oldLogFile 0.12.54 及以前超过 10 MB 时换下来的记录；清理时并回 calls.jsonl。
func oldLogFile() string { return filepath.Join(filepath.Dir(LogFile()), "calls.1.jsonl") }

func writeLog(e callEntry) {
	withLogLock(func() {
		p := LogFile()
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
		f.Close()
		// 超过最多占用，或这个进程一小时没按天数清理过：按设置（设置 → 存储）清理
		days, maxBytes := logLimitsLocked()
		fi, err := os.Stat(p)
		if (err == nil && maxBytes > 0 && fi.Size() > maxBytes) || time.Since(lastClean) > time.Hour {
			cleanLocked(days, maxBytes)
		}
	})
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

// ClientConfirmTool 编程软件里确认那条记录在筛选里的「工具」。
const ClientConfirmTool = "（编程软件里确认）"

// changeTools 请求改动服务器的工具。
var changeTools = map[string]bool{
	"deploy": true, "app_control": true, "rollback": true, "software_control": true,
	"port_control": true, "firewall_enable": true, "ssh_disconnect": true,
	"backup_run": true, "backup_fetch": true, "run_job": true,
	"run_change_command": true, "write_file": true, "files_upload": true,
	"software_install": true, "software_upgrade": true, "software_restore": true,
	"software_backup": true, "save_software_template": true, "ask_secret": true,
	"db_execute": true, "redis_execute": true, "config_edit": true,
	"create_project": true, "confirm_change": true,
}

func toolKey(tool string) string {
	if strings.HasSuffix(tool, ResultTool) {
		return ResultTool
	}
	if strings.HasSuffix(tool, ClientConfirmTool) {
		return ClientConfirmTool
	}
	return tool
}

// CallFilter 筛选调用记录；零值 = 全部。
type CallFilter struct {
	Tools   []string  // 只看这些工具（ResultTool = 改动的结果）
	Client  string    // 编程软件（记录里的名称和版本）
	Channel string    // 确认渠道：desktop | mcp-app | elicitation | two-step
	Result  string    // ok = 只看成功，fail = 只看失败
	Changes bool      // 只看改动服务器的（请求和结果）
	Since   time.Time // 不早于
	Until   time.Time // 早于
	Keyword string    // 在参数、错误、编程软件、同意与否里找（不区分大小写）
}

func entryChannel(e *callEntry) string {
	if len(e.Args) > 0 {
		var a struct {
			Channel string `json:"channel"`
		}
		if json.Unmarshal(e.Args, &a) == nil && a.Channel != "" {
			return a.Channel
		}
	}
	// 兼容旧记录：带 Decision 的结果条目若未写 channel 字段，属于桌面端弹窗确认
	if e.Decision != "" && strings.HasSuffix(e.Tool, ResultTool) {
		return "desktop"
	}
	return ""
}

func (f CallFilter) match(e *callEntry) bool {
	key := toolKey(e.Tool)
	switch {
	case len(f.Tools) > 0 && !slices.Contains(f.Tools, key),
		f.Client != "" && e.Client != f.Client,
		f.Channel != "" && entryChannel(e) != f.Channel,
		f.Result == "ok" && !e.OK,
		f.Result == "fail" && e.OK,
		f.Changes && !changeTools[key] && e.Op == "",
		!f.Since.IsZero() && e.Time.Before(f.Since),
		!f.Until.IsZero() && !e.Time.Before(f.Until):
		return false
	}
	if kw := strings.ToLower(strings.TrimSpace(f.Keyword)); kw != "" {
		hay := strings.ToLower(string(e.Args) + "\n" + e.Error + "\n" + e.Client + "\n" + e.Decision + "\n" + e.Tool + "\n" + e.Op)
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
	var all [][]byte
	withLogLock(func() {
		all = readLog()
	})
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
	withLogLock(func() {
		for _, p := range []string{oldLogFile(), LogFile()} {
			if fi, err := os.Stat(p); err == nil {
				size += fi.Size()
			}
		}
		count = len(readLog())
	})
	return count, size
}

// CleanLog 删掉超过 days 天的记录；还超过 maxBytes 时从最旧的删起，删到 maxBytes 的九成（免得之后每记一条都要重写）。
// 返回删掉的条数。
func CleanLog(days int, maxBytes int64) int {
	var n int
	withLogLock(func() {
		n = cleanLocked(days, maxBytes)
	})
	return n
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
	var n int
	withLogLock(func() {
		n = len(readLog())
		os.Remove(LogFile())
		os.Remove(oldLogFile())
	})
	return n
}
