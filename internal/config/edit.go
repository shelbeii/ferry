package config

import (
	"bytes"
	"fmt"
	"os"
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
