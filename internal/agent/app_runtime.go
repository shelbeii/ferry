package agent

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func (a *Agent) appRuntimes() int {
	if len(a.req.Apps) != 1 {
		return a.result(proto.CodeBadInput, "请选择单个应用", nil)
	}
	s := &a.req.Apps[0]
	r := proto.AppRuntimeChoices{App: s.Name, Type: s.Type, CanSwitch: true, Port: s.SitePort, Choices: []proto.AppRuntimeChoice{}}
	args := unitExecArgs(s.UnitText)
	first := commandExecutable(args)
	switch s.Type {
	case proto.TypeJava:
		canUse := false
		for _, arg := range args {
			if path.Base(arg) == "java" {
				canUse = true
			}
		}
		if !canUse {
			r.CanSwitch = false
			r.Reason = "当前 Java 启动命令无法独立替换解释器，请先在配置中使用 ${java} 或独立的 java 参数。"
		}
	case proto.TypeBinary:
		r.CanSwitch = false
		r.Reason = "原生程序直接运行自身，没有独立的 JDK 或解释器可切换。请通过版本 / 回滚切换已有程序版本。"
	case proto.TypePython:
		if !strings.Contains(first, "/.venv/bin/") && !strings.HasPrefix(path.Base(first), "python") {
			r.CanSwitch = false
			r.Reason = "当前 Python 由包装脚本启动，请先在配置中改用 python 或项目 .venv 中的入口。"
		}
	case proto.TypeNode:
		if path.Base(first) != "node" && path.Base(first) != "nodejs" {
			r.CanSwitch = false
			r.Reason = "当前 Node.js 由包装脚本或进程管理器启动，请先在配置中改用直接的 node 启动命令。"
		}
	case proto.TypeDotnet:
		if path.Base(first) != "dotnet" {
			r.CanSwitch = false
			r.Reason = "当前应用通过 apphost 或自带运行时程序启动。请重新发布对应运行时版本，或先改为 dotnet 应用.dll 的启动方式。"
		}
		r.Notes = append(r.Notes, "候选项列出 dotnet 主机及其已安装运行时；具体兼容版本仍由应用的 runtimeconfig.json 决定。")
	}
	if s.Type == proto.TypeStatic {
		a.nginxRuntimeChoices(s, &r)
	} else if s.Type != proto.TypeBinary {
		p := runtimeProcess(s.Type, a.Sys.Processes(a.Sys.UnitProps(s.Unit).PID), s.FileName)
		r.Current = p.Exe
		if r.Current == "" {
			r.Current = first
			if current := a.current(s.Name); current != "" {
				if text, err := infoReadFile(a.verFile(s.Name, current, "unit.service"), 256*1024); err == nil {
					deployedArgs := unitExecArgs(string(text))
					r.Current = commandExecutable(deployedArgs)
					for _, arg := range deployedArgs {
						if runtimeMatches(s.Type, ProcessSnapshot{Exe: arg}) {
							r.Current = arg
							break
						}
					}
				}
			}
		}
		a.programRuntimeChoices(s, &r)
	}
	if len(r.Choices) == 0 && r.CanSwitch {
		r.Notes = append(r.Notes, "没有检测到可切换的运行环境；可先在软件中安装，或检查当前运行用户是否有访问权限后重新检测。")
	}
	return a.result(proto.CodeOK, "", r)
}

