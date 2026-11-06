package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// EditPlan 改配置之前先算好的：改前、改后的内容，改后校验出的问题，改到了哪些字段。
// 确认框里给用户看的就是它；用户同意后 Write 写入（文件在这期间被改过就不写）。
type EditPlan struct {
	Config   string   // ferry.yaml 的路径
	Local    bool     // 改的是这台电脑的本机设置（不是 ferry.yaml）
	File     string   // 实际写的文件（ferry.yaml，或数据目录里的 local.json）
	Old, New string   // 改前、改后的内容（\n 换行）
	Base     string   // 改前内容的摘要（sha256）：写入时文件变了就不写
	Problems []string // 改后校验出的问题：有问题时不能写
	Changed  []string // 改到的字段，如 apps.api.service.jvmArgs
	Danger   string   // 要特别提醒用户的（会执行的命令、登录方式……）

	raw   []byte // 改后要写的（保留原来的换行）
	old   any    // 改前解析出来的数据（看是不是生产服务器）
	local *Local
}

func digest(b []byte) string {
	if b == nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Unchanged 改前改后一样。
func (p *EditPlan) Unchanged() bool { return p.Old == p.New }

// PlanEdit 算好对 ferry.yaml 的修改（不写）。edits 有错（路径不对、写法没法自动改）时返回错误；
// 改后校验不通过时放在 Problems 里。
func PlanEdit(configPath string, edits []Edit) (*EditPlan, error) {
	if len(edits) == 0 {
		return nil, errors.New("没有要改的（edits 是空的）")
	}
	src, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	if k := peekKind(src); k == KindConnections {
		return nil, errors.New("这是「新建连接」保存的服务器，不是项目配置：请在 Ferry 里改")
	}
	out, err := ApplyEdits(src, edits)
	if err != nil {
		return nil, err
	}
	p := &EditPlan{Config: configPath, File: configPath, Base: digest(src), raw: out,
		Old: strings.ReplaceAll(string(src), "\r\n", "\n"), New: strings.ReplaceAll(string(out), "\r\n", "\n")}
	p.old, _ = decodeGeneric(p.Old)
	newData, _ := decodeGeneric(p.New)
	p.Changed = changedPaths(p.old, newData)
	if _, err := LoadContent(out, configPath); err != nil {
		p.Problems = problemLines(err)
	}
	return p, p.check()
}

func peekKind(src []byte) string {
	var head struct {
		Kind string `yaml:"kind"`
	}
	_ = yaml.Unmarshal(src, &head)
	return head.Kind
}

func problemLines(err error) []string {
	var out []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, " "))
		}
	}
	return out
}

// Write 写入（用户同意后）：文件在算好之后被改过、改后有问题时不写。
func (p *EditPlan) Write() error {
	if len(p.Problems) > 0 {
		return errors.New("改后的配置有问题，没有写入：\n" + strings.Join(p.Problems, "\n"))
	}
	cur, err := os.ReadFile(p.File)
	if err != nil && !(p.Local && errors.Is(err, os.ErrNotExist)) {
		return err
	}
	if digest(cur) != p.Base {
		return fmt.Errorf("%s 在确认期间被改过了，没有写入：请重新读一遍再改", p.File)
	}
	if p.Local {
		f, err := Load(p.Config)
		if err != nil {
			return err
		}
		return WriteLocal(f, p.local)
	}
	return writeAtomic(p.File, p.raw)
}

// ---------------------------------------------------------------- 本机设置

// PlanLocalEdit 算好对这台电脑的本机设置的修改（不写）。能改的见 LocalSchemaDoc。
func PlanLocalEdit(configPath string, edits []Edit) (*EditPlan, error) {
	if len(edits) == 0 {
		return nil, errors.New("没有要改的（edits 是空的）")
	}
	f, err := Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("ferry.yaml 有问题，先把它改好再改本机设置：%w", err)
	}
	if f.Kind == KindConnections {
		return nil, errors.New("这是「新建连接」保存的服务器，没有本机设置")
	}
	file := LocalFile(f.Project, f.Path)
	raw, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := ReadLocal(f.Project, f.Path)
	if err != nil {
		return nil, err
	}
	before := localJSON(l)
	var data any
	json.Unmarshal([]byte(before), &data)
	for i, e := range edits {
		path, err := ParseEditPath(e.Path)
		if err != nil {
			return nil, err
		}
		where := ""
		if len(edits) > 1 {
			where = fmt.Sprintf("第 %d 处（%s %s）：", i+1, e.Op, e.Path)
		}
		if err := localPathOK(path); err != nil {
			return nil, errors.New(where + err.Error())
		}
		var val any
		switch e.Op {
		case "set":
			var s string
			if json.Unmarshal(e.Value, &s) != nil {
				return nil, errors.New(where + "本机设置的值都是字符串")
			}
			val = s
		case "delete":
		default:
			return nil, errors.New(where + "本机设置只能 set 或 delete")
		}
		if data, err = genericApply(data, path, e.Op, val); err != nil {
			return nil, errors.New(where + err.Error())
		}
	}
	b, _ := json.Marshal(data)
	var nl Local
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&nl); err != nil {
		return nil, fmt.Errorf("本机设置的写法不对：%v", err)
	}
	for k, t := range nl.Targets {
		if t.Empty() {
			delete(nl.Targets, k)
		}
	}
	p := &EditPlan{Config: f.Path, Local: true, File: file, Base: digest(raw), local: &nl, Old: before, New: localJSON(&nl)}
	if raw == nil {
		p.Base = ""
	}
	if err := CheckLocal(f, &nl); err != nil {
		p.Problems = problemLines(err)
	}
	var od, nd any
	json.Unmarshal([]byte(p.Old), &od)
	json.Unmarshal([]byte(p.New), &nd)
	p.Changed = changedPaths(od, nd)
	// 生产服务器、不许 AI 改的：按 ferry.yaml 里的设置看
	var shared any
	if src, err := os.ReadFile(f.Path); err == nil {
		shared, _ = decodeGeneric(string(src))
	}
	p.old = shared
	return p, p.check()
}

