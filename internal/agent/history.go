package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 部署历史（<root>/.ferry/history.jsonl）一行一条，按时间先后追加，从不改写。
// 从文件末尾分块读取，只保留当前页。计数时检查匹配所需的字段，避免逐条构建完整记录。

func (a *Agent) history() int {
	path := filepath.Join(a.meta(), "history.jsonl")
	var apps []string
	for _, s := range a.req.Apps {
		apps = append(apps, s.Name)
	}
	if q := a.req.HistoryQuery; q != nil {
		page, err := readHistory(path, append(apps, q.Apps...), *q, true)
		if err != nil {
			return a.result(proto.CodeFailed, "读部署历史失败："+err.Error(), nil)
		}
		return a.result(proto.CodeOK, "", page)
	}
	limit := a.req.Limit
	if limit <= 0 {
		limit = 50
	}
	page, err := readHistory(path, apps, proto.HistoryQuery{Limit: limit}, false)
	if err != nil {
		return a.result(proto.CodeFailed, "读部署历史失败："+err.Error(), nil)
	}
	if len(page.Items) == 0 {
		return a.result(proto.CodeOK, "", []proto.HistoryEntry(nil))
	}
	return a.result(proto.CodeOK, "", page.Items)
}

// historyFacetLines 下拉框里的操作人、应用从最近这么多条里找。
const historyFacetLines = 2000

// readHistory 按条件读一页（新的在前）。total 为 false 时这一页够了就停，不数总条数。
func readHistory(path string, apps []string, q proto.HistoryQuery, total bool) (proto.HistoryPage, error) {
	page := proto.HistoryPage{Items: []proto.HistoryEntry{}}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if total {
		q.Limit = min(q.Limit, proto.HistoryMaxLimit)
	}
	q.Offset = max(q.Offset, 0)
	if q.Snapshot != nil && *q.Snapshot < 0 {
		return page, fmt.Errorf("部署历史已变动，请重新读取第一页")
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		if q.Snapshot != nil && *q.Snapshot > 0 {
			return page, fmt.Errorf("部署历史已变动，请重新读取第一页")
		}
		return page, nil
	}
	if err != nil {
		return page, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return page, err
	}
	page.Snapshot = st.Size()
	if q.Snapshot != nil {
		if *q.Snapshot > st.Size() {
			return page, fmt.Errorf("部署历史已变动，请重新读取第一页")
		}
		page.Snapshot = *q.Snapshot
	}
	m := newHistoryMatcher(apps, q)
	n := 0
	err = eachLineBack(f, page.Snapshot, func(line []byte) bool {
		inPage := n >= q.Offset && n-q.Offset < q.Limit
		if !bytes.HasPrefix(line, historyTimeKey) || bytes.Contains(line, []byte(`": `)) ||
			bytes.Contains(line, []byte(`" :`)) || bytes.ContainsAny(line, "\t\r") {
			var e proto.HistoryEntry
			if json.Unmarshal(line, &e) != nil || !m.match(e) {
				return true
			}
			if inPage {
				page.Items = append(page.Items, e)
			}
			n++
			return total || n-q.Offset < q.Limit
		}
		if m.timed {
			t, ok := lineTime(line)
			if !ok {
				return true
			}
			if !q.Until.IsZero() && t.After(q.Until) {
				return true
			}
			if !q.Since.IsZero() && t.Before(q.Since) {
				return true
			}
		}
		if !m.quick(line) {
			return true
		}
		if inPage {
			var e proto.HistoryEntry
			if json.Unmarshal(line, &e) != nil || !m.match(e) {
				return true
			}
			page.Items = append(page.Items, e)
		} else if (m.decode && !m.rawMatch(line)) || !json.Valid(line) {
			return true
		}
		n++
		return total || n-q.Offset < q.Limit
	})
	if err != nil {
		return page, err
	}
	page.Total = n
	if q.Facets {
		page.Actors, page.Apps, err = historyFacets(f, page.Snapshot)
	}
	return page, err
}

// historyFacets 最近的记录里有哪些操作人、应用（按名字排好）。
func historyFacets(r io.ReaderAt, size int64) (actors, apps []string, err error) {
	seenActor, seenApp := map[string]bool{}, map[string]bool{}
	n := 0
	err = eachLineBack(r, size, func(line []byte) bool {
		var e struct{ Actor, App string }
		if json.Unmarshal(line, &e) == nil {
			if e.Actor != "" && !seenActor[e.Actor] {
				seenActor[e.Actor] = true
				actors = append(actors, e.Actor)
			}
			if e.App != "" && !seenApp[e.App] {
				seenApp[e.App] = true
				apps = append(apps, e.App)
			}
		}
		n++
		return n < historyFacetLines
	})
	slices.Sort(actors)
	slices.Sort(apps)
	return actors, apps, err
}

