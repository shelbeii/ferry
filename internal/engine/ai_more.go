package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
)

// 服务器助手的其余工具（和编程软件经 MCP 能用的一样，只对着当前这台服务器）：端口和防火墙、登录用户、
// 连接检查、部署前检查、软件日志、部署会带上线的改动、构建日志；允许改动时还有放行 / 关闭端口、打开防火墙、
// 断开 SSH 连接、现在备份、部署。

var (
	rePortArg     = regexp.MustCompile(`^([1-9][0-9]{0,4})(?:/(tcp|udp))?$`)
	reSoftwareArg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reBackupArg   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// parsePortArg 8080、8080/tcp、53/udp → 端口和协议（默认 tcp）。
func parsePortArg(s string) (int, string, error) {
	m := rePortArg.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, "", fmt.Errorf("端口不对：%q（写 8080/tcp 或 53/udp）", s)
	}
	n, _ := strconv.Atoi(m[1])
	if n > 65535 {
		return 0, "", fmt.Errorf("端口不对：%q", s)
	}
	pr := m[2]
	if pr == "" {
		pr = "tcp"
	}
	return n, pr, nil
}

func parseArgs(in json.RawMessage, v any) error {
	if len(in) == 0 {
		return nil
	}
	if err := json.Unmarshal(in, v); err != nil {
		return fmt.Errorf("参数格式不对：%v", err)
	}
	return nil
}

