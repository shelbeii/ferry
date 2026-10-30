package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 连接与延迟、从本机测端口、预检、软件日志（只读）；断开 SSH 连接、打开防火墙（要确认）。

type portCheckIn struct {
	targetIn
	Ports   []int `json:"ports" jsonschema:"要测的 TCP 端口，如 [80, 443, 8080]"`
	Timeout int   `json:"timeout,omitempty" jsonschema:"每个端口最多等几秒，默认 3，最多 30"`
}

type preflightIn struct {
	targetIn
	Apps []string `json:"apps,omitempty" jsonschema:"只检查这些应用；不填 = 这台服务器上的全部应用"`
}

type softwareLogsIn struct {
	targetIn
	Name      string `json:"name,omitempty" jsonschema:"Ferry 装的软件的实例名（server_software 结果里的 instance）"`
	Unit      string `json:"unit,omitempty" jsonschema:"不是 Ferry 装的软件：systemd 服务名，如 redis.service"`
	Container string `json:"container,omitempty" jsonschema:"不是 Ferry 装的软件：Docker 容器名"`
	File      bool   `json:"file,omitempty" jsonschema:"Ferry 装的软件：看软件自己的日志文件（如 MySQL 的错误日志）；默认看服务输出（journald）"`
	Lines     int    `json:"lines,omitempty" jsonschema:"最后多少行，默认 200，最多 2000"`
}

type sshKickIn struct {
	targetIn
	Pids   []int  `json:"pids" jsonschema:"要断开的连接（ssh_sessions 结果里的 pid）"`
	Reason string `json:"reason" jsonschema:"为什么要断开（显示在 Ferry 的确认框里）"`
}

type fwEnableIn struct {
	targetIn
	Allow         []string `json:"allow,omitempty" jsonschema:"打开前先放行的端口，如 [\"80/tcp\", \"443/tcp\"]（SSH 端口自动放行）；没写的端口打开后外部都连不上"`
	RestartDocker bool     `json:"restartDocker,omitempty" jsonschema:"打开后重启 Docker（firewalld 会动 Docker 的网络规则，Docker 在运行时要重启它，容器的端口映射才正常；所有容器会重启一次）"`
	Reason        string   `json:"reason" jsonschema:"为什么要打开（显示在 Ferry 的确认框里）"`
}

