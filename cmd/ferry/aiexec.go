package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

// aiExecCmd ferry ai-exec <目标> <工具> --scope <范围>：编程软件经 MCP 请求、用户在 Ferry 里同意后，由桌面版用服务器助手的工具执行
// （改动服务器的命令、写文件、用模板装软件、保存软件模板）。工具的参数从环境变量 FERRY_AI_INPUT 读（JSON）；
// 范围是那次 MCP 连接，命令里用到的用户填的密码按它找。输出里出现这些密码时遮住。
func aiExecCmd(e *engine.Engine, c *cli, cio *cliIO, js *ui.JSON, fail func(error) int) int {
	if len(c.pos) != 2 || c.flags["scope"] == "" {
		return fail(errors.New("用法：ferry ai-exec <目标> <工具> --scope <范围>（参数用环境变量 FERRY_AI_INPUT 给）"))
	}
	input := json.RawMessage(strings.TrimSpace(cio.getenv("FERRY_AI_INPUT")))
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	// 工具执行期间 e.Rep 换成了把进度转给这里的那个（withProgress）：这里要用原来的，不然进度在里面绕圈，直到内存用完
	rep := e.Rep
	out, err := e.AIExec(c.pos[0], c.flags["scope"], c.pos[1], input, func(text string) {
		for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			rep.Info("%s", l)
		}
	})
	if err != nil {
		if out != "" {
			err = fmt.Errorf("%s\n%v", out, err)
		}
		return fail(err)
	}
	if js != nil {
		return ok0(js, map[string]string{"output": out})
	}
	fmt.Fprintln(cio.stdout, out)
	return 0
}

// aiSecretCmd ferry ai-secret --scope <范围>：记下用户在 Ferry 里填的密码、账号（编程软件经 MCP 请求填写时）。
// 值从环境变量 FERRY_AI_SECRETS 读（[{"name","value","remember","label"}]），只记在桌面版的 ferry serve 里。
func aiSecretCmd(c *cli, cio *cliIO, js *ui.JSON, fail func(error) int) int {
	if cio.pool == nil {
		return fail(errors.New("只能在 Ferry 桌面版里填写"))
	}
	scope := c.flags["scope"]
	if scope == "" {
		return fail(errors.New("用法：ferry ai-secret --scope <范围>"))
	}
	var in []engine.AISecret
	if err := json.Unmarshal([]byte(cio.getenv("FERRY_AI_SECRETS")), &in); err != nil || len(in) == 0 {
		return fail(errors.New("没有填写的内容"))
	}
	if err := engine.TakeAISecrets(scope, in); err != nil {
		return fail(err)
	}
	return ok0(js, map[string]int{"stored": len(in)})
}
