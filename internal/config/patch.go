package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// 按路径改 ferry.yaml（Ferry 的 AI 助手、编程软件经 MCP 用）：只改要改的那几行，别的行（注释、空行、写法）原样保留。
// 做法：yaml.v3 解析出各项在第几行、第几列，按缩进找出一项占了哪几行，换成新写的；
// 写成一行的 {…}、[…] 整个重写。改完重新解析，和「对解析出来的数据做同样的修改」比一遍，
// 对不上（这个文件的写法太特别）就不改、报错，不会悄悄改坏。

// Edit 一处修改。
type Edit struct {
	// Op set：设置值（没有的上级自动建）；delete：删掉；append：在列表末尾加一项（没有这个列表时新建）
	Op   string `json:"op"`
	Path string `json:"path"`
	// Value set、append 的值（JSON：字符串、数字、true/false、对象、数组）
	Value json.RawMessage `json:"value,omitempty"`
}

// EditPathHelp 路径的写法（给 AI 看的）。
const EditPathHelp = "路径用 . 分开，如 apps.api.service.jvmArgs、targets.test.host、vars.port；" +
	"列表里的项写它的 name（应用名、备份名）或从 0 起的序号，如 apps.0、backups.mysql-daily、apps.api.hooks.beforeSwitch.0；" +
	"键名里有 . 时用方括号括起来，如 apps.api.service.env[spring.profiles.active]"

