package scaffold

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec 向导填写的内容。路径（Workdir、Artifact）相对项目目录 ProjectDir。
type Spec struct {
	ProjectDir string       `json:"projectDir"`
	Project    string       `json:"project"`
	Targets    []TargetSpec `json:"targets"`
	Apps       []AppSpec    `json:"apps"`
}

// TargetSpec 一台服务器。
type TargetSpec struct {
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Sudo          bool   `json:"sudo"`
	Auth          string `json:"auth"` // key / password
	IdentityFile  string `json:"identityFile"`
	Root          string `json:"root"`     // 部署目录，空 = /opt/<project>
	FerryDir      string `json:"ferryDir"` // ferry 组件目录（agent、JDK），空 = 默认
	RunAs         string `json:"runAs"`    // 服务的运行用户
	Arch          string `json:"arch"`
	HostKey       string `json:"hostKey"` // 固定的主机公钥指纹（从已有连接填入时沿用）
	NginxInstance string `json:"nginxInstance"`
}

// AppSpec 一个应用。
type AppSpec struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"` // java / binary / static / python / node / dotnet
	Workdir  string            `json:"workdir"`
	Command  string            `json:"command"` // 空 = 不构建
	Env      map[string]string `json:"env"`
	Artifact string            `json:"artifact"`
	// Files 附带文件（配置文件等）：和程序放在同一级目录，随版本走（java / binary）
	Files []FileSpec `json:"files"`
	Root  string     `json:"root"` // 应用自己的部署目录（可选）
	// 同一个 Maven 工程的模块共用一条构建
	SharedBuild  bool   `json:"sharedBuild"`
	BuildGroup   string `json:"buildGroup"`
	GroupCommand string `json:"groupCommand"`
	Module       string `json:"module"`
	// java / binary
	Unit        string   `json:"unit"`
	Java        string   `json:"java"`
	RuntimeJava int      `json:"runtimeJava"` // 由 Ferry 提供的 JDK 大版本（设置后不用 Java）
	JvmArgs     []string `json:"jvmArgs"`
	Args        []string `json:"args"`
	Port        int      `json:"port"`
	Health      string   `json:"health"` // tcp / http / none
	HealthURL   string   `json:"healthUrl"`
	FileLogs    bool     `json:"fileLogs"` // Spring Boot 日志写到 ${logs}（运行中版本目录里的 logs/，和程序放在一起）
	// static：Port > 0 时由 Ferry 提供的 nginx 运行（Proxy、SPA 只在这时有用），否则用 Link、Reload
	Link          string            `json:"link"`
	Reload        string            `json:"reload"`
	Proxy         map[string]string `json:"proxy"` // 路径前缀 → 后端地址
	NoSPA         bool              `json:"noSpa"` // 不是前端路由（找不到的路径返回 404）
	NginxInstance string            `json:"nginxInstance"`
	Domains       []string          `json:"domains"`
	// python、node、dotnet：启动命令（每项一个参数）、服务的环境变量、不上传的、装依赖的镜像源
	Exec       []string          `json:"exec"`
	ServiceEnv map[string]string `json:"serviceEnv"`
	Exclude    []string          `json:"exclude"`
	Registry   string            `json:"registry"`
}

// FileSpec 一个附带文件或目录。
type FileSpec struct {
	From string `json:"from"`           // 本机路径（相对项目目录或完整路径）
	To   string `json:"to"`             // 放到服务器上叫什么（相对程序所在的目录），空 = 同名
	Exec bool   `json:"exec,omitempty"` // 可执行（脚本）
}

