// Package swtpl 软件模板：一种软件怎么装（参数、按服务器系统分的变体、安装步骤）。
//
// 内置模板随 Ferry 发布（builtin/*.yaml），用户自己的模板放在数据目录的 software/templates 下，
// 同 ID 时用户的覆盖内置的。安装时按服务器的系统、架构选出变体，填上参数渲染成 proto.SoftwarePlan 交给 agent 执行。
//
// 字符串里可以用 ${变量}：
//
//	${name} ${dir} ${user} ${version} ${java} ${package} ${host}   内置变量
//	${参数}                    参数的值（bool 为 true / false）
//	${参数?是|否}              bool 参数为 true 时取「是」，否则取「否」
//	${参数+1000}               数字参数加上一个数
//	${random:24}               随机字母数字串；${base64random:48} 再做 base64
package swtpl

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/captain/ferry/internal/proto"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Template 一种软件。
type Template struct {
	ID       string `yaml:"id" json:"id"`
	Name     string `yaml:"name" json:"name"`
	Category string `yaml:"category" json:"category"` // 数据库、缓存、中间件、存储、其他……
	Desc     string `yaml:"desc" json:"desc"`
	Homepage string `yaml:"homepage,omitempty" json:"homepage,omitempty"`
	// Origin 谁写的：ai = AI 助手保存的（安装前界面会提醒看一下步骤）；空 = 内置或用户自己写的
	Origin   string    `yaml:"origin,omitempty" json:"origin,omitempty"`
	Params   []Param   `yaml:"params,omitempty" json:"params"`
	Variants []Variant `yaml:"variants" json:"variants"`

	Builtin bool   `yaml:"-" json:"builtin"`
	File    string `yaml:"-" json:"file,omitempty"` // 用户模板的文件
	// Overrides 用户模板覆盖了同 ID 的内置模板
	Overrides bool `yaml:"-" json:"overrides,omitempty"`
}

// Param 安装时填的一个参数（界面按它生成表单）。
type Param struct {
	Key     string   `yaml:"key" json:"key"`
	Label   string   `yaml:"label" json:"label"`
	Type    string   `yaml:"type" json:"type"` // text | port | password | account | bool | memory | select | number
	Default string   `yaml:"default,omitempty" json:"default,omitempty"`
	Options []string `yaml:"options,omitempty" json:"options,omitempty"` // select 的选项
	Help    string   `yaml:"help,omitempty" json:"help,omitempty"`
	// Offsets port 参数：还要占用的端口（主端口加上这些数，如 Nacos 的 +1000）
	Offsets []int `yaml:"offsets,omitempty" json:"offsets,omitempty"`
	// Proto port 参数：tcp（默认）| udp | both（TCP 和 UDP 都用）
	Proto string `yaml:"proto,omitempty" json:"proto,omitempty"`
}

// TCP、UDP 端口参数用不用 TCP / UDP。
func (p *Param) TCP() bool { return p.Proto != "udp" }
func (p *Param) UDP() bool { return p.Proto == "udp" || p.Proto == "both" }

// Variant 一种装法：适用的系统与架构、程序从哪来、怎么配置和运行。
type Variant struct {
	Label string   `yaml:"label" json:"label"`                   // 如「官方安装包」「系统软件源」
	OS    []string `yaml:"os,omitempty" json:"os,omitempty"`     // /etc/os-release 的 ID 或 ID_LIKE（rhel、debian……）；空 = 任何 Linux
	Arch  []string `yaml:"arch,omitempty" json:"arch,omitempty"` // amd64 / arm64；空 = 任何
	Note  string   `yaml:"note,omitempty" json:"note,omitempty"` // 给用户看的说明（如需要服务器能访问软件源）
	Java  int      `yaml:"java,omitempty" json:"java,omitempty"` // 需要 Ferry 提供的 JDK
	User  string   `yaml:"user" json:"user"`                     // 默认运行用户

	Package *Package `yaml:"package,omitempty" json:"package,omitempty"` // 上传安装包
	// System 用服务器的软件源装这些包（dnf / yum / apt 自动选）
	System *System `yaml:"system,omitempty" json:"system,omitempty"`

	Files   []File              `yaml:"files,omitempty" json:"files,omitempty"`
	Props   []Props             `yaml:"props,omitempty" json:"props,omitempty"`
	Steps   []Step              `yaml:"steps,omitempty" json:"steps,omitempty"`
	Service Service             `yaml:"service" json:"service"`
	Logs    []string            `yaml:"logs,omitempty" json:"logs,omitempty"` // 软件自己的日志文件（第一个为主日志）
	Notes   []string            `yaml:"notes,omitempty" json:"notes,omitempty"`
	Configs []ConfigFile        `yaml:"configs,omitempty" json:"configs,omitempty"`
	Hosting *proto.NginxHosting `yaml:"hosting,omitempty" json:"hosting,omitempty"`
}