// ParseEditPath 把 a.b[c.d].0 拆成 [a b c.d 0]。
func ParseEditPath(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("路径是空的")
	}
	var out []string
	var cur strings.Builder
	have := false // cur 里有内容（或刚结束一个方括号）
	flush := func() error {
		if !have {
			return fmt.Errorf("路径 %q 里有空的一段", s)
		}
		out = append(out, cur.String())
		cur.Reset()
		have = false
		return nil
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '.':
			if err := flush(); err != nil {
				return nil, err
			}
		case '[':
			if have {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			j := strings.IndexByte(s[i+1:], ']')
			if j < 0 {
				return nil, fmt.Errorf("路径 %q 里的 [ 没有对应的 ]", s)
			}
			cur.WriteString(s[i+1 : i+1+j])
			have = true
			i += j + 1
			if i+1 < len(s) && s[i+1] != '.' && s[i+1] != '[' {
				return nil, fmt.Errorf("路径 %q 的 ] 后面要跟 . 或 [", s)
			}
			if cur.Len() == 0 { // []：列表里的任意一项（查说明用）
				cur.WriteString("[]")
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// ApplyEdits 按顺序做这些修改，返回改后的原文。src 的换行（\r\n）、末尾换行保持不变。
func ApplyEdits(src []byte, edits []Edit) ([]byte, error) {
	crlf := bytes.Contains(src, []byte("\r\n"))
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	for i, e := range edits {
		out, err := applyEdit(text, e)
		if err != nil {
			if len(edits) > 1 {
				return nil, fmt.Errorf("第 %d 处（%s %s）：%w", i+1, e.Op, e.Path, err)
			}
			return nil, err
		}
		text = out
	}
	if crlf {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	return []byte(text), nil
}

func applyEdit(text string, e Edit) (string, error) {
	path, err := ParseEditPath(e.Path)
	if err != nil {
		return "", err
	}
	var val *yaml.Node
	switch e.Op {
	case "set", "append":
		if len(bytes.TrimSpace(e.Value)) == 0 {
			return "", fmt.Errorf("%s 要给 value", e.Op)
		}
		if val, err = jsonToNode(e.Value); err != nil {
			return "", err
		}
		if val.Tag == "!!null" {
			return "", errors.New("value 不能是 null：要去掉一项用 delete")
		}
		// 对象的键按表单里的顺序写（name、type 在前……），和手写、表单写的一样
		if sn, _, err := SchemaLookup(path); err == nil {
			if e.Op == "append" {
				sn = sn.Item
			}
			orderBySchema(val, sn)
		}
	case "delete":
	default:
		return "", fmt.Errorf("op 只能是 set、delete、append，不是 %q", e.Op)
	}
	d, err := parseYDoc(text)
	if err != nil {
		return "", err
	}
	root := d.root
	if root == nil {
		root = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
	}
	if err := d.apply(path, e.Op, val); err != nil {
		return "", err
	}
	// 改完的原文解析出来，要和「在原来的节点上按字面做同样的修改」一模一样
	want, err := nodeApply(cloneNode(root), path, e.Op, val)
	if err != nil {
		return "", err
	}
	out := d.text()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		return "", fmt.Errorf("这个文件里 %s 附近的写法比较特别，改完不是正确的 YAML，没有改：请直接改文件", e.Path)
	}
	var got *yaml.Node
	if len(doc.Content) > 0 {
		got = doc.Content[0]
	}
	if !sameNode(want, got) {
		return "", fmt.Errorf("这个文件里 %s 附近的写法比较特别，改完核对不上，没有改：请直接改文件", e.Path)
	}
	return out, nil
}

// ---------------------------------------------------------------- 解析出来的数据上做同样的修改（核对用）

func decodeGeneric(text string) (any, error) {
	var v any
	if err := yaml.Unmarshal([]byte(text), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// canon 转成 JSON 再转回来：数字都是 float64，没写（nil）当作空对象。
func canon(v any) (any, bool) {
	if v == nil {
		return map[string]any{}, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil, false
	}
	return out, true
}

func sameData(a, b any) bool {
	x, ok1 := canon(a)
	y, ok2 := canon(b)
	return ok1 && ok2 && reflect.DeepEqual(x, y)
}

// matchItem 列表里按 name（不分大小写）、值或序号找一项；找不到返回 -1。
func matchItem(n int, seg string, name func(i int) (string, bool), scalar func(i int) (string, bool)) int {
	for i := 0; i < n; i++ {
		if s, ok := name(i); ok && strings.EqualFold(s, seg) {
			return i
		}
	}
	if k, err := strconv.Atoi(seg); err == nil && k >= 0 && k < n && strconv.Itoa(k) == seg {
		return k
	}
	for i := 0; i < n; i++ {
		if s, ok := scalar(i); ok && s == seg {
			return i
		}
	}
	return -1
}

func genericIndex(list []any, seg string) int {
	return matchItem(len(list), seg, func(i int) (string, bool) {
		if m, ok := list[i].(map[string]any); ok {
			if s, ok := m["name"].(string); ok {
				return s, true
			}
		}
		return "", false
	}, func(i int) (string, bool) {
		switch v := list[i].(type) {
		case string:
			return v, true
		case int, float64, bool:
			return fmt.Sprint(v), true
		}
		return "", false
	})
}

// genericApply 在解析出来的数据上做修改，返回改后的（不改 v 本身之外的语义：v 会被改动）。
func genericApply(v any, path []string, op string, val any) (any, error) {
	if len(path) == 0 {
		switch op {
		case "set":
			return val, nil
		case "append":
			switch x := v.(type) {
			case nil:
				return []any{val}, nil
			case []any:
				return append(x, val), nil
			case map[string]any:
				return nil, errors.New("这里是对象，不是列表：不能 append")
			default:
				return []any{x, val}, nil
			}
		}
		return nil, errors.New("不能删掉整个文件")
	}
	seg, rest := path[0], path[1:]
	switch x := v.(type) {
	case nil:
		if op == "delete" {
			return nil, fmt.Errorf("没有 %s", seg)
		}
		c, err := genericApply(nil, rest, op, val)
		if err != nil {
			return nil, err
		}
		return map[string]any{seg: c}, nil
	case map[string]any:
		child, ok := x[seg]
		if op == "delete" && len(rest) == 0 {
			if !ok {
				return nil, fmt.Errorf("没有 %s", seg)
			}
			delete(x, seg)
			return x, nil
		}
		if !ok && op == "delete" {
			return nil, fmt.Errorf("没有 %s", seg)
		}
		c, err := genericApply(child, rest, op, val)
		if err != nil {
			return nil, prefixErr(seg, err)
		}
		x[seg] = c
		return x, nil
	case []any:
		i := genericIndex(x, seg)
		if i < 0 {
			return nil, fmt.Errorf("列表里没有 %s（列表里的项写 name 或从 0 起的序号；要加一项用 append）", seg)
		}
		if op == "delete" && len(rest) == 0 {
			return append(x[:i:i], x[i+1:]...), nil
		}
		c, err := genericApply(x[i], rest, op, val)
		if err != nil {
			return nil, prefixErr(seg, err)
		}
		x[i] = c
		return x, nil
	default:
		return nil, fmt.Errorf("%s 的上一级是一个值（%v），不是对象或列表：要改就整个设置上一级", seg, x)
	}
}

func prefixErr(seg string, err error) error {
	if strings.HasPrefix(err.Error(), "没有 ") || strings.HasPrefix(err.Error(), "列表里没有") {
		return fmt.Errorf("%s 下%w", seg, err)
	}
	return err
}

// ---------------------------------------------------------------- JSON → YAML 节点（保持键的顺序）

func jsonToNode(raw json.RawMessage) (*yaml.Node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := readNode(dec)
	if err != nil {
		return nil, fmt.Errorf("value 不是正确的 JSON：%w", err)
	}
	if dec.More() {
		return nil, errors.New("value 不是正确的 JSON：后面还有多余的内容")
	}
	return n, nil
}

func readNode(dec *json.Decoder) (*yaml.Node, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := t.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				c, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, strNode(k), c)
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for dec.More() {
				c, err := readNode(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, c)
			}
			_, err := dec.Token()
			return n, err
		}
	case string:
		return strNode(v), nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(v.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("看不懂 %v", t)
}

func strNode(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

// orderBySchema 对象的键按说明里字段的顺序排（不认识的放在后面，保持原来的先后）。
func orderBySchema(n *yaml.Node, s *SchemaNode) {
	if n == nil || s == nil {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		type pair struct{ k, v *yaml.Node }
		var pairs []pair
		for i := 0; i+1 < len(n.Content); i += 2 {
			pairs = append(pairs, pair{n.Content[i], n.Content[i+1]})
		}
		if len(s.Fields) > 0 {
			rank := func(k string) int {
				for i, f := range s.Fields {
					if f.Name == k {
						return i
					}
				}
				return len(s.Fields)
			}
			sort.SliceStable(pairs, func(i, j int) bool { return rank(pairs[i].k.Value) < rank(pairs[j].k.Value) })
		}
		n.Content = n.Content[:0]
		for _, p := range pairs {
			orderBySchema(p.v, s.Child(p.k.Value))
			n.Content = append(n.Content, p.k, p.v)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			orderBySchema(c, s.Item)
		}
	}
}

// ---------------------------------------------------------------- 按行改原文

type ydoc struct {
	lines []string
	root  *yaml.Node // 顶层对象；空文件（或只有注释）时 nil
	unit  int        // 缩进几格
	seqIn int        // 列表比它的键多缩进几格（0 或 unit）
	final bool       // 原文末尾有换行
}

func parseYDoc(text string) (*ydoc, error) {
	d := &ydoc{unit: 2, seqIn: 2, final: text == "" || strings.HasSuffix(text, "\n")}
	body := strings.TrimSuffix(text, "\n")
	if body != "" {
		d.lines = strings.Split(body, "\n")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("原文件不是正确的 YAML：%w", err)
	}
	if len(doc.Content) > 0 {
		r := doc.Content[0]
		switch {
		case r.Kind == yaml.MappingNode && r.Style&yaml.FlowStyle == 0:
			d.root = r
		case r.Kind == yaml.ScalarNode && r.Tag == "!!null":
		default:
			return nil, errors.New("文件最外层不是按行写的 key: value，没法自动修改")
		}
	}
	d.detectIndent()
	return d, nil
}

// detectIndent 照着文件里已有的缩进写新行。
func (d *ydoc) detectIndent() {
	unitSet, seqSet := false, false
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil || n.Kind != yaml.MappingNode || n.Style&yaml.FlowStyle != 0 {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if v.Style&yaml.FlowStyle == 0 && len(v.Content) > 0 && v.Line > k.Line {
				switch v.Kind {
				case yaml.MappingNode:
					if diff := v.Content[0].Column - k.Column; !unitSet && diff > 0 {
						d.unit, unitSet = diff, true
					}
				case yaml.SequenceNode:
					if dc := d.dashCol(v.Content[0]); !seqSet && dc >= 0 {
						d.seqIn, seqSet = max(dc-(k.Column-1), 0), true
					}
				}
			}
			walk(v)
			if v.Kind == yaml.SequenceNode {
				for _, it := range v.Content {
					walk(it)
				}
			}
		}
	}
	walk(d.root)
	if !seqSet {
		d.seqIn = d.unit
	}
}

func (d *ydoc) text() string {
	s := strings.Join(d.lines, "\n")
	if d.final && s != "" {
		s += "\n"
	}
	return s
}

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

func isComment(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "#") }

func spaces(n int) string { return strings.Repeat(" ", n) }

// dashCol 列表项前面的 - 在第几列（从 0 起）；- 不在同一行（或看不懂）时 -1。
func (d *ydoc) dashCol(item *yaml.Node) int {
	if item.Line-1 >= len(d.lines) {
		return -1
	}
	r := []rune(d.lines[item.Line-1])
	j := item.Column - 2
	for j >= 0 && j < len(r) && r[j] == ' ' {
		j--
	}
	if j < 0 || j >= len(r) || r[j] != '-' {
		return -1
	}
	for _, c := range r[:j] {
		if c != ' ' {
			return -1 // - - x 这种
		}
	}
	return j
}

// blockEnd 从 start 行开始的一项占到哪一行（不含）：后面缩进比 indent 多的行（和中间的空行）都算它的；
// dash 时缩进相同、以 - 开头的行也算（列表和键对齐写的）。末尾的空行不算。
func (d *ydoc) blockEnd(start, indent int, dash bool) int {
	last := start
	for i := start + 1; i < len(d.lines); i++ {
		l := d.lines[i]
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		ind := indentOf(l)
		if ind > indent || (dash && ind == indent && (t == "-" || strings.HasPrefix(t, "- "))) {
			last = i
			continue
		}
		break
	}
	return last + 1
}

// cut 删掉 [from, to) 行；删完前后多出来的空行（上下都是空行，或上面是「键:」）去掉一行。
func (d *ydoc) cut(from, to int) {
	d.splice(from, to, nil)
	if from < len(d.lines) && strings.TrimSpace(d.lines[from]) == "" &&
		(from == 0 || strings.TrimSpace(d.lines[from-1]) == "" || strings.HasSuffix(strings.TrimSpace(d.lines[from-1]), ":")) {
		d.splice(from, from+1, nil)
	}
}

func (d *ydoc) splice(from, to int, repl []string) {
	out := make([]string, 0, len(d.lines)-(to-from)+len(repl))
	out = append(out, d.lines[:from]...)
	out = append(out, repl...)
	out = append(out, d.lines[to:]...)
	d.lines = out
}

// step 路径上的一级。
type step struct {
	node   *yaml.Node // 这一级的值
	parent *yaml.Node // 所在的对象、列表
	key    *yaml.Node // 在对象里时的键
	index  int        // 在对象（键的位置）或列表里的位置
}

// locate 沿路径找到已有的各级；没有的那一级停下（返回的比 path 短）。
// merged：停下的那一级是带 <<: *锚点 的对象（没找到的键可能是合并进来的）。
func (d *ydoc) locate(path []string) (steps []step, merged bool, err error) {
	cur := d.root
	at := func(i int) string { return strings.Join(path[:i], ".") }
	for n, seg := range path {
		if cur == nil {
			break
		}
		if cur.Kind == yaml.AliasNode {
			return nil, false, fmt.Errorf("%s 是用 *%s 引用的别处的内容，没法在这里改：去改 &%s 定义的地方，或者整个 set %s", at(n), cur.Value, cur.Value, at(n))
		}
		var st *step
		switch cur.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(cur.Content); i += 2 {
				if cur.Content[i].Value == "<<" {
					merged = true
				}
			}
			for i := 0; i+1 < len(cur.Content); i += 2 {
				if cur.Content[i].Value == seg && seg != "<<" {
					st = &step{node: cur.Content[i+1], parent: cur, key: cur.Content[i], index: i}
					break
				}
			}
		case yaml.SequenceNode:
			items := cur.Content
			i := matchItem(len(items), seg, func(i int) (string, bool) {
				if items[i].Kind == yaml.MappingNode {
					for k := 0; k+1 < len(items[i].Content); k += 2 {
						if items[i].Content[k].Value == "name" && items[i].Content[k+1].Kind == yaml.ScalarNode {
							return items[i].Content[k+1].Value, true
						}
					}
				}
				return "", false
			}, func(i int) (string, bool) {
				if items[i].Kind == yaml.ScalarNode {
					return items[i].Value, true
				}
				return "", false
			})
			if i >= 0 {
				st = &step{node: items[i], parent: cur, index: i}
			}
		}
		if st == nil {
			break
		}
		merged = false
		if st.node.Kind == yaml.AliasNode && n < len(path)-1 {
			return nil, false, fmt.Errorf("%s 是用 *%s 引用的别处的内容，没法在这里改：去改 &%s 定义的地方，或者整个 set %s", at(n+1), st.node.Value, st.node.Value, at(n+1))
		}
		steps = append(steps, *st)
		cur = st.node
	}
	if len(steps) == len(path) {
		merged = false
	}
	return steps, merged, nil
}

// anchorIn 节点里面（含它自己）定义的第一个锚点名；没有时 ""。
func anchorIn(n *yaml.Node) string {
	if n.Anchor != "" {
		return n.Anchor
	}
	for _, c := range n.Content {
		if a := anchorIn(c); a != "" {
			return a
		}
	}
	return ""
}

func hasMerge(m *yaml.Node) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "<<" {
			return true
		}
	}
	return false
}