// Generate 生成 ferry.yaml 的内容。configPath 是要写入的位置（相对路径据此换算）。
func Generate(s Spec, configPath string) (string, error) {
	if s.Project == "" {
		return "", errors.New("没有项目名")
	}
	if len(s.Targets) == 0 {
		return "", errors.New("至少需要一台服务器")
	}
	if len(s.Apps) == 0 {
		return "", errors.New("至少需要一个应用")
	}
	rel := relPather(s.ProjectDir, filepath.Dir(configPath))
	var b strings.Builder
	w := func(indent int, format string, args ...any) {
		b.WriteString(strings.Repeat("  ", indent))
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}
	w(0, "# %s 的 ferry 部署配置，由 Ferry「新建项目」生成。字段说明见 ferry 的 docs/config.md。", s.Project)
	w(0, "# 相对路径都按本文件所在的目录计算。")
	w(0, "version: 1")
	w(0, "project: %s", q(s.Project))

	// 共用的 Maven 构建
	groups := map[string]string{} // 工程目录 → 构建名
	var groupOrder []string
	for _, a := range s.Apps {
		if a.SharedBuild && a.BuildGroup != "" && a.Module != "" && a.GroupCommand != "" {
			if _, ok := groups[a.BuildGroup]; !ok {
				groups[a.BuildGroup] = ""
				groupOrder = append(groupOrder, a.BuildGroup)
			}
		}
	}
	if len(groupOrder) > 0 {
		w(0, "")
		w(0, "builds:")
		for i, g := range groupOrder {
			name := "maven"
			if i > 0 {
				name = "maven-" + strconv.Itoa(i+1)
			}
			groups[g] = name
			var cmd string
			for _, a := range s.Apps {
				if a.SharedBuild && a.BuildGroup == g {
					cmd = a.GroupCommand
					break
				}
			}
			w(1, "# 同一个 Maven 工程的几个应用一起部署时只打包一次（${each:module} 换成各应用的 module）")
			w(1, "%s:", name)
			w(2, "workdir: %s", q(rel(g)))
			w(2, "command: %s", q(cmd))
		}
	}

	w(0, "")
	w(0, "targets:")
	for _, t := range s.Targets {
		w(1, "%s:", t.Name)
		w(2, "host: %s", q(t.Host))
		if t.Port != 0 && t.Port != 22 {
			w(2, "port: %d", t.Port)
		}
		user := t.User
		if user == "" {
			user = "root"
		}
		w(2, "user: %s", q(user))
		if t.Sudo && user != "root" {
			w(2, "sudo: true          # 普通用户登录，用免密 sudo 执行部署")
		}
		if t.Auth == "password" {
			w(2, "auth: password      # 密码不写在这里：第一次连接时输入，可以保存到系统凭据库")
		} else if t.IdentityFile != "" {
			w(2, "identityFile: %s", q(t.IdentityFile))
		}
		root := t.Root
		if root == "" {
			root = "/opt/" + s.Project
		}
		w(2, "root: %s   # 部署目录：各应用的版本、运行中的版本、日志都在这里", q(root))
		if t.NginxInstance != "" {
			w(2, "vars: {nginxInstance: %s}", q(t.NginxInstance))
		}
		if t.RunAs != "" {
			w(2, "runAs: %s   # 服务的运行用户（不能是 root；服务器上没有时部署会自动创建）", q(t.RunAs))
		}
		if t.Arch != "" {
			w(2, "arch: %s", q(t.Arch))
		}
		if t.HostKey != "" {
			w(2, "hostKey: %s", q(t.HostKey))
		}
		if fd := strings.TrimRight(t.FerryDir, "/"); fd != "" && fd != "/usr/local/lib/ferry" {
			w(2, "ferryDir: %s   # ferry 组件（agent、Ferry 提供的 JDK）在服务器上的目录", q(fd))
		}
	}

	w(0, "")
	w(0, "apps:")
	for i, a := range s.Apps {
		w(1, "- name: %s", q(a.Name))
		w(2, "type: %s", a.Type)
		w(2, "order: %d", (i+1)*10)
		switch {
		case a.SharedBuild && groups[a.BuildGroup] != "":
			w(2, "build: %s", groups[a.BuildGroup])
			w(2, "vars: {module: %s}", q(a.Module))
		case strings.TrimSpace(a.Command) != "":
			w(2, "build:")
			w(3, "command: %s", q(a.Command))
			if a.Workdir != "" && a.Workdir != "." || rel(".") != "." {
				w(3, "workdir: %s", q(rel(a.Workdir)))
			}
			if len(a.Env) > 0 {
				w(3, "env:")
				for _, k := range sortedKeys(a.Env) {
					w(4, "%s: %s", k, q(a.Env[k]))
				}
			}
		}
		w(2, "artifact:")
		w(3, "path: %s", q(rel(a.Artifact)))
		if a.Type == "java" {
			w(3, "verify:")
			w(4, "springBoot: {}   # 检查是可执行的 Spring Boot jar")
		}
		dirApp := a.Type == "python" || a.Type == "node" || a.Type == "dotnet"
		if dirApp && len(a.Exclude) > 0 {
			list(&b, 3, "exclude", a.Exclude)
		}
		var files []FileSpec
		for _, x := range a.Files {
			if strings.TrimSpace(x.From) != "" {
				files = append(files, x)
			}
		}
		if a.Type != "static" && len(files) > 0 {
			w(3, "files:   # 附带文件：和程序放在同一级目录，每次部署都上传，回滚时一起回滚")
			for _, x := range files {
				from := rel(strings.TrimSpace(x.From))
				to := strings.TrimRight(strings.TrimSpace(x.To), "/")
				if (to == "" || to == path.Base(filepath.ToSlash(from))) && !x.Exec {
					w(4, "- %s", q(from))
					continue
				}
				w(4, "- from: %s", q(from))
				if to != "" {
					w(5, "to: %s", q(to))
				}
				if x.Exec {
					w(5, "exec: true")
				}
			}
		}
		if a.Root != "" {
			w(2, "root: %s   # 这个应用单独的部署目录（如另一个分区）", q(a.Root))
		}
		if a.Type == "static" && a.Port > 0 {
			w(2, "static:")
			w(3, "port: %d", a.Port)
			if a.NginxInstance != "" {
				w(3, "nginxInstance: %s   # 在软件中预先安装的 nginx 实例", q(a.NginxInstance))
			}
			if len(a.Domains) > 0 {
				w(3, "domains: [%s]", strings.Join(quoteAll(a.Domains), ", "))
			}
			if a.NoSPA {
				w(3, "spa: false   # 找不到的路径返回 404（不是前端路由）")
			}
			if len(a.Proxy) > 0 {
				w(3, "proxy:   # 接口转发：页面里请求这些路径时转给后端")
				paths := make([]string, 0, len(a.Proxy))
				for p := range a.Proxy {
					paths = append(paths, p)
				}
				sort.Strings(paths)
				for _, p := range paths {
					w(4, "%s: %s", q(p), q(a.Proxy[p]))
				}
			}
			if a.Unit != "" && a.NginxInstance == "" {
				w(2, "service:")
				w(3, "unit: %s", q(a.Unit))
			}
			continue
		}
		if a.Type == "static" {
			if a.Link != "" || a.Reload != "" {
				w(2, "static:")
				if a.Link != "" {
					w(3, "link: %s   # 指向运行中版本（running/）的链接，nginx 的 root 指向这里", q(a.Link))
				}
				if a.Reload != "" {
					w(3, "reload: %s", q(a.Reload))
				}
			}
			continue
		}
		if (a.Type == "python" || a.Type == "node") && strings.TrimSpace(a.Registry) != "" {
			w(2, "deps: {registry: %s}   # 服务器上装依赖用的镜像源", q(strings.TrimSpace(a.Registry)))
		}
		w(2, "service:")
		if a.Unit != "" {
			w(3, "unit: %s", q(a.Unit))
		}
		if dirApp {
			list(&b, 3, "exec", a.Exec)
			if len(a.ServiceEnv) > 0 {
				w(3, "env:")
				for _, k := range sortedKeys(a.ServiceEnv) {
					w(4, "%s: %s", k, q(a.ServiceEnv[k]))
				}
			}
		}
		jvm := append([]string{}, a.JvmArgs...)
		if a.Type == "java" {
			if a.RuntimeJava > 0 {
				w(3, "runtime: {java: %d}   # 由 Ferry 提供 JDK %d：部署时自动下载（本机已有就不下载）并上传到服务器", a.RuntimeJava, a.RuntimeJava)
			} else {
				java := a.Java
				if java == "" {
					java = "/usr/bin/java"
				}
				w(3, "java: %s", q(java))
			}
			if a.FileLogs {
				jvm = append(jvm, "-Dlogging.file.path=${logs}")
			}
			list(&b, 3, "jvmArgs", jvm)
		}
		list(&b, 3, "args", a.Args)
		if a.FileLogs && a.Type == "java" {
			w(3, "diagLogs: [\"${logs}/spring.log\"]   # 启动失败时显示它的末尾")
		}
		switch a.Health {
		case "http":
			w(3, "health: {http: %s, timeout: 180s}", q(a.HealthURL))
		case "tcp":
			if a.Port > 0 {
				w(3, "health: {tcp: %s, timeout: 180s}", q(strconv.Itoa(a.Port)))
			}
		}
	}
	return b.String(), nil
}

