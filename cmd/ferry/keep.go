package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

const keepUsage = "用法：ferry keep scan <目标> <应用> [--dir <目录>] [--keep-files <JSON>] | keep set <应用> <JSON>"

// keepCmd ferry keep scan|set：部署时保留服务器上的（artifact.keep）。
func keepCmd(c *cli, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	ok := func(data any) int { return ok0(js, data) }
	if len(c.pos) == 0 {
		return fail(errors.New(keepUsage))
	}
	switch c.pos[0] {
	case "scan":
		if len(c.pos) != 3 {
			return fail(errors.New(keepUsage))
		}
		var keep []*config.KeepItem
		if v := c.flags["keep-files"]; v != "" {
			keep = []*config.KeepItem{}
			if err := json.Unmarshal([]byte(v), &keep); err != nil {
				return fail(fmt.Errorf("--keep-files 不是有效的 JSON：%v", err))
			}
		}
		r, err := e.KeepScan(c.pos[1], c.pos[2], keep, c.flags["dir"])
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(r)
		}
		if r.Release == "" {
			con.Info("%s 还没部署过", r.App)
		} else {
			con.Info("%s 运行中的版本 %s", r.App, r.Release)
		}
		if len(r.Items) == 0 {
			con.Info("没有要保留的（ferry.yaml 的 artifact.keep）")
		}
		for _, it := range r.Items {
			name := it.Path
			if it.Shared {
				name += "（共用）"
			}
			if it.Differs {
				con.Warn("%s：%s", name, it.Note)
			} else {
				con.Info("%s：%s", name, it.Note)
			}
		}
		for _, x := range r.Entries {
			kind := ""
			switch {
			case x.Link:
				kind = "（链接）"
			case x.Dir:
				kind = "/"
			}
			con.Info("  %s%s", x.Path, kind)
		}
		if r.More {
			con.Info("  …还有更多")
		}
		return 0
	case "set":
		if len(c.pos) != 3 {
			return fail(errors.New(keepUsage))
		}
		var keep []*config.KeepItem
		if err := json.Unmarshal([]byte(c.pos[2]), &keep); err != nil {
			return fail(fmt.Errorf("要保留的不是有效的 JSON：%v", err))
		}
		if err := e.SetKeep(c.pos[1], keep); err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(map[string]any{"app": c.pos[1], "keep": keep})
		}
		con.OK("已写进 ferry.yaml：%s 的 artifact.keep", c.pos[1])
		return 0
	}
	return fail(errors.New(keepUsage))
}