func isBlock(n *yaml.Node) bool {
	return (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && n.Style&yaml.FlowStyle == 0 && len(n.Content) > 0
}

func (d *ydoc) apply(path []string, op string, val *yaml.Node) error {
	steps, merged, err := d.locate(path)
	if err != nil {
		return err
	}
	if merged && (op != "set" || len(steps) < len(path)-1) {
		// <<: *锚点 合并进来的只能在这里整个覆盖（合并只合并一层）
		at := strings.Join(path[:len(steps)+1], ".")
		return fmt.Errorf("%s 是从 << 合并进来的（或者没有）：要改它就 set 整个 %s（写全，会覆盖合并来的）", at, at)
	}
	// 路径经过写成一行的 {…}、[…]（或者要在没有内容的项下面建）：从那一级起整个重写
	for j, st := range steps {
		inside := j < len(path)-1 || (op == "append" && j == len(path)-1)
		if inside && !isBlock(st.node) {
			c := cloneNode(st.node)
			c, err := nodeApply(c, path[j+1:], op, val, path[:j+1]...)
			if err != nil {
				return err
			}
			return d.replace(st, c, st.node.Style&yaml.FlowStyle != 0)
		}
	}
	full := len(steps) == len(path)
	if full {
		last := steps[len(steps)-1]
		if a := anchorIn(last.node); a != "" && (op != "append") {
			return fmt.Errorf("%s 里面用 &%s 定义了锚点，别处用 *%s 引用它，整个换掉、删掉会影响引用的地方：请直接改文件", strings.Join(path, "."), a, a)
		}
		if op == "delete" && last.key != nil && hasMerge(last.parent) {
			return fmt.Errorf("%s 所在的对象用了 <<: *锚点，删掉后会改用合并来的值：请直接改文件", strings.Join(path, "."))
		}
	}
	switch op {
	case "delete":
		if !full {
			return fmt.Errorf("没有 %s", strings.Join(path, "."))
		}
		return d.remove(steps)
	case "set":
		if full {
			last := steps[len(steps)-1]
			return d.replace(last, val, last.node.Style&yaml.FlowStyle != 0)
		}
	case "append":
		if full {
			last := steps[len(steps)-1]
			if last.node.Kind != yaml.SequenceNode {
				return errors.New("这里不是列表：不能 append")
			}
			return d.appendItem(last.node, val)
		}
		val = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{val}}
	}
	// 新建：最深的已有的一级是按行写的对象（或文件是空的），在它下面加一项
	rest := path[len(steps):]
	v := nest(rest[1:], val)
	var m *yaml.Node
	if len(steps) == 0 {
		m = d.root
	} else {
		m = steps[len(steps)-1].node
	}
	if m == nil {
		lines := d.entryLines("", rest[0], v, false, "")
		if len(d.lines) > 0 && strings.TrimSpace(d.lines[len(d.lines)-1]) != "" {
			lines = append([]string{""}, lines...)
		}
		d.lines = append(d.lines, lines...)
		return nil
	}
	if m.Kind == yaml.SequenceNode {
		return fmt.Errorf("列表 %s 里没有 %s（列表里的项写 name 或从 0 起的序号；要加一项用 append）", strings.Join(path[:len(steps)], "."), rest[0])
	}
	if m.Kind != yaml.MappingNode {
		return fmt.Errorf("%s 现在是一个值，不是对象：要改就整个 set %s", strings.Join(path[:len(steps)], "."), strings.Join(path[:len(steps)], "."))
	}
	return d.insertEntry(m, rest[0], v)
}

