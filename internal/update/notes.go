package update

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ReleaseMetaFile 发布流程附带的轻量元数据文件（含发布说明、发布时间、各安装包大小），
// 在 GitHub API 触发 60 次/小时限流或只能通过 GitHub 加速访问时作为兜底来源。
const ReleaseMetaFile = "release.json"

// metaClient 拉取发布说明、元数据和 Atom 源用的客户端（允许跟随 301/302 跳转）。
var metaClient = &http.Client{Timeout: 8 * time.Second}

// notes 尽量取发布说明、发布时间、安装包大小（都取不到不算错）。
// 直连可用时先试 GitHub API（能拿到网页上后改过的说明），失败或限流时依次回退到 Release 里的 release.json 和 releases.atom；
// 若查最新版本时直连就失败、只能走 GitHub 加速，则先走加速取 release.json / releases.atom，避免白等 API 超时。
func notes(ctx context.Context, s Source, info *Info, usedMirror bool) {
	if usedMirror {
		notesFromAsset(ctx, s, info, true)
		if info.Notes == "" {
			notesFromAtom(ctx, s, info, true)
		}
		if info.Notes == "" {
			notesFromAPI(ctx, s, info)
		}
		return
	}
	notesFromAPI(ctx, s, info)
	if info.Notes == "" {
		notesFromAsset(ctx, s, info, false)
	}
	if info.Notes == "" {
		notesFromAtom(ctx, s, info, false)
	}
}

