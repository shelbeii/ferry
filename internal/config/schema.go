package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ferry.yaml 的结构说明：每个字段叫什么、什么意思、怎么写。和桌面版表单、补全用的是同一份
// （desktop/src/editor/schema.ts 导出成 schema.json，desktop 的 schemaExport.test.ts 检查两边一致，
// TestSchemaFields 检查和 fields.txt 一致）。Ferry 的 AI 助手、编程软件（MCP 的 config_schema）用它。

//go:embed schema.json
var schemaJSON []byte

// SchemaNode 一个字段的说明。
type SchemaNode struct {
	Name  string `json:"name,omitempty"`
	Label string `json:"label,omitempty"`
	Doc   string `json:"doc"`
	// Kind 怎么写：str 一行文字、num 数字、bool 是 / 否、dur 时长（90s、2m）、list 一组值、map 名字 → 文字、
	// obj 固定字段的对象、objMap 名字 → 对象、objList 对象的列表
	Kind      string          `json:"kind,omitempty"`
	Type      string          `json:"type,omitempty"`
	Fields    []*SchemaNode   `json:"fields,omitempty"`
	Map       *SchemaNode     `json:"map,omitempty"`
	Item      *SchemaNode     `json:"item,omitempty"`
	Values    []string        `json:"values,omitempty"`
	Open      bool            `json:"open,omitempty"`
	Default   string          `json:"default,omitempty"`
	Required  json.RawMessage `json:"required,omitempty"` // true，或只对这些应用类型必填的数组
	AppTypes  []string        `json:"appTypes,omitempty"`
	Needs     string          `json:"needs,omitempty"`
	Unless    string          `json:"unless,omitempty"`
	OneOf     [][]string      `json:"oneOf,omitempty"`
	OneOfReq  bool            `json:"oneOfRequired,omitempty"`
	Short     string          `json:"short,omitempty"`
	Numbers   bool            `json:"numbers,omitempty"`
	Single    bool            `json:"single,omitempty"`
	Min       *float64        `json:"min,omitempty"`
	Max       *float64        `json:"max,omitempty"`
	Unit      string          `json:"unit,omitempty"`
	fieldsMap map[string]*SchemaNode
}

type schemaFile struct {
	Schema      *SchemaNode       `json:"schema"`
	BuiltinVars map[string]string `json:"builtinVars"`
}

var loadSchema = sync.OnceValue(func() *schemaFile {
	var f schemaFile
	if err := json.Unmarshal(schemaJSON, &f); err != nil || f.Schema == nil {
		panic(fmt.Sprintf("schema.json 有问题：%v", err))
	}
	var index func(n *SchemaNode)
	index = func(n *SchemaNode) {
		if n == nil {
			return
		}
		n.fieldsMap = map[string]*SchemaNode{}
		for _, c := range n.Fields {
			n.fieldsMap[c.Name] = c
			index(c)
		}
		index(n.Map)
		index(n.Item)
	}
	index(f.Schema)
	return &f
})

// Schema 整个 ferry.yaml 的说明（顶层）。
func Schema() *SchemaNode { return loadSchema().Schema }

// BuiltinVars 内置变量（${project}、${running}……）和说明。
func BuiltinVars() map[string]string { return loadSchema().BuiltinVars }

// Field 固定字段里的这一个；没有时 nil。
func (n *SchemaNode) Field(name string) *SchemaNode {
	if n == nil {
		return nil
	}
	return n.fieldsMap[name]
}

// Child 路径的下一段对应的说明：固定字段、映射的任意键、列表的任意一项（名字、序号或 []）。
func (n *SchemaNode) Child(seg string) *SchemaNode {
	if n == nil {
		return nil
	}
	if c := n.fieldsMap[seg]; c != nil {
		return c
	}
	if n.Item != nil {
		return n.Item
	}
	return n.Map
}

// RequiredText 必填的说明：""、"必填"、"java、binary 应用必填"。
func (n *SchemaNode) RequiredText() string {
	r := strings.TrimSpace(string(n.Required))
	switch {
	case r == "" || r == "false":
		return ""
	case r == "true":
		return "必填"
	}
	var types []string
	if json.Unmarshal(n.Required, &types) == nil && len(types) > 0 {
		return strings.Join(types, "、") + " 应用必填"
	}
	return ""
}

// SchemaLookup 按路径（ParseEditPath 的写法，列表项可以写名字、序号或 []，映射的键写实际的名字或 *）找说明。
// 返回说明和规范的路径（列表项写 []，映射的键写 *），找不到时返回错误，说明哪一段不对、那一级有哪些字段。
func SchemaLookup(path []string) (*SchemaNode, string, error) {
	n := Schema()
	var norm []string
	for i, seg := range path {
		var next *SchemaNode
		switch {
		case n.fieldsMap[seg] != nil:
			next = n.fieldsMap[seg]
		case n.Item != nil:
			next, seg = n.Item, "[]"
		case n.Map != nil:
			next, seg = n.Map, "*"
		}
		if next == nil {
			where := strings.Join(path[:i], ".")
			if where == "" {
				where = "顶层"
			}
			return nil, "", fmt.Errorf("%s 没有字段 %s；有：%s", where, seg, strings.Join(n.FieldNames(), "、"))
		}
		n = next
		norm = append(norm, seg)
	}
	return n, strings.Join(norm, "."), nil
}