// nest 把 a.b = v 写成 {a: {b: v}}。
func nest(path []string, v *yaml.Node) *yaml.Node {
	for i := len(path) - 1; i >= 0; i-- {
		v = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{strNode(path[i]), v}}
	}
	return v
}

func cloneNode(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, x := range n.Content {
		c.Content[i] = cloneNode(x)
	}
	return &c
}

// nodeApply 在节点（副本）上做修改，返回改后的节点（没有内容的会换成新建的对象、列表）。
// 按字面改（<<、& 都当普通的内容）：改完的原文解析出来要和它一模一样。at 是 n 所在的路径（出错时说明用）。
func nodeApply(n *yaml.Node, path []string, op string, val *yaml.Node, at ...string) (*yaml.Node, error) {
	where := strings.Join(at, ".")
	if n.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("%s 是用 *%s 引用的别处的内容，没法在这里改：去改 &%s 定义的地方，或者整个 set %s", where, n.Value, n.Value, where)
	}
	null := n.Kind == yaml.ScalarNode && n.Tag == "!!null"
	if len(path) == 0 {
		switch op {
		case "set":
			return val, nil
		case "append":
			switch {
			case null:
				return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{val}}, nil
			case n.Kind == yaml.SequenceNode:
				n.Content = append(n.Content, val)
				return n, nil
			case n.Kind == yaml.ScalarNode:
				return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle, Content: []*yaml.Node{n, val}}, nil
			}
			return nil, fmt.Errorf("%s 是对象，不是列表：不能 append", where)
		}
		return nil, errors.New("不能删掉整个文件")
	}
	seg, rest := path[0], path[1:]
	here := append(at[:len(at):len(at)], seg)
	if null {
		if op == "delete" {
			return nil, fmt.Errorf("没有 %s", strings.Join(here, "."))
		}
		n = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: n.Style & yaml.FlowStyle}
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value != seg || seg == "<<" {
				continue
			}
			if op == "delete" && len(rest) == 0 {
				n.Content = append(n.Content[:i:i], n.Content[i+2:]...)
				return n, nil
			}
			c, err := nodeApply(n.Content[i+1], rest, op, val, here...)
			if err != nil {
				return nil, err
			}
			n.Content[i+1] = c
			return n, nil
		}
		if op == "delete" {
			return nil, fmt.Errorf("没有 %s", strings.Join(here, "."))
		}
		c, err := nodeApply(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}, rest, op, val, here...)
		if err != nil {
			return nil, err
		}
		n.Content = append(n.Content, strNode(seg), c)
		return n, nil
	case yaml.SequenceNode:
		items := n.Content
		i := matchItem(len(items), seg, func(i int) (string, bool) {
			if items[i].Kind == yaml.MappingNode {
				for k := 0; k+1 < len(items[i].Content); k += 2 {
					if items[i].Content[k].Value == "name" && items[i].Content[k+1].Kind == yaml.ScalarNode {
						return items[i].Content[k+1].Value, true
					}
				}
			}
			return "", false
		}, func(i int) (string, bool) {
			if items[i].Kind == yaml.ScalarNode {
				return items[i].Value, true
			}
			return "", false
		})
		if i < 0 {
			return nil, fmt.Errorf("列表 %s 里没有 %s（列表里的项写 name 或从 0 起的序号；要加一项用 append）", orTop(where), seg)
		}
		if op == "delete" && len(rest) == 0 {
			n.Content = append(n.Content[:i:i], n.Content[i+1:]...)
			return n, nil
		}
		c, err := nodeApply(n.Content[i], rest, op, val, here...)
		if err != nil {
			return nil, err
		}
		n.Content[i] = c
		return n, nil
	}
	return nil, fmt.Errorf("%s 现在是一个值（%s），不是对象或列表：要改就整个 set %s", orTop(where), n.Value, orTop(where))
}

