package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 和 Ferry 服务器助手同一套的工具：执行改动服务器的命令、写文件、用模板装软件、保存软件模板、让用户填密码。
// 改动的和 deploy 一样交给 Ferry 桌面版确认、执行；确认框里的内容由桌面版按参数自己算（危险命令标红、写文件显示差异）。
// 用户填的密码只在 Ferry 里填、只记在桌面版：编程软件只拿到代号，执行时由桌面版换成真值，交回的输出里遮住。

// aiActionTool 请求的操作 → 用服务器助手的哪个工具执行。
var aiActionTool = map[string]string{"exec": "run_command", "write-file": "write_file", "install": "software_install", "tpl-save": "save_software_template"}

type changeCmdIn struct {
	targetIn
	Command string `json:"command" jsonschema:"要执行的命令（不能需要交互，加 -y 等参数）"`
	Reason  string `json:"reason" jsonschema:"为什么要执行（显示在确认框里）"`
}

type writeFileIn struct {
	targetIn
	Path    string `json:"path" jsonschema:"文件的绝对路径"`
	Content string `json:"content" jsonschema:"文件的完整内容；要写用户填的密码时写代号 {{secret:名字}}"`
	Mode    string `json:"mode,omitempty" jsonschema:"权限（八进制，如 644）；不填 = 新文件 644、已有文件不变"`
	Reason  string `json:"reason" jsonschema:"为什么要改（显示在确认框里）"`
}

type swInstallIn struct {
	targetIn
	Template     string            `json:"template,omitempty" jsonschema:"模板 ID（software_templates 里的）"`
	TemplateYAML string            `json:"template_yaml,omitempty" jsonschema:"直接给出的模板内容（试装自己写的模板时用，格式见 template_guide）"`
	Version      string            `json:"version,omitempty" jsonschema:"版本；不填 = 最新"`
	Name         string            `json:"name,omitempty" jsonschema:"实例名；不填 = 模板 ID"`
	Params       map[string]string `json:"params,omitempty" jsonschema:"模板参数（如 port、public）；密码不填时自动生成，用户要自己定密码时先用 ask_secret 让用户填，这里写代号 {{secret:名字}}"`
	Reason       string            `json:"reason" jsonschema:"为什么要装（显示在确认框里）"`
}

type tplSaveIn struct {
	targetIn
	YAML   string `json:"yaml" jsonschema:"模板的完整 YAML（格式见 template_guide）"`
	Reason string `json:"reason,omitempty" jsonschema:"说明（显示在确认框里）"`
}

type askSecretIn struct {
	targetIn
	Fields  []ai.SecretField `json:"fields" jsonschema:"要填的项（最多 5 项）：name 是代号里的名字（小写字母开头，只能有小写字母、数字、下划线），label 是给用户看的说明，kind 为 password（默认）或 text，confirm 为 true 时输两遍"`
	Purpose string           `json:"purpose" jsonschema:"要这些做什么（显示在 Ferry 的输入框上）"`
}

