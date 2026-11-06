package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
)

// 项目配置（ferry.yaml）和这台电脑的本机设置：查字段的意思、看现在怎么写的、改。
// Ferry 的 AI 助手（config_schema、config_get、config_edit）和编程软件（MCP 里同名的工具，外加 config_check）用同一套。
// 改的时候按路径改（只动要改的那几行，注释、格式保留），改完校验；给用户看差异，同意后写入。

// ConfigEditsDesc edits 参数的说明。
const ConfigEditsDesc = "要做的修改，按顺序做：每项 {op, path, value}。op 为 set（设置，没有的上级自动建）、delete（删掉）、append（在列表末尾加一项）；" +
	"value 是 JSON（字符串、数字、true/false、对象、数组；delete 不用）。" + config.EditPathHelp

// ConfigToolsHelp 给 AI 的总说明（MCP 的说明、助手的系统提示里用）。
const ConfigToolsHelp = "项目配置（ferry.yaml）：config_schema 查有哪些配置、每项是什么意思、怎么写（按路径看，或按关键字搜）；" +
	"config_get 看现在怎么写的；config_edit 按路径增删改（用户确认差异后写入，注释和格式保留，改完自动校验）。" +
	"本机设置（只属于这台电脑的私钥、登录方式、源码目录等变量的值，不写进 ferry.yaml）也用它们，scope 填 local。"

// ConfigEditInput config_edit 的参数。
type ConfigEditInput struct {
	Scope string        `json:"scope,omitempty"` // yaml（默认）| local
	Edits []config.Edit `json:"edits"`
	// Base 用户确认时的原文摘要：执行时文件变了就不改（桌面版执行经编程软件请求的修改时用）
	Base string `json:"base,omitempty"`
}

// Local 改的是本机设置。
func (in ConfigEditInput) Local() (bool, error) {
	switch in.Scope {
	case "", "yaml":
		return false, nil
	case "local":
		return true, nil
	}
	return false, fmt.Errorf("scope 只能是 yaml 或 local，不是 %q", in.Scope)
}

// PlanConfigEdit 算好修改（不写）。
func PlanConfigEdit(configPath string, in ConfigEditInput) (*config.EditPlan, error) {
	local, err := in.Local()
	if err != nil {
		return nil, err
	}
	if len(in.Edits) > 50 {
		return nil, errors.New("一次最多改 50 处")
	}
	if local {
		return config.PlanLocalEdit(configPath, in.Edits)
	}
	return config.PlanEdit(configPath, in.Edits)
}

// ConfigPlanProblem 改了不能写的原因（校验不通过、没有变化）；能写时 nil。
func ConfigPlanProblem(p *config.EditPlan) error {
	if len(p.Problems) > 0 {
		return errors.New("改后的配置校验不通过，没有改（按下面的问题调整后再试）：\n" + strings.Join(p.Problems, "\n"))
	}
	if p.Unchanged() {
		return errors.New("改完和现在一样，不用改")
	}
	return nil
}

// ConfigPlanDetail 确认框里显示的：第一行是说明，后面是改到的字段和差异。
func ConfigPlanDetail(p *config.EditPlan) []string {
	what := "ferry.yaml"
	if p.Local {
		what = "这台电脑的本机设置"
	}
	head := what + "的差异（- 删掉的行，+ 加上的行）："
	if changed := groupPaths(p.Changed); len(changed) > 0 {
		if len(changed) > 12 {
			changed = append(changed[:12:12], fmt.Sprintf("……共 %d 处", len(changed)))
		}
		head = "改到的：" + strings.Join(changed, "、") + "。" + head
	}
	d := ai.LineDiff(p.Old, p.New)
	if d == "" {
		d = "（太大，不显示差异）改后：\n" + ai.Clip(p.New, 8000)
	}
	return []string{head, d}
}