// NormalizePath 规范的路径（列表项写 []，映射的键写 *）；有不认识的段时原样保留那一段之后的。
func NormalizePath(path []string) string {
	n := Schema()
	out := make([]string, 0, len(path))
	for i, seg := range path {
		switch {
		case n != nil && n.fieldsMap[seg] != nil:
			n = n.fieldsMap[seg]
		case n != nil && n.Item != nil:
			n, seg = n.Item, "[]"
		case n != nil && n.Map != nil:
			n, seg = n.Map, "*"
		default:
			return strings.Join(append(out, path[i:]...), ".")
		}
		out = append(out, seg)
	}
	return strings.Join(out, ".")
}

// FieldNames 固定字段的名字（按表单里的顺序）。
func (n *SchemaNode) FieldNames() []string {
	var out []string
	for _, c := range n.Fields {
		out = append(out, c.Name)
	}
	return out
}

// SchemaHit 搜索结果的一项。
type SchemaHit struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Doc   string `json:"doc"`
}

// SchemaSearch 按关键字找字段：字段名、显示的名字、说明里有这些字的（几个词用空格分开，都要有）。最多 limit 项。
func SchemaSearch(query string, limit int) []SchemaHit {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil
	}
	type scored struct {
		SchemaHit
		score int
	}
	var hits []scored
	var walk func(n *SchemaNode, at string)
	walk = func(n *SchemaNode, at string) {
		for _, c := range n.Fields {
			p := c.Name
			if at != "" {
				p = at + "." + c.Name
			}
			name, label, doc := strings.ToLower(c.Name), strings.ToLower(c.Label), strings.ToLower(c.Doc+" "+strings.Join(c.Values, " "))
			score, all := 0, true
			for _, w := range words {
				switch {
				case name == w:
					score += 6
				case strings.Contains(name, w) || strings.Contains(label, w):
					score += 3
				case strings.Contains(doc, w):
					score++
				default:
					all = false
				}
			}
			if all {
				hits = append(hits, scored{SchemaHit{Path: p, Label: c.Label, Doc: c.Doc}, score*100 - strings.Count(p, ".")})
			}
			walk(c, p)
			if c.Map != nil && c.Kind == "objMap" {
				walk(c.Map, p+".*")
			}
			if c.Item != nil {
				walk(c.Item, p+".[]")
			}
		}
	}
	walk(Schema(), "")
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]SchemaHit, len(hits))
	for i, h := range hits {
		out[i] = h.SchemaHit
	}
	return out
}

// SchemaFieldPaths 所有字段路径（映射的任意键写 *，列表元素写 []），和 fields.txt 对照用。
func SchemaFieldPaths() []string {
	var out []string
	var walk func(n *SchemaNode, at string)
	walk = func(n *SchemaNode, at string) {
		for _, c := range n.Fields {
			p := c.Name
			if at != "" {
				p = at + "." + c.Name
			}
			out = append(out, p)
			walk(c, p)
		}
		if n.Kind == "objMap" && n.Map != nil {
			walk(n.Map, at+".*")
		}
		if n.Kind == "objList" && n.Item != nil {
			walk(n.Item, at+".[]")
		}
	}
	walk(Schema(), "")
	sort.Strings(out)
	return out
}

// LocalSchemaDoc 本机设置（只属于这台电脑、不写进 ferry.yaml 的）能改的项。
const LocalSchemaDoc = `本机设置：只属于这台电脑，存在 Ferry 的数据目录里，不写进 ferry.yaml（别的电脑、团队里的人看不到）。本机设置优先于 ferry.yaml。
ferry.yaml 写大家共用的配置；这台电脑上不一样的（私钥放在哪、源码目录、各台服务器上 JDK 的位置……）写在这里。能改的：
- vars.<变量名>：项目变量在这台电脑上的值，所有服务器共用（如源码目录）。只能改 ferry.yaml 的 vars 里已有的变量
- targets.<服务器>.auth：这台电脑上登录这台服务器的方式，key（私钥或 ssh-agent）或 password（密码存在系统凭据库，由用户在 Ferry 里填，这里不能写密码）
- targets.<服务器>.identityFile：私钥文件（这台电脑上的路径，相对 ferry.yaml 所在目录，支持 ~）；auth 为 password 时不要写
- targets.<服务器>.vars.<变量名>：变量只在这台服务器上的值（JDK、脚本的位置……）。只能改 ferry.yaml 里已有的变量（项目的 vars 或这台服务器的 vars）
值都是字符串；delete 去掉一项后按 ferry.yaml。`