// historyMatcher 条件。quick 只看原样的文字：应用、操作、结果是整个值对上（"app":"api" 这样连引号一起找，
// 说明里的引号写进文件时是 \"，不会误中）；含这几个字的条件先在整行里粗找，找到了才解析整行细看（decode）。
type historyMatcher struct {
	q      proto.HistoryQuery
	timed  bool
	any    [][][]byte // 每组里有一个对上就行（应用、操作、结果各一组）
	rough  [][]byte   // 整行里要有的字（小写）；含 JSON 要转义的字符时不粗找
	actor  []byte
	text   []byte
	decode bool // 数总条数时也要解析整行（有「含这几个字」的条件）
	appSet map[string]bool
	actSet map[string]bool
	resSet map[string]bool
}

func newHistoryMatcher(apps []string, q proto.HistoryQuery) *historyMatcher {
	m := &historyMatcher{q: q, timed: !q.Since.IsZero() || !q.Until.IsZero()}
	group := func(field string, vals []string) map[string]bool {
		if len(vals) == 0 {
			return nil
		}
		set := map[string]bool{}
		var g [][]byte
		for _, v := range vals {
			if v == "" || set[v] {
				continue
			}
			set[v] = true
			b, _ := json.Marshal(v)
			g = append(g, append([]byte(`"`+field+`":`), b...))
		}
		if len(g) == 0 {
			return nil
		}
		m.any = append(m.any, g)
		return set
	}
	m.appSet = group("app", apps)
	m.actSet = group("action", q.Action)
	m.resSet = group("result", q.Result)
	for _, s := range []string{q.Actor, q.Text} {
		if s == "" {
			continue
		}
		m.decode = true
		if b := []byte(s); !needsJSONEscape(b) {
			m.rough = append(m.rough, lowerASCII(b))
		}
	}
	m.actor = lowerASCII([]byte(q.Actor))
	m.text = lowerASCII([]byte(q.Text))
	return m
}