// groupPaths 同一个上级下改了好几项时只写上级：apps.api.service.restart（3 项）。
func groupPaths(paths []string) []string {
	parent := func(p string) string {
		segs, err := config.ParseEditPath(p)
		if err != nil || len(segs) < 2 {
			return ""
		}
		return p[:len(p)-len(segs[len(segs)-1])-1]
	}
	count := map[string]int{}
	for _, p := range paths {
		count[parent(p)]++
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if up := parent(p); up != "" && count[up] > 1 && !strings.HasSuffix(p, "]") {
			if !seen[up] {
				seen[up] = true
				out = append(out, fmt.Sprintf("%s（%d 项）", up, count[up]))
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

// ConfigPlanResult 改完（或试改）交回给 AI 的。
func ConfigPlanResult(p *config.EditPlan, written bool) map[string]any {
	out := map[string]any{"file": p.File, "changed": p.Changed}
	if written {
		out["written"] = true
	}
	if p.Danger != "" {
		out["danger"] = p.Danger
	}
	if len(p.Problems) > 0 {
		out["problems"] = p.Problems
	}
	if len(p.Warnings) > 0 {
		out["warnings"] = p.Warnings
	}
	if d := ai.LineDiff(p.Old, p.New); d != "" {
		out["diff"] = d
	}
	return out
}

// ---------------------------------------------------------------- 查说明

// ConfigSchemaInfo 字段的说明：path 是这一项的路径（不填 = 顶层和总说明），query 是关键字（按关键字搜）。
func ConfigSchemaInfo(path, query string) (any, error) {
	if q := strings.TrimSpace(query); q != "" {
		hits := config.SchemaSearch(q, 20)
		if len(hits) == 0 {
			return nil, fmt.Errorf("没有找到和「%s」有关的配置：换个词，或者不填 query、按 path 一级一级看", q)
		}
		return map[string]any{"query": q, "hits": hits, "note": "用 path 看某一项的详细说明（[] 是列表里的任意一项，* 是任意名字）"}, nil
	}
	path = strings.TrimSpace(path)
	if path == "" || path == "local" {
		top := schemaView(config.Schema(), "")
		top["howTo"] = "按路径看下一级，如 path=apps.[].service、targets.*；路径里列表项写 [] 或名字，映射的键写 * 或名字。" +
			"kind：str 一行文字、num 数字、bool true/false、dur 时长（90s、2m、1h）、list 一组值、map 名字 → 文字、obj 固定字段的对象、objMap 名字 → 对象、objList 对象的列表。" +
			"字符串里可以用 ${变量}：vars（项目、服务器、应用的）和内置变量（builtinVars）。改之前先用 config_get 看现在怎么写的。"
		top["builtinVars"] = config.BuiltinVars()
		top["local"] = config.LocalSchemaDoc
		if path == "local" {
			return map[string]any{"local": config.LocalSchemaDoc}, nil
		}
		return top, nil
	}
	segs, err := config.ParseEditPath(path)
	if err != nil {
		return nil, err
	}
	n, norm, err := config.SchemaLookup(segs)
	if err != nil {
		return nil, err
	}
	return schemaView(n, norm), nil
}

// schemaView 一项的说明，加上下一级各项的简要说明。
func schemaView(n *config.SchemaNode, norm string) map[string]any {
	out := map[string]any{}
	if norm != "" {
		out["path"] = norm
	}
	put := func(k string, v any) {
		switch x := v.(type) {
		case string:
			if x == "" {
				return
			}
		case []string:
			if len(x) == 0 {
				return
			}
		case bool:
			if !x {
				return
			}
		case nil:
			return
		}
		out[k] = v
	}
	put("label", n.Label)
	put("doc", n.Doc)
	put("kind", n.Kind)
	put("type", n.Type)
	put("values", n.Values)
	if n.Open {
		put("otherValues", "除了 values 也可以写别的")
	}
	put("default", n.Default)
	put("required", n.RequiredText())
	put("appTypes", n.AppTypes)
	if n.Needs != "" {
		put("needs", "同一个对象里写了 "+n.Needs+" 才有用")
	}
	if n.Unless != "" {
		put("unless", "同一个对象里写了 "+n.Unless+" 时不用写")
	}
	if len(n.OneOf) > 0 {
		var g []string
		for _, x := range n.OneOf {
			g = append(g, strings.Join(x, " / "))
		}
		s := "这几项只能写一个：" + strings.Join(g, "；")
		if n.OneOfReq {
			s += "（必须写一个）"
		}
		put("oneOf", s)
	}
	if n.Short != "" {
		if n.Short == "@build" {
			put("short", "也可以只写一个名字：引用 builds 里的构建")
		} else {
			put("short", "也可以只写一个值：等于只写了 "+n.Short)
		}
	}
	if n.Numbers {
		put("items", "列表里是数字")
	}
	if n.Single {
		put("single", "只有一项时也可以不写成列表")
	}
	if n.Min != nil || n.Max != nil {
		r := ""
		if n.Min != nil {
			r += fmt.Sprintf("最小 %g", *n.Min)
		}
		if n.Max != nil {
			if r != "" {
				r += "，"
			}
			r += fmt.Sprintf("最大 %g", *n.Max)
		}
		if n.Kind == "dur" {
			r += "（秒）"
		}
		put("range", r+n.Unit)
	}
	child := n
	switch {
	case n.Kind == "objList" && n.Item != nil:
		child = n.Item
		out["item"] = "列表里的每一项：" + n.Item.Doc + "（路径里写 [] 或这一项的 name）"
	case n.Kind == "objMap" && n.Map != nil:
		child = n.Map
		out["each"] = "键是名字（自己起），值是：" + n.Map.Doc + "（路径里写 * 或这个名字）"
	case n.Kind == "map":
		out["each"] = "键是名字（自己起），值是文字（可以用 ${变量}）"
	}
	if child != n && len(child.OneOf) > 0 && out["oneOf"] == nil {
		var g []string
		for _, x := range child.OneOf {
			g = append(g, strings.Join(x, " / "))
		}
		out["oneOf"] = "每一项里这几项只能写一个：" + strings.Join(g, "；")
	}
	if len(child.Fields) > 0 {
		var fields []map[string]any
		for _, c := range child.Fields {
			f := map[string]any{"name": c.Name, "label": c.Label, "type": c.Type, "doc": c.Doc}
			if c.Default != "" {
				f["default"] = c.Default
			}
			if r := c.RequiredText(); r != "" {
				f["required"] = r
			}
			if len(c.AppTypes) > 0 {
				f["appTypes"] = c.AppTypes
			}
			if len(c.Values) > 0 && c.Kind != "bool" {
				f["values"] = c.Values
			}
			if len(c.Fields) > 0 || c.Item != nil || c.Map != nil {
				f["more"] = true // 下面还有，按路径再看
			}
			fields = append(fields, f)
		}
		out["fields"] = fields
	}
	return out
}

// ---------------------------------------------------------------- 看现在怎么写的

// ConfigRead 现在的配置：path 不填 = 整个文件；local 时看这台电脑的本机设置。
func ConfigRead(configPath, path string, local bool) (any, error) {
	src, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"config": configPath}
	if local {
		f, err := config.Load(configPath)
		if err != nil {
			return nil, fmt.Errorf("ferry.yaml 有问题，先把它改好：%w", err)
		}
		l, err := config.ReadLocal(f.Project, f.Path)
		if err != nil {
			return nil, err
		}
		out["file"] = config.LocalFile(f.Project, f.Path)
		out["local"] = l
		vars := sortedNames(f.Vars)
		tv := map[string][]string{}
		for _, n := range f.TargetNames() {
			names := map[string]bool{}
			for _, k := range vars {
				names[k] = true
			}
			for k := range f.Targets[n].Vars {
				names[k] = true
			}
			tv[n] = sortedKeys(names)
		}
		out["canSet"] = map[string]any{"vars": vars, "targets": tv}
		out["machine"] = f.MachineProblems()
		out["note"] = config.LocalSchemaDoc
		return out, nil
	}
	if strings.TrimSpace(path) == "" {
		out["yaml"] = ai.Clip(strings.ReplaceAll(string(src), "\r\n", "\n"), 60000)
	} else {
		segs, err := config.ParseEditPath(path)
		if err != nil {
			return nil, err
		}
		out["path"] = path
		if s, ok := config.Snippet(src, segs); ok {
			out["yaml"] = s
		}
		v, ok := config.ValueAt(src, segs)
		if !ok {
			out["note"] = "配置里没有写这一项（用的是默认值）；说明见 config_schema"
		} else {
			out["value"] = v
			if out["yaml"] == nil {
				out["note"] = "这一项是从 <<: *锚点 合并来的、或者用 * 引用了别处，原文里不在这里"
			}
		}
	}
	if f, err := config.LoadContent(src, configPath); err != nil {
		out["problems"] = strings.Split(strings.TrimSpace(err.Error()), "\n")
	} else if len(f.Warnings) > 0 {
		out["warnings"] = f.WarningLines()
	}
	return out, nil
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- 助手里的工具

// aiConfigTools 助手的 config_schema、config_get，允许改动时还有 config_edit。改的是这个项目的 ferry.yaml（e.File.Path）。
func (e *Engine) aiConfigTools(t *config.Target, change bool) []ai.Tool {
	if t.Plain || e.File == nil || e.File.Path == "" {
		return nil
	}
	path := e.File.Path
	tools := []ai.Tool{{
		Name: "config_schema",
		Description: "查项目配置（ferry.yaml）和本机设置有哪些项、每项是什么意思、起什么作用、怎么写（类型、可选值、默认值、必填、只对哪类应用有用）。" +
			"不填参数看顶层和总说明；path 看某一项和它下一级（如 apps.[].service、targets.*.vars）；query 按关键字搜（如 健康检查、jvm、重启）。",
		Schema: schema(map[string]any{
			"path":  str("这一项的路径（列表项写 [] 或名字，映射的键写 * 或名字）；填 local 看本机设置能改的"),
			"query": str("关键字（几个词用空格分开）"),
		}),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct{ Path, Query string }
			json.Unmarshal(in, &a)
			v, err := ConfigSchemaInfo(a.Path, a.Query)
			if err != nil {
				return "", err
			}
			return toJSON(v), nil
		},
	}, {
		Name:        "config_get",
		Description: "看这个项目的配置现在怎么写的：不填 path 是整个 ferry.yaml；path 是某一项（原文那几行和展开后的值）；scope 为 local 时看这台电脑的本机设置。也会列出配置现在校验出的问题。",
		Schema: schema(map[string]any{
			"path":  str("这一项的路径，如 apps.api.service；不填 = 整个文件"),
			"scope": map[string]any{"type": "string", "enum": []string{"yaml", "local"}, "description": "yaml（默认）= ferry.yaml；local = 这台电脑的本机设置"},
		}),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct{ Path, Scope string }
			json.Unmarshal(in, &a)
			v, err := ConfigRead(path, a.Path, a.Scope == "local")
			if err != nil {
				return "", err
			}
			return ai.Clip(toJSON(v), ai.ResultLimit*4), nil
		},
	}}
	if !change {
		return tools
	}
	// 用户确认时算好的修改：执行时按它写（文件在确认后被改过就不写）
	var mu sync.Mutex
	plans := map[string]*config.EditPlan{}
	plan := func(in json.RawMessage) (*config.EditPlan, error) {
		var a ConfigEditInput
		if err := json.Unmarshal(in, &a); err != nil {
			return nil, fmt.Errorf("参数格式不对：%v", err)
		}
		return PlanConfigEdit(path, a)
	}
	tools = append(tools, ai.Tool{
		Name: "config_edit",
		Description: "按路径改这个项目的 ferry.yaml（或 scope 为 local 时改这台电脑的本机设置）：只改要改的那几行，注释和格式保留。" +
			"改完先校验，校验不通过不改、返回问题；通过了给用户看差异，同意后写入。改之前先用 config_schema 查写法、config_get 看现在的。" +
			"改了的配置下次部署才生效。生产服务器（production）、aiChanges: false 的服务器的配置，以及 production、aiChanges、deployers 这几项只能用户在 Ferry 里改。",
		Schema: schema(map[string]any{
			"scope": map[string]any{"type": "string", "enum": []string{"yaml", "local"}, "description": "yaml（默认）= ferry.yaml；local = 这台电脑的本机设置"},
			"edits": map[string]any{"type": "array", "description": ConfigEditsDesc, "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"op":    map[string]any{"type": "string", "enum": []string{"set", "delete", "append"}},
					"path":  str("路径"),
					"value": map[string]any{"description": "值（JSON）"},
				},
				"required": []string{"op", "path"},
			}},
		}, "edits"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			p, err := plan(in)
			if err != nil || ConfigPlanProblem(p) != nil {
				return nil // 执行时把原因告诉模型，不用打扰用户
			}
			mu.Lock()
			plans[string(in)] = p
			mu.Unlock()
			title := "改项目配置 ferry.yaml"
			if p.Local {
				title = "改这台电脑的本机设置"
			}
			d := ConfigPlanDetail(p)
			return &ai.Approval{Kind: title, Title: title, Detail: d[0] + "\n" + d[1], Danger: p.Danger}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			mu.Lock()
			p := plans[string(in)]
			delete(plans, string(in))
			mu.Unlock()
			if p == nil {
				var err error
				if p, err = plan(in); err != nil {
					return "", err
				}
			}
			if err := ConfigPlanProblem(p); err != nil {
				return "", err
			}
			if err := p.Write(); err != nil {
				return "", err
			}
			out := ConfigPlanResult(p, true)
			out["note"] = "已写入。改了的配置下次部署时生效"
			return toJSON(out), nil
		},
	})
	return tools
}