func orTop(s string) string {
	if s == "" {
		return "顶层"
	}
	return s
}

// sameNode 两个节点的内容一样（不管写法、位置、注释）：类型、值、锚点、引用都要一样。
func sameNode(a, b *yaml.Node) bool {
	empty := func(n *yaml.Node) bool {
		return n == nil || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null") || (n.Kind == yaml.MappingNode && len(n.Content) == 0)
	}
	if empty(a) || empty(b) {
		return empty(a) && empty(b) && (a == nil || b == nil || a.Kind == b.Kind || true)
	}
	if a.Kind != b.Kind || a.Anchor != b.Anchor || len(a.Content) != len(b.Content) {
		return false
	}
	switch a.Kind {
	case yaml.ScalarNode:
		return a.ShortTag() == b.ShortTag() && a.Value == b.Value
	case yaml.AliasNode:
		return a.Value == b.Value
	}
	for i := range a.Content {
		if !sameNode(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- 写新行

func (d *ydoc) encode(n *yaml.Node) []string {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(d.unit)
	if err := e.Encode(n); err != nil {
		return []string{"null"}
	}
	e.Close()
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

// flowLists 只有简单值的短列表写成一行：[a, b]（和手写的习惯一样）。
func flowLists(n *yaml.Node) *yaml.Node {
	n = cloneNode(n)
	var walk func(x *yaml.Node)
	walk = func(x *yaml.Node) {
		if x.Kind == yaml.SequenceNode && x.Style&yaml.FlowStyle == 0 {
			simple := true
			for _, c := range x.Content {
				if c.Kind != yaml.ScalarNode || strings.Contains(c.Value, "\n") {
					simple = false
				}
			}
			if simple && len(x.Content) <= 8 {
				x.Style |= yaml.FlowStyle
			}
		}
		for _, c := range x.Content {
			walk(c)
		}
	}
	walk(n)
	return n
}

func setFlow(n *yaml.Node) *yaml.Node {
	n = cloneNode(n)
	var walk func(x *yaml.Node)
	walk = func(x *yaml.Node) {
		if x.Kind == yaml.MappingNode || x.Kind == yaml.SequenceNode {
			x.Style |= yaml.FlowStyle
		}
		for _, c := range x.Content {
			walk(c)
		}
	}
	walk(n)
	return n
}

const flowMax = 100

// inline 能写在一行上时的写法。
func (d *ydoc) inline(n *yaml.Node, preferFlow bool) (string, bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		l := d.encode(n)
		if len(l) == 1 && !strings.HasPrefix(l[0], "|") && !strings.HasPrefix(l[0], ">") {
			return l[0], true
		}
		return "", false
	case yaml.MappingNode, yaml.SequenceNode:
		if len(n.Content) == 0 {
			if n.Kind == yaml.MappingNode {
				return "{}", true
			}
			return "[]", true
		}
		simpleList := n.Kind == yaml.SequenceNode
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				simpleList = false
			}
		}
		if !preferFlow && !simpleList {
			return "", false
		}
		l := d.encode(setFlow(n))
		if len(l) == 1 && len([]rune(l[0])) <= flowMax {
			return l[0], true
		}
	}
	return "", false
}

func (d *ydoc) keyText(k string) string {
	l := d.encode(strNode(k))
	return l[0]
}

// entryLines key: value 的几行；prefix 是键前面的内容（缩进，或列表项的「- 」），comment 是原来行尾的注释。
func (d *ydoc) entryLines(prefix, key string, v *yaml.Node, preferFlow bool, comment string) []string {
	k := len([]rune(prefix))
	head := prefix + d.keyText(key) + ":"
	if s, ok := d.inline(v, preferFlow); ok {
		return []string{head + " " + s + comment}
	}
	if v.Kind == yaml.ScalarNode { // 多行文字：|- 后面的行
		l := d.encode(v)
		out := []string{head + " " + l[0] + comment}
		for _, x := range l[1:] {
			out = append(out, spaces(k)+x)
		}
		return out
	}
	ind := k + d.unit
	if v.Kind == yaml.SequenceNode {
		ind = k + d.seqIn
	}
	out := []string{head + comment}
	for _, x := range d.encode(flowLists(v)) {
		if x == "" {
			out = append(out, x)
		} else {
			out = append(out, spaces(ind)+x)
		}
	}
	return out
}

// itemLines 列表里一项的几行（- 在第 dash 列）。
func (d *ydoc) itemLines(dash int, v *yaml.Node, preferFlow bool, comment string) []string {
	if s, ok := d.inline(v, preferFlow); ok {
		return []string{spaces(dash) + "- " + s + comment}
	}
	l := d.encode(&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{flowLists(v)}})
	out := make([]string, len(l))
	for i, x := range l {
		if x == "" {
			out[i] = x
		} else {
			out[i] = spaces(dash) + x
		}
	}
	if comment != "" && len(out) > 0 {
		out[0] += comment
	}
	return out
}

