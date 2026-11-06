package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 项目配置（ferry.yaml）和本机设置：config_schema 查字段的意思，config_get 看现在的，config_check 校验 / 试改，
// config_edit 按路径改（用户在 Ferry 里看过差异、同意后由桌面版写入）。和 Ferry 的 AI 助手用同一套（engine/aiconfig.go）。

type configSchemaIn struct {
	Path  string `json:"path,omitempty" jsonschema:"这一项的路径，如 apps.[].service、targets.*、apps.[].service.health（列表项写 [] 或名字，映射的键写 * 或名字）；填 local 看本机设置能改的；不填 = 顶层和总说明"`
	Query string `json:"query,omitempty" jsonschema:"按关键字搜（如 健康检查、jvm、重启、端口），几个词用空格分开"`
}

type configGetIn struct {
	Project string `json:"project,omitempty" jsonschema:"项目名或 ferry.yaml 的路径（list_projects 里的 name 或 config）；不填 = 当前目录的项目，或只有一个项目时用它"`
	Path    string `json:"path,omitempty" jsonschema:"只看这一项，如 apps.api.service、targets.test（列表项写 name 或序号）；不填 = 整个文件"`
	Scope   string `json:"scope,omitempty" jsonschema:"填 yaml（默认，ferry.yaml）或 local（这台电脑的本机设置）"`
}

type editIn struct {
	Op    string `json:"op" jsonschema:"set（设置，没有的上级自动建）、delete（删掉）或 append（在列表末尾加一项）"`
	Path  string `json:"path" jsonschema:"路径，如 apps.api.service.jvmArgs、targets.test.host；列表项写 name 或从 0 起的序号；键名里有 . 时用方括号，如 env[spring.profiles.active]"`
	Value any    `json:"value,omitempty" jsonschema:"set、append 的值：字符串、数字、true/false、对象或数组（delete 不用填）"`
}

type configCheckIn struct {
	Project string   `json:"project,omitempty" jsonschema:"项目名或 ferry.yaml 的路径；不填 = 当前目录的项目，或只有一个项目时用它"`
	Scope   string   `json:"scope,omitempty" jsonschema:"填 yaml（默认，ferry.yaml）或 local（本机设置）"`
	Content string   `json:"content,omitempty" jsonschema:"要检查的 ferry.yaml 完整内容（自己改好文件内容、还没写入时用）；和 edits 都不填 = 检查现在的文件"`
	Edits   []editIn `json:"edits,omitempty" jsonschema:"试改：按 config_edit 的写法给出修改，返回改后的差异和校验结果，不写入"`
}

type configEditIn struct {
	Project string   `json:"project,omitempty" jsonschema:"项目名或 ferry.yaml 的路径；不填 = 当前目录的项目，或只有一个项目时用它"`
	Scope   string   `json:"scope,omitempty" jsonschema:"填 yaml（默认，改 ferry.yaml）或 local（改这台电脑的本机设置）"`
	Edits   []editIn `json:"edits" jsonschema:"要做的修改（按顺序做，最多 50 处）"`
	Reason  string   `json:"reason" jsonschema:"为什么要改（显示在 Ferry 的确认框里）"`
}

func toEdits(in []editIn) ([]config.Edit, error) {
	out := make([]config.Edit, len(in))
	for i, e := range in {
		out[i] = config.Edit{Op: strings.TrimSpace(e.Op), Path: e.Path}
		if e.Op != "delete" {
			b, err := json.Marshal(e.Value)
			if err != nil {
				return nil, fmt.Errorf("第 %d 处的 value 不对：%v", i+1, err)
			}
			out[i].Value = b
		}
	}
	return out, nil
}

// configProject 找项目的 ferry.yaml（配置有错的也行：要改的可能就是那个错）。
func (s *server) configProject(project string) (projectInfo, error) {
	p, err := s.find(project)
	if err != nil {
		return p, err
	}
	if p.Name == connectionsName || (p.file != nil && p.file.Kind == config.KindConnections) {
		return p, errors.New("「新建连接」保存的服务器不是项目，没有项目配置：请在 Ferry 里改")
	}
	return p, nil
}

