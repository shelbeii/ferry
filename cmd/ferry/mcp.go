package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/mcpserver"
)

// mcpMain ferry mcp：MCP 服务（stdin / stdout），给 Claude Code、Cursor 等编程软件用。
// stdout 只能是 MCP 的消息；出错信息写 stderr（编程软件会显示在它的日志里）。
func mcpMain() int {
	appdir.MigrateUserDir()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	dir, _ := os.Getwd()
	if err := mcpserver.Run(ctx, mcpserver.Options{Version: Version, Dir: dir}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "ferry mcp:", err)
		return 1
	}
	return 0
}
