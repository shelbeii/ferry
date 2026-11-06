package agent

import (
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type infoFields struct{ fields []proto.AppInfoField }

func (f *infoFields) add(label, value string) {
	if strings.TrimSpace(value) != "" {
		f.fields = append(f.fields, proto.AppInfoField{Label: label, Value: value})
	}
}
func (f *infoFields) path(label, value string) {
	if strings.TrimSpace(value) != "" {
		f.fields = append(f.fields, proto.AppInfoField{Label: label, Value: value, Path: filepath.IsAbs(value) && !strings.HasSuffix(value, " (deleted)")})
	}
}
func (f *infoFields) section(id, title string) proto.AppInfoSection {
	return proto.AppInfoSection{ID: id, Title: title, Fields: f.fields}
}

func appInfoUnitCommand(unit string) string {
	return "systemctl show --no-pager -p LoadState -p FragmentPath -p DropInPaths -p ExecStart -p WorkingDirectory -p User -p Group -p EnvironmentFiles -p EnvironmentFile -p Restart -p NRestarts -p NeedDaemonReload " + shellQuote(unit)
}

func appInfoUnitProperties(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "LoadState", "FragmentPath", "DropInPaths", "ExecStart", "WorkingDirectory", "User", "Group", "EnvironmentFiles", "EnvironmentFile", "Restart", "NRestarts", "NeedDaemonReload":
			props[key] = value
		}
	}
	// systemd 219 将环境变量文件序列化为单数的 EnvironmentFile。
	if _, ok := props["EnvironmentFiles"]; !ok {
		if value, ok := props["EnvironmentFile"]; ok {
			props["EnvironmentFiles"] = value
		}
	}
	return props
}

func appInfoCommandError(err error, out string) string {
	message := strings.TrimSpace(out)
	if len(message) > 1024 {
		message = message[:1024] + "…"
	}
	if message == "" {
		return err.Error()
	}
	return err.Error() + "：" + message
}

func (a *Agent) appInfoUnitProperties(unit string) (map[string]string, error) {
	out, err := a.Sys.Shell(appInfoUnitCommand(unit), 5*time.Second)
	props := appInfoUnitProperties(out)
	if props["LoadState"] == "not-found" {
		return nil, fmt.Errorf("服务 %s 不存在", unit)
	}
	// 旧 systemd 对不认识的属性返回非零，但仍输出其他有效属性。
	// 必须确认服务已加载且有核心属性，不能将 D-Bus 故障或不存在的服务当成兼容性问题。
	_, hasCommand := props["ExecStart"]
	if err != nil && !(props["LoadState"] == "loaded" && (hasCommand || props["FragmentPath"] != "")) {
		// 诊断只保留错误文本，启动参数等属性由详情里的脱敏逻辑展示。
		var diagnostics []string
		for _, line := range strings.Split(out, "\n") {
			key, _, _ := strings.Cut(line, "=")
			if _, property := props[key]; !property {
				diagnostics = append(diagnostics, line)
			}
		}
		if state := props["LoadState"]; state != "" {
			diagnostics = append(diagnostics, "LoadState="+state)
		}
		return nil, errors.New(appInfoCommandError(err, strings.Join(diagnostics, "\n")))
	}
	return props, nil
}

