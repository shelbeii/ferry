package swtpl

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Extra 安装时可以补充的配置项：模板参数没有覆盖到的设置（如 MySQL 的 innodb_buffer_pool_size），
// 由用户按「键 = 值」填写，安装时合并进配置文件、服务的环境变量或启动参数。
//
// 格式：
//
//	ini         合并进模板写的 ini 文件的 section 段（没写 section 时键写成「段.键」）
//	keyvalue    「键 值」一行一项（如 redis.conf）
//	properties  「键=值」一行一项（如 application.properties）；文件可以是模板写的，也可以是安装包里的（模板的 props 改过它）
//	env         服务的环境变量（写进只有 root 能读的 ferry.env）
//	args        加在启动命令末尾，每项写成 prefix + 键=值（如 PostgreSQL 的 "-c "、Spring Boot 的 "--"）
//
// 模板里引用了变量的项（端口、目录、密码……）由 Ferry 管理，不能在附加配置里改，protect 里再列出其他不能改的。
type Extra struct {
	ID      string    `yaml:"id" json:"id"`
	Label   string    `yaml:"label" json:"label"`
	Format  string    `yaml:"format" json:"format"`
	File    string    `yaml:"file,omitempty" json:"file,omitempty"`
	Section string    `yaml:"section,omitempty" json:"section,omitempty"`
	Prefix  string    `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	Help    string    `yaml:"help,omitempty" json:"help,omitempty"`
	Protect []string  `yaml:"protect,omitempty" json:"protect,omitempty"`
	Suggest []Suggest `yaml:"suggest,omitempty" json:"suggest,omitempty"`
	// Locked 不能在这里改的键（protect 加上模板里引用了变量的项；各种装法合在一起）
	Locked []string `yaml:"-" json:"locked,omitempty"`
}

// Suggest 常用的附加配置（界面给出选项、AI 参考）。
type Suggest struct {
	Key     string `yaml:"key" json:"key"`
	Example string `yaml:"example,omitempty" json:"example,omitempty"`
	Help    string `yaml:"help,omitempty" json:"help,omitempty"`
}

// ExtraItem 用户填的一项（已检查）。Section 只用于 ini。
type ExtraItem struct {
	Section string `json:"section,omitempty"`
	Key     string `json:"key"`
	Value   string `json:"value"`
}

// ExtraSet 一个附加配置里用户填的全部项。
type ExtraSet struct {
	Extra *Extra
	Items []ExtraItem
}

const (
	maxExtraItems = 100
	maxExtraValue = 4096
)

var (
	reExtraID     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	reExtraKey    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	rePropsKey    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-\[\]]{0,191}$`)
	reEnvKey      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	reINISection  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	reExtraPrefix = regexp.MustCompile(`^-{1,2}[A-Za-z]{0,8} ?$`)
)

// checkKey 键名的写法对不对（ini 的「段.键」由调用方先拆开）。
func (x *Extra) checkKey(k string) bool {
	switch x.Format {
	case "env":
		return reEnvKey.MatchString(k)
	case "properties":
		return rePropsKey.MatchString(k)
	}
	return reExtraKey.MatchString(k)
}

// norm 比较键名时用的写法：MySQL 的选项 - 和 _ 等价；redis、PostgreSQL 的键不区分大小写。
func (x *Extra) norm(section, key string) string {
	switch x.Format {
	case "ini":
		k := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
		if x.Section == "" {
			return strings.ToLower(section) + "." + k
		}
		return k
	case "keyvalue", "args":
		return strings.ToLower(key)
	}
	return key
}

// splitKey 用户写的键 → 段、键（只有没写 section 的 ini 要拆）。
func (x *Extra) splitKey(k string) (section, key string, err error) {
	if x.Format != "ini" || x.Section != "" {
		return x.Section, k, nil
	}
	section, key, ok := strings.Cut(k, ".")
	if !ok || !reINISection.MatchString(section) {
		return "", "", fmt.Errorf("「%s」的键要写成「段.键」（如 general.mediaServerId）：%q", x.Label, k)
	}
	return section, key, nil
}

