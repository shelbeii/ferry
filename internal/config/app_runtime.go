package config

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

// AppRuntime 只覆盖一台服务器上的一个应用，不改变其他目标的运行环境。
type AppRuntime struct {
	Java          string `yaml:"java"`
	NginxInstance string `yaml:"nginxInstance"`
	Python        string `yaml:"python"`
	Node          string `yaml:"node"`
	Dotnet        string `yaml:"dotnet"`
	// SiteDir 前端站点改用服务器上已有站点的网站目录（只换文件，不改 nginx 配置）
	SiteDir string `yaml:"siteDir"`
	Port    string `yaml:"port"` // Nginx 切换时可同时更换此站点端口；用 siteDir 时是已有站点的端口（部署后检查首页能打开）
}

func (f *File) RuntimeApp(t *Target, app *App) (*App, error) {
	r, ok := t.AppRuntimes[app.Name]
	if !ok {
		return app, nil
	}
	a := *app
	a.Service.Exec = append([]string(nil), app.Service.Exec...)
	kind, program, count := "", "", 0
	for k, value := range map[string]string{"java": r.Java, "static": r.NginxInstance, "site": r.SiteDir, "python": r.Python, "node": r.Node, "dotnet": r.Dotnet} {
		if value != "" {
			kind, program, count = k, value, count+1
		}
	}
	if kind == "site" && a.Type == "static" && count == 1 {
		return f.siteDirApp(&a, r)
	}
	if count != 1 || kind != a.Type {
		return nil, fmt.Errorf("运行环境必须且只能选择与 %s 类型一致的一项", a.Type)
	}
	if kind != "static" && (!path.IsAbs(program) || strings.ContainsAny(program, "\x00\r\n")) {
		return nil, fmt.Errorf("运行环境程序必须是服务器上的绝对路径")
	}
	if r.Port != "" && kind != "static" {
		return nil, fmt.Errorf("port 只用于前端站点")
	}
	switch kind {
	case "java":
		a.Service.Runtime, a.Service.Java = nil, program
		if len(a.Service.Exec) > 0 {
			found := false
			for i, arg := range a.Service.Exec {
				if strings.Contains(arg, "${java}") {
					found = true
				}
				if path.Base(arg) == "java" {
					a.Service.Exec[i] = program
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("自定义 Java 启动命令须使用 ${java} 或独立的 java 程序参数才能切换")
			}
		}
	case "python":
		if len(a.Service.Exec) == 0 {
			return nil, fmt.Errorf("没有 Python 启动命令")
		}
		first := a.Service.Exec[0]
		if strings.Contains(first, "/.venv/bin/") {
			a.Deps.Python, a.Deps.Off = program, false
		} else if strings.HasPrefix(path.Base(first), "python") {
			a.Service.Exec[0], a.Deps.Python = program, program
			if !a.Deps.Off {
				a.Service.Exec[0] = "${running}/.venv/bin/python"
			}
		} else {
			return nil, fmt.Errorf("Python 启动命令须直接使用 python 或项目 .venv 中的入口")
		}
	case "node", "dotnet":
		if len(a.Service.Exec) == 0 || (path.Base(a.Service.Exec[0]) != kind && !(kind == "node" && path.Base(a.Service.Exec[0]) == "nodejs")) {
			return nil, fmt.Errorf("启动命令须直接使用 %s；包装脚本或自带运行时程序请先在配置中调整入口", kind)
		}
		a.Service.Exec[0] = program
		if kind == "node" {
			a.Service.Env = map[string]string{}
			for k, v := range app.Service.Env {
				a.Service.Env[k] = v
			}
			old := a.Service.Env["PATH"]
			if old == "" {
				old = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
			}
			a.Service.Env["PATH"] = path.Dir(program) + ":" + old
		}
	case "static":
		oldPort := a.Static.Port
		a.Static.NginxInstance, a.Static.Nginx = program, ""
		a.Static.Link, a.Static.Reload, a.Service.Unit = "", "", ""
		if r.Port != "" {
			a.Static.Port = r.Port
		}
		if a.Static.Port == "" {
			return nil, fmt.Errorf("接入 Nginx 需要填写网站端口")
		}
		if a.Static.Port != oldPort {
			_, newPort := splitRuntimePort(a.Static.Port)
			_, previousPort := splitRuntimePort(oldPort)
			if u, err := url.Parse(a.Static.Health.HTTP); err == nil && a.Static.Health.HTTP != "" && previousPort != "" && u.Port() == previousPort {
				u.Host = net.JoinHostPort(u.Hostname(), newPort)
				a.Static.Health.HTTP = u.String()
			}
			if host, p, err := net.SplitHostPort(a.Static.Health.TCP); err == nil && p == previousPort {
				a.Static.Health.TCP = net.JoinHostPort(host, newPort)
			}
		}
		var issues []string
		checkServedStatic(f, &a, "应用 "+a.Name, map[string]string{}, func(format string, args ...any) { issues = append(issues, fmt.Sprintf(format, args...)) })
		if len(issues) > 0 {
			return nil, fmt.Errorf("%s", strings.Join(issues, "；"))
		}
	}
	return &a, nil
}

func splitRuntimePort(value string) (string, string) {
	if i := strings.LastIndexByte(value, ':'); i >= 0 {
		return value[:i], value[i+1:]
	}
	return "", value
}

// siteDirApp 前端站点改成只换服务器上已有站点网站目录里的文件：Ferry 不运行 nginx，nginx 相关的设置都不要了。
func (f *File) siteDirApp(a *App, r AppRuntime) (*App, error) {
	if !strings.Contains(r.SiteDir, "${") {
		if err := proto.CheckSiteDir(r.SiteDir); err != nil {
			return nil, err
		}
	}
	st := &a.Static
	oldPort := st.Port
	*st = Static{SiteDir: r.SiteDir, Health: st.Health}
	a.Service.Unit, a.Service.User = "", ""
	port := r.Port
	if port == "" {
		port = oldPort
	}
	_, p := splitRuntimePort(port)
	_, prev := splitRuntimePort(oldPort)
	h := &st.Health
	switch {
	case r.Port != "" && p != "":
		// 检查已有站点的首页能打开（状态码 2xx、3xx 都算）
		*h = Health{HTTP: "http://127.0.0.1:" + p + "/", Timeout: h.Timeout}
	case prev != "" && (strings.Contains(h.HTTP, ":"+prev+"/") || strings.HasSuffix(h.HTTP, ":"+prev) || strings.HasSuffix(h.TCP, ":"+prev)):
		// 原来按 Ferry 运行 nginx 的端口生成的默认检查：端口还是那个，状态码放宽
		h.Status = nil
	}
	normalizeStaticHealth(h)
	return a, nil
}