func (s *server) addExtraTools(m *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}

	addTool(s, m, &mcp.Tool{Name: "server_check", Title: "测试连接和网络延迟",
		Description: "连一下服务器（Ferry 已经连着时复用），看能不能连上、连上用了多久、到服务器的网络往返时间（rtt，毫秒；slow 表示 3 秒内没有回应）。" +
			"连不上时错误里说明原因（地址不通、端口没开、密码或私钥不对、主机公钥没核对……）。终端打字卡、操作慢时先看这里。「新建连接」的服务器也能用。",
		Annotations: readOnly},
		logged(s, "server_check", func(ctx context.Context, in targetIn) (any, error) {
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.ServerCheck(t)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "port_check", Title: "从本机测试端口",
		Description: "从运行 Ferry 的这台电脑连服务器的 TCP 端口，看实际能不能连上（不连 SSH、不改动服务器）。" +
			"refused：服务器回绝（没有程序在监听，或防火墙拒绝）；timeout：没有回应（多半被防火墙或云服务商的安全组挡住）。" +
			"server_ports 显示防火墙放行了、这里还连不上，多半是云服务商的安全组没放行。「新建连接」的服务器也能用。",
		Annotations: readOnly},
		logged(s, "port_check", func(ctx context.Context, in portCheckIn) (any, error) {
			if len(in.Ports) == 0 || len(in.Ports) > 50 {
				return nil, errors.New("ports 写 1～50 个端口")
			}
			for _, p := range in.Ports {
				if p < 1 || p > 65535 {
					return nil, fmt.Errorf("端口不对：%d", p)
				}
			}
			timeout := 3
			if in.Timeout > 0 {
				timeout = min(in.Timeout, 30)
			}
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.PortCheck(t, in.Ports, time.Duration(timeout)*time.Second)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "preflight", Title: "部署前检查",
		Description: "检查服务器能不能部署这些应用：磁盘空间、运行用户、Java 版本、端口占用、目录权限、systemd 等，列出问题和部署时会自动修正的权限。只检查，不改动服务器。部署失败、新服务器第一次部署前用它。",
		Annotations: readOnly},
		logged(s, "preflight", func(ctx context.Context, in preflightIn) (any, error) {
			return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.Preflight(t, in.Apps)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "software_logs", Title: "软件的日志",
		Description: "服务器上软件（MySQL、Redis、Nacos 等）的日志，取最后 lines 行。Ferry 装的用 name（server_software 里的 instance），" +
			"可以用 file 看软件自己的日志文件；不是 Ferry 装的，用 unit（systemd 服务）或 container（Docker 容器）。",
		Annotations: readOnly},
		logged(s, "software_logs", func(ctx context.Context, in softwareLogsIn) (any, error) {
			n := 0
			for _, v := range []string{in.Name, in.Unit, in.Container} {
				if v != "" {
					n++
				}
			}
			if n != 1 {
				return nil, errors.New("name、unit、container 写一个")
			}
			if in.Name != "" && !reSoftware.MatchString(in.Name) {
				return nil, fmt.Errorf("软件名称不对：%q（用 server_software 结果里的 instance）", in.Name)
			}
			if strings.ContainsAny(in.Unit+in.Container, " /;&|$`'\"\\") {
				return nil, errors.New("unit、container 只能是名称")
			}
			lines := in.Lines
			if lines <= 0 {
				lines = 200
			}
			lines = min(lines, 2000)
			return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				var out any
				var from string
				var err error
				if in.Name != "" {
					out, from, err = e.SoftwareLogs(t, in.Name, lines, in.File)
				} else {
					out, from, err = e.SoftwareUnit(t, in.Unit, in.Container, "logs", lines)
				}
				if err != nil {
					return nil, err
				}
				return map[string]any{"from": from, "log": out}, nil
			})
		}))

	const how = "和 deploy 一样：需要用户在 Ferry 设置里允许编程软件请求改动，每次在 Ferry 桌面版里确认（不能授权自动同意），同意后由桌面版执行；生产服务器、「新建连接」的服务器不允许。最多等 40 秒，没结束用 task_status 查。"
	addTool(s, m, &mcp.Tool{Name: "ssh_disconnect", Title: "断开 SSH 连接",
		Description: "断开服务器上的 SSH 连接（先用 ssh_sessions 看 pid），里面正在运行的命令会被结束。对方可以重新连上：不想让他再连，要改密码或去掉他的公钥。" +
			"Ferry 自己正在用的连接不能断开。" + how,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}},
		loggedReq(s, "ssh_disconnect", func(ctx context.Context, req *mcp.CallToolRequest, in sshKickIn) (any, error) {
			if len(in.Pids) == 0 || len(in.Pids) > 20 {
				return nil, errors.New("pids 写 1～20 个连接（ssh_sessions 里的 pid）")
			}
			// 先看一眼：不在了、断不开的直接说
			_, err := s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				if err := allowedOn(e, t); err != nil {
					return nil, err
				}
				rep, err := e.SSHSessions(t)
				if err != nil {
					return nil, err
				}
				for _, p := range in.Pids {
					found := false
					for _, x := range rep.Sessions {
						if x.PID == p {
							found = true
							switch {
							case x.Self:
								return nil, fmt.Errorf("PID %d 是 Ferry 自己正在用的连接，不能断开", p)
							case !x.Kick:
								return nil, fmt.Errorf("PID %d 断不开：登录用户 %s 不是 root，也不能免密 sudo，只能断开自己的连接", p, rep.Me)
							}
						}
					}
					if !found {
						return nil, fmt.Errorf("服务器上没有 PID %d 这个连接（已经断开了？用 ssh_sessions 再看一下）", p)
					}
				}
				return nil, nil
			})
			if err != nil {
				return nil, err
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: "ssh-kick", Pids: in.Pids, Reason: in.Reason})
		}))

	addTool(s, m, &mcp.Tool{Name: "firewall_enable", Title: "打开防火墙",
		Description: "打开服务器上装了没开的防火墙（firewalld / ufw；先用 server_ports 看现状：firewall.enable 为 true 才能打开）。" +
			"先放行 SSH 端口和 allow 里的端口再打开，没写的端口打开后外部都连不上；打开后 Ferry 用新的连接试 SSH，连不上时 3 分钟后服务器自动关掉防火墙、恢复原样。" +
			"Docker 映射的端口不经过防火墙。" + how,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}},
		loggedReq(s, "firewall_enable", func(ctx context.Context, req *mcp.CallToolRequest, in fwEnableIn) (any, error) {
			var allow []string
			for _, a := range in.Allow {
				p := strings.TrimSpace(a)
				if !strings.Contains(p, "/") {
					p += "/tcp"
				}
				if !validPort(p) {
					return nil, fmt.Errorf("端口不对：%q（写 8080/tcp 或 53/udp）", a)
				}
				allow = append(allow, p)
			}
			// 先看一眼：已经开着、没有能打开的防火墙时不用问用户
			_, err := s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				if err := allowedOn(e, t); err != nil {
					return nil, err
				}
				rep, err := e.Ports(t)
				if err != nil {
					return nil, err
				}
				switch fw := rep.Firewall; {
				case fw.Active:
					return nil, fmt.Errorf("%s 的防火墙（%s）已经在运行：放行端口用 port_control", t, fw.Kind)
				case !fw.Enable:
					return nil, fmt.Errorf("%s 上没有 Ferry 能打开的防火墙（只支持装了没开的 firewalld、ufw；Ferry 不替你装防火墙）", t)
				}
				return nil, nil
			})
			if err != nil {
				return nil, err
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: "fw-enable", Allow: allow, RestartDocker: in.RestartDocker, Reason: in.Reason})
		}))
}

// allowedOn 连服务器之前先看能不能改动它（生产服务器、没打开开关……）。
func allowedOn(e *engine.Engine, target string) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	return ChangeAllowed(e.Settings, e.File, t)
}