// validateExtras 模板顶层 extras 的检查（Validate 调用）。
func (t *Template) validateExtras(keys map[string]bool) error {
	ids := map[string]bool{}
	for i := range t.Extras {
		x := &t.Extras[i]
		where := fmt.Sprintf("模板 %s 的附加配置 %q", t.ID, x.ID)
		if !reExtraID.MatchString(x.ID) {
			return fmt.Errorf("%s：id 只能用小写字母、数字、横线，以字母开头", where)
		}
		if ids[x.ID] {
			return fmt.Errorf("%s：id 重复", where)
		}
		ids[x.ID] = true
		if x.Label == "" {
			return fmt.Errorf("%s：缺少 label", where)
		}
		switch x.Format {
		case "ini", "keyvalue", "properties":
			if x.File == "" {
				return fmt.Errorf("%s：%s 格式要写 file（配置文件的路径）", where, x.Format)
			}
			for _, name := range varNames(x.File) {
				if !keys[name] && !builtinVars[name] {
					return fmt.Errorf("%s：file 用到了没有定义的变量 %s", where, name)
				}
			}
		case "env", "args":
			if x.File != "" {
				return fmt.Errorf("%s：%s 格式不写 file", where, x.Format)
			}
		default:
			return fmt.Errorf("%s：format 只能是 ini、keyvalue、properties、env、args", where)
		}
		if x.Section != "" && (x.Format != "ini" || !reINISection.MatchString(x.Section)) {
			return fmt.Errorf("%s：section 只用于 ini 格式，只能用字母、数字、_ . -", where)
		}
		if (x.Format == "args") != (x.Prefix != "") {
			return fmt.Errorf("%s：args 格式要写 prefix（如 \"-c \"、\"--\"），其他格式不写", where)
		}
		if x.Prefix != "" && !reExtraPrefix.MatchString(x.Prefix) {
			return fmt.Errorf("%s：prefix 要写成 \"-c \"、\"--\"、\"-D\" 这样：%q", where, x.Prefix)
		}
		for _, k := range x.Protect {
			if _, kk, err := x.splitKey(k); err != nil || !x.checkKey(kk) {
				return fmt.Errorf("%s：protect 里的键名无效：%q", where, k)
			}
		}
		applies := false
		locked := map[string]string{}
		for vi := range t.Variants {
			v := &t.Variants[vi]
			if !x.appliesTo(v) {
				continue
			}
			applies = true
			for n, info := range x.lockedIn(v) {
				locked[n] = info.show
			}
		}
		if !applies {
			return fmt.Errorf("%s：没有哪种装法写了 %s（ini 要由 files 写出；keyvalue、properties 要由 files 写出或在 props 里改过）", where, x.File)
		}
		x.Locked = nil
		for _, k := range locked {
			x.Locked = append(x.Locked, k)
		}
		sort.Strings(x.Locked)
		for _, s := range x.Suggest {
			sec, k, err := x.splitKey(s.Key)
			if err != nil || !x.checkKey(k) {
				return fmt.Errorf("%s：suggest 里的键名无效：%q", where, s.Key)
			}
			if _, ok := locked[x.norm(sec, k)]; ok {
				return fmt.Errorf("%s：suggest 里的 %s 由 Ferry 管理，不能建议修改", where, s.Key)
			}
		}
	}
	return nil
}

// appliesTo 这种装法能不能用这个附加配置。
func (x *Extra) appliesTo(v *Variant) bool {
	switch x.Format {
	case "env", "args":
		return true
	}
	for _, f := range v.Files {
		if f.Path == x.File {
			return true
		}
	}
	if x.Format != "ini" {
		for _, p := range v.Props {
			if p.File == x.File {
				return true
			}
		}
	}
	return false
}

// lockInfo 一个不能改的键：显示的写法、模板里它的值用到的变量（空 = protect 里列的）。
type lockInfo struct {
	show string
	vars []string
}

