package engine

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/artifact"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

var reFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// remoteScope 应用在目标上的变量作用域（服务器路径）。
func remoteScope(f *config.File, t *config.Target, a *config.App) *config.Scope {
	root, err := f.AppRoot(t, a)
	if err != nil {
		root = t.Root // 加载配置时已校验过
	}
	appDir := path.Join(root, "apps", a.Name)
	b := map[string]string{
		"project":  f.Project,
		"target":   t.Name,
		"app":      a.Name,
		"root":     t.Root,
		"appDir":   appDir,
		"versions": path.Join(appDir, "versions"),
		"releases": path.Join(appDir, "versions"), // 以前的名字
		"running":  path.Join(appDir, "running"),  // 运行中的版本：程序和它的日志
		"current":  path.Join(appDir, "running"),  // 以前的名字
		"logs":     path.Join(appDir, "running", "logs"),
		"data":     path.Join(appDir, "data"),
		"shared":   path.Join(appDir, "data"), // 以前的名字
		"host":     t.Host,
		"user":     appUser(t, a),
	}
	if fn := artifactFileName(a); fn != "" {
		b["artifact"] = path.Join(appDir, "running", fn)
	}
	layers := []map[string]string{a.Vars, t.Vars, f.Vars}
	// 不停机发布：${port}、${slot} 在各槽位的单元里是槽位自己的（见 slotScope），别处按槽位 a
	if z := a.Service.ZeroDowntime; z != nil && len(z.Ports) == 2 {
		b["slot"] = "a"
		layers = append([]map[string]string{{"port": strconv.Itoa(z.Ports[0])}}, layers...)
	}
	return &config.Scope{Builtins: b, Layers: layers}
}

// localScope 本机路径用的变量作用域。
func localScope(f *config.File, t *config.Target, a *config.App) *config.Scope {
	return &config.Scope{
		Builtins: map[string]string{"project": f.Project, "target": t.Name, "app": a.Name, "configDir": f.Dir},
		Layers:   []map[string]string{a.Vars, t.Vars, f.Vars},
	}
}

func appUser(t *config.Target, a *config.App) string {
	if a.Service.User != "" {
		return a.Service.User
	}
	return t.RunAs
}

// artifactFileName 版本目录里的文件名：产物路径的文件名（变量在本机展开之前，取字面值的 base）。
func artifactFileName(a *config.App) string {
	if a.Type == proto.TypeStatic {
		return ""
	}
	return filepath.Base(filepath.FromSlash(a.Artifact.Path))
}

// LocalArtifact 产物的本机绝对路径。
func LocalArtifact(f *config.File, t *config.Target, a *config.App) (string, error) {
	p, err := localScope(f, t, a).Expand(a.Artifact.Path)
	if err != nil {
		return "", fmt.Errorf("%s artifact.path：%w", a.Name, err)
	}
	return f.LocalPath(p), nil
}

// LocalFiles 附带文件：本机路径与放到版本目录里的位置。
func LocalFiles(f *config.File, t *config.Target, a *config.App) ([]artifact.ExtraFile, error) {
	sc := localScope(f, t, a)
	var out []artifact.ExtraFile
	for i, x := range a.Artifact.Files {
		p, err := sc.Expand(x.From)
		if err != nil {
			return nil, fmt.Errorf("%s artifact.files[%d]：%w", a.Name, i, err)
		}
		out = append(out, artifact.ExtraFile{Local: f.LocalPath(p), To: x.To, Exec: x.Exec})
	}
	return out, nil
}

// Spec 把配置渲染成发给 agent 的 AppSpec（单元文件在这里生成）。
func Spec(f *config.File, t *config.Target, a *config.App) (proto.AppSpec, error) {
	s, err := baseSpec(f, t, a)
	if err != nil || !a.Hooks.Any() {
		return s, err
	}
	return s, hookSpec(f, t, a, &s)
}

// hookEnvNames 钩子执行时 agent 给的环境变量：命令里写 ${FERRY_RELEASE} 也原样交给 shell，不当作 ferry.yaml 的变量
var hookEnvNames = []string{"FERRY_PROJECT", "FERRY_TARGET", "FERRY_APP", "FERRY_PHASE", "FERRY_RELEASE", "FERRY_PREVIOUS",
	"FERRY_VERSION_DIR", "FERRY_RUNNING_DIR", "FERRY_APP_DIR", "FERRY_DATA_DIR", "FERRY_ACTOR"}

