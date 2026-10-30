package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/captain/ferry/internal/proto"
)

// DefaultFileName 默认配置文件名。
const DefaultFileName = "ferry.yaml"

// KindConnections 桌面版「新建连接」保存的服务器列表（数据目录里的 connections.yaml）。
const KindConnections = "connections"

var (
	reProject = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	reApp     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	reTarget  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	reUnit    = regexp.MustCompile(`^[A-Za-z0-9:_.@-]+\.service$`)
	reVar     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reUser    = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)
)

// Find 从 dir 开始向上查找 ferry.yaml。
func Find(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, DefaultFileName)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("找不到 %s（可以用 -c 指定，或 ferry init 生成）", DefaultFileName)
		}
		dir = parent
	}
}

// Load 读取、补默认值并校验配置。
func Load(path string) (*File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return LoadContent(data, abs)
}

// LoadContent 校验内存中的配置内容，相对路径按 path 所在目录计算（编辑器实时校验，不用先保存）。
func LoadContent(data []byte, path string) (*File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	f.Path = abs
	f.Dir = filepath.Dir(abs)
	if err := f.normalize(); err != nil {
		return nil, fmt.Errorf("%s:\n%w", abs, err)
	}
	return f, nil
}

// Parse 只解析 YAML（未知字段报错），不补默认值。
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, explainUnknown(err)
	}
	return &f, nil
}

// UnknownHint 配置里有不认识的字段时的说明：多半是写错了，也可能是更新版本的 Ferry 写的（别的电脑上装的新版）。
const UnknownHint = "如果没有写错，这个配置可能是更新版本的 Ferry 写的，把这台电脑上的 Ferry 升级到最新版就能用"

var reUnknownField = regexp.MustCompile(`line (\d+): field (\S+) not found in type \S+`)

// explainUnknown 把 yaml 的「field x not found in type …」换成中文，并加上 UnknownHint。
func explainUnknown(err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	unknown := false
	msgs := make([]string, len(te.Errors))
	for i, m := range te.Errors {
		if s := reUnknownField.FindStringSubmatch(m); s != nil {
			msgs[i] = fmt.Sprintf("第 %s 行：不认识的字段 %q", s[1], s[2])
			unknown = true
		} else {
			msgs[i] = m
		}
	}
	if !unknown {
		return err
	}
	return fmt.Errorf("%s\n%s", strings.Join(msgs, "\n"), UnknownHint)
}

