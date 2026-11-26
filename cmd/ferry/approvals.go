package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/mcpserver"
	"github.com/captain/ferry/internal/ui"
)

var (
	hubOnce   sync.Once
	hub       *approve.Hub
	hubPoolMu sync.RWMutex
	hubPool   *engine.Pool
)

func setHubPool(p *engine.Pool) {
	if p == nil {
		return
	}
	hubPoolMu.Lock()
	hubPool = p
	hubPoolMu.Unlock()
}

func getHubPool() *engine.Pool {
	hubPoolMu.RLock()
	defer hubPoolMu.RUnlock()
	return hubPool
}

// approvals 桌面版里等编程软件（ferry mcp）来请求改动的那个 Hub；整个 ferry serve 只有一个。
// 请求交给用户之前再按设置、配置检查一遍：绕过 ferry mcp 直接发来的请求也一样；复用 ferry serve 的连接池避免每次预检重建 SSH。
func approvals() *approve.Hub {
	hubOnce.Do(func() {
		hub = approve.NewHub(approve.DefaultFile())
		hub.Prepare = func(r *approve.Request) error {
			return mcpserver.PrepareRequestWithPool(r, getHubPool(), os.Getenv)
		}
		hub.PrepareClient = mcpserver.PrepareClientApproval
		hub.VerifyCommit = mcpserver.VerifyClientCommit
		hub.OnSessionClose = func(session string) {
			ai.ForgetSecrets(engine.MCPScope(session))
		}
	})
	return hub
}

// mcpApprovalsCmd ferry serve 里：
//
//	mcp-approvals                       一直运行，有请求时输出 {"kind":"mcp-request",...}，请求不用再处理时 {"kind":"mcp-gone","id":…}
//	mcp-decide <ID> approve|reject [--note 说明] [--block]  用户的决定（同意后由桌面版执行；--block 拉黑该 MCP 连接）
//	mcp-decide <ID> progress --note <一行>        执行中的进展
//	mcp-decide <ID> done                          执行完了：结果从环境变量 FERRY_MCP_OUTCOME（JSON）读
//	mcp-sessions                                  查看当前活跃或已拉黑的 MCP 连接及能力诊断
//	mcp-unblock <SESSION>                         解除对某个 MCP 连接的拉黑
func mcpApprovalsCmd(cmd string, c *cli, cio *cliIO, js *ui.JSON, fail func(error) int) int {
	if cio.pool == nil || js == nil {
		return fail(errors.New(cmd + " 只在桌面版里使用"))
	}
	setHubPool(cio.pool)
	if cmd == "mcp-sessions" {
		return ok0(js, map[string]any{"sessions": approvals().Sessions()})
	}
	if cmd == "mcp-unblock" {
		if len(c.pos) < 1 || c.pos[0] == "" {
			return fail(errors.New("用法：ferry mcp-unblock <SESSION>"))
		}
		return ok0(js, map[string]any{"unblocked": approvals().UnblockSession(c.pos[0])})
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
			if err := approvals().DecideWithBlock(id, approve.Decision{Approve: c.pos[1] == "approve", Note: note}, c.pos[1] == "reject" && c.has("block")); err != nil {
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

// mcpExecCmd 统一执行已确认的 MCP 改动请求（桌面版与离线执行共用 mcpserver.ExecuteRequest，不再在前端拼接各子命令参数）。
// 请求 JSON 通过环境变量 FERRY_MCP_REQ 传入；可选 -c 与位置参数 <目标> 供任务面板归属和认证重试使用。
func mcpExecCmd(c *cli, cio *cliIO, rep engine.Reporter, js *ui.JSON, fail func(error) int) int {
	raw := strings.TrimSpace(cio.getenv("FERRY_MCP_REQ"))
	if raw == "" {
		return fail(errors.New("缺少环境变量 FERRY_MCP_REQ（MCP 改动请求 JSON）"))
	}
	var r approve.Request
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return fail(fmt.Errorf("FERRY_MCP_REQ 不是合法的 JSON：%v", err))
	}
	if r.Config == "" && c.flags["config"] != "" {
		r.Config = c.flags["config"]
	}
	if r.Target == "" && len(c.pos) > 0 {
		r.Target = strings.Split(c.pos[0], ",")[0]
	}
	data, err := mcpserver.ExecuteRequest(cio.ctx, r, mcpserver.ExecOptions{
		Rep:           rep,
		Pool:          cio.pool,
		Getenv:        cio.getenv,
		DesktopOnline: true,
	})
	if err != nil {
		if js != nil && data != nil {
			js.Result(false, data, err)
			return 1
		}
		return fail(err)
	}
	if js != nil {
		return ok0(js, data)
	}
	if data != nil {
		b, _ := json.MarshalIndent(data, "", "  ")
		fmt.Fprintln(cio.stdout, string(b))
	}
	return 0
}