// hookSpec 部署钩子：命令里的变量展开，默认以应用的运行用户执行；java / binary 的钩子拿到和服务一样的 env、envFile。
func hookSpec(f *config.File, t *config.Target, a *config.App, s *proto.AppSpec) error {
	sc := remoteScope(f, t, a)
	if s.FileName != "" {
		sc.Builtins["artifact"] = path.Join(sc.Builtins["running"], s.FileName)
	}
	for _, n := range hookEnvNames {
		sc.Builtins[n] = "${" + n + "}"
	}
	conv := func(field string, hs []*config.Hook) ([]proto.Hook, error) {
		var out []proto.Hook
		for i, h := range hs {
			run, err := sc.Expand(h.Run)
			if err != nil {
				return nil, fmt.Errorf("%s hooks.%s[%d]：%w", a.Name, field, i, err)
			}
			user := h.User
			if user == "" {
				user = appUser(t, a)
			}
			out = append(out, proto.Hook{Run: run, User: user, TimeoutSec: int(h.Timeout.Std() / time.Second), Optional: h.Optional})
		}
		return out, nil
	}
	var hooks proto.Hooks
	var err error
	if hooks.BeforeSwitch, err = conv("beforeSwitch", a.Hooks.BeforeSwitch); err != nil {
		return err
	}
	if hooks.AfterSwitch, err = conv("afterSwitch", a.Hooks.AfterSwitch); err != nil {
		return err
	}
	s.Hooks = &hooks
	if a.Type == proto.TypeStatic {
		return nil
	}
	if s.HookEnv, err = sc.ExpandMap(a.Service.Env); err != nil {
		return fmt.Errorf("%s service.env：%w", a.Name, err)
	}
	if s.HookEnvFiles, err = sc.ExpandAll(a.Service.EnvFile); err != nil {
		return fmt.Errorf("%s service.envFile：%w", a.Name, err)
	}
	return nil
}

func baseSpec(f *config.File, t *config.Target, a *config.App) (proto.AppSpec, error) {
	s := proto.AppSpec{Name: a.Name, Type: a.Type, KeepReleases: f.KeepReleases(t, a)}
	sc := remoteScope(f, t, a)
	wrap := func(field string, err error) error { return fmt.Errorf("%s %s：%w", a.Name, field, err) }
	root, err := f.AppRoot(t, a)
	if err != nil {
		return s, wrap("root", err)
	}
	if root != t.Root {
		s.Root = root
	}

	if a.Type == proto.TypeStatic {
		if a.Static.Served() {
			return s, servedStaticSpec(f, t, a, sc, &s)
		}
		var err error
		if s.Link, err = sc.Expand(a.Static.Link); err != nil {
			return s, wrap("static.link", err)
		}
		if s.Reload, err = sc.Expand(a.Static.Reload); err != nil {
			return s, wrap("static.reload", err)
		}
		if s.Health, err = health(sc, a.Static.Health); err != nil {
			return s, wrap("static.health", err)
		}
		return s, nil
	}

	if proto.DirService(a.Type) {
		// 上传的是整个目录：${artifact} 就是运行中的版本目录
		sc.Builtins["artifact"] = sc.Builtins["running"]
		if !a.Deps.Off && (a.Type == proto.TypePython || a.Type == proto.TypeNode) {
			d := proto.Deps{Kind: a.Type}
			var err error
			if d.Registry, err = sc.Expand(a.Deps.Registry); err != nil {
				return s, wrap("deps.registry", err)
			}
			if d.Python, err = sc.Expand(a.Deps.Python); err != nil {
				return s, wrap("deps.python", err)
			}
			if strings.ContainsAny(d.Registry+d.Python, " '\"\n") {
				return s, fmt.Errorf("%s：deps 里不能有空格、引号", a.Name)
			}
			s.Deps = &d
		}
	} else {
		src, err := localScope(f, t, a).Expand(a.Artifact.Path)
		if err != nil {
			return s, wrap("artifact.path", err)
		}
		s.FileName = filepath.Base(filepath.FromSlash(src))
		if !reFileName.MatchString(s.FileName) {
			return s, fmt.Errorf("%s：产物文件名 %q 只能包含字母、数字、. _ -", a.Name, s.FileName)
		}
		for _, x := range a.Artifact.Files {
			if err := proto.CheckExtraFile(x.To, s.FileName); err != nil {
				return s, wrap("artifact.files", err)
			}
		}
		// 文件名里带变量时以展开后的为准
		sc.Builtins["artifact"] = path.Join(sc.Builtins["running"], s.FileName)
	}

	svc := a.Service
	if a.Type == proto.TypeJava && svc.Runtime != nil {
		s.RuntimeJava, s.RuntimeVersion = svc.Runtime.Java, svc.Runtime.Version
	} else if a.Type == proto.TypeJava {
		if j := svc.Java; j != "" {
			if v, err := sc.Expand(j); err == nil {
				s.Java = v
			}
		} else {
			s.Java = "/usr/bin/java"
		}
	}
	s.Unit = svc.Unit
	s.User = appUser(t, a)
	s.Conflict = svc.Conflicts
	if s.SharedDirs, err = sc.ExpandAll(svc.SharedDirs); err != nil {
		return s, wrap("sharedDirs", err)
	}
	if svc.ArchiveLogs != nil {
		if s.ArchiveLogs, err = sc.ExpandAll(*svc.ArchiveLogs); err != nil {
			return s, wrap("archiveLogs", err)
		}
	} else {
		for _, d := range s.SharedDirs {
			if d == "logs" || strings.HasPrefix(d, "logs/") {
				s.ArchiveLogs = []string{"logs"} // 默认：sharedDirs 里有 logs 就按版本归档它
				break
			}
		}
	}
	if s.Health, err = health(sc, svc.Health); err != nil {
		return s, wrap("health", err)
	}
	for _, p := range svc.DiagLogs {
		v, err := sc.Expand(p)
		if err != nil {
			return s, wrap("diagLogs", err)
		}
		if !strings.HasPrefix(v, "/") {
			v = path.Join(sc.Builtins["shared"], v)
		}
		s.DiagLogs = append(s.DiagLogs, v)
	}
	if s.UnitText, err = unitText(f, t, a, sc); err != nil {
		return s, err
	}
	// 目录里的启动程序（.NET 自带运行时发布的程序）：在 Windows 上构建的没有可执行权限，上传时标上
	if proto.DirService(a.Type) && len(svc.Exec) > 0 {
		if p, err := sc.Expand(svc.Exec[0]); err == nil && strings.HasPrefix(p, sc.Builtins["running"]+"/") {
			// .venv、node_modules 里的是服务器上装依赖时才有的，不是上传的
			if rel := strings.TrimPrefix(p, sc.Builtins["running"]+"/"); !strings.HasPrefix(rel, ".venv/") && !strings.HasPrefix(rel, "node_modules/") {
				s.Executables = []string{rel}
			}
		}
	}
	if svc.ZeroDowntime != nil {
		if err := zdSpec(f, t, a, &s); err != nil {
			return s, err
		}
	}
	return s, nil
}