// lockedIn 这种装法里不能改的键：norm 写法 → 说明。
func (x *Extra) lockedIn(v *Variant) map[string]lockInfo {
	out := map[string]lockInfo{}
	add := func(section, key, val string) {
		n := x.norm(section, key)
		info := out[n]
		if info.show == "" {
			info.show = key
			if x.Format == "ini" && x.Section == "" {
				info.show = section + "." + key
			}
		}
		info.vars = append(info.vars, varNames(val)...)
		out[n] = info
	}
	for _, k := range x.Protect {
		sec, kk, _ := x.splitKey(k)
		add(sec, kk, "")
	}
	managed := func(val string) bool { return strings.Contains(val, "${") }
	switch x.Format {
	case "ini", "keyvalue", "properties":
		for _, f := range v.Files {
			if f.Path != x.File {
				continue
			}
			for _, l := range x.parse(f.Content) {
				if managed(l.value) && (x.Format != "ini" || x.Section == "" || strings.EqualFold(l.section, x.Section)) {
					add(l.section, l.key, l.value)
				}
			}
		}
		for _, p := range v.Props {
			if p.File != x.File {
				continue
			}
			for k, val := range p.Set {
				if managed(val) {
					add("", k, val)
				}
			}
		}
	case "env":
		for k, val := range v.Service.Env {
			if managed(val) {
				add("", k, val)
			}
		}
	case "args":
		flag := strings.TrimSpace(x.Prefix)
		toks := strings.Fields(v.Service.Exec)
		for i, tok := range toks {
			arg := ""
			switch {
			case strings.HasSuffix(x.Prefix, " "):
				if i > 0 && toks[i-1] == flag {
					arg = tok
				}
			case strings.HasPrefix(tok, x.Prefix):
				arg = tok[len(x.Prefix):]
			// Java 程序：--键 和 -D键 设置的是同一个 Spring 属性
			case x.Prefix == "--" && strings.HasPrefix(tok, "-D"):
				arg = tok[2:]
			}
			if k, val, ok := strings.Cut(strings.Trim(arg, `"'`), "="); ok && managed(val) {
				add("", k, val)
			}
		}
	}
	return out
}

// ExtraIn 模板里 id 对应的附加配置。
func (t *Template) ExtraIn(id string) *Extra {
	for i := range t.Extras {
		if t.Extras[i].ID == id {
			return &t.Extras[i]
		}
	}
	return nil
}

// ResolveExtras 检查用户填的附加配置（id → 键 → 值；只有一个附加配置时 id 可以写空），按模板中的顺序返回。
// 在服务器上做任何改动之前调用：有问题就停下。
func (t *Template) ResolveExtras(v *Variant, in map[string]map[string]string) ([]ExtraSet, error) {
	total := 0
	for _, kv := range in {
		total += len(kv)
	}
	if total == 0 {
		return nil, nil
	}
	if total > maxExtraItems {
		return nil, fmt.Errorf("附加配置最多 %d 项", maxExtraItems)
	}
	var usable []*Extra
	for i := range t.Extras {
		if t.Extras[i].appliesTo(v) {
			usable = append(usable, &t.Extras[i])
		}
	}
	if len(t.Extras) == 0 {
		return nil, fmt.Errorf("%s 的模板没有可以补充的配置项：用模板参数，或装好后在「配置」里改", t.Name)
	}
	byID := map[string]map[string]string{}
	for id, kv := range in {
		if len(kv) == 0 {
			continue
		}
		if id == "" {
			if len(usable) != 1 {
				return nil, fmt.Errorf("%s 有几处可以补充配置（%s）：写明是哪一处", t.Name, extraIDs(usable))
			}
			id = usable[0].ID
		}
		x := t.ExtraIn(id)
		if x == nil {
			return nil, fmt.Errorf("%s 没有附加配置 %s（可用：%s）", t.Name, id, extraIDs(usable))
		}
		if !x.appliesTo(v) {
			return nil, fmt.Errorf("%s 的「%s」这种装法不能补充「%s」", t.Name, v.Label, x.Label)
		}
		if byID[id] == nil {
			byID[id] = map[string]string{}
		}
		for k, val := range kv {
			if _, dup := byID[id][k]; dup {
				return nil, fmt.Errorf("「%s」里的 %s 写了两次", x.Label, k)
			}
			byID[id][k] = val
		}
	}
	var out []ExtraSet
	for _, x := range usable {
		kv := byID[x.ID]
		if len(kv) == 0 {
			continue
		}
		locked := x.lockedIn(v)
		seen := map[string]string{}
		set := ExtraSet{Extra: x}
		for raw, val := range kv {
			k := strings.TrimSpace(raw)
			sec, key, err := x.splitKey(k)
			if err != nil {
				return nil, err
			}
			if !x.checkKey(key) {
				return nil, fmt.Errorf("「%s」的键名 %q 无效：只能用字母、数字和 _ . -", x.Label, raw)
			}
			n := x.norm(sec, key)
			if info, ok := locked[n]; ok {
				return nil, fmt.Errorf("「%s」里的 %s %s，不能在附加配置里改", x.Label, k, t.lockReason(info))
			}
			if prev, ok := seen[n]; ok {
				return nil, fmt.Errorf("「%s」里的 %s 和 %s 是同一项，只能写一个", x.Label, prev, k)
			}
			seen[n] = k
			val = strings.TrimSpace(val)
			if err := x.checkValue(k, val); err != nil {
				return nil, err
			}
			set.Items = append(set.Items, ExtraItem{Section: sec, Key: key, Value: val})
		}
		sort.Slice(set.Items, func(i, j int) bool {
			if set.Items[i].Section != set.Items[j].Section {
				return set.Items[i].Section < set.Items[j].Section
			}
			return set.Items[i].Key < set.Items[j].Key
		})
		out = append(out, set)
	}
	return out, nil
}