func localJSON(l *Local) string {
	c := Local{Vars: l.Vars, Targets: map[string]*LocalTarget{}}
	for k, t := range l.Targets {
		if !t.Empty() {
			c.Targets[k] = t
		}
	}
	if len(c.Targets) == 0 {
		c.Targets = nil
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return string(b)
}

func localPathOK(p []string) error {
	bad := errors.New("本机设置能改的只有 vars.<变量>、targets.<服务器>.auth、targets.<服务器>.identityFile、targets.<服务器>.vars.<变量>")
	switch {
	case len(p) == 2 && p[0] == "vars":
	case len(p) == 3 && p[0] == "targets" && (p[2] == "auth" || p[2] == "identityFile"):
	case len(p) == 4 && p[0] == "targets" && p[2] == "vars":
	default:
		return bad
	}
	return nil
}

// ---------------------------------------------------------------- 改到了哪些字段、能不能改

// changedPaths 两份数据不一样的地方（列表里有 name 的项按 name 对上）。
func changedPaths(a, b any) []string {
	var out []string
	var walk func(x, y any, at []string)
	add := func(at []string) { out = append(out, strings.Join(quoteSegs(at), ".")) }
	walk = func(x, y any, at []string) {
		// 新加、删掉的一整块：和空的比，列出里面的每一项（新加的应用里有没有部署时执行的命令……）
		if x == nil {
			x = emptyLike(y)
		} else if y == nil {
			y = emptyLike(x)
		}
		xm, xok := x.(map[string]any)
		ym, yok := y.(map[string]any)
		if xok && yok {
			keys := map[string]bool{}
			for k := range xm {
				keys[k] = true
			}
			for k := range ym {
				keys[k] = true
			}
			for _, k := range sortedKeys(keys) {
				walk(xm[k], ym[k], append(at[:len(at):len(at)], k))
			}
			return
		}
		xl, xok := x.([]any)
		yl, yok := y.([]any)
		if xok && yok && named(xl) && named(yl) && len(xl)+len(yl) > 0 {
			names := map[string]bool{}
			find := func(l []any, n string) any {
				for _, it := range l {
					if m, _ := it.(map[string]any); m != nil && fmt.Sprint(m["name"]) == n {
						return it
					}
				}
				return nil
			}
			for _, l := range [][]any{xl, yl} {
				for _, it := range l {
					names[fmt.Sprint(it.(map[string]any)["name"])] = true
				}
			}
			for _, n := range sortedKeys(names) {
				walk(find(xl, n), find(yl, n), append(at[:len(at):len(at)], n))
			}
			return
		}
		if xok && yok && len(xl) == len(yl) {
			for i := range xl {
				walk(xl[i], yl[i], append(at[:len(at):len(at)], strconv.Itoa(i)))
			}
			return
		}
		if !sameData(x, y) {
			add(at)
		}
	}
	walk(a, b, nil)
	return out
}

func emptyLike(v any) any {
	switch v.(type) {
	case map[string]any:
		return map[string]any{}
	case []any:
		return []any{}
	}
	return nil
}

func quoteSegs(at []string) []string {
	out := make([]string, len(at))
	for i, s := range at {
		if strings.ContainsAny(s, ".[]") {
			s = "[" + s + "]"
		}
		out[i] = s
	}
	return out
}

func named(l []any) bool {
	for _, it := range l {
		m, ok := it.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m["name"].(string); !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// 改了要特别提醒用户的字段（规范的路径前缀 → 原因）
var riskyFields = []struct{ prefix, why string }{
	{"apps.[].build", "构建命令（部署时在这台电脑上执行）"},
	{"builds", "构建命令（部署时在这台电脑上执行）"},
	{"apps.[].hooks", "部署时在服务器上执行的命令"},
	{"apps.[].service.exec", "服务的启动命令"},
	{"apps.[].service.execStartPre", "服务启动前执行的命令"},
	{"apps.[].service.unitExtra", "systemd 单元文件的原样内容"},
	{"apps.[].service.java", "服务用的 java"},
	{"apps.[].service.user", "服务以谁的身份运行"},
	{"apps.[].static.reload", "切换后在服务器上执行的命令"},
	{"apps.[].static.server", "nginx 的原样配置"},
	{"backups.[].command", "以 root 执行的备份命令"},
	{"backups.[].restore", "以 root 执行的恢复命令"},
	{"targets.*.host", "服务器地址"},
	{"targets.*.port", "SSH 端口"},
	{"targets.*.user", "SSH 用户"},
	{"targets.*.auth", "登录方式"},
	{"targets.*.identityFile", "登录用的私钥"},
	{"targets.*.hostKey", "主机公钥指纹"},
	{"targets.*.sudo", "是否用 sudo"},
	{"targets.*.root", "部署根目录"},
	{"targets.*.ferryDir", "Ferry 组件目录"},
	{"targets.*.runAs", "服务默认的运行用户"},
}

// 只能由用户在 Ferry 里改的（AI 改了就能绕过限制）
var lockedFields = map[string]string{"production": "生产服务器", "aiChanges": "允许 AI 助手改动", "deployers": "谁能部署"}

func hasPrefixPath(p, prefix string) bool { return p == prefix || strings.HasPrefix(p, prefix+".") }

// check 不允许 AI 改的返回错误，要特别提醒的写进 Danger。
func (p *EditPlan) check() error {
	var whys []string
	seen := map[string]bool{}
	for _, c := range p.Changed {
		segs, err := ParseEditPath(c)
		if err != nil {
			continue
		}
		if len(segs) >= 2 && segs[0] == "targets" {
			t := segs[1]
			if !p.Local && len(segs) >= 3 {
				if what, ok := lockedFields[segs[2]]; ok {
					return fmt.Errorf("「%s」（targets.%s.%s）只能由用户在 Ferry 里改", what, t, segs[2])
				}
			}
			if p.targetFlag(t, "production") == true {
				return fmt.Errorf("%s 是生产服务器，它的配置不能由 AI 改：请用户在 Ferry 里改", t)
			}
			if p.targetFlag(t, "aiChanges") == false {
				return fmt.Errorf("%s 设了 aiChanges: false，它的配置不能由 AI 改：请用户在 Ferry 里改", t)
			}
		}
		why := ""
		if p.Local {
			if len(segs) == 3 && (segs[2] == "auth" || segs[2] == "identityFile") {
				why = "这台电脑登录服务器的方式"
			}
		} else {
			norm := NormalizePath(segs)
			for _, r := range riskyFields {
				if hasPrefixPath(norm, r.prefix) {
					why = r.why
					break
				}
			}
		}
		if why != "" && !seen[why] {
			seen[why] = true
			whys = append(whys, why)
		}
	}
	if !p.Local {
		for _, r := range p.removedItems() {
			whys = append(whys, "删掉了"+r)
		}
	}
	if len(whys) > 0 {
		p.Danger = "改了" + strings.Join(whys, "、")
	}
	return nil
}

// targetFlag 改前 ferry.yaml 里这台服务器的 production、aiChanges（没写时 nil）。
func (p *EditPlan) targetFlag(target, key string) any {
	m, _ := p.old.(map[string]any)
	ts, _ := m["targets"].(map[string]any)
	t, _ := ts[target].(map[string]any)
	return t[key]
}

// removedItems 删掉的整个应用、服务器、数据备份，如「应用 api」。
func (p *EditPlan) removedItems() []string {
	nd, _ := decodeGeneric(p.New)
	om, _ := p.old.(map[string]any)
	nm, _ := nd.(map[string]any)
	var out []string
	for _, k := range []struct{ key, what string }{{"targets", "服务器"}, {"apps", "应用"}, {"backups", "数据备份"}} {
		names := func(v any) map[string]bool {
			out := map[string]bool{}
			switch x := v.(type) {
			case map[string]any:
				for n := range x {
					out[n] = true
				}
			case []any:
				for _, it := range x {
					if m, ok := it.(map[string]any); ok {
						out[strings.ToLower(fmt.Sprint(m["name"]))] = true
					}
				}
			}
			return out
		}
		now := names(nm[k.key])
		for _, n := range sortedKeys(names(om[k.key])) {
			if !now[n] {
				out = append(out, k.what+" "+n)
			}
		}
	}
	return out
}
