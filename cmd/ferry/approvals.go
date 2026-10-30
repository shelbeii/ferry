package main

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/mcpserver"
	"github.com/captain/ferry/internal/ui"
)

var (
	hubOnce sync.Once
	hub     *approve.Hub
)

// approvals 桌面版里等编程软件（ferry mcp）来请求改动的那个 Hub；整个 ferry serve 只有一个。
// 请求交给用户之前再按设置、配置检查一遍：绕过 ferry mcp 直接发来的请求也一样。
func approvals() *approve.Hub {
	hubOnce.Do(func() {
		hub = approve.NewHub(approve.DefaultFile())
		hub.Prepare = mcpserver.PrepareRequest
	})
	return hub
}

// mcpApprovalsCmd ferry serve 里：
//
//	mcp-approvals                       一直运行，有请求时输出 {"kind":"mcp-request",...}，请求不用再处理时 {"kind":"mcp-gone","id":…}
//	mcp-decide <ID> approve|reject [--note 说明]   用户的决定（同意后由桌面版执行）
//	mcp-decide <ID> progress --note <一行>        执行中的进展
//	mcp-decide <ID> done                          执行完了：结果从环境变量 FERRY_MCP_OUTCOME（JSON）读
func mcpApprovalsCmd(cmd string, c *cli, cio *cliIO, js *ui.JSON, fail func(error) int) int {
	if cio.pool == nil || js == nil {
		return fail(errors.New(cmd + " 只在桌面版里使用"))
	}
	if cmd == "mcp-decide" {
		if len(c.pos) < 2 {
			return fail(errors.New("用法：ferry mcp-decide <ID> approve|reject|progress|done"))
		}
		id, note := c.pos[0], c.flags["note"]
		switch c.pos[1] {
		case "approve", "reject":
			if c.pos[1] == "reject" && note == "" {
				note = "你在 Ferry 里拒绝了"
			}
			if err := approvals().Decide(id, approve.Decision{Approve: c.pos[1] == "approve", Note: note}); err != nil {
				return fail(err)
			}
		case "progress":
			approvals().Progress(id, note)
		case "done":
			var o approve.Outcome
			if err := json.Unmarshal([]byte(cio.getenv("FERRY_MCP_OUTCOME")), &o); err != nil {
				return fail(errors.New("FERRY_MCP_OUTCOME 不是合法的 JSON"))
			}
			approvals().Done(id, o)
		default:
			return fail(errors.New("用法：ferry mcp-decide <ID> approve|reject|progress|done"))
		}
		return ok0(js, nil)
	}
	unsub, err := approvals().Subscribe(func(kind string, v any) {
		var m map[string]any
		b, _ := json.Marshal(v)
		json.Unmarshal(b, &m)
		js.Emit("mcp-"+kind, m)
		if fl, ok := cio.stdout.(interface{ Flush() }); ok {
			fl.Flush()
		}
	})
	if err != nil {
		return fail(err)
	}
	defer unsub()
	<-cio.ctx.Done()
	return ok0(js, nil)
}
