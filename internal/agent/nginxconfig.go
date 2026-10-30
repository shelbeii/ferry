package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type nginxInvocation struct {
	binary string
	flags  []string
}

func (n nginxInvocation) command(action string) string {
	cmd := shellQuote(n.binary) + " " + action
	for i := 0; i+1 < len(n.flags); i += 2 {
		cmd += " " + n.flags[i] + " " + shellQuote(n.flags[i+1])
	}
	return cmd
}

func (n nginxInvocation) flag(key string) string {
	for i := 0; i+1 < len(n.flags); i += 2 {
		if n.flags[i] == key {
			return n.flags[i+1]
		}
	}
	return ""
}

// nginx 修改进程标题后，整个 master 命令会变成单个 argv，worker 标题不含启动配置。
func nginxProcessArgs(argv []string, binary string) []string {
	if len(argv) == 0 {
		return nil
	}
	if text, ok := strings.CutPrefix(argv[0], "nginx: master process "); ok {
		return nginxTokens(text)
	}
	if strings.HasPrefix(argv[0], "nginx:") {
		return nil
	}
	if argv[0] == binary || filepath.Base(argv[0]) == "nginx" || filepath.Base(argv[0]) == "openresty" {
		return argv // 原始 argv 的参数边界（包含空格的路径等）需要保留。
	}
	return nil
}

func nginxConfigFlags(argv []string) []string {
	var flags []string
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-g" {
			// master 标题和 systemctl 输出可能不保留 -g 的引号，恢复整段全局指令。
			end := i + 1
			for end < len(argv) {
				next := argv[end]
				if len(next) > 1 && next[0] == '-' && (next[1] >= 'a' && next[1] <= 'z' || next[1] >= 'A' && next[1] <= 'Z') {
					break
				}
				end++
			}
			if end > i+1 {
				flags = append(flags, arg, strings.Join(argv[i+1:end], " "))
			}
			i = end - 1
		} else if arg == "-c" || arg == "-p" || arg == "-e" {
			if i+1 < len(argv) {
				flags = append(flags, arg, argv[i+1])
				i++
			}
		} else if len(arg) > 2 && strings.Contains("cpeg", arg[1:2]) && arg[0] == '-' {
			flags = append(flags, arg[:2], arg[2:])
		}
	}
	return flags
}

func (a *Agent) nginxConfigInvocation(it proto.Software, in *proto.SoftwareInstance) (nginxInvocation, error) {
	inv := nginxInvocation{binary: it.Path}
	var processArgs []string
	for _, argv := range a.Sys.ProcArgs(it.PID) {
		args := nginxProcessArgs(argv, it.Path)
		if len(args) == 0 {
			continue
		}
		processArgs = args
		if strings.HasPrefix(argv[0], "nginx: master process ") {
			break
		}
	}
	if len(processArgs) > 0 {
		inv.flags = nginxConfigFlags(processArgs)
		if inv.binary == "" {
			inv.binary = processArgs[0]
		}
	} else if it.Unit != "" {
		out, err := a.Sys.Shell("systemctl show -p ExecStart --no-pager "+shellQuote(it.Unit), 5*time.Second)
		if err == nil {
			if _, tail, ok := strings.Cut(out, "argv[]="); ok {
				// systemctl 的结构化属性以「 ; 」分隔字段。这里只取 ExecStart 的 argv。
				command, _, _ := strings.Cut(tail, " ; ")
				processArgs = nginxTokens(command)
				inv.flags = nginxConfigFlags(processArgs)
				if inv.binary == "" && len(processArgs) > 0 {
					inv.binary = processArgs[0]
				}
			}
		}
	}
	if len(processArgs) == 0 && in != nil && in.Hosting != nil {
		h := in.Hosting
		inv.binary = h.Binary
		inv.flags = []string{"-p", strings.TrimSuffix(h.Prefix, "/") + "/", "-c", h.Config}
		processArgs = []string{h.Binary}
	}
	if inv.binary == "" || len(processArgs) == 0 {
		return inv, errors.New("无法确认此 nginx 的启动参数，未使用系统默认配置作为替代")
	}
	return inv, nil
}

func (a *Agent) discoverNginxConfigs(it proto.Software, in *proto.SoftwareInstance, report *proto.SoftwareConfigs, add func(string, string, string, bool)) {
	inv, err := a.nginxConfigInvocation(it, in)
	if err != nil {
		report.Notes = append(report.Notes, err.Error())
		return
	}
	out, err := a.Sys.Shell(inv.command("-T")+" 2>&1", 10*time.Second)
	if err == nil {
		first := true
		for _, line := range strings.Split(out, "\n") {
			p, ok := strings.CutPrefix(strings.TrimSpace(line), "# configuration file ")
			if !ok || !strings.HasSuffix(p, ":") {
				continue
			}
			p = strings.TrimSuffix(p, ":")
			label, source := "", "include"
			if first {
				label, source, first = "主配置", "main", false
			}
			add(p, label, source, false)
		}
		if !first {
			return
		}
	}
	// 配置损坏时 -T 不会完整输出。只保留可确认的主配置，不猜测其他实例的目录。
	main := inv.flag("-c")
	if main == "" || !filepath.IsAbs(main) {
		version, versionErr := a.Sys.Shell(shellQuote(inv.binary)+" -V 2>&1", 5*time.Second)
		if versionErr == nil {
			prefix := inv.flag("-p")
			if prefix == "" {
				prefix = nginxBuildArg(version, "prefix", "")
			}
			if main == "" {
				main = nginxBuildArg(version, "conf-path", "")
			}
			if main != "" && !filepath.IsAbs(main) && filepath.IsAbs(prefix) {
				main = filepath.Join(prefix, main)
			}
		}
	}
	if filepath.IsAbs(main) {
		add(filepath.Clean(main), "主配置", "main", false)
	}
	message := "未能读取此 nginx 的完整 include 列表，仅显示可确认的配置文件"
	if err != nil {
		message += fmt.Sprintf("：%v", err)
	}
	report.Notes = append(report.Notes, message)
}
