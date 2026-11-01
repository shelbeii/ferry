package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/captain/ferry/internal/ai"
)

// 编程软件经 MCP 用服务器助手的工具：执行改动服务器的命令、写文件、用模板装软件、保存软件模板，
// 以及只读的看模板库、模板格式。改动的由用户同意后桌面版执行（ferry ai-exec），和助手里的同一套做法：
// 确认框里的内容、危险命令的判断、写文件前备份、用户填的密码（scope 是这次 MCP 连接，密码按它记）都一样。

// AIExecTools 编程软件能经 ferry ai-exec 用的工具；值为 true 的是只读的，ferry mcp 自己执行，不用确认。
var AIExecTools = map[string]bool{
	"run_command": false, "write_file": false, "software_install": false, "save_software_template": false,
	"software_upgrade": false, "software_restore": false, "software_backup": false,
	"software_templates": true, "template_guide": true,
}

// MCPScope 编程软件这次连接（ferry mcp 进程）在用户填的密码里的范围：mcp-<连接标识>。
func MCPScope(session string) string { return "mcp-" + session }

// aiExecTool 找到目标上的这个工具（允许改动的那套）。
func (e *Engine) aiExecTool(s *session, target, scope, tool string) (ai.Tool, error) {
	if _, ok := AIExecTools[tool]; !ok {
		return ai.Tool{}, fmt.Errorf("不支持的工具：%s", tool)
	}
	t, err := e.File.Target(target)
	if err != nil {
		return ai.Tool{}, err
	}
	for _, x := range e.aiTools(s, t, AIConfirm, scope) {
		if x.Name == tool {
			return x, nil
		}
	}
	return ai.Tool{}, fmt.Errorf("%s 上不能用 %s", target, tool)
}

// AIApproval 这次调用要用户确认的内容（确认框里显示）；只读、不用确认的返回 nil。参数不对时返回错误。
func (e *Engine) AIApproval(target, scope, tool string, input json.RawMessage) (*ai.Approval, error) {
	if !json.Valid(input) {
		return nil, errors.New("参数不是 JSON")
	}
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	s, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	x, err := e.aiExecTool(s, target, scope, tool)
	if err != nil {
		return nil, err
	}
	if x.Approve == nil {
		return nil, nil
	}
	return x.Approve(e.ctx(), input), nil
}

// AIExec 执行这次调用，progress 收实时输出。输出、错误里出现用户填的密码时遮住（它们会交回编程软件）。
func (e *Engine) AIExec(target, scope, tool string, input json.RawMessage, progress func(string)) (string, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return "", err
	}
	if e.Pool == nil {
		e.Pool = NewPool(0)
		defer func() { e.Pool.Close(); e.Pool = nil }()
	}
	if err := takeAISecrets(scope, nil); err != nil { // 读回记住的
		return "", err
	}
	s, err := e.connect(t)
	if err != nil {
		return "", err
	}
	defer s.close()
	x, err := e.aiExecTool(s, target, scope, tool)
	if err != nil {
		return "", err
	}
	ctx := ai.WithProgress(e.ctx(), func(text string) {
		if progress != nil {
			progress(ai.MaskSecrets(scope, text))
		}
	})
	out, err := x.Run(ctx, input)
	out = ai.Clip(ai.MaskSecrets(scope, out), ai.ResultLimit)
	if err != nil {
		return out, errors.New(ai.MaskSecrets(scope, err.Error()))
	}
	return out, nil
}

// TakeAISecrets 记下用户在 Ferry 里填的（编程软件经 MCP 请求填写时，scope 是那次连接）。
func TakeAISecrets(scope string, in []AISecret) error { return takeAISecrets(scope, in) }

// SecretTokens 填好后交回编程软件的说明：每项的代号和命令里用的环境变量。
func SecretTokens(fields []ai.SecretField) string {
	var b strings.Builder
	b.WriteString("用户在 Ferry 里填好了（值只在用户电脑上，你看不到，也不要问）：\n")
	for _, f := range fields {
		fmt.Fprintf(&b, "- %s：代号 %s，run_change_command 的命令里用 \"$%s\"，write_file 的内容、software_install 的参数里写代号\n",
			f.Label, ai.SecretToken(f.Name), ai.SecretEnv(f.Name))
	}
	return b.String()
}
