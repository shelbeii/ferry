// mcpcall 端到端测试用：像编程软件一样启动 ferry mcp，调用工具，打印返回的文本。
//
//	go run ./test/e2e/mcpcall <ferry> <目录> <工具> [参数 JSON] [<工具> <参数 JSON>]…
//
// ferry mcp 在 <目录> 里启动（那里的 ferry.yaml 是默认项目）。给了多个工具时在同一个连接里依次调用
// （用户填的密码按连接记，ask_secret 和用到它的命令要在同一个连接里），每个的结果前打印一行 「== 工具」。
// 任何一个工具报错时退出码为 1（后面的照样调用）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "用法：mcpcall <ferry> <目录> <工具> [参数 JSON] [<工具> <参数 JSON>]…")
		os.Exit(2)
	}
	type call struct {
		tool string
		args map[string]any
	}
	var calls []call
	rest := os.Args[3:]
	for len(rest) > 0 {
		c := call{tool: rest[0], args: map[string]any{}}
		rest = rest[1:]
		if len(rest) > 0 {
			if err := json.Unmarshal([]byte(rest[0]), &c.args); err != nil {
				fmt.Fprintln(os.Stderr, "参数不是 JSON：", err)
				os.Exit(2)
			}
			rest = rest[1:]
		}
		calls = append(calls, c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute*time.Duration(len(calls)))
	defer cancel()
	cmd := exec.Command(os.Args[1], "mcp")
	cmd.Dir = os.Args[2]
	cmd.Stderr = os.Stderr
	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连不上 ferry mcp：", err)
		os.Exit(2)
	}
	defer cs.Close()
	code := 0
	for _, x := range calls {
		if len(calls) > 1 {
			fmt.Println("== " + x.tool)
		}
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: x.tool, Arguments: x.args})
		if err != nil {
			fmt.Fprintln(os.Stderr, "调用失败：", err)
			os.Exit(2)
		}
		for _, y := range r.Content {
			if t, ok := y.(*mcp.TextContent); ok {
				fmt.Println(t.Text)
			}
		}
		if r.IsError {
			code = 1
		}
	}
	os.Exit(code)
}
