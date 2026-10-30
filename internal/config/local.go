package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/captain/ferry/internal/appdir"
)

// Local 本机设置：只属于这台电脑的东西（私钥、登录方式、变量的值），存在数据目录里，不写进 ferry.yaml。
// ferry.yaml 写通用的配置；各台服务器自己的值（登录方式、JDK 和脚本的位置……）写在 Targets 里，只对那台生效；
// Vars 是所有服务器共用的（比如这台电脑上的源码目录）。本机设置优先于 ferry.yaml。
type Local struct {
	Vars    map[string]string       `json:"vars,omitempty"`
	Targets map[string]*LocalTarget `json:"targets,omitempty"`
}

// LocalTarget 一个目标的本机设置。
type LocalTarget struct {
	Auth         string `json:"auth,omitempty"`         // key / password；空 = 按 ferry.yaml
	IdentityFile string `json:"identityFile,omitempty"` // 私钥（auth 为 key 时）；空 = 按 ferry.yaml
	// Vars 这台服务器上变量的值（ferry.yaml 里已有的项目变量、这台服务器的变量），只对这台生效
	Vars map[string]string `json:"vars,omitempty"`
}

// Empty 什么都没设置。
func (t *LocalTarget) Empty() bool {
	return t == nil || (t.Auth == "" && t.IdentityFile == "" && len(t.Vars) == 0)
}

// HasLogin 设置了登录方式（否则按 ferry.yaml 登录）。
func (t *LocalTarget) HasLogin() bool { return t != nil && (t.Auth != "" || t.IdentityFile != "") }

// LocalFile 本机设置文件：数据目录/projects/<项目>-<配置路径摘要>/local.json。
func LocalFile(project, configPath string) string {
	return filepath.Join(appdir.ProjectPath(project, configPath), "local.json")
}

// ReadLocal 读本机设置（没有时返回空的）。
func ReadLocal(project, configPath string) (*Local, error) {
	p := LocalFile(project, configPath)
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Local{}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Local
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("本机设置 %s 有问题：%w", p, err)
	}
	return &l, nil
}

// WriteLocal 保存本机设置（先用 CheckLocal 检查）；空的时候删掉文件。
func WriteLocal(f *File, l *Local) error {
	p := LocalFile(f.Project, f.Path)
	for k, t := range l.Targets {
		if t.Empty() {
			delete(l.Targets, k)
		}
	}
	if len(l.Vars) == 0 && len(l.Targets) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return appdir.WriteFileAtomic(p, append(data, '\n'), 0o600)
}

// CheckLocal 检查本机设置能不能用在这份配置上（f 是没有套用本机设置的配置）。
func CheckLocal(f *File, l *Local) error {
	var errs []error
	for k := range l.Vars {
		if _, ok := f.Vars[k]; !ok {
			errs = append(errs, fmt.Errorf("ferry.yaml 的 vars 里没有 %s：本机设置只能改已有变量的值", k))
		}
	}
	for name, t := range l.Targets {
		if t == nil {
			continue
		}
		ft, ok := f.Targets[name]
		if !ok {
			errs = append(errs, fmt.Errorf("没有目标 %s", name))
		}
		for k := range t.Vars {
			if _, inT := ft.varsOrNil()[k]; !inT {
				if _, inF := f.Vars[k]; !inF {
					errs = append(errs, fmt.Errorf("目标 %s：ferry.yaml 的 vars 里没有 %s，本机设置只能改已有变量的值", name, k))
				}
			}
		}
		switch t.Auth {
		case "", "key":
		case "password":
			if t.IdentityFile != "" {
				errs = append(errs, fmt.Errorf("目标 %s：密码登录时不要设置私钥", name))
			}
		default:
			errs = append(errs, fmt.Errorf("目标 %s：登录方式只能是 key 或 password", name))
		}
	}
	return errors.Join(errs...)
}

