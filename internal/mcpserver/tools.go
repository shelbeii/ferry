package mcpserver

import (
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolInfo 一个 MCP 工具（桌面版「MCP 工具列表」看的）：名称、作用、参数。
type ToolInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Change 改动服务器或本机（新建项目），每次要用户在桌面版确认
	Change bool `json:"change"`
	// Server 要连服务器（否则只看本机的项目、构建日志、改动的进展）
	Server bool        `json:"server"`
	Params []ParamInfo `json:"params"`
}

// ParamInfo 工具的一个参数；对象里的字段展开成 spec.apps[].name 这样的名字，Depth 是第几层。
type ParamInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // string、integer、boolean、object、map、string[]、object[]……
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	Depth       int    `json:"depth"`
}

// addTool 注册工具，同时记下它的说明和参数（参数按结构体里字段的顺序）。
func addTool[In, Out any](s *server, m *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(m, t, h)
	info := ToolInfo{Name: t.Name, Title: t.Title, Description: t.Description, Params: []ParamInfo{}}
	if a := t.Annotations; a != nil {
		info.Change = !a.ReadOnlyHint
		info.Server = a.OpenWorldHint != nil && *a.OpenWorldHint
	}
	if in, err := jsonschema.For[In](nil); err == nil {
		info.Params = params(in, "", 0, info.Params)
	}
	s.tools = append(s.tools, info)
}

// params 把对象的字段按顺序展开（最多四层）。
func params(sc *jsonschema.Schema, prefix string, depth int, out []ParamInfo) []ParamInfo {
	names := sc.PropertyOrder
	for name := range sc.Properties {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		p := sc.Properties[name]
		if p == nil {
			continue
		}
		full := prefix + name
		out = append(out, ParamInfo{Name: full, Type: schemaType(p), Required: slices.Contains(sc.Required, name), Description: p.Description, Depth: depth})
		if depth >= 3 {
			continue
		}
		switch {
		case len(p.Properties) > 0:
			out = params(p, full+".", depth+1, out)
		case p.Items != nil && len(p.Items.Properties) > 0:
			out = params(p.Items, full+"[].", depth+1, out)
		}
	}
	return out
}

func schemaType(sc *jsonschema.Schema) string {
	t := sc.Type
	for _, x := range sc.Types {
		if x != "null" {
			t = x
		}
	}
	switch t {
	case "array":
		if sc.Items != nil {
			return schemaType(sc.Items) + "[]"
		}
	case "object":
		if len(sc.Properties) == 0 && sc.AdditionalProperties != nil {
			return "map"
		}
	}
	return t
}

// Tools 编程软件能用的全部工具，按注册的顺序。
func Tools(version string) []ToolInfo {
	s, _ := newServer(Options{Version: version})
	s.pool.Close()
	return s.tools
}