func (s *server) addAITools(m *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	change := func() *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}
	}
	const how = "需要用户在 Ferry 设置里允许编程软件请求改动；每次都要用户确认（在 Ferry 桌面版里，或者用户设置了在编程软件里确认时在这里），不能授权自动同意；" +
		"同意后由 Ferry 桌面版执行，生产服务器、「新建连接」的服务器不允许。最多等 40 秒，没结束返回 id，再用 task_status 查。"
	const secrets = "用到用户填的密码时：先用 ask_secret 让用户在 Ferry 里填，你只拿到代号；"

	addTool(s, m, &mcp.Tool{Name: "run_change_command", Title: "执行改动服务器的命令",
		Description: "在服务器上执行一条会改动服务器的 shell 命令（以登录用户执行，需要 root 时加 sudo），返回输出与退出码，最多 30 分钟。" +
			"只读的命令直接执行、不用确认（和 run_command 一样）。命令不能需要交互，不要在前台运行长期服务；改配置文件用 write_file，装软件先看 software_templates。" +
			secrets + "命令里写环境变量 \"$FERRY_SECRET_<名字大写>\"（带双引号），执行时由 Ferry 赋值，用到它的命令每次都要确认。" + how,
		Annotations: change()},
		loggedReq(s, "run_change_command", func(ctx context.Context, req *mcp.CallToolRequest, in changeCmdIn) (any, error) {
			cmd := strings.TrimSpace(in.Command)
			if cmd == "" {
				return nil, errors.New("command 是空的")
			}
			if ai.ReadOnly(cmd) == nil && !ai.UsesSecrets(cmd) {
				return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
					return e.RunReadOnly(t, cmd)
				})
			}
			return s.aiChange(ctx, req, in.targetIn, "exec", map[string]string{"command": cmd, "purpose": in.Reason}, in.Reason)
		}))

	addTool(s, m, &mcp.Tool{Name: "write_file", Title: "写文件",
		Description: "在服务器上写一个文本文件（整个内容），改配置文件用它：确认框里显示和原文件的差异，写之前自动把原文件备份为 <文件>.ferry-bak-<时间>，以 root 写。" +
			"先用 run_command 的 cat 看原内容，在原内容基础上改。" + secrets + "内容里写代号 {{secret:名字}}，写入时换成真值。" + how,
		Annotations: change()},
		loggedReq(s, "write_file", func(ctx context.Context, req *mcp.CallToolRequest, in writeFileIn) (any, error) {
			if !strings.HasPrefix(in.Path, "/") {
				return nil, errors.New("path 要写绝对路径")
			}
			return s.aiChange(ctx, req, in.targetIn, "write-file", map[string]string{"path": in.Path, "content": in.Content, "mode": in.Mode}, in.Reason)
		}))

	addTool(s, m, &mcp.Tool{Name: "software_templates", Title: "软件模板库",
		Description: "Ferry 软件模板库里能装的软件（数据库、缓存、中间件等）：模板 ID、名称、说明、参数、各种装法（适用系统）。用 software_install 安装。",
		Annotations: readOnly},
		logged(s, "software_templates", func(ctx context.Context, in targetIn) (any, error) {
			return s.aiReadOnly(ctx, in, "software_templates")
		}))

	addTool(s, m, &mcp.Tool{Name: "template_guide", Title: "软件模板的格式",
		Description: "软件模板（YAML）的格式说明：自己写模板试装（software_install 的 template_yaml）或保存（save_software_template）前先看。",
		Annotations: readOnly},
		logged(s, "template_guide", func(ctx context.Context, in targetIn) (any, error) {
			return s.aiReadOnly(ctx, in, "template_guide")
		}))

	addTool(s, m, &mcp.Tool{Name: "software_install", Title: "用模板装软件",
		Description: "用 Ferry 的软件模板在服务器上安装软件：自动按系统选装法、建 systemd 服务（ferry-<名称>.service）、检查端口；" +
			"密码不填时随机生成并存进用户的凭据库（不会告诉你）。template 填模板库里的 ID；也可以用 template_yaml 直接给一个模板试装。" + how,
		Annotations: change()},
		loggedReq(s, "software_install", func(ctx context.Context, req *mcp.CallToolRequest, in swInstallIn) (any, error) {
			if in.Template == "" && strings.TrimSpace(in.TemplateYAML) == "" {
				return nil, errors.New("要给出 template（模板 ID）或 template_yaml")
			}
			input := map[string]any{"template": in.Template, "template_yaml": in.TemplateYAML, "version": in.Version, "name": in.Name, "params": in.Params}
			return s.aiChange(ctx, req, in.targetIn, "install", input, in.Reason)
		}))

	addTool(s, m, &mcp.Tool{Name: "save_software_template", Title: "保存软件模板",
		Description: "把一个软件模板保存到用户的模板库（Ferry「设置 → 运行环境」里的软件模板），以后在 Ferry 的「软件」里能直接用它安装。格式见 template_guide，保存前检查格式。" +
			"target 只用来确定是哪个项目的请求，不连服务器。" + how,
		Annotations: change()},
		loggedReq(s, "save_software_template", func(ctx context.Context, req *mcp.CallToolRequest, in tplSaveIn) (any, error) {
			if strings.TrimSpace(in.YAML) == "" {
				return nil, errors.New("yaml 是空的")
			}
			return s.aiChange(ctx, req, in.targetIn, "tpl-save", map[string]string{"yaml": in.YAML}, in.Reason)
		}))

	addTool(s, m, &mcp.Tool{Name: "ask_secret", Title: "让用户填密码",
		Description: "需要用户提供密码、账号、密钥等敏感信息时用它：Ferry 桌面版弹出输入框让用户填，值只留在用户电脑上的 Ferry 里，你只拿到代号 {{secret:名字}}，看不到值。" +
			"不要让用户在对话里直接发密码。之后在 run_change_command 的命令里用环境变量 \"$FERRY_SECRET_<名字大写>\"，在 write_file 的内容、software_install 的参数里写代号。" +
			"代号只在这次连接里有效（ferry mcp 重启后要重新填）。需要用户在 Ferry 设置里允许编程软件请求改动；最多等 40 秒，没填完返回 id，再用 task_status 查。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		loggedReq(s, "ask_secret", func(ctx context.Context, req *mcp.CallToolRequest, in askSecretIn) (any, error) {
			if err := ai.CheckSecretFields(in.Fields); err != nil {
				return nil, err
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: "secret", Fields: in.Fields, Reason: in.Purpose})
		}))
}