// lockReason 为什么不能改：由参数决定的说是哪个参数。
func (t *Template) lockReason(info lockInfo) string {
	var labels []string
	for _, name := range dedupe(info.vars) {
		for _, p := range t.Params {
			if p.Key == name {
				label := p.Label
				if label == "" {
					label = p.Key
				}
				labels = append(labels, "「"+label+"」")
			}
		}
	}
	switch {
	case len(labels) > 0:
		return "由参数" + strings.Join(labels, "、") + "决定，请改参数"
	case len(info.vars) > 0:
		return "由 Ferry 管理（安装目录、实例名等）"
	}
	return "会影响 Ferry 运行、检查这个软件"
}

func (x *Extra) checkValue(k, val string) error {
	switch {
	case len(val) > maxExtraValue:
		return fmt.Errorf("「%s」里 %s 的值太长（最多 %d 字节）", x.Label, k, maxExtraValue)
	case strings.ContainsAny(val, "\r\n\x00"):
		return fmt.Errorf("「%s」里 %s 的值不能换行", x.Label, k)
	case !utf8.ValidString(val):
		return fmt.Errorf("「%s」里 %s 的值不是有效的文字", x.Label, k)
	case val == "" && x.Format == "keyvalue":
		return fmt.Errorf("「%s」里 %s 要写值", x.Label, k)
	}
	return nil
}

func extraIDs(list []*Extra) string {
	if len(list) == 0 {
		return "这种装法没有"
	}
	var s []string
	for _, x := range list {
		s = append(s, x.ID+"「"+x.Label+"」")
	}
	return strings.Join(s, "、")
}

// ---------------------------------------------------------------- 合并

type confLine struct {
	section, key, value string
}

// lineKey 一行的键和值；不是配置项（空行、注释、段名）时 ok 为 false。
func (x *Extra) lineKey(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || t[0] == '#' || t[0] == ';' || t[0] == '!' || t[0] == '[' {
		return "", "", false
	}
	switch x.Format {
	case "keyvalue":
		if i := strings.IndexAny(t, " \t"); i >= 0 {
			return t[:i], strings.TrimSpace(t[i:]), true
		}
		return t, "", true
	case "properties":
		if i := strings.IndexAny(t, "=:"); i >= 0 {
			return strings.TrimSpace(t[:i]), strings.TrimSpace(t[i+1:]), true
		}
		return t, "", true
	}
	k, val, _ := strings.Cut(t, "=")
	return strings.TrimSpace(k), strings.TrimSpace(val), true
}

func iniSection(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
		return strings.TrimSpace(t[1 : len(t)-1]), true
	}
	return "", false
}

// parse 配置文件里的全部项。
func (x *Extra) parse(content string) []confLine {
	var out []confLine
	section := ""
	for _, l := range strings.Split(content, "\n") {
		if x.Format == "ini" {
			if s, ok := iniSection(l); ok {
				section = s
				continue
			}
		}
		if k, val, ok := x.lineKey(l); ok {
			out = append(out, confLine{section: section, key: k, value: val})
		}
	}
	return out
}

func (x *Extra) line(it ExtraItem) string {
	switch x.Format {
	case "keyvalue":
		return it.Key + " " + it.Value
	case "ini":
		if it.Value == "" {
			return it.Key // 如 MySQL 的 skip-name-resolve
		}
	}
	return it.Key + "=" + it.Value
}

