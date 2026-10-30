// mcpcall 端到端测试用：像编程软件一样启动 ferry mcp，调用一个工具，打印返回的文本。
//
//	go run ./test/e2e/mcpcall <ferry> <目录> <工具> [参数 JSON]
//
// ferry mcp 在 <目录> 里启动（那里的 ferry.yaml 是默认项目）。工具报错时退出码为 1。
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
		fmt.Fprintln(os.Stderr, "用法：mcpcall <ferry> <目录> <工具> [参数 JSON]")
		os.Exit(2)
	}
	args := map[string]any{}
	if len(os.Args) > 4 {
		if err := json.Unmarshal([]byte(os.Args[4]), &args); err != nil {
			fmt.Fprintln(os.Stderr, "参数不是 JSON：", err)
			os.Exit(2)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: os.Args[3], Arguments: args})
	if err != nil {
		fmt.Fprintln(os.Stderr, "调用失败：", err)
		os.Exit(2)
	}
	for _, x := range r.Content {
		if t, ok := x.(*mcp.TextContent); ok {
			fmt.Println(t.Text)
		}
	}
	if r.IsError {
		os.Exit(1)
	}
}