// normalize 补默认值并校验，所有问题一次报出。
func (f *File) normalize() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if f.Version != 1 {
		bad("version 必须是 1（当前 %d）", f.Version)
	}
	switch f.Kind {
	case "":
	case KindConnections:
		// 只有服务器：不部署，不需要项目名和应用
		if f.Project == "" {
			f.Project = "connections"
		}
		if len(f.Apps) > 0 || len(f.Builds) > 0 {
			bad("kind: connections 的文件里只有 targets")
		}
	default:
		bad("kind 只能不写，或是 %s", KindConnections)
	}
	if !reProject.MatchString(f.Project) {
		bad("project %q 无效：小写字母、数字、. _ -", f.Project)
	}
	checkVars := func(where string, vars map[string]string) {
		for k := range vars {
			if !reVar.MatchString(k) {
				bad("%s：变量名 %q 无效", where, k)
			}
			for _, b := range BuiltinNames {
				if k == b {
					bad("%s：%q 是内置变量，不能重新定义", where, k)
				}
			}
		}
	}
	checkVars("vars", f.Vars)

	for name, b := range f.Builds {
		if b == nil {
			b = &Build{}
			f.Builds[name] = b
		}
		b.Name = name
		if strings.TrimSpace(b.Command) == "" {
			bad("builds.%s：command 不能为空", name)
		}
	}

	if len(f.Targets) == 0 && f.Kind != KindConnections {
		bad("至少需要一个 targets")
	}
	for name, t := range f.Targets {
		if t == nil {
			bad("targets.%s 为空", name)
			continue
		}
		t.Name = name
		t.Plain = f.Kind == KindConnections
		where := "targets." + name
		if !reTarget.MatchString(name) {
			bad("%s：名称无效", where)
		}
		if t.Host == "" {
			bad("%s：host 不能为空", where)
		}
		if t.Port == 0 {
			t.Port = 22
		}
		if t.Port < 1 || t.Port > 65535 {
			bad("%s：port 无效", where)
		}
		if t.User == "" {
			t.User = "root"
		}
		switch t.Auth {
		case "":
			t.Auth = "key"
		case "key":
		case "password":
			if t.IdentityFile != "" {
				bad("%s：auth: password 时不要同时设置 identityFile", where)
			}
		default:
			bad("%s：auth 只能是 key 或 password（密码本身不写在配置里，用 ferry secret set 保存到系统凭据库）", where)
		}
		if t.Root == "" {
			t.Root = "/opt/" + f.Project
		}
		if err := checkRoot(t.Root); err != nil {
			bad("%s：root %v", where, err)
		}
		if t.FerryDir == "" {
			t.FerryDir = DefaultFerryDir
		}
		t.FerryDir = strings.TrimRight(t.FerryDir, "/")
		if err := checkRoot(t.FerryDir); err != nil && !strings.HasPrefix(t.FerryDir, "/usr/local/") {
			bad("%s：ferryDir %v", where, err)
		}
		if t.FerryDir == t.Root || strings.HasPrefix(t.FerryDir, t.Root+"/apps") {
			bad("%s：ferryDir 不能是部署目录或在它的 apps 里", where)
		}
		if t.RunAs != "" && !reUser.MatchString(t.RunAs) {
			bad("%s：runAs %q 无效", where, t.RunAs)
		}
		switch t.Arch {
		case "", "amd64", "arm64":
		case "x86_64":
			t.Arch = "amd64"
		case "aarch64":
			t.Arch = "arm64"
		default:
			bad("%s：arch 只能是 amd64 或 arm64", where)
		}
		if t.UploadStreams < 0 || t.UploadStreams > 32 {
			bad("%s：uploadStreams 只能是 1～32", where)
		}
		if t.KeepReleases == 0 {
			t.KeepReleases = 5
		}
		if t.KeepReleases < 2 {
			bad("%s：keepReleases 至少为 2（当前版本 + 一个可回滚的版本）", where)
		}
		if t.HostKey != "" && !strings.HasPrefix(t.HostKey, "SHA256:") {
			bad("%s：hostKey 应为 SHA256:... 格式的指纹", where)
		}
		checkVars(where+".vars", t.Vars)
	}

	if len(f.Apps) == 0 && f.Kind != KindConnections {
		bad("至少需要一个 apps")
	}
	seen := map[string]bool{}
	units := map[string]string{}
	for i, a := range f.Apps {
		if a == nil {
			bad("apps[%d] 为空", i)
			continue
		}
		a.index = i
		where := fmt.Sprintf("apps[%d]", i)
		if a.Name != "" {
			where = "apps." + a.Name
		}
		if !reApp.MatchString(a.Name) {
			bad("%s：name %q 无效", where, a.Name)
		}
		lower := strings.ToLower(a.Name)
		if seen[lower] {
			bad("%s：应用名重复（不区分大小写）", where)
		}
		seen[lower] = true
		checkVars(where+".vars", a.Vars)

		if a.Build != nil && a.Build.Ref != "" {
			if _, ok := f.Builds[a.Build.Ref]; !ok {
				bad("%s：build %q 不在 builds 中", where, a.Build.Ref)
			}
		}
		if a.Build != nil && a.Build.Inline != nil {
			a.Build.Inline.Name = "app:" + a.Name
			if strings.TrimSpace(a.Build.Inline.Command) == "" {
				bad("%s：build.command 不能为空", where)
			}
		}
		if a.Artifact.Path == "" {
			bad("%s：artifact.path 不能为空", where)
		}
		checkExtraFiles(a, where, bad)
		checkHooks(a, where, bad)
		if a.KeepReleases != 0 && a.KeepReleases < 2 {
			bad("%s：keepReleases 至少为 2", where)
		}

		if a.Type != "python" && a.Type != "node" && (a.Deps != Deps{}) {
			bad("%s：deps 只用于 type: python、node", where)
		}
		switch a.Type {
		case "java", "binary", "python", "node", "dotnet":
			s := &a.Service
			if (a.Type == "python" || a.Type == "node" || a.Type == "dotnet") && len(s.Exec) == 0 {
				bad("%s：type: %s 要写 service.exec（启动命令），如 %s", where, a.Type, map[string]string{
					"python": `["${running}/.venv/bin/uvicorn", "main:app", "--host", "127.0.0.1", "--port", "8000"]`,
					"node":   `[node, "${running}/dist/main.js"]`,
					"dotnet": `["${running}/MyApp"]（自带运行时发布）或 [dotnet, "${running}/MyApp.dll"]`,
				}[a.Type])
			}
			if (a.Type == "python" || a.Type == "node" || a.Type == "dotnet") && (s.Java != "" || len(s.JvmArgs) > 0) {
				bad("%s：type: %s 不需要 java、jvmArgs", where, a.Type)
			}
			if len(a.Artifact.Files) > 0 && a.Type != "java" && a.Type != "binary" {
				bad("%s：artifact.files 只用于 java、binary（%s 上传的是整个目录，配置文件放进目录里就行）", where, a.Type)
			}
			if s.Unit == "" {
				s.Unit = f.Project + "-" + lower
			}
			if !strings.HasSuffix(s.Unit, ".service") {
				s.Unit += ".service"
			}
			if !reUnit.MatchString(s.Unit) {
				bad("%s：unit %q 无效", where, s.Unit)
			}
			if other, ok := units[s.Unit]; ok {
				bad("%s：unit %s 与 %s 重复", where, s.Unit, other)
			}
			units[s.Unit] = a.Name
			for j, c := range s.Conflicts {
				if !strings.HasSuffix(c, ".service") {
					c += ".service"
					s.Conflicts[j] = c
				}
				if !reUnit.MatchString(c) {
					bad("%s：conflicts 中的 %q 无效", where, c)
				}
			}
			if s.User != "" && !reUser.MatchString(s.User) {
				bad("%s：user %q 无效", where, s.User)
			}
			if s.Runtime != nil {
				switch {
				case a.Type != "java":
					bad("%s：runtime 只用于 type: java", where)
				case s.Runtime.Java < 8 || s.Runtime.Java > 99:
					bad("%s：runtime.java 要写 JDK 的大版本，如 17、21（当前 %d）", where, s.Runtime.Java)
				case s.Java != "":
					bad("%s：设置了 runtime（由 Ferry 提供 JDK）就不要再写 java（%s）", where, s.Java)
				case len(s.Exec) > 0 && !slices.ContainsFunc(s.Exec, func(x string) bool { return strings.Contains(x, "${java}") }):
					bad("%s：设置了 runtime 时，exec 里要用 ${java} 启动（它就是 Ferry 提供的 JDK），不要写别的 java 路径", where)
				case s.Runtime.Version != "" && !reJDKVersion.MatchString(s.Runtime.Version):
					bad("%s：runtime.version 要写完整的 JDK 版本，如 25.0.4.1+1（java -version 里 build 后面那一段）", where)
				case s.Runtime.Version != "" && jdkMajor(s.Runtime.Version) != s.Runtime.Java:
					bad("%s：runtime.version %s 不是 Java %d 的版本", where, s.Runtime.Version, s.Runtime.Java)
				}
			}
			if s.ArchiveLogs != nil {
				for _, d := range *s.ArchiveLogs {
					if d == "" || d == "." || strings.HasPrefix(d, "/") || strings.Contains(d, "..") {
						bad("%s：archiveLogs 必须是 shared 下的相对目录（如 logs），当前 %q", where, d)
					}
				}
			}
			normalizeHealth(&s.Health, where, bad)
			if z := s.ZeroDowntime; z != nil {
				checkZD(z, where, bad)
			}
			if a.Static.Link != "" || a.Static.Reload != "" {
				bad("%s：static 只用于 type: static", where)
			}
			if a.Type == "binary" && s.Java != "" {
				bad("%s：binary 不需要 java", where)
			}
		case "static":
			st := &a.Static
			if st.Link != "" {
				if err := checkRoot(st.Link); err != nil {
					bad("%s：static.link %v", where, err)
				}
			}
			if st.Served() {
				checkServedStatic(f, a, where, units, bad)
			} else {
				if st.NginxInstance != "" || len(st.Domains) > 0 || st.Nginx != "" || st.SPA != nil || len(st.Proxy) > 0 || st.MaxBody != "" || len(st.Server) > 0 || st.HTTPS != nil {
					bad("%s：static.nginx、spa、proxy、maxBody、server、https 只在设置了 static.port（由 Ferry 的 nginx 运行）时有用", where)
				}
				if a.Service.Unit != "" || a.Service.User != "" {
					bad("%s：没有设置 static.port 的静态站点不是服务，不需要 service", where)
				}
			}
			normalizeStaticHealth(&st.Health)
			if hasServiceExtras(&a.Service) {
				bad("%s：静态站点的 service 只能设置 unit、user、description", where)
			}
			if a.Artifact.Verify.Static == nil {
				a.Artifact.Verify.Static = &StaticVerify{}
			}
			if a.Artifact.Verify.Static.Index == "" {
				a.Artifact.Verify.Static.Index = "index.html"
			}
		default:
			bad("%s：type 只能是 java、binary、static、python、node、dotnet（当前 %q）", where, a.Type)
		}
	}

	for _, t := range f.Targets {
		if t == nil {
			continue
		}
		for _, n := range t.Apps {
			if f.App(n) == nil {
				bad("targets.%s.apps：没有应用 %q", t.Name, n)
			}
		}
		for _, a := range f.Apps {
			if a == nil || a.Type == "static" || !t.Allows(a.Name) {
				continue
			}
			if a.Service.User == "" && t.RunAs == "" {
				bad("targets.%s：应用 %s 没有运行用户（设置 targets.%s.runAs 或 apps.%s.service.user）", t.Name, a.Name, t.Name, a.Name)
			}
		}
		for _, a := range f.Apps {
			if a == nil || a.Type != "static" || !t.Allows(a.Name) || a.Service.User != "" || t.RunAs != "" {
				continue
			}
			for _, h := range append(append([]*Hook{}, a.Hooks.BeforeSwitch...), a.Hooks.AfterSwitch...) {
				if h != nil && h.User == "" {
					bad("targets.%s：应用 %s 的 hooks 不知道以哪个用户执行（给 hooks 写 user，或设置 targets.%s.runAs）", t.Name, a.Name, t.Name)
					break
				}
			}
		}
		for _, a := range f.Apps {
			if a == nil || a.Root == "" || !t.Allows(a.Name) {
				continue
			}
			if _, err := f.AppRoot(t, a); err != nil {
				bad("apps.%s.root（目标 %s）：%v", a.Name, t.Name, err)
			}
		}
	}
	checkBackups(f, bad)
	return errors.Join(errs...)
}