// trailingComment 行尾的注释（连同前面的空格）；没有时 ""。
func trailingComment(line string, comments ...string) string {
	for _, c := range comments {
		if c == "" || strings.Contains(c, "\n") {
			continue
		}
		if i := strings.LastIndex(line, c); i > 0 && strings.TrimSpace(line[i:]) == strings.TrimSpace(c) {
			j := i
			for j > 0 && line[j-1] == ' ' {
				j--
			}
			if j < i {
				return line[j:]
			}
		}
	}
	return ""
}

// entryRange 对象里一项（键 k、值 v）占的行 [start, end)，和键前面的内容（缩进或「- 」）。
func (d *ydoc) entryRange(k, v *yaml.Node) (start, end int, prefix string, err error) {
	start = k.Line - 1
	if start < 0 || start >= len(d.lines) {
		return 0, 0, "", errors.New("看不懂这个文件的行号")
	}
	r := []rune(d.lines[start])
	col := k.Column - 1
	if col > len(r) {
		return 0, 0, "", errors.New("看不懂这个文件的列号")
	}
	prefix = string(r[:col])
	if strings.Trim(prefix, " -") != "" {
		return 0, 0, "", errors.New("这一行的写法没法自动修改")
	}
	dash := v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0 && len(v.Content) > 0 && d.dashCol(v.Content[0]) == col
	return start, d.blockEnd(start, col, dash), prefix, nil
}

