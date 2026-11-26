// mcpcall 端到端测试用：像编程软件一样启动 ferry mcp，调用工具，打印返回的文本。
//
//	go run ./test/e2e/mcpcall <ferry> <目录> <工具> [参数 JSON] [<工具> <参数 JSON>]…
//
// ferry mcp 在 <目录> 里启动（那里的 ferry.yaml 是默认项目）。给了多个工具时在同一个连接里依次调用
// （用户填的密码按连接记，ask_secret 和用到它的命令要在同一个连接里），每个的结果前打印一行 「== 工具」。
// 任何一个工具报错时退出码为 1（后面的照样调用）。
//
// 可选环境变量（用于端到端测试客户端确认渠道与资源模板）：
//   - MCPCALL_ELICIT=approve|decline|fast-approve   开启 MCP Elicitation 表单处理器
//   - MCPCALL_ELICIT_DELAY_MS=1600                  模拟人工查看耗时（毫秒）
//   - MCPCALL_PROTOCOL=2025-11-25                   指定协商的 MCP 协议版本
//   - MCPCALL_APP=approve|reject                    声明支持 MCP Apps（io.modelcontextprotocol/ui），自动拉取卡片资源并通过 app-only 工具确认/拒绝
//   - MCPCALL_TWO_STEP=1                            遇到两段式 pendingConfirmation 时自动带着 confirmArgs 调用 confirm_change
//   - MCPCALL_READ_RESOURCE=<uri>                   连接后直接读取指定 MCP 资源 / 资源模板并打印内容（无需传工具名）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	readResource := os.Getenv("MCPCALL_READ_RESOURCE")
	if len(os.Args) < 3 || (readResource == "" && len(os.Args) < 4) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute*time.Duration(max(len(calls), 1)))
	defer cancel()
	cmd := exec.Command(os.Args[1], "mcp")
	cmd.Dir = os.Args[2]
	cmd.Stderr = os.Stderr

	elicitMode := os.Getenv("MCPCALL_ELICIT")
	appMode := os.Getenv("MCPCALL_APP")
	twoStep := os.Getenv("MCPCALL_TWO_STEP") == "1"
	protoVer := os.Getenv("MCPCALL_PROTOCOL")
	delayMs, _ := strconv.Atoi(os.Getenv("MCPCALL_ELICIT_DELAY_MS"))

	var opts *mcp.ClientOptions
	if elicitMode != "" || appMode != "" {
		opts = &mcp.ClientOptions{}
		if appMode != "" {
			caps := &mcp.ClientCapabilities{}
			caps.AddExtension("io.modelcontextprotocol/ui", map[string]any{"mimeTypes": []string{"text/html;profile=mcp-app"}})
			opts.Capabilities = caps
		}
		if elicitMode != "" {
			opts.ElicitationHandler = func(ctx context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				if elicitMode != "fast-approve" && delayMs > 0 {
					time.Sleep(time.Duration(delayMs) * time.Millisecond)
				}
				switch elicitMode {
				case "approve", "fast-approve":
					return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
				case "decline":
					return &mcp.ElicitResult{Action: "decline"}, nil
				default:
					return &mcp.ElicitResult{Action: "cancel"}, nil
				}
			}
		}
	}

	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, opts)
	var sessOpts *mcp.ClientSessionOptions
	if protoVer != "" {
		sessOpts = &mcp.ClientSessionOptions{ProtocolVersion: protoVer}
	}
	cs, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, sessOpts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连不上 ferry mcp：", err)
		os.Exit(2)
	}
	defer cs.Close()
	if readResource != "" {
		res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: readResource})
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取资源失败：", err)
			os.Exit(1)
		}
		for _, c := range res.Contents {
			fmt.Println(c.Text)
		}
		os.Exit(0)
	}
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
		text := extractText(r)

		// MCP Apps 卡片确认模式：验证资源、检查 appToken 未泄露、调用 app-only 工具并轮询到完成
		if appMode != "" && !r.IsError {
			if sc, ok := r.StructuredContent.(map[string]any); ok && sc["kind"] == "ferry-confirm-card" {
				res, rerr := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ui://ferry/confirm.html"})
				if rerr != nil || len(res.Contents) == 0 || res.Contents[0].MIMEType != "text/html;profile=mcp-app" {
					fmt.Fprintln(os.Stderr, "读取 MCP App 卡片资源失败：", rerr)
					os.Exit(2)
				}
				approvalID, _ := sc["approvalId"].(string)
				appToken, _ := sc["appToken"].(string)
				if appToken == "" || strings.Contains(text, appToken) {
					fmt.Fprintln(os.Stderr, "MCP App appToken 缺失或泄露到了文本内容中")
					os.Exit(2)
				}
				decArgs := map[string]any{"approvalId": approvalID, "appToken": appToken, "decision": appMode}
				if reason := os.Getenv("MCPCALL_APP_REASON"); reason != "" {
					decArgs["reason"] = reason
				}
				dec, derr := cs.CallTool(ctx, &mcp.CallToolParams{
					Name:      "mcp_app_decide",
					Arguments: decArgs,
				})
				if derr != nil {
					fmt.Fprintln(os.Stderr, "mcp_app_decide 失败：", derr)
					os.Exit(2)
				}
				r = dec
				text = extractText(r)
				for {
					stMap, _ := r.StructuredContent.(map[string]any)
					if stMap["state"] != "running" && stMap["state"] != "waiting" {
						break
					}
					time.Sleep(300 * time.Millisecond)
					pol, perr := cs.CallTool(ctx, &mcp.CallToolParams{
						Name:      "mcp_app_status",
						Arguments: map[string]any{"approvalId": approvalID, "appToken": appToken},
					})
					if perr != nil {
						fmt.Fprintln(os.Stderr, "mcp_app_status 失败：", perr)
						os.Exit(2)
					}
					r = pol
					text = extractText(r)
				}
			}
		}

		// 两段式确认模式：自动携带 confirmArgs 调用 confirm_change
		if twoStep && !r.IsError {
			var step1 struct {
				State       string         `json:"state"`
				ConfirmArgs map[string]any `json:"confirmArgs"`
			}
			if json.Unmarshal([]byte(text), &step1) == nil && step1.State == "pendingConfirmation" && len(step1.ConfirmArgs) > 0 {
				r2, err2 := cs.CallTool(ctx, &mcp.CallToolParams{Name: "confirm_change", Arguments: step1.ConfirmArgs})
				if err2 != nil {
					fmt.Fprintln(os.Stderr, "confirm_change 失败：", err2)
					os.Exit(2)
				}
				r = r2
				text = extractText(r)
			}
		}

		fmt.Println(text)
		if r.IsError {
			code = 1
		}
	}
	os.Exit(code)
}

func extractText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, y := range r.Content {
		if t, ok := y.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