// AppRoot 应用在目标上的部署根目录：应用的 root（展开变量）或目标的 root。
// 应用目录为 <部署根目录>/apps/<应用>；版本缓存、上传目录、锁、部署历史始终在目标的 root/.ferry 下。
func (f *File) AppRoot(t *Target, a *App) (string, error) {
	if a.Root == "" {
		return t.Root, nil
	}
	sc := &Scope{
		Builtins: map[string]string{"project": f.Project, "target": t.Name, "app": a.Name, "host": t.Host, "root": t.Root},
		Layers:   []map[string]string{a.Vars, t.Vars, f.Vars},
	}
	r, err := sc.Expand(a.Root)
	if err != nil {
		return "", err
	}
	if err := checkRoot(r); err != nil {
		return "", err
	}
	r = strings.TrimRight(r, "/")
	under := func(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }
	for _, d := range []string{t.Root + "/apps", t.Root + "/.ferry"} {
		if under(r, d) {
			return "", fmt.Errorf("%s 在目标部署目录的 %s 里", r, d)
		}
	}
	if under(t.Root, r+"/apps") {
		return "", fmt.Errorf("目标的部署目录 %s 在 %s/apps 里", t.Root, r)
	}
	return r, nil
}

func normalizeHealth(h *Health, where string, bad func(string, ...any)) {
	if h.TCP != "" && !strings.Contains(h.TCP, ":") {
		h.TCP = "127.0.0.1:" + h.TCP
	}
	if h.HTTP != "" && !strings.HasPrefix(h.HTTP, "http://") && !strings.HasPrefix(h.HTTP, "https://") {
		bad("%s：health.http 必须以 http:// 或 https:// 开头", where)
	}
	if h.Timeout == 0 {
		h.Timeout = Duration(180 * time.Second)
	}
	if h.TCP == "" && h.HTTP == "" && h.Stable == 0 {
		h.Stable = Duration(10 * time.Second)
	}
}