func (a *Agent) appInfo() int {
	if len(a.req.Apps) != 1 {
		return a.result(proto.CodeBadInput, "查看运行情况一次只能指定一个应用", nil)
	}
	s := a.req.Apps[0]
	current := a.current(s.Name)
	var savedUnit []byte
	if current != "" {
		savedUnit, _ = infoReadFile(a.verFile(s.Name, current, "unit.service"), 256*1024)
	}
	binding := a.readSiteBinding(s.Name)
	s = a.deployedStaticSpec(s)
	r := proto.AppInfo{Name: s.Name, Type: s.Type, CollectedAt: a.Sys.Now()}
	r.Status = a.appStatus(&s, a.Sys.NginxDump)
	var installed *proto.SoftwareInstance
	unit := s.Unit
	if s.NginxInstance != "" {
		var err error
		installed, err = a.loadHostingInstance(s.NginxInstance)
		if err != nil {
			r.Notes = append(r.Notes, "无法读取 Nginx 实例："+err.Error())
		} else {
			unit = installed.Unit
		}
	}
	var unitText, source string
	if len(savedUnit) > 0 {
		unitText, source = string(savedUnit), "当前部署版本保存的服务配置"
	}
	if unitText == "" {
		unitText, source = s.UnitText, "当前项目配置（尚未确认已部署）"
	}
	if s.Type == proto.TypeStatic && binding.Instance != "" && current != "" && s.NginxInstance != "" {
		source = "已发布的 Nginx 站点绑定和实例记录"
	}
	props := map[string]string{}
	if unit != "" {
		loaded, err := a.appInfoUnitProperties(unit)
		if err != nil {
			r.Notes = append(r.Notes, "无法读取 systemd 服务详情，使用可确认的部署记录："+err.Error())
		} else {
			props = loaded
		}
	}
	args := unitExecArgs(unitText)
	if loaded := execShowArgs(props["ExecStart"]); len(loaded) > 0 {
		args, source = loaded, "systemd 当前加载的服务配置"
	}
	var processes []ProcessSnapshot
	var unitState UnitProps
	if unit != "" {
		unitState = a.Sys.UnitProps(unit)
		processes = a.Sys.Processes(unitState.PID)
	}
	p := runtimeProcess(s.Type, processes, s.FileName)
	if len(processes) > 0 && p.PID == 0 {
		p = processes[0]
	}
	actualRuntime := p.PID > 0 && runtimeMatches(s.Type, p)
	if s.Type == proto.TypeBinary && s.FileName != "" && filepath.Base(strings.TrimSuffix(p.Exe, " (deleted)")) != s.FileName {
		actualRuntime = false
	}
	if p.PID > 0 {
		r.Notes = append(r.Notes, p.Notes...)
	}
	if r.Status.PID > 0 && len(processes) == 0 {
		r.Notes = append(r.Notes, "服务报告仍在运行，但采集时未能读取进程；进程可能已退出或正在重启。")
	}
	var runtime infoFields
	runtime.add("应用类型", appTypeTitle(s.Type))
	runtime.add("配置依据", source)
	executable := commandExecutable(args)
	if actualRuntime {
		executable = p.Exe
		runtime.add("运行环境依据", fmt.Sprintf("运行中的进程 /proc/%d/exe", p.PID))
	} else {
		runtime.add("运行环境依据", "服务启动命令；未确认有匹配的运行中进程")
	}
	runtime.path("服务配置的启动程序", commandExecutable(args))
	if actualRuntime {
		runtime.path("运行中的程序路径", executable)
	}
	if filepath.IsAbs(executable) {
		runtime.path("程序所在目录", filepath.Dir(executable))
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil && resolved != executable {
		runtime.path("符号链接实际位置", resolved)
	}
	a.runtimeDetails(&s, &runtime, &r, p, executable, actualRuntime, installed)
	r.Sections = append(r.Sections, runtime.section("runtime", "运行环境"))

	var service infoFields
	service.add("systemd 服务", unit)
	if unitState.Active != "" {
		service.add("服务状态", strings.TrimSpace(unitState.Active+" / "+unitState.Sub))
	}
	if unitState.PID > 0 {
		service.add("主进程 PID", strconv.Itoa(unitState.PID))
	}
	if p.PID > 0 {
		service.add("运行进程 PID", strconv.Itoa(p.PID))
		service.add("进程用户 / UID", p.User+" / "+p.UID)
		service.path("进程工作目录", p.Cwd)
		service.add("实际命令行", infoCommand(p.Args))
		if p.Threads > 0 {
			service.add("进程线程数", strconv.Itoa(p.Threads))
		}
		if p.Memory > 0 {
			service.add("进程常驻内存", fmt.Sprintf("%.1f MiB", float64(p.Memory)/(1024*1024)))
		}
	}
	if len(processes) > 1 {
		var summary []string
		for _, process := range processes {
			summary = append(summary, fmt.Sprintf("PID %d · %s", process.PID, process.Exe))
		}
		service.add("服务进程树", strings.Join(summary, "\n"))
	}
	service.add("服务启动时间", unitState.Since)
	service.add("服务启动命令", infoCommand(args))
	service.path("服务工作目录", loadedProperty(props, "WorkingDirectory", lastUnitValue(unitText, "WorkingDirectory"), "/"))
	service.add("服务用户", loadedProperty(props, "User", lastUnitValue(unitText, "User"), "root"))
	service.add("服务用户组", props["Group"])
	service.path("服务文件", props["FragmentPath"])
	service.add("服务覆盖配置", props["DropInPaths"])
	service.add("环境变量文件", loadedProperty(props, "EnvironmentFiles", strings.Join(unitValues(unitText, "EnvironmentFile"), "\n"), "未设置"))
	service.add("开机自启", map[string]string{"enabled": "是", "enabled-runtime": "是（这次开机）", "disabled": "否", "masked": "否（已屏蔽）"}[unitState.Enabled])
	restart := props["Restart"]
	if restart != "" && restart != "no" && unitState.RestartSec != "" {
		restart += "（等 " + unitState.RestartSec + " 再重启）"
	}
	service.add("重启策略", restart)
	service.add("自动重启次数", props["NRestarts"])
	if props["NeedDaemonReload"] == "yes" {
		r.Notes = append(r.Notes, "服务文件已修改，systemd 尚未重新加载；这里的服务启动命令以 systemd 当前加载的配置为准。")
	}
	if len(service.fields) > 0 {
		r.Sections = append(r.Sections, service.section("service", "进程与服务"))
	}
	var paths infoFields
	paths.path("应用目录", a.appDir(s.Name))
	running := a.runningDir(s.Name)
	if current != "" {
		running = a.releaseDir(s.Name, current)
	}
	paths.path("运行版本目录", running)
	paths.path("版本存放目录", a.versionsDir(s.Name))
	paths.path("持久化数据目录", a.sharedDir(s.Name))
	paths.path("部署记录目录", a.appMetaDir(s.Name))
	if s.FileName != "" {
		paths.path("配置的产物位置", filepath.Join(running, s.FileName))
	}
	if s.Type == proto.TypeJava && actualRuntime {
		for i, arg := range p.Args {
			if arg == "-jar" && i+1 < len(p.Args) {
				jar := p.Args[i+1]
				if !filepath.IsAbs(jar) && p.Cwd != "" {
					jar = filepath.Join(p.Cwd, jar)
				}
				paths.path("进程正在运行的 JAR", jar)
				break
			}
		}
	}
	paths.path("版本日志目录", filepath.Join(running, "logs"))
	for _, log := range s.DiagLogs {
		paths.path("配置的诊断日志", log)
	}
	if r.Status.Current != nil {
		meta := r.Status.Current
		paths.add("当前版本", meta.ID)
		paths.add("代码提交", meta.Commit)
		paths.add("代码分支", meta.Branch)
		if !meta.DeployedAt.IsZero() {
			paths.add("部署时间", meta.DeployedAt.Format(time.RFC3339))
		}
		paths.add("部署人", meta.DeployedBy)
	} else {
		r.Notes = append(r.Notes, "没有 Ferry 当前部署版本记录；配置路径不代表这些文件已经部署或运行。")
	}
	r.Sections = append(r.Sections, paths.section("paths", "目录与部署版本"))
	var network infoFields
	network.add("应用监听地址", strings.Join(r.Status.Listen, "\n"))
	for _, port := range r.Status.Extra {
		network.add("附加监听端口", port.Addr+" "+port.Use)
	}
	if s.Health.HTTP != "" {
		network.add("配置的 HTTP 健康检查", infoCommand([]string{s.Health.HTTP}))
	}
	network.add("配置的 TCP 健康检查", s.Health.TCP)
	if r.Status.Healthy != nil {
		network.add("本次健康检查", map[bool]string{true: "通过", false: "未通过"}[*r.Status.Healthy])
	}
	if len(network.fields) > 0 {
		r.Sections = append(r.Sections, network.section("network", "监听与健康检查"))
	}
	return a.result(proto.CodeOK, "", r)
}

func (a *Agent) runtimeDetails(s *proto.AppSpec, f *infoFields, r *proto.AppInfo, p ProcessSnapshot, executable string, actual bool, in *proto.SoftwareInstance) {
	running := a.runningDir(s.Name)
	if cur := a.current(s.Name); cur != "" {
		running = a.releaseDir(s.Name, cur)
	}
	switch s.Type {
	case proto.TypeJava:
		f.add("运行环境", "JDK / Java")
		java := executable
		if resolved, err := filepath.EvalSymlinks(java); err == nil {
			java = resolved
		}
		home := filepath.Dir(filepath.Dir(strings.TrimSuffix(java, " (deleted)")))
		if filepath.IsAbs(home) {
			f.path("Java 安装目录", home)
			for _, release := range []string{filepath.Join(home, "release"), filepath.Join(filepath.Dir(home), "release")} {
				if text, err := infoReadFile(release, 32*1024); err == nil {
					f.path("JDK 版本记录文件", release)
					for _, line := range strings.Split(string(text), "\n") {
						key, value, _ := strings.Cut(line, "=")
						if label := map[string]string{"JAVA_VERSION": "JDK 文件记录版本", "IMPLEMENTOR": "JDK 发行商", "OS_ARCH": "JDK 架构"}[key]; label != "" {
							f.add(label, strings.Trim(value, `"`))
						}
					}
					break
				}
			}
		}
		if actual {
			for _, lib := range p.Libraries {
				if filepath.Base(lib) == "libjvm.so" {
					f.path("JVM 动态库", lib)
				}
			}
		}
	case proto.TypePython:
		f.add("运行环境", "Python")
		venv := filepath.Join(running, ".venv")
		if exists(venv) {
			f.path("项目虚拟环境", venv)
			if real, err := filepath.EvalSymlinks(venv); err == nil {
				f.path("虚拟环境实际目录", real)
			}
			if cfg, err := infoReadFile(filepath.Join(venv, "pyvenv.cfg"), 16*1024); err == nil {
				for _, line := range strings.Split(string(cfg), "\n") {
					key, value, ok := strings.Cut(line, "=")
					if ok && (strings.TrimSpace(key) == "home" || strings.TrimSpace(key) == "version") {
						f.add("虚拟环境 "+strings.TrimSpace(key), strings.TrimSpace(value))
					}
				}
			}
		}
		f.path("依赖缓存目录", a.depsDir(s.Name))
	case proto.TypeNode:
		f.add("运行环境", "Node.js")
		modules := filepath.Join(running, "node_modules")
		if exists(modules) {
			f.path("项目依赖目录", modules)
			if real, err := filepath.EvalSymlinks(modules); err == nil {
				f.path("依赖实际目录", real)
			}
		}
		f.path("依赖缓存目录", a.depsDir(s.Name))
	case proto.TypeDotnet:
		f.add("运行环境", ".NET")
		for _, lib := range p.Libraries {
			if filepath.Base(lib) == "libcoreclr.so" {
				f.path("实际加载的 CoreCLR", lib)
				parent := filepath.Base(filepath.Dir(lib))
				if regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(parent) {
					f.add("实际 CoreCLR 版本", parent)
				}
			}
		}
		if actual && filepath.Base(executable) != "dotnet" {
			f.add("启动方式", "应用可执行程序（可能为 apphost 或自带运行时发布）")
		}
		configs, _ := filepath.Glob(filepath.Join(running, "*.runtimeconfig.json"))
		for _, config := range configs[:min(len(configs), 8)] {
			f.path("运行时声明文件", config)
			var value struct {
				RuntimeOptions struct {
					TFM       string `json:"tfm"`
					Framework struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					} `json:"framework"`
					Frameworks []struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					} `json:"frameworks"`
				} `json:"runtimeOptions"`
			}
			if raw, err := infoReadFile(config, 64*1024); err == nil && json.Unmarshal(raw, &value) == nil {
				f.add("声明的目标框架", value.RuntimeOptions.TFM)
				f.add("声明的运行时要求", strings.TrimSpace(value.RuntimeOptions.Framework.Name+" "+value.RuntimeOptions.Framework.Version))
				for _, framework := range value.RuntimeOptions.Frameworks {
					f.add("声明的运行时要求", framework.Name+" "+framework.Version)
				}
			}
		}
	case proto.TypeBinary:
		f.add("运行环境", "原生可执行程序")
		if actual {
			if build, err := buildinfo.ReadFile(fmt.Sprintf("/proc/%d/exe", p.PID)); err == nil {
				f.add("Go 编译器版本", build.GoVersion)
				f.add("Go 主模块", build.Main.Path+" "+build.Main.Version)
			}
		}
	case proto.TypeStatic:
		a.staticRuntimeDetails(s, f, r, p, in, running)
	}
	if actual && runtimeVersionFlag(s.Type, p.Exe) != "" {
		out, err := a.appInfoRuntimeVersion(s.Type, p)
		if err == nil {
			f.add("运行环境版本", strings.TrimSpace(out))
		} else {
			r.Notes = append(r.Notes, "未能读取运行环境版本："+err.Error())
		}
	}
}

