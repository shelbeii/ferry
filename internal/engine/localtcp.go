package engine

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// localTCPProc 本机上占用某条外连 TCP 源端口的进程信息。
type localTCPProc struct {
	PID     int
	Name    string // 可执行文件名（如 ssh.exe、goland64.exe、ferry）
	CmdLine string // 可选：命令行摘要（用于区分 ferry mcp / ferry serve / CLI）
}

// normalizeIPAddr 归一化 IP（剥离 IPv4-mapped IPv6 前缀与方括号/zone）。
func normalizeIPAddr(raw string) (netip.Addr, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func selfProcessName() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "ferry"
	}
	return filepath.Base(exe)
}

// formatLocalProcOrigin 将本机反查到的进程信息整理为简洁的归属标签和详情。
func formatLocalProcOrigin(proc localTCPProc, localPort int) (label, detail string) {
	name := strings.TrimSpace(proc.Name)
	lower := strings.ToLower(strings.TrimSuffix(name, ".exe"))
	if name == "" {
		if proc.PID > 0 {
			return fmt.Sprintf("本机进程 (PID %d)", proc.PID), fmt.Sprintf("本机进程 PID %d 通过源端口 %d 建立的 SSH 连接", proc.PID, localPort)
		}
		return "本机其他进程", fmt.Sprintf("确认由本机源端口 %d 发起（当前权限无法读取该进程名称）", localPort)
	}
	if lower == "ferry" || strings.HasPrefix(lower, "ferry-") {
		role := ferryProcRole(proc.CmdLine)
		if role != "" {
			label = fmt.Sprintf("本机 · Ferry (%s)", role)
		} else {
			label = "本机 · Ferry (其他进程)"
		}
		if proc.PID > 0 {
			detail = fmt.Sprintf("本机另一 Ferry 进程 %s（PID %d，源端口 %d）", name, proc.PID, localPort)
		} else {
			detail = fmt.Sprintf("本机另一 Ferry 进程 %s（源端口 %d）", name, localPort)
		}
		if proc.CmdLine != "" {
			detail += " · " + proc.CmdLine
		}
		return label, detail
	}
	label = "本机 · " + name
	if proc.PID > 0 {
		detail = fmt.Sprintf("本机进程 %s（PID %d，源端口 %d）", name, proc.PID, localPort)
	} else {
		detail = fmt.Sprintf("本机进程 %s（源端口 %d）", name, localPort)
	}
	if proc.CmdLine != "" && proc.CmdLine != name {
		detail += " · " + proc.CmdLine
	}
	return label, detail
}

func ferryProcRole(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	fields := strings.Fields(cmdline)
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			continue
		}
		switch f {
		case "mcp":
			return "MCP"
		case "serve":
			return "桌面端"
		default:
			return "CLI " + f
		}
	}
	return ""
}