var (
	rePort      = regexp.MustCompile(`^(?:(\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]|localhost):)?(\d{1,5})$`)
	reNgxSize   = regexp.MustCompile(`^\d+[kKmMgG]?$`)
	reProxyPath = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)
	reProxyTo   = regexp.MustCompile(`^https?://[A-Za-z0-9.\-\[\]:]+(/[A-Za-z0-9._~/-]*)?$`)
)

// checkServedStatic 由 Ferry 的 nginx 运行的静态站点：端口、代理等，并补上单元名与默认健康检查。
// checkExtraFiles 检查附带文件，补上 to（不写 = from 的文件名，去掉末尾的 /）。
func checkExtraFiles(a *App, where string, bad func(string, ...any)) {
	if len(a.Artifact.Files) == 0 {
		return
	}
	if a.Type != "java" && a.Type != "binary" {
		bad("%s：artifact.files 只用于 java、binary（静态站点的文件都放在产物目录里）", where)
		return
	}
	program := filepath.Base(filepath.FromSlash(a.Artifact.Path))
	if strings.Contains(program, "${") {
		program = "" // 带变量的在渲染时按展开后的再查
	}
	var seen []string
	for i, x := range a.Artifact.Files {
		w := fmt.Sprintf("%s：artifact.files[%d]", where, i)
		if x == nil || strings.TrimSpace(x.From) == "" {
			bad("%s：from（本机路径）不能为空", w)
			continue
		}
		x.To = strings.TrimRight(strings.TrimSpace(x.To), "/")
		if x.To == "" {
			x.To = filepath.Base(filepath.FromSlash(strings.TrimRight(x.From, `/\`)))
			if strings.Contains(x.To, "${") {
				bad("%s：%s 的文件名里有变量，请写 to（放到服务器上叫什么）", w, x.From)
				continue
			}
		}
		if strings.Contains(x.To, "${") {
			bad("%s：to 不能用变量（%s）", w, x.To)
			continue
		}
		if err := proto.CheckExtraFile(x.To, program); err != nil {
			bad("%s：%v", w, err)
			continue
		}
		for _, o := range seen {
			if o == x.To || strings.HasPrefix(x.To, o+"/") || strings.HasPrefix(o, x.To+"/") {
				bad("%s：放到的位置 %s 和 %s 重复", w, x.To, o)
			}
		}
		seen = append(seen, x.To)
	}
}

func checkServedStatic(f *File, a *App, where string, units map[string]string, bad func(string, ...any)) {
	st, s := &a.Static, &a.Service
	if st.NginxInstance != "" && st.NginxInstance != proto.SystemNginxInstance && !regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`).MatchString(st.NginxInstance) && !strings.Contains(st.NginxInstance, "${") {
		bad("%s：static.nginxInstance 必须是已安装的 nginx 实例名", where)
	}
	if st.NginxInstance != "" && st.Nginx != "" {
		bad("%s：选择 nginxInstance 后不再指定 static.nginx 版本", where)
	}
	for _, name := range st.Domains {
		if !regexp.MustCompile(`^[A-Za-z0-9_.*-]+$`).MatchString(name) {
			bad("%s：无效站点域名 %q", where, name)
		}
	}
	m := rePort.FindStringSubmatch(st.Port)
	if m == nil {
		bad("%s：static.port 要写端口（如 8081）或 地址:端口（如 127.0.0.1:8081），当前 %q", where, st.Port)
	} else if n, _ := strconv.Atoi(m[2]); n < 1 || n > 65535 {
		bad("%s：static.port 超出范围：%s", where, st.Port)
	}
	if st.Nginx != "" && !reSeries.MatchString(st.Nginx) {
		bad("%s：static.nginx 要写 nginx 的版本系列，如 1.30（当前 %q）", where, st.Nginx)
	}
	if st.Reload != "" {
		bad("%s：由 Ferry 的 nginx 运行时不需要 static.reload（部署后会自动重启）", where)
	}
	if st.Link != "" {
		bad("%s：由 Ferry 的 nginx 运行时不需要 static.link", where)
	}
	for p, to := range st.Proxy {
		if !reProxyPath.MatchString(p) {
			bad("%s：static.proxy 的路径要以 / 开头（如 /prod-api/），当前 %q", where, p)
		}
		if !reProxyTo.MatchString(to) {
			bad("%s：static.proxy 的后端要写 http://地址:端口/路径（如 http://127.0.0.1:8080/），当前 %q", where, to)
		}
	}
	if st.MaxBody != "" && !reNgxSize.MatchString(st.MaxBody) {
		bad("%s：static.maxBody 要写大小，如 100m（当前 %q）", where, st.MaxBody)
	}
	for _, l := range st.Server {
		if strings.ContainsAny(l, "{}") && strings.Count(l, "{") != strings.Count(l, "}") {
			bad("%s：static.server 的每一行要是完整的配置（括号成对）：%q", where, l)
		}
	}
	if h := st.HTTPS; h != nil {
		checkHTTPS(h, m, where, bad)
	}
	if s.Unit == "" {
		if st.NginxInstance == "" {
			s.Unit = f.Project + "-" + strings.ToLower(a.Name)
		}
	}
	if st.NginxInstance == "" && !strings.HasSuffix(s.Unit, ".service") {
		s.Unit += ".service"
	}
	if st.NginxInstance == "" && !reUnit.MatchString(s.Unit) {
		bad("%s：unit %q 无效", where, s.Unit)
	}
	if other, ok := units[s.Unit]; ok && st.NginxInstance == "" {
		bad("%s：unit %s 与 %s 重复", where, s.Unit, other)
	}
	if st.NginxInstance == "" {
		units[s.Unit] = a.Name
	} else if s.Unit != "" {
		bad("%s：共用 nginx 的站点不需要 service.unit", where)
	}
	if s.User != "" && !reUser.MatchString(s.User) {
		bad("%s：user %q 无效", where, s.User)
	}
	// 默认健康检查：首页能打开
	if h := &st.Health; h.TCP == "" && h.HTTP == "" {
		host := "127.0.0.1"
		if m != nil && m[1] != "" && m[1] != "localhost" {
			host = m[1]
		}
		if m != nil {
			h.HTTP = "http://" + host + ":" + m[2] + "/"
			h.Status = []int{200}
			if st.HTTPS != nil && st.HTTPS.IsRedirect() {
				h.Status = []int{301} // http 都跳到 https：跳转说明 nginx 起来了（https 的证书这时可能还是临时的）
			}
		}
	}
}

// hasServiceExtras service 里除 unit、user、description 之外还设置了别的。
func hasServiceExtras(s *Service) bool {
	return s.Workdir != "" || s.Java != "" || s.Runtime != nil || len(s.JvmArgs) > 0 || len(s.Args) > 0 || len(s.Exec) > 0 ||
		len(s.Env) > 0 || len(s.EnvFile) > 0 || len(s.ExecStartPre) > 0 || len(s.Conflicts) > 0 || len(s.SharedDirs) > 0 ||
		len(s.DiagLogs) > 0 || s.ArchiveLogs != nil || s.Health.TCP != "" || s.Health.HTTP != "" ||
		len(s.UnitExtra.Unit)+len(s.UnitExtra.Service)+len(s.UnitExtra.Install) > 0 || s.ZeroDowntime != nil
}

var reSeries = regexp.MustCompile(`^\d+\.\d+$`)

func normalizeStaticHealth(h *Health) {
	if h.TCP != "" && !strings.Contains(h.TCP, ":") {
		h.TCP = "127.0.0.1:" + h.TCP
	}
	if h.Timeout == 0 {
		h.Timeout = Duration(30 * time.Second)
	}
}

// checkRoot 服务器上的路径：绝对路径、至少两级、不能是系统目录。
func checkRoot(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q 必须是绝对路径", p)
	}
	clean := strings.TrimRight(p, "/")
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	if len(parts) < 2 {
		return fmt.Errorf("%q 太浅，至少两级目录（如 /opt/myapp）", p)
	}
	for _, s := range parts {
		if s == ".." || s == "." || s == "" {
			return fmt.Errorf("%q 不能包含 . 或 .. 或空段", p)
		}
	}
	switch "/" + parts[0] {
	case "/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/sbin", "/sys", "/usr":
		if !(parts[0] == "usr" && len(parts) >= 3 && (parts[1] == "local" || parts[1] == "share")) {
			return fmt.Errorf("%q 在系统目录下", p)
		}
	}
	return nil
}

// App 按名字（不区分大小写）找应用。
func (f *File) App(name string) *App {
	for _, a := range f.Apps {
		if a != nil && strings.EqualFold(a.Name, name) {
			return a
		}
	}
	return nil
}

// Target 按名字找目标。
func (f *File) Target(name string) (*Target, error) {
	if t, ok := f.Targets[name]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("没有目标 %q（可用：%s）", name, strings.Join(f.TargetNames(), ", "))
}

// TargetNames 目标名（排序）。
func (f *File) TargetNames() []string {
	names := make([]string, 0, len(f.Targets))
	for n := range f.Targets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Allows 目标是否允许部署某应用。
func (t *Target) Allows(app string) bool {
	if len(t.Apps) == 0 {
		return true
	}
	for _, n := range t.Apps {
		if strings.EqualFold(n, app) {
			return true
		}
	}
	return false
}

// Ordered 按 order、配置顺序排列的应用。
func (f *File) Ordered() []*App {
	out := append([]*App(nil), f.Apps...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].index < out[j].index
	})
	return out
}

// Select 解析应用列表（逗号分隔或多个参数，"all" 或空 = 目标上全部），返回按顺序排列的应用。
func (f *File) Select(t *Target, names []string) ([]*App, error) {
	var want []string
	for _, n := range names {
		for _, p := range strings.Split(n, ",") {
			if p = strings.TrimSpace(p); p != "" {
				want = append(want, p)
			}
		}
	}
	all := len(want) == 0
	for _, n := range want {
		if strings.EqualFold(n, "all") {
			all = true
		}
	}
	picked := map[*App]bool{}
	if !all {
		for _, n := range want {
			a := f.App(n)
			if a == nil {
				return nil, fmt.Errorf("没有应用 %q（可用：%s）", n, strings.Join(f.AppNames(), ", "))
			}
			if !t.Allows(a.Name) {
				return nil, fmt.Errorf("目标 %s 不允许部署 %s", t.Name, a.Name)
			}
			picked[a] = true
		}
	}
	var out []*App
	for _, a := range f.Ordered() {
		if (all && t.Allows(a.Name)) || picked[a] {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("目标 %s 上没有可部署的应用", t.Name)
	}
	return out, nil
}

// AppNames 应用名（按顺序）。
func (f *File) AppNames() []string {
	var out []string
	for _, a := range f.Ordered() {
		out = append(out, a.Name)
	}
	return out
}

// ResolveBuild 应用的构建（没有则 nil）。
func (f *File) ResolveBuild(a *App) *Build {
	if a.Build == nil {
		return nil
	}
	if a.Build.Inline != nil {
		return a.Build.Inline
	}
	return f.Builds[a.Build.Ref]
}

// KeepReleases 应用在目标上保留的版本数。
func (f *File) KeepReleases(t *Target, a *App) int {
	if a.KeepReleases > 0 {
		return a.KeepReleases
	}
	return t.KeepReleases
}

// LocalPath 把配置里的本机路径转为绝对路径（相对配置目录，支持 ~）。
func (f *File) LocalPath(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.Dir, p)
	}
	return filepath.Clean(p)
}

// checkHooks 部署钩子：命令不能为空，用户名有效，超时不超过 2 小时。
func checkHooks(a *App, where string, bad func(string, ...any)) {
	for _, g := range []struct {
		name  string
		hooks []*Hook
	}{{"beforeSwitch", a.Hooks.BeforeSwitch}, {"afterSwitch", a.Hooks.AfterSwitch}} {
		for i, h := range g.hooks {
			at := fmt.Sprintf("%s：hooks.%s[%d]", where, g.name, i)
			switch {
			case h == nil || strings.TrimSpace(h.Run) == "":
				bad("%s：run 不能为空", at)
			case h.User != "" && !reUser.MatchString(h.User):
				bad("%s：user %q 无效", at, h.User)
			case h.Timeout < 0 || h.Timeout.Std() > 2*time.Hour:
				bad("%s：timeout 最多 2h（当前 %s）", at, h.Timeout.Std())
			}
		}
	}
}

// checkZD 不停机发布：两个不同的端口，listen 不能和它们一样，drain 最多 10 分钟。
func checkZD(z *ZeroDowntime, where string, bad func(string, ...any)) {
	at := where + "：service.zeroDowntime"
	if len(z.Ports) != 2 || z.Ports[0] == z.Ports[1] {
		bad("%s.ports 要写两个不同的端口，如 [9201, 19201]（新版本在空着的那个上启动）", at)
		return
	}
	for _, p := range z.Ports {
		if p <= 0 || p > 65535 {
			bad("%s.ports：端口 %d 不对", at, p)
		}
	}
	if z.Listen != "" {
		host, port := z.Listen, ""
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			host, port = host[:i], host[i+1:]
		} else {
			host, port = "", host
		}
		n, err := strconv.Atoi(port)
		switch {
		case err != nil || n <= 0 || n > 65535 || strings.ContainsAny(host, " ;"):
			bad("%s.listen 写端口或 地址:端口，如 8080、0.0.0.0:8080（当前 %q）", at, z.Listen)
		case n == z.Ports[0] || n == z.Ports[1]:
			bad("%s.listen 不能和 ports 里的端口一样：nginx 监听 %d，程序轮流用 ports 里的两个", at, n)
		}
	}
	if z.Nginx != "" && z.Listen == "" {
		bad("%s.nginx 只在设置了 listen 时有用", at)
	}
	if z.Drain != nil && (*z.Drain < 0 || z.Drain.Std() > 10*time.Minute) {
		bad("%s.drain 最多 10m（当前 %s）", at, z.Drain.Std())
	}
}

// reJDKVersion 完整的 JDK 版本：25.0.4.1+1、17.0.13+11、21+35，可以带 -LTS 之类的后缀。
var reJDKVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*\+[0-9]+(-[A-Za-z0-9.]+)?$`)

// jdkMajor 完整版本的大版本：25.0.4.1+1 → 25。
func jdkMajor(v string) int {
	n, _ := strconv.Atoi(strings.FieldsFunc(v, func(r rune) bool { return r < '0' || r > '9' })[0])
	return n
}