func notesFromAPI(ctx context.Context, s Source, info *Info) {
	b, ok := getMeta(ctx, s.api()+"/repos/"+Repo+"/releases/tags/v"+info.Latest, "application/vnd.github+json", 4<<20)
	if !ok {
		return
	}
	var r struct {
		Body        string `json:"body"`
		PublishedAt string `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if json.Unmarshal(b, &r) != nil {
		return
	}
	if body := strings.TrimSpace(r.Body); body != "" {
		info.Notes = body
	}
	if info.Published == "" {
		info.Published = strings.TrimSpace(r.PublishedAt)
	}
	if info.Size == 0 {
		for _, a := range r.Assets {
			if a.Name == info.Asset && a.Size > 0 {
				info.Size = a.Size
			}
		}
	}
}

type releaseMeta struct {
	Notes     string           `json:"notes"`
	Published string           `json:"published"`
	Assets    map[string]int64 `json:"assets"`
}

func notesFromAsset(ctx context.Context, s Source, info *Info, preferMirror bool) {
	b, ok := getWithMirror(ctx, s, s.download(info.Latest, ReleaseMetaFile), "application/json", 512<<10, preferMirror)
	if !ok {
		return
	}
	var m releaseMeta
	if json.Unmarshal(b, &m) != nil {
		return
	}
	if body := strings.TrimSpace(m.Notes); body != "" && info.Notes == "" {
		info.Notes = body
	}
	if pub := strings.TrimSpace(m.Published); pub != "" && info.Published == "" {
		info.Published = pub
	}
	if info.Size == 0 && info.Asset != "" && m.Assets[info.Asset] > 0 {
		info.Size = m.Assets[info.Asset]
	}
}

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID      string `xml:"id"`
	Updated string `xml:"updated"`
	Links   []struct {
		Href string `xml:"href,attr"`
	} `xml:"link"`
	Content struct {
		Type string `xml:"type,attr"`
		Body string `xml:",chardata"`
	} `xml:"content"`
}

func notesFromAtom(ctx context.Context, s Source, info *Info, preferMirror bool) {
	b, ok := getWithMirror(ctx, s, s.base()+"/"+Repo+"/releases.atom", "application/atom+xml, application/xml, text/xml", 2<<20, preferMirror)
	if !ok {
		return
	}
	var feed atomFeed
	if xml.Unmarshal(b, &feed) != nil {
		return
	}
	tag := "v" + info.Latest
	for _, e := range feed.Entries {
		if !atomMatchesTag(e, tag) {
			continue
		}
		if pub := strings.TrimSpace(e.Updated); pub != "" && info.Published == "" {
			info.Published = pub
		}
		raw := strings.TrimSpace(e.Content.Body)
		if raw != "" && info.Notes == "" {
			if strings.EqualFold(e.Content.Type, "html") || strings.Contains(raw, "<") {
				info.Notes = htmlToMarkdown(raw)
			} else {
				info.Notes = raw
			}
		}
		return
	}
}

func atomMatchesTag(e atomEntry, tag string) bool {
	if strings.HasSuffix(strings.TrimSpace(e.ID), "/"+tag) {
		return true
	}
	for _, l := range e.Links {
		href := strings.TrimRight(strings.TrimSpace(l.Href), "/")
		if i := strings.IndexAny(href, "?#"); i >= 0 {
			href = strings.TrimRight(href[:i], "/")
		}
		if strings.HasSuffix(href, "/tag/"+tag) {
			return true
		}
	}
	return false
}

func getWithMirror(ctx context.Context, s Source, u, accept string, limit int64, preferMirror bool) ([]byte, bool) {
	urls := []string{u, s.mirrored(u)}
	if preferMirror {
		urls = []string{s.mirrored(u), u}
	}
	for _, link := range urls {
		if link == "" {
			continue
		}
		if b, ok := getMeta(ctx, link, accept, limit); ok {
			return b, true
		}
	}
	return nil, false
}

func getMeta(ctx context.Context, u, accept string, limit int64) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", "ferry")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := metaClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, false
	}
	return b, true
}

// htmlToMarkdown 把 GitHub releases.atom 里的 HTML 发布说明转成干净的 Markdown。
func htmlToMarkdown(src string) string {
	dec := xml.NewDecoder(strings.NewReader(src))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	type listState struct {
		ordered bool
		index   int
	}
	type frame struct {
		kind string // "a", "code", "cell", "quote"
		href string
		buf  strings.Builder
	}
	type tableState struct {
		rows   [][]string
		curRow []string
	}

	var root strings.Builder
	var frames []*frame
	var lists []listState
	var tables []*tableState
	var skipDepth int
	var inPre bool

	cur := func() *strings.Builder {
		if len(frames) > 0 {
			return &frames[len(frames)-1].buf
		}
		return &root
	}
	ensureNewlines := func(n int) {
		b := cur()
		s := b.String()
		if len(s) == 0 {
			return
		}
		have := 0
		for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
			have++
		}
		for have < n {
			b.WriteByte('\n')
			have++
		}
	}
	popFrame := func(kind string) *frame {
		for i := len(frames) - 1; i >= 0; i-- {
			if frames[i].kind == kind {
				f := frames[i]
				frames = frames[:i]
				return f
			}
		}
		return nil
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if skipDepth > 0 {
				skipDepth++
				continue
			}
			switch name {
			case "svg", "style", "script", "noscript":
				skipDepth = 1
			case "h1", "h2", "h3", "h4", "h5", "h6":
				level := int(name[1] - '0')
				ensureNewlines(2)
				cur().WriteString(strings.Repeat("#", level) + " ")
			case "p":
				ensureNewlines(2)
			case "br":
				cur().WriteByte('\n')
			case "hr":
				ensureNewlines(2)
				cur().WriteString("---")
				ensureNewlines(2)
			case "ul":
				lists = append(lists, listState{ordered: false})
				if len(lists) == 1 {
					ensureNewlines(2)
				}
			case "ol":
				lists = append(lists, listState{ordered: true})
				if len(lists) == 1 {
					ensureNewlines(2)
				}
			case "li":
				ensureNewlines(1)
				indent := ""
				prefix := "- "
				if len(lists) > 0 {
					indent = strings.Repeat("  ", len(lists)-1)
					if lists[len(lists)-1].ordered {
						lists[len(lists)-1].index++
						prefix = strconv.Itoa(lists[len(lists)-1].index) + ". "
					}
				}
				cur().WriteString(indent + prefix)
			case "pre":
				inPre = true
				ensureNewlines(2)
				cur().WriteString("```\n")
			case "code":
				if !inPre {
					frames = append(frames, &frame{kind: "code"})
				}
			case "strong", "b":
				cur().WriteString("**")
			case "em", "i":
				cur().WriteString("*")
			case "a":
				href := ""
				for _, a := range t.Attr {
					if strings.EqualFold(a.Name.Local, "href") {
						href = strings.TrimSpace(a.Value)
						break
					}
				}
				frames = append(frames, &frame{kind: "a", href: href})
			case "blockquote":
				frames = append(frames, &frame{kind: "quote"})
			case "table":
				tables = append(tables, &tableState{})
			case "tr":
				if len(tables) > 0 {
					tables[len(tables)-1].curRow = nil
				}
			case "th", "td":
				if len(tables) > 0 {
					frames = append(frames, &frame{kind: "cell"})
				}
			}

		case xml.EndElement:
			name := strings.ToLower(t.Name.Local)
			if skipDepth > 0 {
				skipDepth--
				continue
			}
			switch name {
			case "h1", "h2", "h3", "h4", "h5", "h6", "p":
				ensureNewlines(2)
			case "ul", "ol":
				if len(lists) > 0 {
					lists = lists[:len(lists)-1]
				}
				if len(lists) == 0 {
					ensureNewlines(2)
				}
			case "pre":
				if inPre {
					ensureNewlines(1)
					cur().WriteString("```")
					ensureNewlines(2)
					inPre = false
				}
			case "code":
				if !inPre {
					if f := popFrame("code"); f != nil {
						code := strings.TrimSpace(f.buf.String())
						if code != "" {
							if strings.Contains(code, "`") {
								cur().WriteString("`` " + code + " ``")
							} else {
								cur().WriteString("`" + code + "`")
							}
						}
					}
				}
			case "strong", "b":
				cur().WriteString("**")
			case "em", "i":
				cur().WriteString("*")
			case "a":
				if f := popFrame("a"); f != nil {
					text := strings.TrimSpace(f.buf.String())
					if text != "" {
						lower := strings.ToLower(f.href)
						if (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) && text != f.href {
							cur().WriteString("[" + text + "](" + f.href + ")")
						} else {
							cur().WriteString(text)
						}
					}
				}
			case "blockquote":
				if f := popFrame("quote"); f != nil {
					text := strings.TrimSpace(f.buf.String())
					if text != "" {
						ensureNewlines(2)
						for i, line := range strings.Split(text, "\n") {
							if i > 0 {
								cur().WriteByte('\n')
							}
							cur().WriteString("> " + line)
						}
						ensureNewlines(2)
					}
				}
			case "th", "td":
				if f := popFrame("cell"); f != nil && len(tables) > 0 {
					cell := collapseSpace(f.buf.String())
					cell = strings.ReplaceAll(cell, "|", `\|`)
					tb := tables[len(tables)-1]
					tb.curRow = append(tb.curRow, cell)
				}
			case "tr":
				if len(tables) > 0 {
					tb := tables[len(tables)-1]
					if len(tb.curRow) > 0 {
						tb.rows = append(tb.rows, tb.curRow)
						tb.curRow = nil
					}
				}
			case "table":
				if len(tables) > 0 {
					tb := tables[len(tables)-1]
					tables = tables[:len(tables)-1]
					if len(tb.rows) > 0 {
						ensureNewlines(2)
						writeMarkdownTable(cur(), tb.rows)
						ensureNewlines(2)
					}
				}
			}

		case xml.CharData:
			if skipDepth > 0 {
				continue
			}
			s := string(t)
			if inPre {
				cur().WriteString(s)
				continue
			}
			// 表格外框（<table><tr> 之间）的缩进空白不写入正文
			if len(tables) > 0 && (len(frames) == 0 || frames[len(frames)-1].kind != "cell") {
				if strings.TrimSpace(s) == "" {
					continue
				}
			}
			text := collapseInlineSpace(s)
			if text == "" {
				continue
			}
			b := cur()
			if text == " " {
				prev := b.String()
				if prev == "" || strings.HasSuffix(prev, "\n") || strings.HasSuffix(prev, " ") || strings.HasSuffix(prev, "\t") {
					continue
				}
			}
			if strings.HasPrefix(text, " ") {
				prev := b.String()
				if prev == "" || strings.HasSuffix(prev, "\n") || strings.HasSuffix(prev, " ") {
					text = strings.TrimLeft(text, " ")
				}
			}
			b.WriteString(text)
		}
	}

	return cleanMarkdown(root.String())
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func collapseInlineSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

func writeMarkdownTable(b *strings.Builder, rows [][]string) {
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return
	}
	writeRow := func(r []string) {
		b.WriteString("|")
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteByte('\n')
	}
	writeRow(rows[0])
	b.WriteString("|")
	for i := 0; i < cols; i++ {
		b.WriteString(" --- |")
	}
	b.WriteByte('\n')
	for _, r := range rows[1:] {
		writeRow(r)
	}
}

func cleanMarkdown(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		trimmed := strings.TrimRight(l, " \t")
		if trimmed == "" {
			blank++
			if blank <= 1 && len(out) > 0 {
				out = append(out, "")
			}
			continue
		}
		blank = 0
		out = append(out, trimmed)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
