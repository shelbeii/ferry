package config

import (
	"fmt"
	"os"
	"strings"
)

// Scope 变量作用域。查找顺序：内置变量 → 应用 vars → 目标 vars → 项目 vars。
//
//	${name}      变量；变量的值里还可以再引用变量
//	${env:NAME}  本机环境变量（未设置时报错）
//	${each:name} 只在构建命令里：同一条构建的各应用的 name 变量，逗号连接
//	$${          字面的 ${
//
// 内置变量（部署时）：project target app root appDir releases current shared release user host
// artifact（current 下的程序文件）java（java 可执行文件）。
type Scope struct {
	Builtins map[string]string
	Layers   []map[string]string
	Each     func(name string) (string, error)
}

// Expand 展开字符串里的变量。
func (sc *Scope) Expand(s string) (string, error) { return sc.expand(s, 0) }

// ExpandAll 展开列表中的每一项。
func (sc *Scope) ExpandAll(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		v, err := sc.Expand(s)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ExpandMap 展开 map 的值。
func (sc *Scope) ExpandMap(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for k, s := range in {
		v, err := sc.Expand(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

func (sc *Scope) expand(s string, depth int) (string, error) {
	if depth > 8 {
		return "", fmt.Errorf("变量引用层数过多（循环引用？）：%q", s)
	}
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "$${") {
			b.WriteString("${")
			i += 3
			continue
		}
		if strings.HasPrefix(s[i:], "${") {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("变量没有闭合：%q", s)
			}
			name := strings.TrimSpace(s[i+2 : i+2+end])
			v, err := sc.resolve(name, depth)
			if err != nil {
				return "", err
			}
			b.WriteString(v)
			i += 3 + end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), nil
}

func (sc *Scope) resolve(name string, depth int) (string, error) {
	switch {
	case name == "":
		return "", fmt.Errorf("空变量 ${}")
	case strings.HasPrefix(name, "env:"):
		key := strings.TrimPrefix(name, "env:")
		v, ok := os.LookupEnv(key)
		if !ok {
			return "", fmt.Errorf("本机环境变量 %s 未设置", key)
		}
		return v, nil
	case strings.HasPrefix(name, "each:"):
		if sc.Each == nil {
			return "", fmt.Errorf("${%s} 只能用在构建命令里", name)
		}
		return sc.Each(strings.TrimPrefix(name, "each:"))
	}
	if v, ok := sc.Builtins[name]; ok {
		return v, nil
	}
	for _, layer := range sc.Layers {
		if v, ok := layer[name]; ok {
			return sc.expand(v, depth+1)
		}
	}
	return "", fmt.Errorf("未定义的变量 ${%s}", name)
}

// BuiltinNames 内置变量名（不能在 vars 中重新定义）。
var BuiltinNames = []string{
	"project", "target", "app", "root", "appDir", "versions", "releases", "running", "current", "logs", "data", "shared",
	"release", "user", "host", "artifact", "java", "configDir",
}