func (m *historyMatcher) quick(line []byte) bool {
	for _, g := range m.any {
		ok := false
		for _, needle := range g {
			if bytes.Contains(line, needle) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, w := range m.rough {
		if !containsFold(line, w) {
			return false
		}
	}
	return true
}

func (m *historyMatcher) match(e proto.HistoryEntry) bool {
	if m.appSet != nil && !m.appSet[e.App] || m.actSet != nil && !m.actSet[e.Action] || m.resSet != nil && !m.resSet[e.Result] {
		return false
	}
	if !m.q.Since.IsZero() && e.Time.Before(m.q.Since) || !m.q.Until.IsZero() && e.Time.After(m.q.Until) {
		return false
	}
	if len(m.actor) > 0 && !containsFold([]byte(e.Actor), m.actor) {
		return false
	}
	if len(m.text) > 0 {
		found := false
		for _, s := range []string{e.App, e.Release, e.Commit, e.Actor, e.Msg} {
			if containsFold([]byte(s), m.text) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// rawMatch 不解析整行，只取出要看的几个字段比「含这几个字」的条件（数总条数时用，和 match 的结果一样）。
func (m *historyMatcher) rawMatch(line []byte) bool {
	if len(m.actor) > 0 && !containsFold(rawString(line, keyActor), m.actor) {
		return false
	}
	if len(m.text) > 0 {
		for _, k := range textKeys {
			if containsFold(rawString(line, k), m.text) {
				return true
			}
		}
		return false
	}
	return true
}

var (
	keyActor = []byte(`"actor":"`)
	textKeys = [][]byte{[]byte(`"app":"`), []byte(`"release":"`), []byte(`"commit":"`), keyActor, []byte(`"msg":"`)}
)

// rawString 行里 "key":"…" 这个字段的值。别的字段的值里的引号都写成了 \"，所以 "key":" 只会是这个字段本身；
// 值里没有反斜杠时就是原样的字，不用分配内存，有的（转义过的）才解出来。没有这个字段时为 nil。
func rawString(line, key []byte) []byte {
	i := bytes.Index(line, key)
	if i < 0 {
		return nil
	}
	start := i + len(key)
	v := line[start:]
	for j := 0; j < len(v); j++ {
		switch v[j] {
		case '\\':
			j++
		case '"':
			raw := v[:j]
			if bytes.IndexByte(raw, '\\') < 0 {
				return raw
			}
			var str string
			if json.Unmarshal(line[start-1:start+j+1], &str) != nil {
				return raw
			}
			return []byte(str)
		}
	}
	return nil
}

var historyTimeKey = []byte(`{"time":"`)

// lineTime 从已经确认以 historyTimeKey 开头的记录里读取时间。
func lineTime(line []byte) (time.Time, bool) {
	rest := line[len(historyTimeKey):]
	i := bytes.IndexByte(rest, '"')
	if i < 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(rest[:i]))
	return t, err == nil
}

// needsJSONEscape 写进 JSON 时会变样的字符（引号、反斜杠、<>&、控制字符、U+2028/2029）：含这些的不能在原样的行里找。
func needsJSONEscape(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			return true
		}
	}
	return bytes.Contains(b, []byte(" ")) || bytes.Contains(b, []byte(" "))
}

func lowerASCII(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// containsFold s 里有没有 sub（sub 已是小写；英文字母不分大小写，其他字原样比）。不分配内存。
func containsFold(s, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	// 找一个不是英文字母的字（汉字、数字、符号）当锚点：用 bytes.IndexByte（很快）跳到它，再比前后
	anchor := -1
	for k, c := range sub {
		if c < 'a' || c > 'z' {
			anchor = k
			break
		}
	}
	if anchor < 0 { // 全是英文字母：小写、大写的首字母各找一遍，取近的
		lo, up := sub[0], sub[0]-('a'-'A')
		for i := 0; i+len(sub) <= len(s); i++ {
			if c := s[i]; (c == lo || c == up) && equalFold(s[i:i+len(sub)], sub) {
				return true
			}
		}
		return false
	}
	for i := anchor; i+len(sub)-anchor <= len(s); {
		p := indexFrom(s, i, sub[anchor])
		if p < 0 || p+len(sub)-anchor > len(s) {
			return false
		}
		if equalFold(s[p-anchor:p-anchor+len(sub)], sub) {
			return true
		}
		i = p + 1
	}
	return false
}

func indexFrom(s []byte, i int, c byte) int {
	if p := bytes.IndexByte(s[i:], c); p >= 0 {
		return i + p
	}
	return -1
}

// equalFold 一样长的 s、sub（sub 是小写）是否只差英文字母的大小写。
func equalFold(s, sub []byte) bool {
	for j, c := range s {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != sub[j] {
			return false
		}
	}
	return true
}

// backChunk eachLineBack 一次往前读多少（测试里改小，测跨块的行）。
var backChunk = 128 << 10

// backMaxLine 比这还长的行跳过（坏掉的文件不至于把内存吃光）。
const backMaxLine = 4 << 20

// eachLineBack 从末尾往前一行一行地交给 fn（不含换行，新的在前），fn 返回 false 时停下。
// fn 不能留着 line：下一块会复用。
func eachLineBack(r io.ReaderAt, size int64, fn func(line []byte) bool) error {
	buf := make([]byte, 0, backChunk)
	var rest []byte // 右边读过、还没找到行首的那一截
	long := false   // 正在跳过一行太长的：下一个换行右边那截也是它的
	pos := size
	for pos > 0 {
		n := int64(backChunk)
		if pos < n {
			n = pos
		}
		pos -= n
		need := int(n) + len(rest)
		if cap(buf) < need {
			buf = make([]byte, 0, need*2)
		}
		data := buf[:need]
		copy(data[n:], rest) // rest 可能就在 buf 开头：copy 处理重叠
		if _, err := r.ReadAt(data[:n], pos); err != nil {
			return err
		}
		end := len(data)
		for i := end - 1; i >= 0; i-- {
			if data[i] != '\n' {
				continue
			}
			line := bytes.TrimSuffix(data[i+1:end], []byte{'\r'})
			end = i
			if long {
				long = false
				continue
			}
			if len(line) > 0 && len(line) <= backMaxLine && !fn(line) {
				return nil
			}
		}
		rest = data[:end]
		if len(rest) > backMaxLine {
			rest, long = rest[:0], true
		}
		buf = data
	}
	if line := bytes.TrimSuffix(rest, []byte{'\r'}); len(line) > 0 && !long {
		fn(line)
	}
	return nil
}