func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// aiMoreTools 只读的查看工具，change 时加上改动的工具。
func (e *Engine) aiMoreTools(t *config.Target, change bool) []ai.Tool {
	tools := []ai.Tool{{
		Name: "server_ports",
		Description: "这台服务器上的端口和防火墙：哪些端口在监听、是哪个进程（项目里的应用、Ferry 装的软件、Docker 容器……）、对外还是只在本机，" +
			"防火墙（firewalld / ufw）开没开、放行了哪些端口。",
		Schema: schema(map[string]any{}),
		Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			r, err := e.Ports(t.Name)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name:        "ssh_sessions",
		Description: "服务器上连着的 SSH 连接：登录用户、从哪个 IP 连的、什么时候连上、多久没输入、在干什么，还有多少个连接正在登录（多了多半是有人在试密码）。",
		Schema:      schema(map[string]any{}),
		Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			r, err := e.SSHSessions(t.Name)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name:        "server_check",
		Description: "测一下这台电脑连服务器：能不能连上、网络延迟。",
		Schema:      schema(map[string]any{}),
		Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			r, err := e.ServerCheck(t.Name)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name:        "port_check",
		Description: "从用户这台电脑测服务器的 TCP 端口实际能不能连上（服务器上在监听、外面却连不上时，多半是防火墙、安全组挡了）。",
		Schema: schema(map[string]any{
			"ports":   map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "要测的端口，如 [80, 443]"},
			"timeout": map[string]any{"type": "integer", "description": "每个端口最多等几秒，默认 3，最多 30"},
		}, "ports"),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Ports   []int
				Timeout int
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if len(a.Ports) == 0 || len(a.Ports) > 50 {
				return "", errors.New("ports 写 1～50 个端口")
			}
			r, err := e.PortCheck(t.Name, a.Ports, time.Duration(min(max(a.Timeout, 0), 30))*time.Second)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name:        "preflight",
		Description: "部署前检查服务器环境：目录和权限、磁盘空间、Java 等运行环境、端口有没有被别的程序占着。",
		Schema:      schema(map[string]any{"apps": strList("只检查这些应用；不填 = 这台服务器上的全部应用")}),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct{ Apps []string }
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			r, err := e.Preflight(t.Name, a.Apps)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name: "software_logs",
		Description: "服务器上软件（MySQL、Redis、Nacos 等）的日志，取最后 lines 行。Ferry 装的用 name（list_software 里的实例名），" +
			"file 为 true 时看软件自己的日志文件；不是 Ferry 装的，用 unit（systemd 服务）或 container（Docker 容器）。",
		Schema: schema(map[string]any{
			"name":      str("Ferry 装的软件的实例名"),
			"unit":      str("不是 Ferry 装的：systemd 服务名，如 redis.service"),
			"container": str("不是 Ferry 装的：Docker 容器名"),
			"file":      map[string]any{"type": "boolean", "description": "Ferry 装的软件：看软件自己的日志文件（默认看服务输出）"},
			"lines":     map[string]any{"type": "integer", "description": "最后多少行，默认 200，最多 2000"},
		}),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Name, Unit, Container string
				File                  bool
				Lines                 int
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			n := 0
			for _, v := range []string{a.Name, a.Unit, a.Container} {
				if v != "" {
					n++
				}
			}
			if n != 1 {
				return "", errors.New("name、unit、container 写一个")
			}
			if a.Name != "" && !reSoftwareArg.MatchString(a.Name) {
				return "", fmt.Errorf("软件名称不对：%q", a.Name)
			}
			if strings.ContainsAny(a.Unit+a.Container, " /;&|$`'\"\\") {
				return "", errors.New("unit、container 只能是名称")
			}
			lines := a.Lines
			if lines <= 0 {
				lines = 200
			}
			lines = min(lines, 2000)
			var out any
			var from string
			var err error
			if a.Name != "" {
				out, from, err = e.SoftwareLogs(t.Name, a.Name, lines, a.File)
			} else {
				out, from, err = e.SoftwareUnit(t.Name, a.Unit, a.Container, "logs", lines)
			}
			if err != nil {
				return "", err
			}
			return toJSON(map[string]any{"from": from, "log": out}), nil
		},
	}, {
		Name:        "deploy_changes",
		Description: "部署会带上线的改动：本机代码和服务器上运行的版本相比，多了哪些提交、单元文件和附带文件有什么变化。",
		Schema:      schema(map[string]any{"apps": strList("只看这些应用；不填 = 全部")}),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct{ Apps []string }
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			r, err := e.Changes(t.Name, a.Apps)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name:        "build_log",
		Description: "这台电脑上最近的构建日志（部署前在本机构建的完整输出）。不给 file 时列出最近的日志文件；给了时看它最后 lines 行。",
		Schema: schema(map[string]any{
			"file":  str("日志文件名（列表里的 name）"),
			"lines": map[string]any{"type": "integer", "description": "最后多少行，默认 200，最多 2000"},
		}),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				File  string
				Lines int
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			logs := e.BuildLogs(200)
			if a.File == "" {
				return toJSON(logs[:min(len(logs), 10)]), nil
			}
			for _, x := range logs {
				if x.Name == a.File {
					lines := a.Lines
					if lines <= 0 {
						lines = 200
					}
					return TailFile(x.Path, min(lines, 2000))
				}
			}
			return "", fmt.Errorf("没有这个构建日志：%s", a.File)
		},
	}}
	if !change {
		return tools
	}
	return append(tools, e.aiChangeTools(t)...)
}