// replace 把这一级的值整个换掉。
func (d *ydoc) replace(st step, v *yaml.Node, preferFlow bool) error {
	if st.parent == nil {
		return errors.New("不能整个换掉文件")
	}
	if st.key != nil {
		start, end, prefix, err := d.entryRange(st.key, st.node)
		if err != nil {
			return err
		}
		comment := ""
		if st.node.Line == st.key.Line {
			comment = trailingComment(d.lines[start], st.node.LineComment, st.key.LineComment)
		} else {
			comment = trailingComment(d.lines[start], st.key.LineComment)
		}
		d.splice(start, end, d.entryLines(prefix, st.key.Value, v, preferFlow, comment))
		return nil
	}
	dash := d.dashCol(st.node)
	if dash < 0 {
		return errors.New("这个列表的写法没法自动修改")
	}
	start := st.node.Line - 1
	end := d.blockEnd(start, dash, false)
	comment := ""
	if st.node.Kind == yaml.ScalarNode || st.node.Style&yaml.FlowStyle != 0 {
		comment = trailingComment(d.lines[start], st.node.LineComment)
	}
	d.splice(start, end, d.itemLines(dash, v, preferFlow, comment))
	return nil
}

// headStart 紧挨在 start 上面、缩进相同的注释行从哪一行开始（删掉一项时一起删）。
func (d *ydoc) headStart(start, indent int) int {
	for start > 0 && isComment(d.lines[start-1]) && indentOf(d.lines[start-1]) == indent {
		start--
	}
	return start
}

