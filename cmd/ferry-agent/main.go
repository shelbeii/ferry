// ferry-agent：在目标服务器上以 root 运行的部署助手，由 ferry 通过 SSH 上传并调用。
//
//	ferry-agent version                 输出版本（JSON）
//	ferry-agent <命令> < request.json    preflight | prepare | deploy | rollback | control | status | releases | history
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/proto"
)

func main() {
	// ferry-acme acme-renew [--force]：HTTPS 证书续期的定时器（ferry-acme.timer）每天运行，不读请求
	if len(os.Args) >= 2 && os.Args[1] == "acme-renew" {
		os.Exit(agent.RenewAll(agent.NewSystem(), os.Stdout, len(os.Args) > 2 && os.Args[2] == "--force"))
	}
	// ferry-monitor monitor-run：服务器监控的定时器（ferry-monitor.timer）每隔几分钟运行
	if len(os.Args) == 2 && os.Args[1] == "monitor-run" {
		os.Exit(agent.MonitorRun(agent.NewSystem(), os.Stdout))
	}
	// ferry-backup backup-run <部署目录> <名字>：数据备份的定时器（ferry-backup-*.timer）按时运行
	if len(os.Args) == 4 && os.Args[1] == "backup-run" {
		os.Exit(agent.BackupRun(agent.NewSystem(), os.Stdout, os.Args[2], os.Args[3]))
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：ferry-agent version | ferry-agent <命令> < request.json")
		os.Exit(proto.CodeBadInput)
	}
	cmd := os.Args[1]
	if cmd == "version" {
		v := map[string]any{"version": agent.Version, "proto": proto.Version, "arch": runtime.GOARCH, "sha256": selfSHA()}
		json.NewEncoder(os.Stdout).Encode(v)
		return
	}
	// 连接断开也要做完：本机取消、网络中断时 SSH 连接关掉，再往 stdout 写会收到 SIGPIPE，Go 默认因此退出，
	// 部署可能停在「旧服务已停、新服务还没起来」。忽略它：写不出去的输出丢掉，照常做完（失败照常回滚），结果记在部署历史里。
	// agent 不在终端里运行、也没有一直不结束的命令，忽略 SIGHUP 不会留下进程。
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<20))
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取请求失败：", err)
		os.Exit(proto.CodeBadInput)
	}
	var req proto.Request
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Fprintln(os.Stderr, "请求不是有效的 JSON：", err)
		os.Exit(proto.CodeBadInput)
	}
	a := &agent.Agent{Sys: agent.NewSystem(), Out: os.Stdout}
	os.Exit(a.Run(cmd, &req))
}

func selfSHA() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