type ConfigFile struct {
	Label  string `yaml:"label" json:"label"`
	Path   string `yaml:"path" json:"path"`
	Format string `yaml:"format,omitempty" json:"format,omitempty"`
	Apply  string `yaml:"apply,omitempty" json:"apply,omitempty"`
}

// Package 上传到服务器的安装包。
type Package struct {
	URL      string   `yaml:"url,omitempty" json:"url,omitempty"`           // 下载地址；空 = 只能导入
	File     string   `yaml:"file,omitempty" json:"file,omitempty"`         // 官方文件名（导入时按它认出是哪个模板、哪个版本）
	GitHub   string   `yaml:"github,omitempty" json:"github,omitempty"`     // 最新版本看这个仓库的 releases/latest；下载走 GitHub 加速
	Probe    string   `yaml:"probe,omitempty" json:"probe,omitempty"`       // 最新版本：从 fallback 起把最后一段加 1，试这个地址是否存在
	Fallback string   `yaml:"fallback,omitempty" json:"fallback,omitempty"` // 查不到最新版本时用的版本
	Mirrors  []string `yaml:"mirrors,omitempty" json:"mirrors,omitempty"`   // url 下载不到时依次试这些地址
	Size     string   `yaml:"size,omitempty" json:"size,omitempty"`         // 大约多大
	Format   string   `yaml:"format,omitempty" json:"format,omitempty"`     // tar.gz | tar.xz | zip | jar | binary；空 = 按文件名
	Strip    int      `yaml:"strip,omitempty" json:"strip,omitempty"`       // 解压时去掉几层目录
	Into     string   `yaml:"into,omitempty" json:"into,omitempty"`         // 解到安装目录下的哪里（jar / binary 为文件名）
	Check    string   `yaml:"check,omitempty" json:"check,omitempty"`       // 压缩包里必须有的文件（去掉 strip 之后的路径）
	MD5URL   string   `yaml:"md5,omitempty" json:"md5,omitempty"`           // 校验用的 md5 文件地址
}

// System 用服务器的软件源安装。
type System struct {
	DNF     []string `yaml:"dnf,omitempty" json:"dnf,omitempty"`         // dnf / yum 的包名
	APT     []string `yaml:"apt,omitempty" json:"apt,omitempty"`         // apt 的包名
	Disable []string `yaml:"disable,omitempty" json:"disable,omitempty"` // 装完后停掉并禁用的系统自带服务（Ferry 用自己的服务运行）
	Require string   `yaml:"require,omitempty" json:"require,omitempty"` // 装完后必须有的程序
}

// File 要写的文件。
type File struct {
	Path    string `yaml:"path" json:"path"`
	Content string `yaml:"content" json:"content"`
	Mode    string `yaml:"mode,omitempty" json:"mode,omitempty"` // 默认 0640
	At      string `yaml:"at,omitempty" json:"at,omitempty"`     // install（默认）| ready（就绪后改写）
	If      string `yaml:"if,omitempty" json:"if,omitempty"`     // bool 参数为 true 时才写（!参数 = 为 false 时）
}

// Props 修改配置文件里的项（key=value 或 key value）。
type Props struct {
	File string            `yaml:"file" json:"file"`
	Sep  string            `yaml:"sep,omitempty" json:"sep,omitempty"` // 默认 =
	Set  map[string]string `yaml:"set" json:"set"`
}

