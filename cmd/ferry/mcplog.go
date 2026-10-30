package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/mcpserver"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/ui"
)

// mcpLogCmd ferry mcp-log [list] [--offset N] [--limit N] [筛选] | clean [--all]：编程软件经 MCP 的调用记录，新的在前。
func mcpLogCmd(c *cli, cio *cliIO, js *ui.JSON, st settings.Settings, fail func(error) int) int {
	action := "list"
	if len(c.pos) > 0 {
		action = c.pos[0]
	}
	usage := func() map[string]any {
		n, b := mcpserver.LogUsage()
		return map[string]any{"count": n, "bytes": b, "keepDays": st.MCP.LogKeepDays, "maxMB": st.MCP.LogMaxMB}
	}
	var res map[string]any
	switch action {
	case "list":
		offset, limit := 0, 200
		if v, err := strconv.Atoi(c.flags["offset"]); err == nil {
			offset = max(v, 0)
		}
		if v, err := strconv.Atoi(c.flags["limit"]); err == nil {
			limit = max(v, 1)
		}
		f := mcpserver.CallFilter{Client: c.flags["client"], Result: c.flags["result"], Changes: c.has("changes"), Keyword: c.flags["q"]}
		if v := c.flags["tool"]; v != "" {
			f.Tools = strings.Split(v, ",")
		}
		var err error
		if f.Since, err = parseLogTime(c.flags["since"]); err != nil {
			return fail(err)
		}
		if f.Until, err = parseLogTime(c.flags["until"]); err != nil {
			return fail(err)
		}
		pg := mcpserver.Calls(f, offset, limit)
		if js == nil {
			for _, l := range pg.Items {
				fmt.Fprintln(cio.stdout, string(l))
			}
			return 0
		}
		res = usage()
		res["items"], res["total"], res["clients"], res["tools"] = pg.Items, pg.Total, pg.Clients, pg.Tools
	case "clean":
		var n int
		if c.has("all") {
			n = mcpserver.CleanAllLog()
		} else {
			// 设置里「清理」用表单上（可能还没保存）的值
			if v, err := strconv.Atoi(c.flags["days"]); err == nil && v > 0 {
				st.MCP.LogKeepDays = v
			}
			if v, err := strconv.Atoi(c.flags["max-mb"]); err == nil && v > 0 {
				st.MCP.LogMaxMB = v
			}
			n = mcpserver.CleanLog(st.MCP.LogKeepDays, int64(st.MCP.LogMaxMB)<<20)
		}
		if js == nil {
			fmt.Fprintf(cio.stdout, "删除了 %d 条调用记录\n", n)
			return 0
		}
		res = usage()
		res["removed"] = n
	default:
		return fail(errors.New("用法：ferry mcp-log [list] [--offset N] [--limit N] | clean [--all]"))
	}
	js.Result(true, res, nil)
	return 0
}

// mcpToolsCmd ferry mcp-tools：编程软件能用的 MCP 工具（桌面版「MCP 工具列表」用）。
func mcpToolsCmd(cio *cliIO, js *ui.JSON) int {
	tools := mcpserver.Tools(Version)
	if js != nil {
		js.Result(true, map[string]any{"tools": tools}, nil)
		return 0
	}
	for _, t := range tools {
		kind := "查看"
		if t.Change {
			kind = "改动，要确认"
		}
		fmt.Fprintf(cio.stdout, "%s  %s（%s）\n    %s\n", t.Name, t.Title, kind, t.Description)
		for _, p := range t.Params {
			req := ""
			if p.Required {
				req = "，必填"
			}
			fmt.Fprintf(cio.stdout, "    %s- %s（%s%s）%s\n", strings.Repeat("  ", p.Depth), p.Name, p.Type, req, p.Description)
		}
	}
	return 0
}

// parseLogTime 筛选用的时间：RFC 3339（桌面版传的），或本地时间 2006-01-02、2006-01-02 15:04；空 = 不限。
func parseLogTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("时间格式不对：%s（写成 2026-10-22 或 2026-10-22 15:04）", v)
}
