package config

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Warning 配置能用、但有写了不起作用的地方（如 java 应用写了 static.port）：不挡部署，检查、部署时提示。
type Warning struct {
	App  string `json:"app,omitempty"` // 哪个应用的；空 = 不是某个应用的
	Path string `json:"path"`          // 如 apps.api.static.port
	Text string `json:"text"`
}

func (w Warning) String() string { return w.Path + "：" + w.Text }

// WarningLines 警告写成一行一条。
func (f *File) WarningLines() []string {
	out := make([]string, len(f.Warnings))
	for i, w := range f.Warnings {
		out[i] = w.String()
	}
	return out
}

// AppWarnings 这几个应用的警告，加上不属于某个应用的。
func (f *File) AppWarnings(apps []*App) []Warning {
	var out []Warning
	for _, w := range f.Warnings {
		if w.App == "" || slices.ContainsFunc(apps, func(a *App) bool { return a.Name == w.App }) {
			out = append(out, w)
		}
	}
	return out
}

// unusedFields 应用里写了、但这种应用用不到的字段（说明里的 appTypes，和桌面版表单的「用不到这项」同一个依据）。
// 按原文找（只看写了的），一项整个用不到时只报这一项，不再报它下面的。
func unusedFields(f *File, data []byte) []Warning {
	var doc yaml.Node
	if yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	apps := pairValue(doc.Content[0], "apps")
	if apps == nil || apps.Kind != yaml.SequenceNode {
		return nil
	}
	appSchema := Schema().Field("apps").Child("[]")
	var out []Warning
	for i, n := range apps.Content {
		if i >= len(f.Apps) || f.Apps[i] == nil {
			break
		}
		a := f.Apps[i]
		var walk func(n *yaml.Node, s *SchemaNode, path string)
		walk = func(n *yaml.Node, s *SchemaNode, path string) {
			for _, kv := range mapPairs(n) {
				c := s.Field(kv[0].Value)
				if c == nil {
					continue
				}
				p := path + "." + kv[0].Value
				if len(c.AppTypes) > 0 && !slices.Contains(c.AppTypes, a.Type) {
					out = append(out, Warning{App: a.Name, Path: p,
						Text: fmt.Sprintf("%s 应用用不到这一项（只用于 %s），写了不起作用，可以删掉", a.Type, strings.Join(c.AppTypes, "、"))})
					continue
				}
				if c.Kind == "obj" && len(c.Fields) > 0 {
					walk(kv[1], c, p)
				}
			}
		}
		walk(n, appSchema, "apps."+a.Name)
	}
	return out
}

// mapPairs 映射里的键和值：跟着 *引用 走，<<: 合并进来的也算（自己写的在前，同名的以自己写的为准）。
func mapPairs(n *yaml.Node) [][2]*yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var own, merged [][2]*yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value == "<<" {
			for v != nil && v.Kind == yaml.AliasNode {
				v = v.Alias
			}
			srcs := []*yaml.Node{v}
			if v != nil && v.Kind == yaml.SequenceNode {
				srcs = v.Content
			}
			for _, m := range srcs {
				merged = append(merged, mapPairs(m)...)
			}
			continue
		}
		seen[k.Value] = true
		own = append(own, [2]*yaml.Node{k, v})
	}
	for _, kv := range merged {
		if !seen[kv[0].Value] {
			seen[kv[0].Value] = true
			own = append(own, kv)
		}
	}
	return own
}

// pairValue 映射里这个键的值（跟着引用、合并走）。
func pairValue(n *yaml.Node, key string) *yaml.Node {
	for _, kv := range mapPairs(n) {
		if kv[0].Value == key {
			v := kv[1]
			for v != nil && v.Kind == yaml.AliasNode {
				v = v.Alias
			}
			return v
		}
	}
	return nil
}