func appInfoVersionCommand(p ProcessSnapshot, program, flag string) string {
	command := shellQuote(program) + " " + flag + " 2>&1"
	if p.UID == "0" {
		return command
	}
	if p.User == "" {
		return ""
	}
	return "runuser -u " + shellQuote(p.User) + " -- " + command
}

func appInfoVersionFallback(kind string, p ProcessSnapshot) string {
	flag := runtimeVersionFlag(kind, p.Exe)
	if flag == "" || !path.IsAbs(p.Exe) || strings.HasSuffix(p.Exe, " (deleted)") {
		return ""
	}
	command := appInfoVersionCommand(p, p.Exe, flag)
	if command == "" {
		return ""
	}
	// 由 agent 核对 inode；版本读取仍使用运行用户。不能把升级后磁盘上的新程序
	// 或 PID 被复用后的其他程序版本，标成当前进程的版本。
	return "if [ " + shellQuote(fmt.Sprintf("/proc/%d/exe", p.PID)) + " -ef " + shellQuote(p.Exe) + " ]; then " + command + "; else printf '%s\\n' '运行进程已退出或程序文件已替换，无法确认版本' >&2; exit 1; fi"
}

func (a *Agent) appInfoRuntimeVersion(kind string, p ProcessSnapshot) (string, error) {
	flag := runtimeVersionFlag(kind, p.Exe)
	if flag == "" || p.PID <= 0 {
		return "", errors.New("未识别到可读取版本的运行程序")
	}
	command := appInfoVersionCommand(p, fmt.Sprintf("/proc/%d/exe", p.PID), flag)
	if command == "" {
		return "", errors.New("无法确认运行用户，未执行版本检查")
	}
	probe := func(command string) (string, error) {
		out, err := a.Sys.Shell(command, 5*time.Second)
		if err == nil && strings.TrimSpace(out) == "" {
			err = errors.New("程序未返回版本信息")
		}
		if err != nil {
			return "", errors.New(appInfoCommandError(err, out))
		}
		if len(out) > 4096 {
			out = out[:4096]
		}
		return out, nil
	}
	if version, err := probe(command); err == nil {
		return version, nil
	} else if fallback := appInfoVersionFallback(kind, p); fallback != "" {
		version, fallbackErr := probe(fallback)
		if fallbackErr == nil {
			return version, nil
		}
		return "", fmt.Errorf("进程程序入口：%v；实际程序文件：%w", err, fallbackErr)
	} else {
		return "", err
	}
}