// aiChangeTools 改动服务器的工具：每次先给用户确认。
func (e *Engine) aiChangeTools(t *config.Target) []ai.Tool {
	type portArgs struct{ Port, Action string }
	type fwArgs struct {
		Allow         []string
		RestartDocker bool
	}
	type kickArgs struct{ Pids []int }
	type backupArgs struct{ Name string }
	type deployArgs struct {
		Apps      []string
		SkipBuild bool
	}
	return []ai.Tool{{
		Name: "port_control",
		Description: "用服务器的防火墙（firewalld / ufw）放行或关闭一个端口（先用 server_ports 看现状）。SSH 端口不能关；" +
			"Docker 映射的端口不经过防火墙，关闭时在 DOCKER-USER 规则链里拦外部访问。",
		Schema: schema(map[string]any{
			"port":   str("端口，如 8080/tcp、53/udp（不写协议 = tcp）"),
			"action": map[string]any{"type": "string", "enum": []string{"open", "close"}},
		}, "port", "action"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			var a portArgs
			if parseArgs(in, &a) != nil || (a.Action != "open" && a.Action != "close") {
				return nil
			}
			n, pr, err := parsePortArg(a.Port)
			if err != nil {
				return nil
			}
			if a.Action == "open" {
				return &ai.Approval{Kind: "放行端口", Title: fmt.Sprintf("放行端口 %d/%s", n, pr), AsRoot: true,
					Once: "改防火墙每次都要确认", Detail: "放行后外部都能连这个端口。"}
			}
			return &ai.Approval{Kind: "关闭端口", Title: fmt.Sprintf("关闭端口 %d/%s", n, pr), AsRoot: true,
				Danger: "关闭后外部连不上这个端口（服务器本机不受影响）"}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a portArgs
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if a.Action != "open" && a.Action != "close" {
				return "", errors.New("action 只能是 open、close")
			}
			n, pr, err := parsePortArg(a.Port)
			if err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) { return e.PortSet(t.Name, n, pr, a.Action) })
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name: "firewall_enable",
		Description: "打开服务器上装了没开的防火墙（firewalld / ufw；先用 server_ports 看现状）。先放行 SSH 端口和 allow 里的端口再打开，" +
			"没写的端口打开后外部都连不上；打开后 Ferry 用新的连接试 SSH，连不上时 3 分钟后服务器自动关掉防火墙、恢复原样。",
		Schema: schema(map[string]any{
			"allow":         strList("打开前先放行的端口，如 [\"80/tcp\", \"443/tcp\"]（SSH 端口自动放行）"),
			"restartDocker": map[string]any{"type": "boolean", "description": "打开后重启 Docker（Docker 在运行时要重启，容器的端口映射才正常；所有容器会重启一次）"},
		}),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			var a fwArgs
			if parseArgs(in, &a) != nil {
				return nil
			}
			allow, err := portSpecs(a.Allow)
			if err != nil {
				return nil
			}
			d := "先放行：SSH 端口"
			if len(allow) > 0 {
				d += "、" + strings.Join(allow, "、")
			}
			d += "\n没写的端口打开后外部都连不上。"
			if a.RestartDocker {
				d += "\n打开后重启 Docker（所有容器重启一次）。"
			}
			return &ai.Approval{Kind: "打开防火墙", Title: "打开防火墙", Detail: d, AsRoot: true, Danger: "没放行的端口外部都会连不上"}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a fwArgs
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			allow, err := portSpecs(a.Allow)
			if err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) { return e.FirewallEnable(t.Name, allow, a.RestartDocker) })
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name: "ssh_disconnect",
		Description: "断开服务器上的 SSH 连接（先用 ssh_sessions 看 pid），里面正在运行的命令会被结束。对方可以重新连上。" +
			"Ferry 自己正在用的连接不能断开。",
		Schema: schema(map[string]any{
			"pids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "要断开的连接（ssh_sessions 里的 pid）"},
		}, "pids"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			var a kickArgs
			if parseArgs(in, &a) != nil || len(a.Pids) == 0 || len(a.Pids) > 20 {
				return nil
			}
			ps := make([]string, len(a.Pids))
			for i, p := range a.Pids {
				ps[i] = strconv.Itoa(p)
			}
			return &ai.Approval{Kind: "断开 SSH 连接", Title: "断开 SSH 连接（PID " + strings.Join(ps, "、") + "）", AsRoot: true,
				Danger: "连接里正在运行的命令会被结束"}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a kickArgs
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if len(a.Pids) == 0 || len(a.Pids) > 20 {
				return "", errors.New("pids 写 1～20 个连接")
			}
			for _, p := range a.Pids {
				if p <= 1 {
					return "", fmt.Errorf("PID 不对：%d", p)
				}
			}
			r, err := withProgress(e, ctx, func() (any, error) { return e.SSHKick(t.Name, a.Pids) })
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name: "backup_run",
		Description: "现在备份一次配置里的 backups（一项或全部；没打开数据备份也能用），备份文件留在服务器上，按 keep 只保留最近几份。" +
			"改数据库、改数据目录之类的操作前先备份。",
		Schema: schema(map[string]any{"name": str("备份哪一项（backup_status 里 items 的 name）；不填 = 全部")}),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			var a backupArgs
			if parseArgs(in, &a) != nil || e.checkBackup(t.Name, a.Name) != nil {
				return nil
			}
			return &ai.Approval{Kind: "现在备份", Title: "现在备份 " + orDefault(a.Name, "全部"), AsRoot: true}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a backupArgs
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if err := e.checkBackup(t.Name, a.Name); err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) { return e.Backup(t.Name, "run", BackupOptions{Name: a.Name}) })
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name: "deploy",
		Description: "把项目里的应用部署到这台服务器：先在用户电脑上构建（skipBuild 时跳过），上传，逐个切换到新版本并等健康检查通过；失败自动回滚该应用。" +
			"先用 deploy_changes 看会带上线什么。构建可能要几分钟，输出实时显示给用户；失败时用 build_log 看完整的构建输出。生产服务器不能用，要用户自己在 Ferry 里点「部署」。",
		Schema: schema(map[string]any{
			"apps":      strList("要部署的应用（必须写明）"),
			"skipBuild": map[string]any{"type": "boolean", "description": "跳过本机构建，直接用现有的产物（确认产物已经是最新的才用）"},
		}, "apps"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			var a deployArgs
			if parseArgs(in, &a) != nil || e.checkDeploy(t, a.Apps) != nil {
				return nil
			}
			var d []string
			if git, steps, err := e.DeployPreview(t.Name, a.Apps); err == nil {
				if git.Commit != "" {
					line := fmt.Sprintf("代码：%s 分支，提交 %s", orDefault(git.Branch, "-"), git.Short())
					if git.Dirty {
						line += fmt.Sprintf("，有 %d 个文件改了没提交", len(git.Files))
					}
					d = append(d, line)
				}
				if a.SkipBuild {
					d = append(d, "构建：跳过，用本机现有的产物")
				} else {
					for _, st := range steps {
						d = append(d, fmt.Sprintf("构建 %s：%s", st.Name, st.Command))
					}
				}
			}
			return &ai.Approval{Kind: "部署", Title: "部署 " + strings.Join(a.Apps, "、"), Detail: strings.Join(d, "\n"), AsRoot: true}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a deployArgs
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if err := e.checkDeploy(t, a.Apps); err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) {
				return e.Deploy([]string{t.Name}, a.Apps, DeployOptions{SkipBuild: a.SkipBuild})
			})
			if err != nil {
				return toJSON(r), fmt.Errorf("%v（构建失败时用 build_log 看完整输出）", err)
			}
			return toJSON(r), nil
		},
	}}
}