// Step 在服务器上执行的一步（root）。
type Step struct {
	At      string `yaml:"at,omitempty" json:"at,omitempty"` // prepare（默认，交给运行用户之前）| init（交给运行用户之后、启动之前）| ready（启动就绪之后）
	Run     string `yaml:"run,omitempty" json:"run,omitempty"`
	HTTP    *HTTP  `yaml:"http,omitempty" json:"http,omitempty"`
	Timeout int    `yaml:"timeout,omitempty" json:"timeout,omitempty"` // 秒，默认 600
	Error   string `yaml:"error,omitempty" json:"error,omitempty"`     // 失败时告诉用户的话
	Soft    string `yaml:"soft,omitempty" json:"soft,omitempty"`       // 失败不算安装失败，只提示这句话
	If      string `yaml:"if,omitempty" json:"if,omitempty"`
	// Sets 这一步让哪个参数生效（如 password：设置管理员密码）；soft 失败时 Ferry 不把这个值当作可用的交给用户
	Sets string `yaml:"sets,omitempty" json:"sets,omitempty"`
}

// HTTP 就绪后调用的接口（如初始化管理员密码）。
type HTTP struct {
	Post  string            `yaml:"post" json:"post"`
	Form  map[string]string `yaml:"form,omitempty" json:"form,omitempty"`
	Retry int               `yaml:"retry,omitempty" json:"retry,omitempty"` // 重试秒数
}

// Service 怎么运行（systemd 单元 ferry-<名称>.service）。
type Service struct {
	Exec    string            `yaml:"exec" json:"exec"`
	Workdir string            `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"` // 写进只有 root 能读的文件（密码放这里）
	Ready   Ready             `yaml:"ready" json:"ready"`
}

// Ready 就绪判断：端口能连上。
type Ready struct {
	Port    string `yaml:"port" json:"port"`
	Timeout int    `yaml:"timeout,omitempty" json:"timeout,omitempty"` // 秒，默认 180
}

//go:embed builtin/*.yaml
var builtinFS embed.FS

var reID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// Parse 解析并检查一个模板。
func Parse(b []byte) (*Template, error) {
	var t Template
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("模板格式不对：%w", err)
	}
	return &t, t.Validate()
}

var paramTypes = map[string]bool{"text": true, "port": true, "password": true, "account": true, "bool": true, "memory": true, "select": true, "number": true}