// LoadWithLocal 读配置，再套用这台电脑的本机设置（命令行、桌面版连接服务器时用）。
// 编辑器校验、生成配置这些只看 ferry.yaml 本身的地方用 Load。
func LoadWithLocal(path string) (*File, error) {
	f, err := Load(path)
	if err != nil || f.Kind == KindConnections {
		return f, err
	}
	l, err := ReadLocal(f.Project, f.Path)
	if err != nil {
		return nil, err
	}
	f.ApplyLocal(l)
	return f, nil
}

// ApplyLocal 用本机设置覆盖配置；ferry.yaml 里已经没有的变量、目标跳过。
func (f *File) ApplyLocal(l *Local) {
	for k, v := range l.Vars {
		if _, ok := f.Vars[k]; ok {
			f.Vars[k] = v
			f.LocalVars = append(f.LocalVars, k)
		}
	}
	sort.Strings(f.LocalVars)
	for name, lt := range l.Targets {
		t := f.Targets[name]
		if t == nil || lt.Empty() {
			continue
		}
		vars := map[string]string{}
		for k, v := range t.Vars {
			vars[k] = v
		}
		for k, v := range lt.Vars {
			_, inT := t.Vars[k]
			if _, inF := f.Vars[k]; inT || inF {
				vars[k] = v
				t.LocalVars = append(t.LocalVars, k)
			}
		}
		if len(t.LocalVars) > 0 {
			sort.Strings(t.LocalVars)
			t.Vars = vars
		}
		if !lt.HasLogin() {
			continue
		}
		c := *lt
		c.Vars = nil
		t.Local = &c
		switch {
		case lt.Auth == "password":
			t.Auth, t.IdentityFile = "password", ""
		case lt.IdentityFile != "":
			t.Auth, t.IdentityFile = "key", lt.IdentityFile
		case lt.Auth == "key":
			t.Auth = "key" // 不指定私钥：用 ~/.ssh 下的默认私钥、ssh-agent
			t.IdentityFile = ""
		}
	}
}

// KeyMissing 目标指定的私钥在这台电脑上不存在时返回它的位置。
func (f *File) KeyMissing(t *Target) string {
	if t.Auth == "password" || t.IdentityFile == "" {
		return ""
	}
	p := f.LocalPath(t.IdentityFile)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	return ""
}

// MachineProblem 这台电脑上缺的东西（导入别的电脑上的配置后检查）。
type MachineProblem struct {
	Kind   string   `json:"kind"`             // key-missing / workdir-missing
	Target string   `json:"target,omitempty"` // key-missing
	Build  string   `json:"build,omitempty"`  // workdir-missing
	Path   string   `json:"path"`
	Vars   []string `json:"vars,omitempty"` // workdir-missing：目录里用到的项目变量（可以在本机设置里改）
	Msg    string   `json:"msg"`
}

var reVarRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// MachineProblems 检查这台电脑上缺什么：目标指定的私钥、构建目录。
func (f *File) MachineProblems() []MachineProblem {
	var out []MachineProblem
	for _, n := range f.TargetNames() {
		t := f.Targets[n]
		if p := f.KeyMissing(t); p != "" {
			out = append(out, MachineProblem{Kind: "key-missing", Target: n, Path: p,
				Msg: fmt.Sprintf("目标 %s 的私钥 %s 在这台电脑上不存在", n, p)})
		}
	}
	if f.Kind == KindConnections {
		return out
	}
	seen := map[string]bool{}
	for _, a := range f.Ordered() {
		b := f.ResolveBuild(a)
		if b == nil {
			continue
		}
		wd := b.Workdir
		if wd == "" {
			wd = "."
		}
		name := b.Name
		if name == "" {
			name = a.Name
		}
		v, err := f.localScope(a).Expand(wd)
		if err != nil {
			continue // 用到部署时才有的变量：这里判断不了
		}
		p := f.LocalPath(v)
		if seen[p] {
			continue
		}
		seen[p] = true
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			continue
		}
		var vars []string
		for _, m := range reVarRef.FindAllStringSubmatch(wd, -1) {
			if _, ok := f.Vars[m[1]]; ok {
				vars = append(vars, m[1])
			}
		}
		msg := fmt.Sprintf("构建 %s 的目录 %s 在这台电脑上不存在", name, p)
		if len(vars) > 0 {
			msg += "（在本机设置里把变量 " + strings.Join(vars, "、") + " 改成这台电脑上的位置）"
		}
		out = append(out, MachineProblem{Kind: "workdir-missing", Build: name, Path: p, Vars: vars, Msg: msg})
	}
	return out
}