func (s *server) addConfigTools(m *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	addTool(s, m, &mcp.Tool{Name: "config_schema", Title: "配置项的说明",
		Description: "查 Ferry 项目配置（ferry.yaml）和本机设置有哪些配置项、每项是什么意思、起什么作用、怎么写：类型、可选值、默认值、是否必填、只对哪类应用有用、和别的项的关系。" +
			"不填参数看顶层和总说明（路径写法、内置变量、本机设置）；path 看某一项和它下一级的各项；query 按关键字搜。写、改 ferry.yaml 之前先查。",
		Annotations: readOnly},
		logged(s, "config_schema", func(ctx context.Context, in configSchemaIn) (any, error) {
			return engine.ConfigSchemaInfo(in.Path, in.Query)
		}))
	addTool(s, m, &mcp.Tool{Name: "config_get", Title: "现在的配置",
		Description: "看项目配置现在怎么写的：不填 path 是整个 ferry.yaml；path 是某一项（原文那几行，和合并、引用展开后的值）；scope 为 local 时看这台电脑的本机设置（私钥、登录方式、变量在这台电脑上的值）。" +
			"也会列出配置现在校验出的问题（problems）。",
		Annotations: readOnly},
		logged(s, "config_get", func(ctx context.Context, in configGetIn) (any, error) {
			p, err := s.configProject(in.Project)
			if err != nil {
				return nil, err
			}
			return engine.ConfigRead(p.Config, in.Path, in.Scope == "local")
		}))
	addTool(s, m, &mcp.Tool{Name: "config_check", Title: "检查配置",
		Description: "校验项目配置，不写入：不填 content、edits 时检查现在的文件；content 是自己改好的完整 ferry.yaml；edits 是试改（返回改后的差异、校验出的问题、改到的字段）。" +
			"直接改了 ferry.yaml 文件之后也用它检查一遍。",
		Annotations: readOnly},
		logged(s, "config_check", func(ctx context.Context, in configCheckIn) (any, error) {
			p, err := s.configProject(in.Project)
			if err != nil {
				return nil, err
			}
			if len(in.Edits) > 0 {
				edits, err := toEdits(in.Edits)
				if err != nil {
					return nil, err
				}
				plan, err := engine.PlanConfigEdit(p.Config, engine.ConfigEditInput{Scope: in.Scope, Edits: edits})
				if err != nil {
					return nil, err
				}
				out := engine.ConfigPlanResult(plan, false)
				out["ok"] = len(plan.Problems) == 0
				return out, nil
			}
			var verr error
			var f *config.File
			if in.Content != "" {
				f, verr = config.LoadContent([]byte(in.Content), p.Config)
			} else if in.Scope == "local" {
				if f, verr = config.Load(p.Config); verr == nil {
					var l *config.Local
					if l, verr = config.ReadLocal(f.Project, f.Path); verr == nil {
						verr = config.CheckLocal(f, l)
					}
				}
				f = nil // 本机设置：ferry.yaml 里的提示不在这里报
			} else {
				f, verr = config.Load(p.Config)
			}
			if verr != nil {
				return map[string]any{"ok": false, "problems": strings.Split(strings.TrimSpace(verr.Error()), "\n")}, nil
			}
			out := map[string]any{"ok": true}
			if f != nil && len(f.Warnings) > 0 {
				out["warnings"] = f.WarningLines()
				out["note"] = "warnings 里的能用、不挡部署，只是写了不起作用：删掉，或者换成这种应用用得到的写法"
			}
			return out, nil
		}))
	addTool(s, m, &mcp.Tool{Name: "config_edit", Title: "改配置",
		Description: "按路径增删改项目配置 ferry.yaml（scope 为 local 时改这台电脑的本机设置）：只改要改的那几行，注释、空行和写法保留；改完先校验，不通过不改、返回问题。" +
			"通过了交给 Ferry 桌面版，用户看过差异、同意后写入（最多等 5 分钟确认；需要用户在 Ferry 设置里允许编程软件请求改动）。" +
			"改之前先用 config_schema 查写法、config_get 看现在的；拿不准时先用 config_check 试改。改了的配置下次部署才生效。" +
			"生产服务器（production）、aiChanges: false 的服务器的配置，以及 production、aiChanges、deployers 这几项只能用户在 Ferry 里改。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}},
		loggedReq(s, "config_edit", s.configEdit))
}

func (s *server) configEdit(ctx context.Context, req *mcp.CallToolRequest, in configEditIn) (any, error) {
	st, _ := settings.Load()
	if !st.MCP.AllowChanges {
		return nil, errors.New("Ferry 没有允许编程软件改动：用户可以在 Ferry「设置 → 命令行与 MCP」里打开「允许编程软件请求部署、启停、回滚等改动」，或者自己在 Ferry 里改配置")
	}
	p, err := s.configProject(in.Project)
	if err != nil {
		return nil, err
	}
	edits, err := toEdits(in.Edits)
	if err != nil {
		return nil, err
	}
	ci := engine.ConfigEditInput{Scope: in.Scope, Edits: edits}
	plan, err := engine.PlanConfigEdit(p.Config, ci)
	if err != nil {
		return nil, err
	}
	if err := engine.ConfigPlanProblem(plan); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(ci)
	name := p.Project
	if name == "" {
		name = p.Name
	}
	o := s.submit(approve.Request{ID: newID(), Session: s.session, Client: clientName(req), Action: "config-edit",
		Project: name, Config: p.Config, Input: raw, Detail: engine.ConfigPlanDetail(plan), Danger: plan.Danger,
		Reason: strings.TrimSpace(in.Reason)})
	return o.wait(ctx, req, waitCap)
}

// prepareConfigEdit 桌面版收到请求后按 Input 重新算一遍（确认框里显示的就是要写的），记下原文的摘要。
func prepareConfigEdit(r *approve.Request) error {
	st, _ := settings.Load()
	if !st.MCP.AllowChanges {
		return errors.New("Ferry 没有允许编程软件改动")
	}
	if r.ClientConfirmed {
		return errors.New("改配置只能在 Ferry 里确认")
	}
	var in engine.ConfigEditInput
	if err := json.Unmarshal(r.Input, &in); err != nil {
		return fmt.Errorf("参数格式不对：%v", err)
	}
	plan, err := engine.PlanConfigEdit(r.Config, in)
	if err != nil {
		return err
	}
	if err := engine.ConfigPlanProblem(plan); err != nil {
		return err
	}
	in.Base = plan.Base
	r.Input, _ = json.Marshal(in)
	r.Detail, r.Danger = engine.ConfigPlanDetail(plan), plan.Danger
	return nil
}