func health(sc *config.Scope, h config.Health) (proto.Health, error) {
	var out proto.Health
	var err error
	if out.TCP, err = sc.Expand(h.TCP); err != nil {
		return out, err
	}
	if out.HTTP, err = sc.Expand(h.HTTP); err != nil {
		return out, err
	}
	if out.Contains, err = sc.Expand(h.Contains); err != nil {
		return out, err
	}
	out.Status = h.Status
	out.TimeoutSec = int(h.Timeout.Std() / time.Second)
	out.StableSec = int(h.Stable.Std() / time.Second)
	return out, nil
}

// unitText 生成 systemd 单元文件。unitExtra 的行追加在各段末尾（systemd 中后出现的单值设置覆盖前面的）。
func unitText(f *config.File, t *config.Target, a *config.App, sc *config.Scope) (string, error) {
	svc := a.Service
	wrap := func(field string, err error) error { return fmt.Errorf("%s service.%s：%w", a.Name, field, err) }
	java := svc.Java
	if java == "" {
		java = "/usr/bin/java"
	}
	if svc.Runtime != nil {
		java = t.RuntimeJava(svc.Runtime.Java) // Ferry 提供的 JDK
		if svc.Runtime.Version != "" {
			java = t.RuntimeJavaVersion(svc.Runtime.Version) // 锁定的版本：它自己的链接
		}
	}
	java, err := sc.Expand(java)
	if err != nil {
		return "", wrap("java", err)
	}
	sc.Builtins["java"] = java

	var argv []string
	if len(svc.Exec) > 0 {
		if argv, err = sc.ExpandAll(svc.Exec); err != nil {
			return "", wrap("exec", err)
		}
	} else {
		jvm, err := sc.ExpandAll(svc.JvmArgs)
		if err != nil {
			return "", wrap("jvmArgs", err)
		}
		args, err := sc.ExpandAll(svc.Args)
		if err != nil {
			return "", wrap("args", err)
		}
		if a.Type == proto.TypeJava {
			argv = append(append([]string{java}, jvm...), "-jar", sc.Builtins["artifact"])
		} else {
			argv = []string{sc.Builtins["artifact"]}
		}
		argv = append(argv, args...)
	}
	if len(argv) > 0 && proto.DirService(a.Type) && !strings.Contains(argv[0], "/") {
		argv = append([]string{"/usr/bin/env"}, argv...) // node、python3、dotnet：在 PATH 里找
	}
	if len(argv) == 0 || !strings.HasPrefix(argv[0], "/") {
		return "", fmt.Errorf("%s：启动命令的第一项必须是绝对路径（当前 %q）", a.Name, strings.Join(argv, " "))
	}
	workdir := svc.Workdir
	if workdir == "" {
		workdir = "${running}" // 运行中的版本目录：程序按相对路径写的日志（logs/…）也和程序放在一起
	}
	if workdir, err = sc.Expand(workdir); err != nil {
		return "", wrap("workdir", err)
	}
	env, err := sc.ExpandMap(svc.Env)
	if err != nil {
		return "", wrap("env", err)
	}
	if _, ok := env["PYTHONUNBUFFERED"]; a.Type == proto.TypePython && !ok {
		if env == nil {
			env = map[string]string{}
		}
		env["PYTHONUNBUFFERED"] = "1" // print 的输出马上进日志
	}
	if _, ok := env["NODE_ENV"]; a.Type == proto.TypeNode && !ok {
		if env == nil {
			env = map[string]string{}
		}
		env["NODE_ENV"] = "production"
	}
	envFiles, err := sc.ExpandAll(svc.EnvFile)
	if err != nil {
		return "", wrap("envFile", err)
	}
	pre, err := sc.ExpandAll(svc.ExecStartPre)
	if err != nil {
		return "", wrap("execStartPre", err)
	}
	extra := map[string][]string{}
	for sec, lines := range map[string][]string{"unit": svc.UnitExtra.Unit, "service": svc.UnitExtra.Service, "install": svc.UnitExtra.Install} {
		if extra[sec], err = sc.ExpandAll(lines); err != nil {
			return "", wrap("unitExtra."+sec, err)
		}
	}
	desc := svc.Description
	if desc == "" {
		desc = f.Project + " " + a.Name
	}
	if desc, err = sc.Expand(desc); err != nil {
		return "", wrap("description", err)
	}

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("%s — 由 ferry 生成，手工修改会在下次部署时被覆盖", agent.ManagedMarker)
	line("# project=%s app=%s target=%s", f.Project, a.Name, t.Name)
	line("[Unit]")
	line("Description=%s", oneLine(desc))
	line("After=network-online.target")
	line("Wants=network-online.target")
	if len(svc.Conflicts) > 0 {
		line("Conflicts=%s", strings.Join(svc.Conflicts, " "))
	}
	for _, l := range extra["unit"] {
		line("%s", oneLine(l))
	}
	line("")
	line("[Service]")
	line("Type=simple")
	line("User=%s", appUser(t, a))
	line("WorkingDirectory=%s", sdEscapePercent(workdir))
	for _, ef := range envFiles {
		line("EnvironmentFile=%s", sdEscapePercent(ef))
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line("Environment=%s", sdQuote(k+"="+env[k], true))
	}
	for _, p := range pre {
		line("ExecStartPre=%s", oneLine(p))
	}
	line("ExecStart=%s", sdCommand(argv))
	if a.Type == proto.TypeJava {
		line("SuccessExitStatus=143")
	}
	line("Restart=on-failure")
	line("RestartSec=5")
	line("TimeoutStopSec=45")
	line("LimitNOFILE=65536")
	for _, l := range extra["service"] {
		line("%s", oneLine(l))
	}
	line("")
	line("[Install]")
	line("WantedBy=multi-user.target")
	for _, l := range extra["install"] {
		line("%s", oneLine(l))
	}
	return b.String(), nil
}

func oneLine(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", " ") }

func sdEscapePercent(s string) string { return strings.ReplaceAll(oneLine(s), "%", "%%") }

// sdCommand 拼 ExecStart：需要时加引号；% 写成 %%（systemd 的说明符）。$NAME 保留，由 systemd 按环境变量展开。
func sdCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = sdQuote(a, false)
	}
	return strings.Join(parts, " ")
}

func sdQuote(s string, always bool) string {
	s = sdEscapePercent(s)
	if !always && s != "" && !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