func (a *Agent) staticRuntimeDetails(s *proto.AppSpec, f *infoFields, r *proto.AppInfo, p ProcessSnapshot, in *proto.SoftwareInstance, running string) {
	conf := ""
	if in != nil && in.Hosting != nil {
		f.add("运行环境", "共享 Nginx 实例")
		f.add("Nginx 实例", in.Name)
		f.add("实例登记版本", in.Version)
		f.path("实例安装目录", in.Dir)
		f.path("实例登记程序", in.Hosting.Binary)
		f.path("Nginx 主配置", in.Hosting.Config)
		f.path("站点配置目录", in.Hosting.Sites)
		conf = a.nginxSitePath(s, in)
		if binding := a.readSiteBinding(s.Name); binding.File != "" {
			conf = binding.File
		}
		f.path("本站点配置", conf)
		f.path("Nginx prefix", in.Hosting.Prefix)
	} else if s.Unit != "" {
		f.add("运行环境", "Ferry 独立 Nginx")
		conf = filepath.Join(a.appMetaDir(s.Name), "nginx.conf")
		f.path("Nginx 配置", conf)
	} else {
		f.add("运行方式", "仅发布静态文件，Web 服务器由外部配置管理")
		for _, site := range r.Status.Sites {
			f.path("检测到的站点配置", site.File)
			f.add("站点域名", strings.Join(site.Names, ", "))
			f.add("站点监听", strings.Join(site.Listen, ", "))
		}
		f.add("站点检测说明", r.Status.Serving)
	}
	if p.PID > 0 {
		if args := nginxProcessArgs(p.Args, p.Exe); len(args) > 0 {
			inv := nginxInvocation{binary: p.Exe, flags: nginxConfigFlags(args)}
			f.path("进程指定的主配置", inv.flag("-c"))
			f.path("进程指定的 prefix", inv.flag("-p"))
		}
	}
	if text, err := infoReadFile(conf, 256*1024); conf != "" && err == nil {
		for _, server := range parseNginx(string(text)) {
			f.add("站点监听", strings.Join(server.listen, ", "))
			f.add("站点域名", strings.Join(server.names, ", "))
			for _, root := range server.roots {
				f.path("站点文件目录", root.path)
			}
		}
	} else if conf != "" {
		r.Notes = append(r.Notes, "本站点配置文件不存在或不可读，站点可能尚未部署或已停用。")
	}
	if s.NginxInstance != "" || s.Unit != "" {
		f.path("站点文件目录（部署布局）", filepath.Join(running, dirWWW))
	}
}

