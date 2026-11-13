package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetTargetIdentity 把目标改成用私钥登录：设置 identityFile，删除 auth 行。
// 按行修改原文件，保留注释、锚点和格式；只支持块格式书写的目标（target: 下一行起缩进的 key: value）。
// 改完重新加载校验，不通过则恢复原文件。
func SetTargetIdentity(path, target, identity string) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(orig, &root); err != nil {
		return err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s 不是 YAML 对象", path)
	}
	targets := mapValue(root.Content[0], "targets")
	if targets == nil || targets.Kind != yaml.MappingNode {
		return fmt.Errorf("%s 里没有 targets", path)
	}
	t := mapValue(targets, target)
	if t == nil {
		return fmt.Errorf("没有目标 %s", target)
	}
	if t.Kind != yaml.MappingNode || t.Style&yaml.FlowStyle != 0 || len(t.Content) == 0 {
		return fmt.Errorf("目标 %s 不是块格式书写，无法自动修改：请手工设置 identityFile: %s 并删除 auth", target, identity)
	}

	crlf := bytes.Contains(orig, []byte("\r\n"))
	text := strings.ReplaceAll(string(orig), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	indent := strings.Repeat(" ", t.Content[0].Column-1)
	newLine := indent + "identityFile: " + yamlQuote(identity)

	var drop []int // 要删除的行（0 起）
	replaced := false
	for i := 0; i+1 < len(t.Content); i += 2 {
		k, v := t.Content[i], t.Content[i+1]
		switch k.Value {
		case "identityFile":
			if v.Line != k.Line {
				return fmt.Errorf("目标 %s 的 identityFile 不在同一行，无法自动修改", target)
			}
			lines[k.Line-1] = newLine
			replaced = true
		case "auth":
			if v.Line != k.Line {
				return fmt.Errorf("目标 %s 的 auth 不在同一行，无法自动修改", target)
			}
			drop = append(drop, k.Line-1)
		}
	}
	if !replaced {
		at := t.Content[0].Line - 1 // 插在第一个字段之前
		lines = append(lines[:at], append([]string{newLine}, lines[at:]...)...)
		for i := range drop {
			if drop[i] >= at {
				drop[i]++
			}
		}
	}
	for i := len(drop) - 1; i >= 0; i-- {
		lines = append(lines[:drop[i]], lines[drop[i]+1:]...)
	}
	out := strings.Join(lines, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if err := writeAtomic(path, []byte(out)); err != nil {
		return err
	}
	if _, err := Load(path); err != nil {
		writeAtomic(path, orig)
		return fmt.Errorf("修改后的配置校验未通过，已恢复原文件：%w", err)
	}
	return nil
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlQuote 单引号字符串（路径里有反斜杠、冒号等也安全）。
func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// SoftwareBackup Ferry 装的数据库的一项备份（AddBackup 写进配置）。
type SoftwareBackup struct {
	Name, Kind, Database, User, Bin, EnvFile, Target, Comment string
	Port                                                      int
}

// AddBackup 在配置的 backups 里加上一项（按行追加，保留注释和格式）；没有 backups 时在文件末尾加上。
// backups 不是块格式书写时不自动改。改完重新校验，不通过则恢复。
func AddBackup(path string, b SoftwareBackup) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(orig, &root); err != nil {
		return err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s 不是 YAML 对象", path)
	}
	top := root.Content[0]
	crlf := bytes.Contains(orig, []byte("\r\n"))
	text := strings.TrimRight(strings.ReplaceAll(string(orig), "\r\n", "\n"), "\n")
	lines := strings.Split(text, "\n")
	db := fmt.Sprintf("{database: %s, port: %d, user: %s", yamlQuote(b.Database), b.Port, yamlQuote(b.User))
	if b.Bin != "" {
		db += ", bin: " + yamlQuote(b.Bin)
	}
	db += "}"
	item := func(indent string) []string {
		return []string{
			indent + "- name: " + b.Name + "   # " + b.Comment,
			indent + "  " + b.Kind + ": " + db,
			indent + "  envFile: " + yamlQuote(b.EnvFile) + "   # 服务器上只有 root 能读的密码文件",
			indent + "  targets: [" + yamlQuote(b.Target) + "]",
		}
	}
	var keyLine int
	var seq *yaml.Node
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "backups" {
			keyLine, seq = top.Content[i].Line, top.Content[i+1]
		}
	}
	switch {
	case seq == nil:
		lines = append(lines, "", "# 数据备份：在桌面版的「数据备份」里打开后按时备份")
		lines = append(lines, "backups:")
		lines = append(lines, item("  ")...)
	case seq.Kind == yaml.ScalarNode && seq.Tag == "!!null":
		lines = append(lines[:keyLine], append(item("  "), lines[keyLine:]...)...)
	case seq.Kind != yaml.SequenceNode || seq.Style&yaml.FlowStyle != 0 || len(seq.Content) == 0:
		return fmt.Errorf("%s 里的 backups 不是块格式书写，无法自动修改：请手工加上 %s", path, b.Name)
	default:
		// 插在 backups 之后下一个顶层字段之前（没有就是文件末尾），去掉末尾的空行、注释
		end := len(lines)
		for i := 0; i+1 < len(top.Content); i += 2 {
			if l := top.Content[i].Line; l > keyLine && l-1 < end {
				end = l - 1
			}
		}
		for end > keyLine && (strings.TrimSpace(lines[end-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#")) {
			end--
		}
		indent := strings.Repeat(" ", seq.Content[0].Column-3)
		if seq.Content[0].Column < 3 {
			indent = ""
		}
		lines = append(lines[:end], append(item(indent), lines[end:]...)...)
	}
	out := strings.Join(lines, "\n") + "\n"
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if err := writeAtomic(path, []byte(out)); err != nil {
		return err
	}
	if _, err := Load(path); err != nil {
		writeAtomic(path, orig)
		return fmt.Errorf("修改后的配置校验未通过，已恢复原文件：%w", err)
	}
	return nil
}

var reBareYAML = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// keepFlow 保留的写成一行：[config.js, {path: uploads, shared: true}]。
func keepFlow(keep []*KeepItem) string {
	var parts []string
	for _, k := range keep {
		p := k.Path
		if !reBareYAML.MatchString(p) {
			p = yamlQuote(p)
		}
		if k.Shared {
			parts = append(parts, "{path: "+p+", shared: true}")
		} else {
			parts = append(parts, p)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// lastLine 节点（含下面的）占到的最后一行。
func lastLine(n *yaml.Node) int {
	l := n.Line
	for _, c := range n.Content {
		if x := lastLine(c); x > l {
			l = x
		}
	}
	return l
}

// hasAliasOrAnchor 节点（含下面的）用了 YAML 锚点或引用。
func hasAliasOrAnchor(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return true
	}
	for _, c := range n.Content {
		if hasAliasOrAnchor(c) {
			return true
		}
	}
	return false
}

// flowText 把节点写成一行（对象、列表用花括号、方括号）；写不成一行时 ok 为 false。
func flowText(n *yaml.Node) (string, bool) {
	var cp func(*yaml.Node) *yaml.Node
	cp = func(n *yaml.Node) *yaml.Node {
		c := *n
		c.HeadComment, c.LineComment, c.FootComment = "", "", ""
		if c.Kind == yaml.MappingNode || c.Kind == yaml.SequenceNode {
			c.Style |= yaml.FlowStyle
		}
		c.Content = nil
		for _, x := range n.Content {
			c.Content = append(c.Content, cp(x))
		}
		return &c
	}
	b, err := yaml.Marshal(cp(n))
	if err != nil {
		return "", false
	}
	s := strings.TrimSuffix(string(b), "\n")
	if strings.Contains(s, "\n") {
		return "", false
	}
	return s, true
}

// findApp 在 apps 里按名字找应用。
func findApp(root *yaml.Node, path, app string) (*yaml.Node, error) {
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s 不是 YAML 对象", path)
	}
	apps := mapValue(root.Content[0], "apps")
	if apps == nil || apps.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s 里没有 apps", path)
	}
	var a *yaml.Node
	for _, n := range apps.Content {
		if n.Kind == yaml.MappingNode {
			if v := mapValue(n, "name"); v != nil && strings.EqualFold(v.Value, app) {
				a = n
			}
		}
	}
	if a == nil {
		return nil, fmt.Errorf("没有应用 %s", app)
	}
	return a, nil
}

// artifactToBlock 应用的 artifact 写成一行的（artifact: {path: dist}）改成分行写，便于加 keep。
// 返回改好的文本；ok 为 false 表示没法自动改（多行的花括号写法、锚点等）。
func artifactToBlock(text string, a *yaml.Node) (string, bool) {
	lines := strings.Split(text, "\n")
	var key, art *yaml.Node
	for i := 0; i+1 < len(a.Content); i += 2 {
		if a.Content[i].Value == "artifact" {
			key, art = a.Content[i], a.Content[i+1]
		}
	}
	if art == nil || art.Kind != yaml.MappingNode || hasAliasOrAnchor(art) || key.Line != art.Line || lastLine(art) != art.Line {
		return "", false
	}
	line := lines[key.Line-1]
	prefix := []rune(line)
	if key.Column-1 > len(prefix) {
		return "", false
	}
	head := string(prefix[:key.Column-1])
	if strings.TrimLeft(head, " -") != "" { // 同一行前面还有别的内容
		return "", false
	}
	comment := key.LineComment
	if comment == "" {
		comment = art.LineComment
	}
	if comment == "" && len(art.Content) > 0 {
		last := art.Content[len(art.Content)-1]
		comment = last.LineComment
	}
	first := head + key.Value + ":"
	if comment != "" {
		first += "   " + comment
	}
	repl := []string{first}
	sub := strings.Repeat(" ", key.Column+1)
	for i := 0; i+1 < len(art.Content); i += 2 {
		k, ok1 := flowText(art.Content[i])
		v, ok2 := flowText(art.Content[i+1])
		if !ok1 || !ok2 {
			return "", false
		}
		repl = append(repl, sub+k+": "+v)
	}
	lines = append(lines[:key.Line-1], append(repl, lines[key.Line:]...)...)
	return strings.Join(lines, "\n"), true
}

// SetAppKeep 改写应用的 artifact.keep（部署时保留服务器上的）：写成一行，空 = 去掉。
// 和 SetAppRuntime 一样按行修改、保留注释；artifact 写成一行（{path: dist}）时先改成分行写；
// 用了锚点、合并等没法自动改的给出手工写法。改完重新校验，不通过则恢复。
func SetAppKeep(path, app string, keep []*KeepItem) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	crlf := bytes.Contains(orig, []byte("\r\n"))
	text := strings.ReplaceAll(string(orig), "\r\n", "\n")
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return err
	}
	a, err := findApp(&root, path, app)
	if err != nil {
		return err
	}
	manual := fmt.Errorf("应用 %s 的 artifact 用了 YAML 锚点或合并等写法，无法自动修改：请手工在它的 artifact 下写 keep: %s", app, keepFlow(keep))
	art := mapValue(a, "artifact")
	if art != nil && art.Kind == yaml.MappingNode && art.Style&yaml.FlowStyle != 0 {
		if len(keep) == 0 && mapValue(art, "keep") == nil {
			return nil // 本来就没有
		}
		t, ok := artifactToBlock(text, a)
		if !ok {
			return manual
		}
		text = t
		root = yaml.Node{}
		if err := yaml.Unmarshal([]byte(text), &root); err != nil {
			return manual
		}
		if a, err = findApp(&root, path, app); err != nil {
			return err
		}
		art = mapValue(a, "artifact")
	}
	if art == nil || art.Kind != yaml.MappingNode || art.Style&yaml.FlowStyle != 0 || art.Anchor != "" || len(art.Content) == 0 {
		return manual
	}
	for i := 0; i+1 < len(art.Content); i += 2 {
		if art.Content[i].Value == "<<" {
			return manual
		}
	}
	lines := strings.Split(text, "\n")
	indent := strings.Repeat(" ", art.Content[0].Column-1)
	newLine := indent + "keep: " + keepFlow(keep) + "   # 部署时保留服务器上的（不用包里的）"
	from, to := 0, -1 // 原来的 keep 占的行（1 起），没有为 0
	for i := 0; i+1 < len(art.Content); i += 2 {
		if k := art.Content[i]; k.Value == "keep" {
			from, to = k.Line, lastLine(art.Content[i+1])
		}
	}
	var repl []string
	if len(keep) > 0 {
		repl = []string{newLine}
	}
	if from > 0 {
		lines = append(lines[:from-1], append(repl, lines[to:]...)...)
	} else if len(keep) > 0 {
		at := lastLine(art) // 加在 artifact 的最后
		lines = append(lines[:at], append(repl, lines[at:]...)...)
	}
	return writeKeep(path, orig, strings.Join(lines, "\n"), crlf)
}