// Merge 把附加配置合并进配置文件的内容：同名的项改成填的值（重复的只留一处），没有的加上（ini 加在对应段的末尾，没有这一段时在文件末尾新建）。
func (s ExtraSet) Merge(content string) string {
	x := s.Extra
	want := map[string]ExtraItem{}
	for _, it := range s.Items {
		want[x.norm(it.Section, it.Key)] = it
	}
	lines := strings.Split(content, "\n")
	trailingNL := strings.HasSuffix(content, "\n")
	if trailingNL || content == "" {
		lines = lines[:len(lines)-1]
	}
	done := map[string]bool{}
	var out []string
	// 每一段最后一个非空行之后的位置：没有的项加在这里
	lastIn := map[string]int{}
	section := ""
	for _, l := range lines {
		if x.Format == "ini" {
			if sec, ok := iniSection(l); ok {
				section = sec
				out = append(out, l)
				lastIn[strings.ToLower(section)] = len(out)
				continue
			}
		}
		if k, _, ok := x.lineKey(l); ok && x.inScope(section) {
			n := x.norm(section, k)
			if it, hit := want[n]; hit {
				if done[n] {
					continue // 同一项写了几次：只留第一处
				}
				done[n] = true
				l = x.line(it)
			}
		}
		out = append(out, l)
		if strings.TrimSpace(l) != "" {
			lastIn[strings.ToLower(section)] = len(out)
		}
	}
	// 按段收集还没写进去的
	var order []string
	missing := map[string][]string{}
	for _, it := range s.Items {
		if done[x.norm(it.Section, it.Key)] {
			continue
		}
		sec := strings.ToLower(it.Section)
		if _, ok := missing[sec]; !ok {
			order = append(order, sec)
		}
		missing[sec] = append(missing[sec], x.line(it))
	}
	type insert struct {
		sec string
		pos int
	}
	var ins []insert
	for _, sec := range order {
		pos, ok := lastIn[sec]
		if x.Format != "ini" && !ok {
			pos, ok = len(out), true
		}
		if ok {
			ins = append(ins, insert{sec, pos})
		}
	}
	sort.SliceStable(ins, func(i, j int) bool { return ins[i].pos > ins[j].pos }) // 从后往前插，前面的位置不变
	for _, in := range ins {
		out = append(out[:in.pos], append(append([]string{}, missing[in.sec]...), out[in.pos:]...)...)
		delete(missing, in.sec)
	}
	for _, sec := range order { // ini 里没有的段：加在文件末尾
		ls, ok := missing[sec]
		if !ok {
			continue
		}
		name := sec
		for _, it := range s.Items {
			if strings.ToLower(it.Section) == sec {
				name = it.Section
				break
			}
		}
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, "["+name+"]")
		out = append(out, ls...)
	}
	res := strings.Join(out, "\n")
	if trailingNL || len(content) == 0 {
		res += "\n"
	}
	return res
}

// inScope ini 里这一段的项归不归这个附加配置管。
func (x *Extra) inScope(section string) bool {
	return x.Format != "ini" || x.Section == "" || strings.EqualFold(section, x.Section)
}

// Set 不能合并进文件内容时（安装包里的文件），交给 agent 改的键值（props）。
func (s ExtraSet) Set() map[string]string {
	m := map[string]string{}
	for _, it := range s.Items {
		m[it.Key] = it.Value
	}
	return m
}

// Sep props 的分隔符。
func (s ExtraSet) Sep() string {
	if s.Extra.Format == "keyvalue" {
		return " "
	}
	return "="
}

// Args args 格式：加在启动命令末尾的参数（已按 systemd 的规则加引号、转义 $）。
func (s ExtraSet) Args() string {
	var b strings.Builder
	for _, it := range s.Items {
		b.WriteString(" ")
		arg := it.Key + "=" + it.Value
		if strings.HasSuffix(s.Extra.Prefix, " ") {
			b.WriteString(s.Extra.Prefix + SystemdArg(arg))
		} else {
			b.WriteString(SystemdArg(s.Extra.Prefix + arg))
		}
	}
	return b.String()
}

// SystemdArg ExecStart 里的一个参数：$ 写成 $$（不展开环境变量）；有空白、引号、反斜杠、分号时加双引号。% 由 agent 统一转义。
func SystemdArg(s string) string {
	s = strings.ReplaceAll(s, "$", "$$")
	if s != "" && !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// EnvValue EnvironmentFile 里的值：有引号、反斜杠或首尾空白时加双引号并转义。
func EnvValue(s string) string {
	if !strings.ContainsAny(s, "\"'\\") && strings.TrimSpace(s) == s {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