// remove 删掉路径最后一级。
func (d *ydoc) remove(steps []step) error {
	st := steps[len(steps)-1]
	p := st.parent
	var up step // 上一级（p 是它的值）
	if len(steps) >= 2 {
		up = steps[len(steps)-2]
	}
	if st.key != nil {
		if len(p.Content) == 2 { // 只有这一项：上一级变成 {}
			if up.parent == nil { // 顶层
				start, end, _, err := d.entryRange(st.key, st.node)
				if err != nil {
					return err
				}
				d.cut(d.headStart(start, 0), end)
				return nil
			}
			return d.replace(up, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, false)
		}
		start, end, prefix, err := d.entryRange(st.key, st.node)
		if err != nil {
			return err
		}
		col := len([]rune(prefix))
		if strings.TrimSpace(prefix) != "" { // 列表项的第一个键（- name: x）：下一个键挪到 - 后面
			nk := p.Content[st.index+2]
			nl := nk.Line - 1
			if nl < end || nl >= len(d.lines) || indentOf(d.lines[nl]) != col || nk.Column-1 != col {
				return errors.New("这一项的写法没法自动修改")
			}
			d.lines[nl] = prefix + string([]rune(d.lines[nl])[col:])
			d.splice(start, nl, nil)
			return nil
		}
		d.cut(d.headStart(start, col), end)
		return nil
	}
	if len(p.Content) == 1 { // 列表里只有这一项：变成 []
		return d.replace(up, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}, false)
	}
	dash := d.dashCol(st.node)
	if dash < 0 {
		return errors.New("这个列表的写法没法自动修改")
	}
	start := st.node.Line - 1
	end := d.blockEnd(start, dash, false)
	d.cut(d.headStart(start, dash), end)
	return nil
}

// insertEntry 在按行写的对象 m 的末尾加上 key: v。
func (d *ydoc) insertEntry(m *yaml.Node, key string, v *yaml.Node) error {
	fk := m.Content[0]
	col := fk.Column - 1
	lk, lv := m.Content[len(m.Content)-2], m.Content[len(m.Content)-1]
	_, end, _, err := d.entryRange(lk, lv)
	if err != nil {
		return err
	}
	if fl := fk.Line - 1; fl < len(d.lines) {
		// 第一个键前面是「- 」时，新行用同样多的空格
		if r := []rune(d.lines[fl]); col <= len(r) && strings.Trim(string(r[:col]), " -") != "" {
			return errors.New("这个对象的写法没法自动修改")
		}
	}
	lines := d.entryLines(spaces(col), key, v, false, "")
	// 顶层的各项之间空着一行的，新加的也空一行
	if col == 0 && end-1 > lk.Line-1 && lk.Line >= 2 && strings.TrimSpace(d.lines[lk.Line-2]) == "" {
		lines = append([]string{""}, lines...)
	}
	d.splice(end, end, lines)
	return nil
}

// appendItem 在按行写的列表 s 末尾加一项。
func (d *ydoc) appendItem(s *yaml.Node, v *yaml.Node) error {
	if !isBlock(s) {
		return errors.New("这个列表的写法没法自动修改")
	}
	dash := d.dashCol(s.Content[0])
	last := s.Content[len(s.Content)-1]
	ld := d.dashCol(last)
	if dash < 0 || ld < 0 {
		return errors.New("这个列表的写法没法自动修改")
	}
	end := d.blockEnd(last.Line-1, ld, false)
	d.splice(end, end, d.itemLines(dash, v, false, ""))
	return nil
}

// ---------------------------------------------------------------- 看现在怎么写的

// Snippet 原文里这一项占的那几行（键和它下面的）；找不到（没写、经过别名、合并来的）时 ok 为 false。
func Snippet(src []byte, path []string) (string, bool) {
	d, err := parseYDoc(strings.ReplaceAll(string(src), "\r\n", "\n"))
	if err != nil || len(path) == 0 {
		return "", false
	}
	steps, _, err := d.locate(path)
	if err != nil || len(steps) != len(path) {
		return "", false
	}
	st := steps[len(steps)-1]
	var start, end int
	if st.key != nil {
		s, e, _, err := d.entryRange(st.key, st.node)
		if err != nil {
			return "", false
		}
		start, end = s, e
	} else {
		dash := d.dashCol(st.node)
		if dash < 0 {
			return "", false
		}
		start = st.node.Line - 1
		end = d.blockEnd(start, dash, false)
	}
	return strings.Join(d.lines[start:end], "\n"), true
}

// ValueAt 解析出来的数据里（合并、别名都展开了）这一项的值；没有时 ok 为 false。
func ValueAt(src []byte, path []string) (any, bool) {
	v, err := decodeGeneric(string(src))
	if err != nil {
		return nil, false
	}
	for _, seg := range path {
		switch x := v.(type) {
		case map[string]any:
			c, ok := x[seg]
			if !ok {
				return nil, false
			}
			v = c
		case []any:
			i := genericIndex(x, seg)
			if i < 0 {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}