// writeKeep 写回改好的文本（没变就不写），重新校验，不通过则恢复原文件。
func writeKeep(path string, orig []byte, out string, crlf bool) error {
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if out == string(orig) {
		return nil
	}
	if err := writeAtomic(path, []byte(out)); err != nil {
		return err
	}
	if _, err := Load(path); err != nil {
		writeAtomic(path, orig)
		return fmt.Errorf("修改后的配置校验未通过，已恢复原文件：%w", err)
	}
	return nil
}

// SetAppRuntime 让应用改用 Ferry 提供的 JDK：service 里写 runtime: {java: N}，去掉 java 行。
// 和 SetTargetIdentity 一样按行修改、保留注释；service 用了 YAML 锚点（与别的应用共用）时不自动改。改完重新校验，不通过则恢复。
func SetAppRuntime(path, app string, java int) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(orig, &root); err != nil {
		return err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s 不是 YAML 对象", path)
	}
	apps := mapValue(root.Content[0], "apps")
	if apps == nil || apps.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s 里没有 apps", path)
	}
	var a *yaml.Node
	for _, n := range apps.Content {
		if n.Kind == yaml.MappingNode {
			if v := mapValue(n, "name"); v != nil && strings.EqualFold(v.Value, app) {
				a = n
			}
		}
	}
	if a == nil {
		return fmt.Errorf("没有应用 %s", app)
	}
	manual := fmt.Errorf("应用 %s 的 service 不是单独的块格式书写（可能用了 YAML 锚点与其他应用共用），无法自动修改：请手工在它的 service 下写 runtime: {java: %d} 并去掉 java", app, java)
	svc := mapValue(a, "service")
	if svc == nil || svc.Kind != yaml.MappingNode || svc.Style&yaml.FlowStyle != 0 || svc.Anchor != "" || len(svc.Content) == 0 {
		return manual
	}
	for i := 0; i+1 < len(svc.Content); i += 2 {
		if svc.Content[i].Value == "<<" {
			return manual
		}
	}
	crlf := bytes.Contains(orig, []byte("\r\n"))
	lines := strings.Split(strings.ReplaceAll(string(orig), "\r\n", "\n"), "\n")
	indent := strings.Repeat(" ", svc.Content[0].Column-1)
	newLine := fmt.Sprintf("%sruntime: {java: %d}   # 由 Ferry 提供 JDK %d（自动下载、上传到服务器）", indent, java, java)
	done := false
	for i := 0; i+1 < len(svc.Content); i += 2 {
		k, v := svc.Content[i], svc.Content[i+1]
		if k.Value != "java" && k.Value != "runtime" {
			continue
		}
		if v.Line != k.Line || v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle == 0 {
			return manual
		}
		if !done {
			lines[k.Line-1] = newLine
			done = true
		} else {
			lines[k.Line-1] = "\x00" // 删除
		}
	}
	if !done {
		at := svc.Content[0].Line - 1
		lines = append(lines[:at], append([]string{newLine}, lines[at:]...)...)
	}
	var kept []string
	for _, l := range lines {
		if l != "\x00" {
			kept = append(kept, l)
		}
	}
	out := strings.Join(kept, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if err := writeAtomic(path, []byte(out)); err != nil {
		return err
	}
	if _, err := Load(path); err != nil {
		writeAtomic(path, orig)
		return fmt.Errorf("修改后的配置校验未通过，已恢复原文件：%w", err)
	}
	return nil
}
