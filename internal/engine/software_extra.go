package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/swpkg"
	"github.com/captain/ferry/internal/swtpl"
)

// extrasWritten 写进文件的附加配置，这次安装要真的写那个文件（模板里写它的条件按参数满足）。
// 在服务器上做任何改动之前检查。
func extrasWritten(v *swtpl.Variant, sets []swtpl.ExtraSet, vars swtpl.Vars) error {
	for _, s := range sets {
		x := s.Extra
		if x.Format == "env" || x.Format == "args" {
			continue
		}
		ok := false
		for _, f := range v.Files {
			ok = ok || (f.Path == x.File && vars.Cond(f.If))
		}
		if x.Format != "ini" { // keyvalue、properties：模板的 props 改过的文件（安装包里的）也行
			for _, p := range v.Props {
				ok = ok || p.File == x.File
			}
		}
		if !ok {
			return fmt.Errorf("按现在的参数这次安装不写 %s，「%s」用不上", x.File, x.Label)
		}
	}
	return nil
}

// applyExtras 把附加配置合并进渲染好的安装计划：模板写的配置文件直接改内容，安装包里的文件交给 agent 改（props），
// 环境变量写进 ferry.env，启动参数加在启动命令末尾。用户填的值不展开变量。
func applyExtras(pl *proto.SoftwarePlan, sets []swtpl.ExtraSet, vars swtpl.Vars) error {
	for _, s := range sets {
		x := s.Extra
		switch x.Format {
		case "ini", "keyvalue", "properties":
			file, err := vars.Expand(x.File)
			if err != nil {
				return fmt.Errorf("附加配置「%s」的文件：%w", x.Label, err)
			}
			merged := false
			for i := range pl.Files {
				if pl.Files[i].Path == file {
					pl.Files[i].Content = s.Merge(pl.Files[i].Content)
					merged = true
				}
			}
			// 模板的 props 也改这个文件时，它们在写文件之后执行：附加配置再交给 agent 改一次，排在模板的后面
			props := false
			for _, p := range pl.Props {
				props = props || p.File == file
			}
			switch {
			case x.Format == "ini" && !merged:
				return fmt.Errorf("这次安装不写 %s，「%s」用不上", file, x.Label)
			case !merged || props:
				pl.Props = append(pl.Props, proto.PlanProps{File: file, Sep: s.Sep(), Set: s.Set()})
			}
		case "env":
			if pl.Env == nil {
				pl.Env = map[string]string{}
			}
			for _, it := range s.Items {
				pl.Env[it.Key] = swtpl.EnvValue(it.Value)
			}
		case "args":
			pl.Exec += s.Args()
		default:
			return fmt.Errorf("附加配置「%s」的格式 %s 不认识", x.Label, x.Format)
		}
	}
	return nil
}

func extrasPreview(sets []swtpl.ExtraSet, vars swtpl.Vars) []ExtraPreview {
	var out []ExtraPreview
	for _, s := range sets {
		file, _ := vars.Expand(s.Extra.File)
		out = append(out, ExtraPreview{ID: s.Extra.ID, Label: s.Extra.Label, Format: s.Extra.Format, File: file, Items: s.Items})
	}
	return out
}

// extraItemText 一项附加配置给人看的写法。
func extraItemText(format string, it swtpl.ExtraItem) string {
	k := it.Key
	if it.Section != "" && format == "ini" {
		k = "[" + it.Section + "] " + k
	}
	if format == "keyvalue" {
		return k + " " + it.Value
	}
	return k + "=" + it.Value
}

// extrasRendered 预检时给用户看的附加配置：除了填的项，还有合并后的配置文件、启动命令。
// 先按现在的参数渲染一次安装计划（JDK、安装包的位置用占位），参数里的密码遮住；渲染不了时只列填的项。
func extrasRendered(tpl *swtpl.Template, v *swtpl.Variant, vars swtpl.Vars, sets []swtpl.ExtraSet, version string, java int, pkg *swpkg.Meta) []ExtraPreview {
	out := extrasPreview(sets, vars)
	if len(out) == 0 {
		return out
	}
	vs := swtpl.Vars{}
	for k, val := range vars {
		vs[k] = val
	}
	vs["version"], vs["package"], vs["java"] = version, "<安装包>", ""
	if java > 0 {
		vs["java"] = "<JDK " + strconv.Itoa(java) + ">/bin/java"
	}
	var secrets []string
	for _, p := range tpl.Params {
		if p.Type == "password" && len(vs[p.Key]) >= 4 {
			secrets = append(secrets, vs[p.Key])
		}
	}
	mask := func(s string) string {
		for _, sec := range secrets {
			s = strings.ReplaceAll(s, sec, "******")
		}
		return s
	}
	pl, err := renderPlan(tpl, v, vs, pkg)
	if err != nil || applyExtras(pl, sets, vs) != nil {
		return out
	}
	for i, s := range sets {
		switch s.Extra.Format {
		case "args":
			out[i].Exec = mask(pl.Exec)
		case "ini", "keyvalue", "properties":
			for _, f := range pl.Files { // 同一个文件写了几次（如就绪后改写）：给最后留下的那份
				if f.Path == out[i].File {
					out[i].Content = mask(f.Content)
				}
			}
		}
	}
	return out
}

// ParseExtras 附加配置的 JSON：{"附加配置ID": {"名称": "值"}}，模板只有一处附加配置时也可以直接写 {"名称": "值"}。
// 值可以是文字、数字或 true / false（模型常把 max_connections 写成数字）。
func ParseExtras(raw []byte) (map[string]map[string]string, error) {
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("附加配置要写成 JSON 对象 {\"名称\": \"值\"}：%v", err)
	}
	out := map[string]map[string]string{}
	put := func(id, k string, v json.RawMessage) error {
		val, err := extraValue(v)
		if err != nil {
			return fmt.Errorf("附加配置 %s %v", k, err)
		}
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		if _, dup := out[id][k]; dup {
			return fmt.Errorf("附加配置 %s 写了两次", k)
		}
		out[id][k] = val
		return nil
	}
	for id, v := range top {
		var nested map[string]json.RawMessage
		if json.Unmarshal(v, &nested) == nil && nested != nil {
			for k, val := range nested {
				if err := put(id, k, val); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := put("", id, v); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func extraValue(v json.RawMessage) (string, error) {
	if strings.TrimSpace(string(v)) == "null" {
		return "", errors.New("的值要写成文字或数字")
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s, nil
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(v)))
	dec.UseNumber()
	if dec.Decode(&n) == nil {
		return n.String(), nil
	}
	var b bool
	if json.Unmarshal(v, &b) == nil {
		return strconv.FormatBool(b), nil
	}
	return "", errors.New("的值要写成文字或数字")
}