// Validate 检查模板。
func (t *Template) Validate() error {
	if !reID.MatchString(t.ID) {
		return fmt.Errorf("模板 id 只能用小写字母、数字、横线，以字母开头：%q", t.ID)
	}
	if t.Name == "" {
		return fmt.Errorf("模板 %s 缺少 name", t.ID)
	}
	keys := map[string]bool{}
	for _, p := range t.Params {
		if !regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`).MatchString(p.Key) {
			return fmt.Errorf("模板 %s：参数名无效 %q", t.ID, p.Key)
		}
		if builtinVars[p.Key] {
			return fmt.Errorf("模板 %s：参数名 %s 和内置变量重名", t.ID, p.Key)
		}
		if !paramTypes[p.Type] {
			return fmt.Errorf("模板 %s：参数 %s 的类型 %q 不认识（text、port、password、account、bool、memory、select、number）", t.ID, p.Key, p.Type)
		}
		switch p.Proto {
		case "", "tcp", "udp", "both":
			if p.Proto != "" && p.Type != "port" {
				return fmt.Errorf("模板 %s：参数 %s 不是 port，不能写 proto", t.ID, p.Key)
			}
		default:
			return fmt.Errorf("模板 %s：参数 %s 的 proto 只能是 tcp、udp、both", t.ID, p.Key)
		}
		keys[p.Key] = true
	}
	if len(t.Variants) == 0 {
		return fmt.Errorf("模板 %s 没有 variants", t.ID)
	}
	for i, v := range t.Variants {
		where := fmt.Sprintf("模板 %s 的第 %d 种装法", t.ID, i+1)
		if v.Package == nil && v.System == nil {
			return fmt.Errorf("%s：要写 package（上传安装包）或 system（系统软件源）", where)
		}
		if v.Service.Exec == "" || v.Service.Ready.Port == "" {
			return fmt.Errorf("%s：缺少 service.exec 或 service.ready.port", where)
		}
		if v.User == "" || v.User == "root" {
			return fmt.Errorf("%s：user 要写一个非 root 的运行用户", where)
		}
		for _, c := range v.Configs {
			if c.Path == "" || (c.Apply != "" && c.Apply != "reload" && c.Apply != "restart") {
				return fmt.Errorf("%s：configs 需要 path，apply 只能是 reload 或 restart", where)
			}
		}
		if v.Hosting != nil && t.ID != "nginx" {
			return fmt.Errorf("%s：hosting 只用于 nginx 模板", where)
		}
		if p := v.Package; p != nil {
			switch p.Format {
			case "", "tar.gz", "tar.xz", "zip", "jar", "binary":
			default:
				return fmt.Errorf("%s：package.format 只能是 tar.gz、tar.xz、zip、jar、binary", where)
			}
		}
		for _, s := range v.Steps {
			if (s.Run == "") == (s.HTTP == nil) {
				return fmt.Errorf("%s：每一步要写 run 或 http 其中之一", where)
			}
			switch s.At {
			case "", "prepare", "init", "ready":
			default:
				return fmt.Errorf("%s：步骤的 at 只能是 prepare、init、ready", where)
			}
			if s.Sets != "" && !keys[s.Sets] {
				return fmt.Errorf("%s：步骤的 sets 要写参数名，没有参数 %s", where, s.Sets)
			}
		}
		// 用到的变量都要有
		var unknown []string
		walk(&v, func(s string) {
			for _, name := range varNames(s) {
				if !keys[name] && !builtinVars[name] {
					unknown = append(unknown, name)
				}
			}
		})
		if len(unknown) > 0 {
			return fmt.Errorf("%s：用到了没有定义的变量 %s", where, strings.Join(dedupe(unknown), "、"))
		}
	}
	return nil
}

var builtinVars = map[string]bool{"name": true, "dir": true, "user": true, "version": true, "java": true, "package": true, "host": true, "random": true, "base64random": true}

// ---------------------------------------------------------------- 加载

// Builtin 内置模板。
func Builtin() []*Template {
	ents, _ := builtinFS.ReadDir("builtin")
	var out []*Template
	for _, e := range ents {
		b, _ := builtinFS.ReadFile("builtin/" + e.Name())
		t, err := Parse(b)
		if err != nil {
			panic(fmt.Sprintf("内置模板 %s：%v", e.Name(), err)) // 测试会发现
		}
		t.Builtin = true
		out = append(out, t)
	}
	return out
}

// BuiltinSource 内置模板的原文（「复制为我的模板」用）。
func BuiltinSource(id string) ([]byte, error) {
	b, err := builtinFS.ReadFile("builtin/" + id + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("没有内置模板 %s", id)
	}
	return b, nil
}

// All 内置 + 用户目录里的模板（同 ID 用户的优先）；读不了的用户模板放在 errs 里。
func All(userDir string) (list []*Template, errs []error) {
	byID := map[string]*Template{}
	for _, t := range Builtin() {
		byID[t.ID] = t
	}
	ents, _ := os.ReadDir(userDir)
	for _, e := range ents {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		p := filepath.Join(userDir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		t, err := Parse(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s：%w", e.Name(), err))
			continue
		}
		t.File = p
		if old, ok := byID[t.ID]; ok && old.Builtin {
			t.Overrides = true
		}
		byID[t.ID] = t
	}
	for _, t := range byID {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Category != list[j].Category {
			return catOrder(list[i].Category) < catOrder(list[j].Category)
		}
		return list[i].ID < list[j].ID
	})
	return list, errs
}

var categories = []string{"数据库", "缓存", "中间件", "存储", "Web 服务器", "工具", "其他", "自定义"}

func catOrder(c string) int {
	for i, x := range categories {
		if x == c {
			return i
		}
	}
	return len(categories)
}

// Find 按 ID 找模板。
func Find(userDir, id string) (*Template, error) {
	list, _ := All(userDir)
	for _, t := range list {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("没有模板 %q", id)
}

// Save 把模板存进用户目录（文件名 <id>.yaml）。
func Save(userDir string, content []byte) (*Template, error) {
	t, err := Parse(content)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(userDir, t.ID+".yaml")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		return nil, err
	}
	t.File = p
	return t, nil
}

// Delete 删除用户模板（内置的删不了）。
func Delete(userDir, id string) error {
	for _, ext := range []string{".yaml", ".yml"} {
		p := filepath.Join(userDir, id+ext)
		if _, err := os.Stat(p); err == nil {
			return os.Remove(p)
		}
	}
	return fmt.Errorf("没有自己的模板 %s（内置模板不能删除）", id)
}

// ---------------------------------------------------------------- 选变体

// OS 服务器的系统（/etc/os-release）。
type OS struct {
	ID      string   `json:"id"`      // rocky、ubuntu……
	Like    []string `json:"like"`    // ID_LIKE：rhel centos fedora / debian
	Version string   `json:"version"` // VERSION_ID
	Pretty  string   `json:"pretty"`
}

// Fits 变体能不能用在这台服务器上；不能时返回原因。
func (v *Variant) Fits(os OS, arch string) (bool, string) {
	if len(v.Arch) > 0 && !contains(v.Arch, arch) {
		return false, "不支持 " + arch + " 架构"
	}
	if len(v.OS) > 0 {
		ok := contains(v.OS, os.ID)
		for _, l := range os.Like {
			ok = ok || contains(v.OS, l)
		}
		if !ok {
			name := os.Pretty
			if name == "" {
				name = os.ID
			}
			return false, "只适用于 " + strings.Join(v.OS, "、") + " 系列，这台服务器是 " + name
		}
	}
	return true, ""
}

// Pick 适合这台服务器的第一种装法；want ≥ 0 时用指定的（也要适合）。
func (t *Template) Pick(os OS, arch string, want int) (int, error) {
	if want >= 0 {
		if want >= len(t.Variants) {
			return -1, fmt.Errorf("%s 没有第 %d 种装法", t.Name, want+1)
		}
		if ok, why := t.Variants[want].Fits(os, arch); !ok {
			return -1, fmt.Errorf("%s 的「%s」%s", t.Name, t.Variants[want].Label, why)
		}
		return want, nil
	}
	var whys []string
	for i := range t.Variants {
		if ok, why := t.Variants[i].Fits(os, arch); ok {
			return i, nil
		} else {
			whys = append(whys, t.Variants[i].Label+"："+why)
		}
	}
	return -1, fmt.Errorf("%s 没有适合这台服务器的装法（%s）", t.Name, strings.Join(whys, "；"))
}

// ---------------------------------------------------------------- 渲染

// Vars 渲染用的变量。
type Vars map[string]string

var reVar = regexp.MustCompile(`\$\{([^{}]+)\}`)

// varNames 字符串里用到的变量名。
func varNames(s string) []string {
	var out []string
	s = strings.ReplaceAll(s, "$${", escaped)
	for _, m := range reVar.FindAllStringSubmatch(s, -1) {
		e := m[1]
		if strings.HasPrefix(e, "random:") || strings.HasPrefix(e, "base64random:") {
			continue
		}
		name := e
		if i := strings.IndexAny(e, "?+"); i >= 0 {
			name = e[:i]
		}
		out = append(out, strings.TrimSpace(name))
	}
	return out
}

// Truthy bool 参数的值算不算「是」。
func Truthy(v string) bool { return v == "true" || v == "1" || v == "yes" }

// escaped $${…} 在占位期间换成的字符（渲染完再换回 ${）
const escaped = "\x00ESC{"

// Expand 把 ${…} 换成值。由内向外展开（${public?…${password}…|}）；$${…} 原样留下 ${…}（给 systemd、shell 用）。
func (vs Vars) Expand(s string) (string, error) {
	s = strings.ReplaceAll(s, "$${", escaped)
	var err error
	for i := 0; i < 5 && err == nil && reVar.MatchString(s); i++ {
		s, err = vs.expandOnce(s)
	}
	return strings.ReplaceAll(s, escaped, "${"), err
}

func (vs Vars) expandOnce(s string) (string, error) {
	var err error
	out := reVar.ReplaceAllStringFunc(s, func(m string) string {
		e := m[2 : len(m)-1]
		switch {
		case strings.HasPrefix(e, "random:"):
			n, _ := strconv.Atoi(e[7:])
			return RandomString(n)
		case strings.HasPrefix(e, "base64random:"):
			n, _ := strconv.Atoi(e[13:])
			return base64.StdEncoding.EncodeToString([]byte(RandomString(n)))
		}
		if i := strings.IndexByte(e, '?'); i >= 0 {
			yes, no, _ := strings.Cut(e[i+1:], "|")
			if Truthy(vs[strings.TrimSpace(e[:i])]) {
				return yes
			}
			return no
		}
		if i := strings.IndexByte(e, '+'); i >= 0 {
			a, err1 := strconv.Atoi(vs[strings.TrimSpace(e[:i])])
			b, err2 := strconv.Atoi(strings.TrimSpace(e[i+1:]))
			if err1 != nil || err2 != nil {
				err = fmt.Errorf("%s 不是数字", m)
				return m
			}
			return strconv.Itoa(a + b)
		}
		v, ok := vs[strings.TrimSpace(e)]
		if !ok {
			err = fmt.Errorf("没有变量 %s", m)
			return m
		}
		return v
	})
	return out, err
}

// Cond if 条件：参数名（为 true 时）或 !参数名（为 false 时）；空 = 总是。
func (vs Vars) Cond(c string) bool {
	c = strings.TrimSpace(c)
	if c == "" {
		return true
	}
	if n, ok := strings.CutPrefix(c, "!"); ok {
		return !Truthy(vs[strings.TrimSpace(n)])
	}
	return Truthy(vs[c])
}

// RandomString 随机字母数字串（避开容易看错的 0O1lI）。
func RandomString(n int) string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[k.Int64()]
	}
	return string(b)
}

// walk 对变体里每个会渲染的字符串调用 fn。
func walk(v *Variant, fn func(string)) {
	if p := v.Package; p != nil {
		for _, s := range append([]string{p.URL, p.File, p.Into, p.Check, p.Probe, p.MD5URL}, p.Mirrors...) {
			fn(s)
		}
	}
	for _, f := range v.Files {
		fn(f.Path)
		fn(f.Content)
	}
	for _, p := range v.Props {
		fn(p.File)
		for _, val := range p.Set {
			fn(val)
		}
	}
	for _, s := range v.Steps {
		fn(s.Run)
		if s.HTTP != nil {
			fn(s.HTTP.Post)
			for _, val := range s.HTTP.Form {
				fn(val)
			}
		}
	}
	fn(v.Service.Exec)
	fn(v.Service.Workdir)
	fn(v.Service.Ready.Port)
	for _, val := range v.Service.Env {
		fn(val)
	}
	for _, s := range append(append([]string{}, v.Logs...), v.Notes...) {
		fn(s)
	}
	if v.System != nil {
		fn(v.System.Require)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// MatchFile 文件名是不是这个安装包（官方文件名）；是的话返回其中的版本。
func (p *Package) MatchFile(name string) (version string, ok bool) {
	if p == nil || p.File == "" {
		return "", false
	}
	parts := strings.Split(p.File, "${version}")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re, err := regexp.Compile("^" + strings.Join(parts, `([0-9A-Za-z][0-9A-Za-z._+-]*?)`) + "$")
	if err != nil {
		return "", false
	}
	m := re.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	if len(m) > 1 {
		version = m[1]
	}
	return version, true
}

// FormatOf 安装包的格式：写了 format 用它，否则按文件名。
func FormatOf(format, file string) string {
	if format != "" {
		return format
	}
	switch f := strings.ToLower(file); {
	case strings.HasSuffix(f, ".tar.gz"), strings.HasSuffix(f, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(f, ".tar.xz"), strings.HasSuffix(f, ".txz"):
		return "tar.xz"
	case strings.HasSuffix(f, ".zip"):
		return "zip"
	case strings.HasSuffix(f, ".jar"), strings.HasSuffix(f, ".war"):
		return "jar"
	}
	return "binary"
}

// ErrNoPackage 变体要上传安装包，但没有下载地址，本机库里也没有。
var ErrNoPackage = errors.New("需要先导入安装包")