// aiChange 用服务器助手的工具改动服务器：交给桌面版确认、执行。
func (s *server) aiChange(ctx context.Context, req *mcp.CallToolRequest, in targetIn, action string, input any, reason string) (any, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	return s.change(ctx, req, in, approve.Request{Action: action, Tool: aiActionTool[action], Input: b, Reason: reason})
}

// aiReadOnly 只读的工具：ferry mcp 自己执行。
func (s *server) aiReadOnly(ctx context.Context, in targetIn, tool string) (any, error) {
	return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
		out, err := e.AIExec(t, engine.MCPScope(s.session), tool, json.RawMessage("{}"), nil)
		if err != nil {
			return nil, err
		}
		return out, nil
	})
}

// aiDetail 确认框里显示的内容（按服务器助手的确认内容）：一句说明、以 root、每次都要确认的原因、完整的命令或差异。
func aiDetail(ap *ai.Approval) []string {
	var out []string
	head := ap.Title
	if ap.AsRoot {
		head += "（以 root 执行）"
	}
	out = append(out, head)
	if ap.Once != "" && ap.Danger == "" {
		out = append(out, "每次都要确认："+ap.Once)
	}
	if ap.Detail != "" {
		out = append(out, ap.Detail)
	}
	return out
}

// prepareAI 桌面版收到用助手工具改动的请求：按参数重新算确认框里的内容（不用请求方给的）。
func prepareAI(e *engine.Engine, r *approve.Request) error {
	tool, ok := aiActionTool[r.Action]
	if !ok || r.Tool != tool {
		return fmt.Errorf("操作和工具对不上：%s / %s", r.Action, r.Tool)
	}
	ap, err := e.AIApproval(r.Target, engine.MCPScope(r.Session), tool, r.Input)
	if err != nil {
		return err
	}
	if ap == nil {
		return errors.New("没有要确认的改动：参数不对、文件内容没变，或者用到的密码还没填（先用 ask_secret）")
	}
	r.Detail, r.Danger = aiDetail(ap), ap.Danger
	return nil
}

// aiPreview ferry mcp 这边先算一遍确认框里的内容（在编程软件里确认时给用户看；交给桌面版后它会重新算）。
// 用到的密码只在桌面版里，这边算不出来时显示参数本身。
func aiPreview(e *engine.Engine, target string, r approve.Request) ([]string, string) {
	if r.Action == "secret" {
		var out []string
		for _, f := range r.Fields {
			out = append(out, "要填："+f.Label)
		}
		return out, ""
	}
	if ap, err := e.AIApproval(target, engine.MCPScope(r.Session), r.Tool, r.Input); err == nil && ap != nil {
		return aiDetail(ap), ap.Danger
	}
	var in map[string]any
	json.Unmarshal(r.Input, &in)
	b, _ := json.MarshalIndent(in, "", "  ")
	return []string{ai.Clip(string(b), 8000)}, ""
}
