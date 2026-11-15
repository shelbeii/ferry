package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/ui"
)

// parseSinceFlag --since、--until 的值：时长（24h、7d：从现在往前这么久）、日期（2026-11-01）或日期时间。
// end 为 true（--until）时只写日期表示那天结束。
func parseSinceFlag(name, v string, end bool) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		if n, err := strconv.ParseInt(days, 10, 32); err == nil && n > 0 && n <= int64((1<<63-1)/(24*time.Hour)) {
			return time.Now().Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return time.Now().Add(-d), nil
	}
	t, err := parseLogTime(v)
	if err != nil {
		return t, fmt.Errorf("--%s 写成 24h、7d 或 2026-11-01：%s", name, v)
	}
	if end && len(v) == len("2006-01-02") {
		t = t.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}
	return t, nil
}

// historyQuery ferry history 的筛选、分页条件。
func historyQuery(c *cli) (proto.HistoryQuery, error) {
	q := proto.HistoryQuery{Actor: strings.TrimSpace(c.flags["actor"]), Text: strings.TrimSpace(c.flags["q"]), Facets: c.has("facets")}
	if app := strings.TrimSpace(c.flags["app"]); app != "" {
		q.Apps = []string{app}
	}
	list := func(v string) []string {
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	q.Action = list(c.flags["action"])
	q.Result = list(c.flags["result"])
	q.Limit = 30
	if v := c.flags["limit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return q, fmt.Errorf("--limit 要写正整数：%s", v)
		}
		q.Limit = n
	}
	if v := c.flags["offset"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return q, fmt.Errorf("--offset 要写非负整数：%s", v)
		}
		q.Offset = n
	}
	if v := c.flags["snapshot"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return q, fmt.Errorf("--snapshot 要写非负整数：%s", v)
		}
		q.Snapshot = &n
	}
	var err error
	if q.Since, err = parseSinceFlag("since", c.flags["since"], false); err != nil {
		return q, err
	}
	if q.Until, err = parseSinceFlag("until", c.flags["until"], true); err != nil {
		return q, err
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && q.Since.After(q.Until) {
		return q, fmt.Errorf("--since 不能晚于 --until")
	}
	return q, nil
}

// historyCmd ferry history <目标> [应用…]：服务器上的部署历史，按条件分页读。
// --json 时输出这一页的记录；加 --page 输出 {items, total}（还有 --facets 时带操作人、应用的列表）。
func historyCmd(c *cli, e *engine.Engine, t string, apps []string, js *ui.JSON, con *ui.Console, out io.Writer, fail func(error) int) int {
	q, err := historyQuery(c)
	if err != nil {
		return fail(err)
	}
	if !c.has("page") && !q.NeedsPage() && len(q.Apps) == 0 {
		hs, err := e.History(t, apps, q.Limit)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, hs)
		}
		printHistory(con, out, hs)
		return 0
	}
	if q.Limit > proto.HistoryMaxLimit {
		return fail(fmt.Errorf("--limit 最多 %d", proto.HistoryMaxLimit))
	}
	page, err := e.HistoryPage(t, append(apps, q.Apps...), q)
	if err != nil {
		return fail(err)
	}
	if js != nil {
		if c.has("page") || q.Facets {
			return ok0(js, page)
		}
		return ok0(js, page.Items)
	}
	end := q.Offset + len(page.Items)
	switch {
	case page.Total == 0:
		fmt.Fprintln(out, "没有符合条件的记录")
	case len(page.Items) == 0:
		fmt.Fprintf(out, "没有这一页，共 %d 条\n", page.Total)
	default:
		printHistory(con, out, page.Items)
		fmt.Fprintf(out, "\n第 %d–%d 条，共 %d 条", q.Offset+1, end, page.Total)
		if end < page.Total {
			fmt.Fprintf(out, "（--offset %d 看下一页）", end)
		}
		fmt.Fprintln(out)
	}
	return 0
}
