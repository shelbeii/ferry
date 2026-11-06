package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

// configToolCmd ferry config-schema / config-get / config-edit：和 AI 助手、MCP 的同名工具一样。
// config-edit 也是桌面版执行编程软件请求的修改时用的（FERRY_CONFIG_EDIT 里带着用户确认时的原文摘要 base，文件变了就不写）。
func configToolCmd(cmd string, c *cli, cio *cliIO, js *ui.JSON, fail func(error) int) int {
	show := func(v any) int {
		if js != nil {
			return ok0(js, v)
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintln(cio.stdout, string(b))
		return 0
	}
	arg := ""
	if len(c.pos) > 0 {
		arg = c.pos[0]
	}
	if cmd == "config-schema" {
		v, err := engine.ConfigSchemaInfo(arg, c.flags["query"])
		if err != nil {
			return fail(err)
		}
		return show(v)
	}
	path := c.flags["config"]
	if path == "" {
		var err error
		if path, err = config.Find("."); err != nil {
			return fail(err)
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if cmd == "config-get" {
		v, err := engine.ConfigRead(path, arg, c.has("local"))
		if err != nil {
			return fail(err)
		}
		return show(v)
	}
	var in engine.ConfigEditInput
	raw := cio.getenv("FERRY_CONFIG_EDIT")
	if strings.TrimSpace(raw) == "" {
		return fail(errors.New(`用环境变量 FERRY_CONFIG_EDIT 给出修改：{"edits":[{"op":"set","path":"apps.api.service.jvmArgs","value":["-Xmx512m"]}]}`))
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return fail(fmt.Errorf("FERRY_CONFIG_EDIT 不是正确的 JSON：%v", err))
	}
	if c.has("local") {
		in.Scope = "local"
	}
	plan, err := engine.PlanConfigEdit(path, in)
	if err != nil {
		return fail(err)
	}
	if c.has("dry-run") {
		out := engine.ConfigPlanResult(plan, false)
		if js != nil {
			return ok0(js, out)
		}
		fmt.Fprintln(cio.stdout, ai.LineDiff(plan.Old, plan.New))
		for _, p := range plan.Problems {
			fmt.Fprintln(cio.stdout, "✗ "+p)
		}
		return 0
	}
	if err := engine.ConfigPlanProblem(plan); err != nil {
		return fail(err)
	}
	if in.Base != "" && in.Base != plan.Base {
		return fail(fmt.Errorf("%s 在确认之后被改过了，没有写入：请重新改", plan.File))
	}
	if err := plan.Write(); err != nil {
		return fail(err)
	}
	cio.invalidate()
	out := engine.ConfigPlanResult(plan, true)
	if js != nil {
		return ok0(js, out)
	}
	fmt.Fprintf(cio.stdout, "已写入 %s（改了 %s）\n", plan.File, strings.Join(plan.Changed, "、"))
	return 0
}