func runtimePatterns(kind, ferryDir string) []string {
	switch kind {
	case proto.TypeJava:
		return []string{"/usr/bin/java", "/usr/lib/jvm/*/bin/java", "/usr/lib/jvm/*/jre/bin/java", "/usr/java/*/bin/java", "/usr/local/*/bin/java", "/opt/*/bin/java", shellQuote(ferryDir) + "/runtimes/*/bin/java"}
	case proto.TypePython:
		return []string{"/usr/bin/python3*", "/usr/local/bin/python3*", "/usr/local/*/bin/python3*", "/opt/*/bin/python3*", "/home/*/.pyenv/versions/*/bin/python3*", "/root/.pyenv/versions/*/bin/python3*"}
	case proto.TypeNode:
		return []string{"/usr/bin/node", "/usr/bin/nodejs", "/usr/local/bin/node", "/opt/*/bin/node", "/usr/local/*/bin/node", "/home/*/.nvm/versions/node/*/bin/node", "/root/.nvm/versions/node/*/bin/node", "/home/*/.local/share/fnm/node-versions/*/installation/bin/node"}
	case proto.TypeDotnet:
		return []string{"/usr/bin/dotnet", "/usr/share/dotnet/dotnet", "/usr/local/share/dotnet/dotnet", "/opt/dotnet/dotnet", "/opt/*/dotnet", "/opt/*/dotnet/dotnet", "/home/*/.dotnet/dotnet", "/root/.dotnet/dotnet"}
	}
	return nil
}

func (a *Agent) programRuntimeChoices(s *proto.AppSpec, r *proto.AppRuntimeChoices) {
	fd := a.req.FerryDir
	if fd == "" {
		fd = "/usr/local/lib/ferry"
	}
	patterns := runtimePatterns(s.Type, fd)
	for _, candidate := range []string{s.Java, r.Current} {
		if path.IsAbs(candidate) && !strings.HasSuffix(candidate, " (deleted)") && !strings.HasPrefix(candidate, a.appDir(s.Name)+"/") {
			patterns = append(patterns, shellQuote(candidate))
		}
	}
	for _, in := range a.instances() {
		if in.Kind == "java" && s.Type == proto.TypeJava {
			patterns = append(patterns, shellQuote(path.Join(in.Dir, "bin/java")), shellQuote(path.Join(in.Dir, "jdk/bin/java")))
		}
	}
	cmd := "for p in " + strings.Join(patterns, " ") + `; do [ -x "$p" ] || continue; printf '%s\t%s\n' "$p" "$(readlink -f "$p" 2>/dev/null || printf '%s' "$p")"; done`
	out, err := a.Sys.Shell(cmd, 8*time.Second)
	if err != nil {
		r.Notes = append(r.Notes, "检测运行环境失败："+err.Error())
		return
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		alias, real, ok := strings.Cut(line, "\t")
		if !ok || !path.IsAbs(real) || strings.ContainsAny(real, "\r\n\x00") || seen[real] || !runtimeMatches(s.Type, ProcessSnapshot{Exe: real}) {
			continue
		}
		if len(r.Choices) >= 64 {
			r.Notes = append(r.Notes, "最多显示 64 个运行环境，请移除不再使用的版本后重新检测。")
			break
		}
		seen[real] = true
		choice := proto.AppRuntimeChoice{ID: s.Type + ":" + real, Path: real, Source: "服务器已安装", Current: real == r.Current || alias == r.Current}
		if strings.HasPrefix(real, fd+"/") {
			choice.Source = "Ferry 运行环境"
		}
		if allowed, err := a.Sys.AccessAs(s.User, real, 'x'); err != nil || !allowed {
			choice.Disabled = "运行用户 " + s.User + " 无法执行该程序"
		} else {
			flag := runtimeVersionFlag(s.Type, real)
			if s.Type == proto.TypeDotnet {
				flag = "--list-runtimes"
			}
			versionCmd := shellQuote(real) + " " + flag + " 2>&1"
			if s.User != "" && s.User != "root" {
				versionCmd = "runuser -u " + shellQuote(s.User) + " -- " + versionCmd
			}
			if version, err := a.Sys.Shell(versionCmd, 5*time.Second); err == nil {
				if len(version) > 4096 {
					version = version[:4096]
				}
				choice.Version = strings.TrimSpace(version)
				if choice.Version == "" {
					choice.Disabled = "该程序未返回可验证的版本信息"
				}
			} else {
				choice.Disabled = "无法以运行用户验证版本：" + err.Error()
			}
		}
		r.Choices = append(r.Choices, choice)
	}
	sort.SliceStable(r.Choices, func(i, j int) bool {
		if r.Choices[i].Current != r.Choices[j].Current {
			return r.Choices[i].Current
		}
		return r.Choices[i].Path < r.Choices[j].Path
	})
}