func runtimeProcess(kind string, processes []ProcessSnapshot, fileName string) ProcessSnapshot {
	for _, p := range processes {
		if kind == proto.TypeBinary && fileName != "" && filepath.Base(strings.TrimSuffix(p.Exe, " (deleted)")) != fileName {
			continue
		}
		if runtimeMatches(kind, p) {
			return p
		}
	}
	return ProcessSnapshot{}
}
func runtimeMatches(kind string, p ProcessSnapshot) bool {
	base := filepath.Base(strings.TrimSuffix(p.Exe, " (deleted)"))
	switch kind {
	case proto.TypeJava:
		return base == "java"
	case proto.TypeStatic:
		return base == "nginx" || base == "openresty"
	case proto.TypePython:
		return regexp.MustCompile(`^python(?:\d+(?:\.\d+)*)?$`).MatchString(base)
	case proto.TypeNode:
		return base == "node" || base == "nodejs"
	case proto.TypeDotnet:
		if base == "dotnet" {
			return true
		}
		for _, lib := range p.Libraries {
			if filepath.Base(lib) == "libcoreclr.so" {
				return true
			}
		}
		return false
	case proto.TypeBinary:
		return true
	}
	return false
}
func runtimeVersionFlag(kind, executable string) string {
	base := filepath.Base(strings.TrimSuffix(executable, " (deleted)"))
	if kind == proto.TypeJava && base == "java" {
		return "-version"
	}
	if kind == proto.TypeStatic && (base == "nginx" || base == "openresty") {
		return "-v"
	}
	if kind == proto.TypePython && runtimeMatches(kind, ProcessSnapshot{Exe: executable}) || kind == proto.TypeNode && (base == "node" || base == "nodejs") {
		return "--version"
	}
	return ""
}
func unitExecArgs(text string) []string { return sdSplit(lastUnitValue(text, "ExecStart")) }
func lastUnitValue(text, key string) string {
	values := unitValues(text, key)
	if len(values) > 0 {
		return values[len(values)-1]
	}
	return ""
}
func loadedProperty(props map[string]string, key, fallback, defaultValue string) string {
	if value, ok := props[key]; ok {
		if value == "" {
			return defaultValue
		}
		return value
	}
	return fallback
}
func execShowArgs(value string) []string {
	if _, tail, ok := strings.Cut(value, "argv[]="); ok {
		command, _, _ := strings.Cut(tail, " ; ")
		return sdSplit(command)
	}
	return nil
}
func commandExecutable(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if filepath.Base(args[0]) == "env" {
		for _, arg := range args[1:] {
			if !strings.HasPrefix(arg, "-") && !strings.Contains(arg, "=") {
				return arg
			}
		}
		return ""
	}
	return strings.TrimLeft(args[0], "-+!:@")
}

var infoSecret = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[-_]?key|credential)`)

func infoCommand(args []string) string {
	var out []string
	hideNext := false
	for _, arg := range args {
		if hideNext {
			out = append(out, "******")
			hideNext = false
			continue
		}
		key, _, hasValue := strings.Cut(arg, "=")
		if infoSecret.MatchString(key) {
			if hasValue {
				arg = key + "=******"
			} else if strings.HasPrefix(arg, "-") {
				hideNext = true
			}
		}
		out = append(out, strconv.Quote(arg))
	}
	return strings.Join(out, " ")
}
func appTypeTitle(kind string) string {
	return map[string]string{"java": "Java（JAR）", "binary": "原生程序", "static": "前端静态站点", "python": "Python 服务", "node": "Node.js 服务", "dotnet": ".NET 服务"}[kind]
}