// TailFile 文件的最后 n 行（前面省略的说一声）。
func TailFile(path string, n int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("…（前面还有 %d 行）", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n"), nil
}

// portSpecs 打开防火墙前要放行的端口，统一写成 8080/tcp。
func portSpecs(in []string) ([]string, error) {
	var out []string
	for _, p := range in {
		n, pr, err := parsePortArg(p)
		if err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%d/%s", n, pr))
	}
	return out, nil
}

// checkBackup 配置里这台服务器上有没有要备份的（name 为空 = 有任何一项）。
func (e *Engine) checkBackup(target, name string) error {
	if name != "" && !reBackupArg.MatchString(name) {
		return fmt.Errorf("备份的名字不对：%q", name)
	}
	var names []string
	for _, b := range e.File.Backups {
		if b.AppliesTo(target) {
			if b.Name == name {
				return nil
			}
			names = append(names, b.Name)
		}
	}
	switch {
	case len(names) == 0:
		return fmt.Errorf("项目配置里没有在 %s 上的 backups：要用户先在 ferry.yaml 里写上要备份什么", target)
	case name == "":
		return nil
	}
	return fmt.Errorf("%s 上没有备份 %s（有：%s）", target, name, strings.Join(names, "、"))
}

// checkDeploy 能不能由助手部署：生产服务器要用户自己点，应用要写明、在这台服务器上。
func (e *Engine) checkDeploy(t *config.Target, apps []string) error {
	if t.Production {
		return fmt.Errorf("%s 是生产服务器：部署要用户自己在 Ferry 里点「部署」", t.Name)
	}
	if len(apps) == 0 {
		return errors.New("要用 apps 写明部署哪些应用")
	}
	for _, a := range apps {
		if e.File.App(a) == nil || !t.Allows(a) {
			return fmt.Errorf("%s 上没有应用 %s", t.Name, a)
		}
	}
	return nil
}