func (a *Agent) nginxRuntimeChoices(s *proto.AppSpec, r *proto.AppRuntimeChoices) {
	binding := a.readSiteBinding(s.Name)
	r.Current = binding.Instance
	if r.Current == "" {
		r.Current = s.NginxInstance
	}
	independent := s.NginxInstance == "" && s.Unit != ""
	if current := a.current(s.Name); current != "" {
		if text, err := infoReadFile(a.verFile(s.Name, current, "unit.service"), 256*1024); err == nil && len(text) > 0 {
			independent = true
		}
	}
	deployed := a.current(s.Name) != ""
	switch {
	case s.SiteDir != "":
		independent = false
		r.Current = "已有站点的网站目录 " + s.SiteDir
	case independent && !deployed:
		r.Current = "还没部署：按配置会由 Ferry 下载 Nginx，单独运行这个站点"
	case independent:
		r.Current = "项目独立 Nginx"
	}
	instances := a.instances()
	if system, err := a.loadHostingInstance(proto.SystemNginxInstance); err == nil {
		instances = append(instances, *system)
	} else if !strings.Contains(err.Error(), "未找到 nginx.service") {
		r.Notes = append(r.Notes, "系统 Nginx 暂不能接入："+err.Error())
	}
	for _, in := range instances {
		if in.Kind != "nginx" || in.Hosting == nil {
			continue
		}
		state := a.Sys.UnitProps(in.Unit)
		choice := proto.AppRuntimeChoice{ID: "static:" + in.Name, Path: in.Name, Version: in.Version, Source: "Ferry 已安装实例", Unit: in.Unit, State: state.Active, Current: in.Name == r.Current}
		if in.Hosting.System {
			choice.Source = "系统 Nginx"
		}
		choice.Version = strings.TrimSpace(choice.Version + "\n" + in.Hosting.Binary + "\n站点目录：" + in.Hosting.Sites)
		if !isRunning(state) {
			choice.Disabled = "该 Nginx 实例未运行，请先在软件中启动它"
		}
		r.Choices = append(r.Choices, choice)
	}
	if independent && deployed {
		r.Notes = append(r.Notes, "当前使用项目独立 Nginx；切换后会迁移到所选共享实例，仅重载本站点，不重启共享实例。")
	}
	// 服务器上已有的 nginx / OpenResty 里的站点（1Panel、宝塔、容器里的……）：选它就只换网站目录里的文件
	sites, notes := DiscoverNginxSites(a.Sys.Shell)
	r.Notes = append(r.Notes, notes...)
	for i := range sites {
		site := sites[i]
		dir := site.Dir
		if dir == "" {
			dir = site.Root
		}
		desc := []string{site.Server}
		if d := nginxSiteDesc(site); d != "" {
			desc = append(desc, d)
		}
		if site.Root != site.Dir && site.Dir != "" {
			desc = append(desc, "容器里是 "+site.Root)
		}
		if site.Conf != "" {
			desc = append(desc, "配置："+site.Conf)
		}
		choice := proto.AppRuntimeChoice{ID: "static:site:" + dir, Path: dir, Version: strings.Join(desc, "\n"), Source: "已有站点", Current: s.SiteDir != "" && s.SiteDir == site.Dir, Disabled: site.Disabled, Site: &site}
		if choice.Disabled == "" && site.Dir != "" {
			if err := a.checkSiteDirSafe(&proto.AppSpec{Name: s.Name, SiteDir: site.Dir}); err != nil {
				choice.Disabled = err.Error()
			}
		}
		r.Choices = append(r.Choices, choice)
	}
	sort.SliceStable(r.Choices, func(i, j int) bool {
		return fmt.Sprint(!r.Choices[i].Current, r.Choices[i].Path) < fmt.Sprint(!r.Choices[j].Current, r.Choices[j].Path)
	})
}