func list(b *strings.Builder, indent int, key string, items []string) {
	var xs []string
	for _, s := range items {
		if s = strings.TrimSpace(s); s != "" {
			xs = append(xs, s)
		}
	}
	if len(xs) == 0 {
		return
	}
	pad := strings.Repeat("  ", indent)
	b.WriteString(pad + key + ":\n")
	for _, s := range xs {
		b.WriteString(pad + "  - " + q(s) + "\n")
	}
}

// q YAML 标量：需要时加引号。
func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = q(v)
	}
	return out
}

func q(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return strconv.Quote(s)
	}
	v := strings.TrimSuffix(string(out), "\n")
	if strings.Contains(v, "\n") { // 多行（不应出现）：用双引号
		return strconv.Quote(s)
	}
	return v
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// relPather 把相对项目目录的路径换算成相对配置文件目录的；不在同一个盘时用绝对路径。
func relPather(projectDir, configDir string) func(string) string {
	return func(p string) string {
		if p == "" {
			p = "."
		}
		if filepath.IsAbs(filepath.FromSlash(p)) || strings.HasPrefix(p, "${") {
			return p
		}
		full := filepath.Join(filepath.FromSlash(projectDir), filepath.FromSlash(p))
		r, err := filepath.Rel(configDir, full)
		if err != nil {
			return filepath.ToSlash(full)
		}
		r = filepath.ToSlash(r)
		if r == "" {
			return "."
		}
		return path.Clean(r)
	}
}