// localScope 展开本机路径里的变量（用第一个目标的变量；部署时才有的变量展开失败）。
func (f *File) localScope(a *App) *Scope {
	layers := []map[string]string{a.Vars, f.Vars}
	if ts := f.TargetNames(); len(ts) > 0 {
		layers = []map[string]string{a.Vars, f.Targets[ts[0]].Vars, f.Vars}
	}
	return &Scope{Builtins: map[string]string{"project": f.Project, "configDir": f.Dir, "app": a.Name},
		Layers: layers, Each: func(string) (string, error) { return "", nil }}
}

// RelativePaths 配置里按配置文件所在目录计算的本机路径（私钥、构建目录、产物、附带文件）：
// 配置文件换了目录，它们就指向新目录下了。返回「位置：写法」，给人看。
func (f *File) RelativePaths() []string {
	var out []string
	seen := map[string]bool{}
	add := func(where, raw string, sc *Scope) {
		v := raw
		if sc != nil {
			if strings.Contains(raw, "${configDir}") {
				v = "."
			} else if x, err := sc.Expand(raw); err == nil {
				v = x
			} else {
				return // 用到部署时才有的变量：判断不了
			}
		}
		if v == "" || filepath.IsAbs(v) || strings.HasPrefix(v, "~") || filepath.VolumeName(v) != "" || seen[where] {
			return
		}
		seen[where] = true
		out = append(out, where+"："+raw)
	}
	for _, n := range f.TargetNames() {
		if p := f.Targets[n].IdentityFile; p != "" {
			add("targets."+n+".identityFile", p, nil)
		}
	}
	if f.Kind == KindConnections {
		return out
	}
	for _, a := range f.Ordered() {
		sc := f.localScope(a)
		if b := f.ResolveBuild(a); b != nil {
			wd := b.Workdir
			if wd == "" {
				wd = "."
			}
			where := a.Name + " 的 build.workdir"
			if b.Name != "" {
				where = "builds." + b.Name + ".workdir"
			}
			add(where, wd, sc)
		}
		if a.Artifact.Path != "" {
			add(a.Name+" 的 artifact.path", a.Artifact.Path, sc)
		}
		for i, x := range a.Artifact.Files {
			add(fmt.Sprintf("%s 的 artifact.files[%d]", a.Name, i), x.From, sc)
		}
	}
	return out
}

func (t *Target) varsOrNil() map[string]string {
	if t == nil {
		return nil
	}
	return t.Vars
}

// LocalUseVars 用在这台电脑上的项目变量：构建目录和命令、产物、附带文件（比如源码目录 ${repo}），
// 连同它们的值里再引用的。这些是这台电脑的路径，所有服务器共用；其余的变量才是各台服务器自己的。
func (f *File) LocalUseVars() map[string]bool {
	out := map[string]bool{}
	var scan func(s string, app map[string]string, depth int)
	scan = func(s string, app map[string]string, depth int) {
		if depth > 8 {
			return
		}
		for _, m := range reVarRef.FindAllStringSubmatch(s, -1) {
			n := m[1]
			if v, ok := app[n]; ok {
				scan(v, app, depth+1)
				continue
			}
			if v, ok := f.Vars[n]; ok && !out[n] {
				out[n] = true
				scan(v, app, depth+1)
			}
		}
	}
	for _, a := range f.Ordered() {
		if b := f.ResolveBuild(a); b != nil {
			scan(b.Workdir, a.Vars, 0)
			scan(b.Command, a.Vars, 0)
		}
		scan(a.Artifact.Path, a.Vars, 0)
		for _, x := range a.Artifact.Files {
			scan(x.From, a.Vars, 0)
		}
	}
	for _, t := range f.Targets {
		scan(t.IdentityFile, nil, 0)
	}
	return out
}
