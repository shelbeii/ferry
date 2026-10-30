// ferry：把后端（JAR / 原生程序）与前端静态站点部署到 Linux 服务器（systemd），无需在服务器上预装任何东西。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/agentbin"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/build"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/jdk"
	"github.com/captain/ferry/internal/nginxrt"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/scaffold"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/ui"
	"golang.org/x/crypto/ssh"
)

// Version 构建时注入。
var Version = "dev"

const usage = `ferry —— 后端 / 前端部署工具

用法：ferry <命令> [参数] [选项]

  init                          在当前目录生成 ferry.yaml 模板
  detect   <目录> [--ai [--model <模型 ID>]]  识别目录里的后端 / 前端应用（新建项目向导用，配合 --json）；--ai 再让 AI 看项目文件检查、补充
  create   -c <路径> [--dry-run|--check] 按环境变量 FERRY_CREATE_VALUE 里的 JSON 生成 ferry.yaml（向导用；不覆盖已有文件）
  probe    prepare | <目标> -c <临时配置> [--create-user <用户>]  新建项目时测试连接、检查服务器（向导用，内容在 FERRY_PROBE_VALUE）
  validate                      校验配置
  plan     <目标> [应用…]        查看将执行的构建、产物和 systemd 单元（不连接服务器）
  trust    <目标> [--expect <指纹>]  核对并记录服务器主机公钥（首次连接前执行）
  hostkey  <目标>               只取服务器主机公钥指纹与记录状态
  inspect                       输出规范化后的配置（配合 --json）
  ai       <目标> <要问的话> [--conv <对话>] [--mode confirm] [--model <模型 ID>] [--effort low|medium|high|xhigh|max|none]
           [--image <图片>[;<图片>…]]
                                服务器助手：大模型查看服务器、回答问题（模型在设置 → AI 里配置）；--image 附上截图等图片；
                                --mode confirm 允许改动服务器，每个改动停下来等确认：
                                ferry ai <目标> --conv <对话> --decide approve|always|reject
  ai-key   set|delete|status --model <ID>  各模型的 API Key（存在系统凭据库，值从 FERRY_SECRET_VALUE 读）
  ai-test  [--model <ID>]       用设置里的模型发一句话，确认能用
  ai-image <图片…>              读图片文件（桌面版拖进 AI 助手的图片），输出格式和 base64 内容
  ai-history list [-c <配置>] [<目标>] | show <对话> | export <对话> [--out <文件>] | delete <对话> | clean [--days N] [--max-mb N] [--all]
                                AI 助手的历史对话；clean 按设置（保留天数、最大占用）清理，--all 全部删除
  task-history list [--interrupt] | show <任务> | save | delete <任务…> | clean [--days N] [--max-mb N] [--keep N] [--all]
                                桌面版任务面板的任务记录（save 从 FERRY_TASK_VALUE 读 JSON）；clean 按设置清理，--all 全部删除
  busy     <目标> [应用…] [--kill <进程号>]
                                本机（Windows）正占着要构建的 jar 的进程；--kill 结束其中一个
  keygen   <目标> [--key <私钥>] [--local | --write]
                                生成密钥对（已有则复用），用密码登录把公钥装到服务器，验证免密登录；
                                --local 把私钥记到这台电脑的本机设置，--write 写回 ferry.yaml。密码只用这一次，不保存
  local    [check]              本机设置（只属于这台电脑：私钥、登录方式、变量的值，不写进 ferry.yaml）；check 只列出这台电脑缺什么
  local    set <目标> --key <私钥> | --password | --default-key   这台电脑上用哪个私钥 / 密码 / 默认私钥登录这个目标
  local    unset <目标>         去掉目标的本机设置，按 ferry.yaml
  local    var <变量> [<值>]    这台电脑上变量的值（如源码目录）；不给值 = 去掉，按 ferry.yaml
  local    save                 桌面版用：从 FERRY_LOCAL_VALUE 读整份本机设置（JSON）保存
  move-config <新位置> -c <ferry.yaml> [--force]   移动配置文件，本机设置、部署历史、构建日志、项目列表跟着改过去
  secret   set|delete|status <目标>          服务器登录密码（auth: password）存到系统凭据库
  secret   set|delete|status --passphrase <私钥>  私钥密码
  secret   list | delete --account <账户>    已保存的凭据（列表只含账户；桌面版可单独查看内容）
  settings [save]               查看设置（--json）；save 从环境变量 FERRY_SETTINGS_VALUE 读 JSON 保存
  notify   [list] | save <ID> [--sign] | forget <ID> [--sign] | test [<ID>]
                                部署通知（企业微信、钉钉、飞书、自定义接口；在设置 → 通知里添加）：save 保存机器人地址或
                                加签密钥（存在系统凭据库，值从 FERRY_SECRET_VALUE 读），test 发一条测试消息。
                                CI 里可以不用设置：FERRY_NOTIFY="dingtalk:https://…"，加签密钥放 FERRY_NOTIFY_SECRET
  dns-key  [list] | set aliyun|dnspod --id <ID> | set cloudflare | delete <服务商>
                                HTTPS 证书 DNS 验证用的密钥（static.https.dns；存在系统凭据库，Secret / Token 从 FERRY_SECRET_VALUE 读）；
                                CI 里用环境变量 FERRY_DNS_ALIYUN_ID、FERRY_DNS_ALIYUN_SECRET、FERRY_DNS_CLOUDFLARE_TOKEN……
  datadir  [set <目录> | reset] [--move]   数据目录（密钥、主机公钥记录、构建日志、设置）；--move 同时搬数据
                                set 的值从终端输入，或环境变量 FERRY_SECRET_VALUE
  preflight <目标> [应用…]       检查服务器环境
  deploy   <目标>[,<目标>…] [应用…]  构建、上传、逐个切换；失败自动回滚该应用并停止后续应用和目标
  status   <目标> [应用…]        查看状态
  start | stop | restart <目标> [应用…]
  releases <目标> [应用…]        列出各版本
  changes  <目标> [应用…]        这次部署会带上线的改动：线上运行的版本、本地的提交、中间新增的提交（不改动服务器）
  monitor  <目标> [status | on [--notify ID,…] [--interval 5] [--disk 90] [--cert-days 14] | off | check | test]
                                服务器监控：服务器上每隔几分钟检查应用停了 / 反复重启、磁盘快满、证书快到期、防火墙被关、
                                新出现对外开放的端口，出现、恢复时各发一次通知（发到设置 → 通知里的地方）
  backup   <目标> [status | on | off | run [名字] | list [名字] | restore <名字> <文件> [--force] | delete <名字> <文件> | download <名字> <文件> [本机目录]]
                                数据备份（配置里的 backups：MySQL、PostgreSQL、目录、自己写的命令）：打开后服务器上按时备份、
                                部署前备份（beforeDeploy），随时可以关；恢复前先自动备份一份现在的
  rollback <目标> <应用> [--to <版本ID|legacy>]
  history  <目标> [应用…] [--limit N]
  software <目标>               查看服务器上已有的软件（MySQL、Redis、Nacos……，包括 Docker / 1Panel 装的）；只读
  software install <目标> <模板> [选项]   按模板安装软件（ferry software 查看全部用法）
  software start|stop|restart|remove|logs|password <目标> <名称>   Ferry 安装的软件
  software templates | template | latest | lib   软件模板、本机的安装包库
  port     <目标>               服务器上的端口（谁在用）和防火墙放行了哪些
  port     open|close <目标> <端口>[/udp]   用服务器的防火墙（firewalld / ufw）放行 / 关闭端口
  port     check <目标> <端口>… [--timeout 3]   从本机连服务器的端口，看能不能连上
  port     enable <目标> [--allow 8080,443/tcp] [--restart-docker]   打开装了没开的防火墙（先放行 SSH 端口，连不上自动恢复）
  port     disable <目标> [--restart-docker]   关闭正在运行的防火墙（开机不再启动，规则留着）
  who      <目标>               服务器上连着的 SSH：登录用户、来源 IP、连上的时间、在干什么
  server-info <目标>            服务器系统、CPU、内存、Swap、各挂载点容量与 inode、网卡地址（只读）
  who      kick <目标> <PID>…   断开这些 SSH 连接
  logs     <目标> <应用> [-n 200] [-f] [--file | --log <文件>]   当前运行的日志：journald（从这次运行开始），
                                --file 看 diagLogs 的第一个文件，--log 指定 shared 下的日志文件
  logs     <目标> <应用> --runs                    列出各次运行的日志（每次切换版本时归档）
  logs     <目标> <应用> --grep <关键字> [--file | --log <文件> | --all-files] [--case] [--context 2] [-n 最多条数]
                                搜索：默认搜服务输出；--file 搜 diagLogs 的文件，--all-files 搜这次运行的全部日志文件
  logs     <目标> <应用> --log <文件> --line N        看日志文件第 N 行附近（--log 可以写通配符，如 logs/app-*.log）
  logs     <目标> <应用> --release <版本> [--run <归档>] [--file | --log <文件>]   以往某次运行的日志
  runtime  list | download --java 17 [--arch x64|aarch64] | import <JDK 压缩包> | remove <ID>
                                本机的 JDK 库（service.runtime 用）；下载源在设置里改
  runtime  use <应用> --java 17   让应用改用 Ferry 提供的 JDK（改写 ferry.yaml）
  terminal <目标> [--cols 120 --rows 30]   打开服务器的交互式终端（只在 serve 里，桌面版使用）
  files    ls <目标> [目录] | mkdir <目标> <目录> | mv <目标> <从> <到> | rm <目标> <路径…>
  files    preview <目标> <文件>   只读预览文本内容（最多读取前 1 MiB）
  files    save <目标> <文件>      保存文本；版本及内容从 FERRY_FILE_VALUE 的 JSON 读取
  files    upload <目标> --dir <远程目录> [--overwrite] <本机文件…> | download <目标> --to <本机目录> [--overwrite] <远程路径…>
                                服务器上的文件（SFTP，以登录用户的身份）
  connection list | save | remove <名称>   桌面版「新建连接」保存的服务器（数据目录的 connections.yaml，用 -c 指定它来连接）
  path     [status] | add | remove   把 ferry 所在目录加进 / 移出当前用户的 PATH（Windows），之后可以直接敲 ferry
  mcp-log  [--offset 0] [--limit 200] [--tool a,b] [--client 名称] [--result ok|fail] [--changes]
           [--since 时间] [--until 时间] [--q 关键字] | clean [--all]
                                编程软件经 MCP 的调用记录（新的在前，可以筛选）；clean 按设置清理
  mcp-tools                     编程软件能用的 MCP 工具：作用、参数、是否要确认
  mcp                           MCP 服务（stdin / stdout）：让 Claude Code、Cursor 等编程软件查看 Ferry 的项目、服务器状态和日志
  version
  serve                         常驻模式（桌面版使用）：stdin 逐行读 JSON 请求，连接在请求之间复用

应用可写多个或逗号分隔，不写 = 目标上的全部应用（按 order 顺序）。

选项：
  -c, --config <文件>   配置文件（默认从当前目录向上查找 ferry.yaml）
  -y, --yes             不询问确认
  -v, --verbose         实时显示构建输出
      --json            以 JSON 行输出（供 GUI 调用）
      --skip-build      deploy：跳过构建，直接用现有产物
      --full            deploy：全量上传（不用增量）
      --no-rollback     deploy：失败时保留失败版本（排查用）
      --keep-perms      deploy：不自动修正权限（默认会把运行用户访问不了的目录、不安全的部署目录改好）
      --accept-new-host-key  首次连接时自动记录主机公钥（等同 ssh 的 accept-new）
      --units           plan：输出完整的 systemd 单元文件
`

type cli struct {
	pos   []string
	flags map[string]string
}

var valueFlags = map[string]bool{"pid": true, "c": true, "config": true, "to": true, "limit": true, "offset": true, "tool": true, "client": true, "result": true, "since": true, "until": true, "q": true, "n": true, "expect": true, "key": true, "passphrase": true, "account": true,
	"release": true, "run": true, "log": true, "java": true, "arch": true, "nginx": true, "nginx-version": true,
	"version": true, "name": true, "dir": true, "port": true, "console-port": true, "memory": true, "grep": true, "context": true, "line": true, "unit": true, "container": true,
	"variant": true, "package": true, "user": true, "java-dir": true, "in": true, "set": true, "cols": true, "rows": true, "kill": true, "conv": true, "mode": true, "decide": true, "days": true, "max-mb": true, "keep": true, "target": true, "network": true, "model": true, "effort": true, "rewind": true, "out": true, "create-user": true, "note": true, "timeout": true, "allow": true, "id": true, "notify": true, "interval": true, "disk": true, "cert-days": true}
var boolFlags = map[string]bool{
	"y": true, "yes": true, "v": true, "verbose": true, "json": true, "skip-build": true, "full": true,
	"no-rollback": true, "accept-new-host-key": true, "units": true, "h": true, "help": true, "f": true, "follow": true, "file": true, "write": true, "move": true, "from-env": true, "runs": true, "dry-run": true, "check": true, "keep-perms": true,
	"public": true, "purge": true, "overwrite": true, "lower-case-tables": true, "main-log": true, "latest": true, "case": true, "all-files": true, "all": true, "keep-choice": true, "interrupt": true, "changes": true, "restart-docker": true,
	"local": true, "fix-parents": true, "password": true, "default-key": true, "loaded-mark": true, "background": true, "sign": true, "force": true,
	"ai": true,
}

func parseArgs(argv []string) (*cli, error) {
	c := &cli{flags: map[string]string{}}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			c.pos = append(c.pos, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			c.pos = append(c.pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val := ""
		hasVal := false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		switch {
		case valueFlags[name]:
			if !hasVal {
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("%s 需要一个值", a)
				}
				i++
				val = argv[i]
			}
			c.flags[name] = val
		case boolFlags[name]:
			c.flags[name] = "true"
		default:
			return nil, fmt.Errorf("未知选项 %s", a)
		}
	}
	alias := map[string]string{"c": "config", "y": "yes", "v": "verbose", "h": "help", "f": "follow"}
	for short, long := range alias {
		if v, ok := c.flags[short]; ok {
			c.flags[long] = v
			delete(c.flags, short)
		}
	}
	return c, nil
}

func (c *cli) has(name string) bool { return c.flags[name] == "true" }

// cliIO 一次命令的输入输出。命令行下就是本进程的；ferry serve 里每个请求一份。
type cliIO struct {
	ctx    context.Context
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	pool   *engine.Pool // ferry serve 的连接池；命令行下为空（每次新建连接）
	tty    *ttyIO       // ferry serve 里终端请求的键盘输入、窗口大小与输出；命令行下为空
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(serve(os.Stdin, os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(mcpMain())
	}
	os.Exit(run(os.Args[1:], &cliIO{ctx: context.Background(), stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}))
}

func run(argv []string, cio *cliIO) int {
	// 选项可以写在命令前后任意位置：第一个非选项参数是命令
	c, err := parseArgs(argv)
	if err != nil {
		fmt.Fprintln(cio.stderr, "ferry:", err)
		return 2
	}
	if len(c.pos) == 0 || c.pos[0] == "help" || c.has("help") {
		fmt.Fprint(cio.stdout, usage)
		return 0
	}
	cmd := c.pos[0]
	c.pos = c.pos[1:]
	serving := cio.pool != nil
	if serving && cmd != "logs" {
		c.flags["json"] = "true" // ferry serve 里没有终端：除了日志（原样输出）都用 JSON
	}
	switch cmd {
	case "version":
		info := versionInfo()
		if c.has("json") {
			b, _ := json.Marshal(map[string]any{"kind": "result", "ok": true, "data": info})
			fmt.Fprintln(cio.stdout, string(b))
			return 0
		}
		fmt.Fprintf(cio.stdout, "ferry %s（agent 协议 %d，%s，%s/%s）\n", Version, proto.Version, info["go"], info["os"], info["arch"])
		for _, a := range info["agents"].([]map[string]string) {
			fmt.Fprintf(cio.stdout, "  agent linux/%s  %s\n", a["arch"], a["sha256"])
		}
		return 0
	case "init":
		if !serving {
			return cmdInit()
		}
	case "serve", "mcp":
		fmt.Fprintf(cio.stderr, "ferry: %s 必须是第一个参数\n", cmd)
		return 2
	case "path": // 不读设置、不迁移数据：安装、卸载程序也会调用
		return pathCmd(c, cio)
	}

	// 旧版本的数据：用户目录（%APPDATA%\Ferry 等）里的搬到安装目录。要在读设置之前：设置（含项目列表）也在里面
	migMsg, migFrom, migErr := appdir.MigrateUserDir()
	st, stErr := settings.Load()
	var con *ui.Console
	var js *ui.JSON
	var rep engine.Reporter
	if c.has("json") {
		js = ui.NewJSON(cio.stdout)
		js.Verbose = c.has("verbose") || st.Build.Verbose
		rep = js
	} else if serving {
		con = ui.NewConsoleTo(cio.stderr, c.has("verbose") || st.Build.Verbose)
		rep = con
	} else {
		con = ui.NewConsole(c.has("verbose") || st.Build.Verbose)
		rep = con
	}
	if stErr != nil {
		rep.Warn("%v（使用默认设置）", stErr)
	}
	if migErr != nil {
		rep.Warn("%v", migErr)
	} else if migMsg != "" {
		rep.Info("%s", migMsg)
		if migFrom != "" {
			fixMovedKeys(rep, st, migFrom)
		}
	}
	if msg, err := appdir.MigrateLegacy(); err != nil {
		rep.Warn("迁移旧数据失败：%v", err)
	} else if msg != "" {
		rep.Info("%s", msg)
	}
	fail := func(err error) int {
		if js != nil {
			js.Result(false, nil, err)
		} else if isCanceled(err) {
			con.Warn("已取消")
		} else {
			con.Fail("%v", err)
		}
		return 1
	}

	// 不需要 ferry.yaml 的命令
	switch {
	case cmd == "settings":
		return settingsCmd(c, cio, js, con, st, fail)
	case cmd == "datadir":
		return datadirCmd(c, cio, js, con, fail)
	case cmd == "secret" && (len(c.pos) > 0 && (c.pos[0] == "list" || c.pos[0] == "show") || c.flags["account"] != ""):
		return secretAccountCmd(c, cio, js, con, fail)
	case cmd == "pool" && serving && c.flags["config"] == "":
		return ok0(js, cio.pool.List())
	case cmd == "detect":
		if len(c.pos) == 0 {
			return fail(errors.New("用法：ferry detect <目录> [--ai [--model <模型 ID>]]"))
		}
		if c.has("ai") {
			return aiDetectCmd(c, cio, js, st, fail)
		}
		d, err := scaffold.Detect(c.pos[0])
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, d)
		}
		for _, a := range d.Apps {
			fmt.Fprintf(cio.stdout, "%-20s %-24s %s → %s\n", a.Name, a.Kind, a.Command, a.Artifact)
		}
		return 0
	case cmd == "create":
		return createCmd(c, cio, js, con, fail)
	case cmd == "probe" && len(c.pos) > 0 && c.pos[0] == "prepare":
		return probePrepareCmd(cio, js, fail)
	case cmd == "connection":
		return connectionCmd(c, cio, js, con, fail)
	case cmd == "ai-key":
		return aiKeyCmd(c, cio, js, con, st, fail)
	case cmd == "notify":
		return notifyCmd(c, cio, js, con, st, fail)
	case cmd == "dns-key":
		return dnsKeyCmd(c, cio, js, con, fail)
	case cmd == "ai-test":
		return aiTestCmd(c, cio, js, st, fail)
	case cmd == "ai-image":
		return aiImageCmd(c, cio, js, fail)
	case cmd == "ai-history":
		return aiHistoryCmd(c, cio, js, st, fail)
	case cmd == "mcp-approvals" || cmd == "mcp-decide":
		return mcpApprovalsCmd(cmd, c, cio, js, fail)
	case cmd == "mcp-log":
		return mcpLogCmd(c, cio, js, st, fail)
	case cmd == "mcp-tools":
		return mcpToolsCmd(cio, js)
	case cmd == "task-history":
		return taskHistoryCmd(c, cio, js, st, fail)
	case cmd == "move-config":
		return moveConfigCmd(c, js, con, fail)
	case cmd == "software" && len(c.pos) > 0 && softwareLocal[c.pos[0]]:
		return softwareLocalCmd(c, cio, js, con, st, fail)
	case cmd == "runtime" && !(len(c.pos) > 0 && c.pos[0] == "use"):
		return runtimeCmd(c, cio, js, con, st, fail)
	}

	path := c.flags["config"]
	if path == "" {
		if path, err = config.Find("."); err != nil {
			return fail(err)
		}
	}
	var f *config.File
	if cmd == "validate" && c.has("from-env") {
		// 编辑器实时校验：内容来自环境变量，相对路径仍按 -c 指定的文件位置计算
		f, err = config.LoadContent([]byte(cio.getenv("FERRY_CONFIG_CONTENT")), path)
	} else {
		f, err = config.LoadWithLocal(path)
	}
	if err != nil {
		return fail(err)
	}
	if f.Kind == config.KindConnections && !connCommands[cmd] {
		return fail(fmt.Errorf("%s 里是「新建连接」保存的服务器，只能打开终端、管理文件、看登录用户与服务器信息、用 AI 助手，不能 %s", f.Path, cmd))
	}
	e := &engine.Engine{File: f, Rep: rep, Yes: c.has("yes"), Settings: st,
		AcceptNewHostKey: c.has("accept-new-host-key") || st.Connect.AcceptNewHostKey,
		Pool:             cio.pool, Ctx: cio.ctx, Getenv: cio.getenv,
		// 桌面版的后台自动刷新：只用已有的连接，不推迟空闲断开
		Background: c.has("background") && cio.pool != nil}
	if c.has("keep-perms") {
		e.Settings.Deploy.KeepPerms = true // 不自动修正权限（preflight 只报告问题）
	}

	need := func(n int, what string) error {
		if len(c.pos) < n {
			return fmt.Errorf("用法：ferry %s %s", cmd, what)
		}
		return nil
	}
	targetArgs := func() (string, []string, error) {
		if err := need(1, "<目标> [应用…]"); err != nil {
			return "", nil, fmt.Errorf("%w（可用目标：%s）", err, strings.Join(f.TargetNames(), ", "))
		}
		return c.pos[0], c.pos[1:], nil
	}
	ok := func(data any) int { return ok0(js, data) }

	switch cmd {
	case "runtime": // runtime use <应用> --java N
		if len(c.pos) < 2 {
			return fail(errors.New("用法：ferry runtime use <应用> --java 17"))
		}
		n, _ := strconv.Atoi(c.flags["java"])
		if n < 8 {
			return fail(errors.New("用 --java 指定 JDK 大版本，如 --java 17"))
		}
		if err := config.SetAppRuntime(f.Path, c.pos[1], n); err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("%s 改为使用 Ferry 提供的 JDK %d（已写入 %s）", c.pos[1], n, f.Path)
		}
		return ok(map[string]any{"app": c.pos[1], "java": n})

	case "pool": // ferry serve：各目标的连接状态
		if !serving {
			return fail(errors.New("pool 只在 ferry serve 里可用"))
		}
		return ok(cio.pool.Status(f))

	case "disconnect": // ferry serve：断开目标的连接
		if !serving {
			return fail(errors.New("disconnect 只在 ferry serve 里可用（命令行每次操作后就会断开）"))
		}
		if err := need(1, "<目标>"); err != nil {
			return fail(err)
		}
		t, err := f.Target(c.pos[0])
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"disconnected": cio.pool.Disconnect(f, t)})

	case "terminal": // ferry serve：服务器的交互式终端（桌面版）
		if !serving || cio.tty == nil {
			return fail(errors.New("terminal 只在桌面版里可用；命令行下请直接用 ssh 登录"))
		}
		if err := need(1, "<目标>"); err != nil {
			return fail(err)
		}
		cols, _ := strconv.Atoi(c.flags["cols"])
		rows, _ := strconv.Atoi(c.flags["rows"])
		code, err := e.Terminal(c.pos[0], engine.TermIO{Cols: max(cols, 20), Rows: max(rows, 5),
			Stdin: cio.tty.in, Stdout: cio.tty.out, Resize: cio.tty.resize,
			OnOpen: func(label string) {
				js.Emit("terminal-open", map[string]any{"label": label})
				// 马上告诉桌面版已经连上（stdout 平时攒一小会儿才发）
				if fl, ok := cio.stdout.(interface{ Flush() }); ok {
					fl.Flush()
				}
			},
			OnLatency: func(ms float64, slow bool) {
				js.Emit("latency", map[string]any{"ms": ms, "slow": slow})
			}})
		if err != nil && !isCanceled(err) {
			return fail(err)
		}
		return ok(map[string]any{"exit": code})

	case "files":
		return filesCmd(e, c, cio, js, con, fail)

	case "ai": // ai <目标> <要问的话…> [--conv <对话>] [--mode readonly|confirm] [--decide approve|always|reject]
		return aiCmd(e, c, cio, js, st, fail)

	case "validate":
		if js != nil {
			return ok(map[string]any{"project": f.Project, "apps": f.AppNames(), "targets": f.TargetNames()})
		}
		con.OK("%s：项目 %s，%d 个应用，%d 个目标", f.Path, f.Project, len(f.Apps), len(f.Targets))
		return 0

	case "plan":
		if serving {
			return fail(errors.New("plan 只在命令行里使用"))
		}
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		if err := printPlan(e, t, apps, c.has("units")); err != nil {
			return fail(err)
		}
		return 0

	case "keygen":
		if err := need(1, "<目标> [--key <私钥>] [--local | --write]"); err != nil {
			return fail(err)
		}
		to := engine.KeyToNone
		switch {
		case c.has("local") && c.has("write"):
			return fail(errors.New("--local 和 --write 只能选一个"))
		case c.has("local"):
			to = engine.KeyToLocal
		case c.has("write"):
			to = engine.KeyToYAML
		}
		r, err := e.KeyGen(c.pos[0], c.flags["key"], to)
		if js != nil {
			if err != nil {
				js.Result(false, r, err)
				return 1
			}
			cio.invalidate()
			return ok(r)
		}
		if err != nil {
			return fail(err)
		}
		if !r.ConfigUpdated && !r.LocalUpdated {
			con.Info("加 --local 只记到这台电脑的本机设置，或加 --write 写回 ferry.yaml（targets.%s.identityFile: '%s'）", r.Target, r.ConfigValue)
		}
		con.OK("免密登录已配置：%s（%s）", r.KeyPath, r.Fingerprint)
		return 0

	case "secret":
		res, err := secretCmd(f, c, cio, con)
		if err != nil {
			return fail(err)
		}
		if c.pos[0] != "status" {
			cio.invalidate()
		}
		if js != nil {
			return ok(res)
		}
		return 0

	case "inspect":
		return ok(inspect(f))
	case "probe":
		return probeCmd(c, cio, js, e, fail)

	case "busy": // busy <目标> [应用…] [--kill <进程号>]
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		var items []engine.BusyItem
		if k := c.flags["kill"]; k != "" {
			pid, err := strconv.Atoi(k)
			if err != nil || pid <= 0 {
				return fail(errors.New("--kill 后面写进程号"))
			}
			if items, err = e.KillBusy(t, apps, pid); err != nil {
				return fail(err)
			}
		} else if items, err = e.BusyArtifacts(t, apps); err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(items)
		}
		if len(items) == 0 {
			fmt.Fprintln(cio.stdout, "没有被本机程序占着的产物")
		}
		for _, it := range items {
			fmt.Fprintf(cio.stdout, "%s  %s\n", it.App, it.Path)
			for _, h := range it.Holders {
				fmt.Fprintf(cio.stdout, "    进程 %d  %s  %s\n", h.PID, h.Name, h.Cmd)
			}
		}
		return 0

	case "hostkey":
		if err := need(1, "<目标>"); err != nil {
			return fail(err)
		}
		t, key, err := e.HostKey(c.pos[0])
		if err != nil {
			return fail(err)
		}
		hk := map[string]any{"target": t.Name, "host": t.Host, "port": t.Port, "type": key.Type(),
			"fingerprint": ssh.FingerprintSHA256(key), "state": remote.HostKeyState(t.Host, t.Port, key), "pinned": t.HostKey}
		if js != nil {
			return ok(hk)
		}
		fmt.Printf("%s:%d  %s %s  %s\n", t.Host, t.Port, key.Type(), hk["fingerprint"], hk["state"])
		return 0

	case "trust":
		if err := need(1, "<目标>"); err != nil {
			return fail(err)
		}
		fp, err := trust(e, c.pos[0], c.flags["expect"], con)
		if err != nil {
			return fail(err)
		}
		cio.invalidate()
		return ok(map[string]any{"fingerprint": fp})

	case "preflight":
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		p, err := e.Preflight(t, apps)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(p)
		}
		printPreflight(con, p)
		if len(p.Problems) > 0 {
			return 1
		}
		return 0

	case "deploy":
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		reports, err := e.Deploy(strings.Split(t, ","), apps, engine.DeployOptions{SkipBuild: c.has("skip-build"), Full: c.has("full"), NoRollback: c.has("no-rollback"), KeepPerms: c.has("keep-perms")})
		if js != nil {
			if err != nil {
				js.Result(false, reports, err)
				return 1
			}
			return ok(reports)
		}
		for _, r := range reports {
			if len(r.Apps) == 0 {
				continue
			}
			if len(reports) > 1 {
				fmt.Fprintf(cio.stderr, "\n%s", r.Target)
			}
			printOutcome(con, r.Apps)
			con.Info("版本 %s，提交 %s，上传 %s（全量 %s），用时 %s", r.Release, r.Git.Short(), human(r.Upload), human(r.Total), r.Elapsed.Round(time.Second))
		}
		if err != nil {
			return fail(err)
		}
		con.OK("部署完成：%s", t)
		return 0

	case "logs":
		if err := need(2, "<目标> <应用> [-n 行数] [-f] [--file]"); err != nil {
			return fail(err)
		}
		lines := 200
		if v := c.flags["n"]; v != "" {
			fmt.Sscanf(v, "%d", &lines)
		}
		stop := cio.ctx.Done()
		if !serving {
			ch := make(chan struct{})
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			go func() { <-sig; close(ch) }()
			stop = ch
		}
		if c.has("runs") {
			rs, err := e.LogRuns(c.pos[0], c.pos[1])
			if err != nil {
				return fail(err)
			}
			if js != nil {
				return ok(rs)
			}
			printLogRuns(cio.stdout, rs)
			return 0
		}
		if kw := c.flags["grep"]; kw != "" {
			so := engine.SearchOptions{Release: c.flags["release"], Run: c.flags["run"], Keyword: kw, Case: c.has("case"),
				Journal: !c.has("file") && c.flags["log"] == "" && !c.has("all-files"), Path: c.flags["log"], DiagLog: c.has("file") && c.flags["log"] == ""}
			so.Context = 2 // 默认带前后两行
			if v := c.flags["context"]; v != "" {
				fmt.Sscanf(v, "%d", &so.Context)
			}
			if c.flags["n"] != "" {
				so.Limit = lines
			}
			res, err := e.SearchLogs(c.pos[0], c.pos[1], so)
			if err != nil {
				return fail(err)
			}
			if js != nil {
				return ok(res)
			}
			printSearch(cio.stdout, res)
			return 0
		}
		o := engine.LogOptions{Lines: lines, Follow: c.has("follow"), File: c.has("file") || c.flags["log"] != "", Path: c.flags["log"],
			Release: c.flags["release"], Run: c.flags["run"]}
		if v := c.flags["line"]; v != "" {
			fmt.Sscanf(v, "%d", &o.Around)
		}
		if c.has("loaded-mark") { // 桌面版：最后几行都给完了、开始持续输出时告诉它（这一行不显示）
			o.Loaded = func() { fmt.Fprintln(cio.stdout, logsLoadedMark) }
		}
		if err := e.Logs(c.pos[0], c.pos[1], o, cio.stdout, stop); err != nil {
			return fail(err)
		}
		return 0

	case "status":
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		st, err := e.Status(t, apps)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(st)
		}
		printStatus(con, st)
		return 0

	case "start", "stop", "restart":
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		out, err := e.Control(t, apps, cmd)
		if js != nil {
			if err != nil {
				js.Result(false, out, err)
				return 1
			}
			return ok(out)
		}
		if len(out) > 0 {
			printOutcome(con, out)
		}
		if err != nil {
			return fail(err)
		}
		con.OK("%s 完成", cmd)
		return 0

	case "releases":
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		rs, err := e.Releases(t, apps)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(rs)
		}
		printReleases(con, rs)
		return 0

	case "monitor":
		return monitorCmd(c, e, js, con, fail)

	case "backup":
		return backupCmd(c, e, js, con, fail)

	case "changes":
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		cs, err := e.Changes(t, apps)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(cs)
		}
		e.ReportChanges(t, cs)
		return 0

	case "rollback":
		if err := need(2, "<目标> <应用> [--to <版本ID|legacy>]"); err != nil {
			return fail(err)
		}
		if err := e.Rollback(c.pos[0], c.pos[1], c.flags["to"]); err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(nil)
		}
		con.OK("回滚完成")
		return 0

	case "software":
		return softwareCmd(c, cio, e, js, con, fail)

	case "port":
		return portCmd(c, e, js, con, fail)

	case "who":
		return whoCmd(c, e, js, con, fail)

	case "server-info":
		return serverInfoCmd(c, cio, e, js, fail)

	case "local":
		return localCmd(c, cio, e, js, con, fail)

	case "history":
		t, apps, err := targetArgs()
		if err != nil {
			return fail(err)
		}
		limit := 30
		if v := c.flags["limit"]; v != "" {
			fmt.Sscanf(v, "%d", &limit)
		}
		hs, err := e.History(t, apps, limit)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(hs)
		}
		printHistory(con, hs)
		return 0
	}
	fmt.Fprintf(cio.stderr, "ferry: 未知命令 %q\n\n%s", cmd, usage)
	return 2
}

// logsLoadedMark ferry logs --loaded-mark：最后几行都输出了，下面是持续输出的新内容。
const logsLoadedMark = "\x00ferry:loaded"

// connCommands 对「新建连接」保存的服务器能用的命令（它们只登录，不装 agent）。
var connCommands = map[string]bool{"software": true, "terminal": true, "files": true, "trust": true, "hostkey": true, "secret": true,
	"pool": true, "disconnect": true, "validate": true, "inspect": true, "ai": true, "probe": true, "who": true, "server-info": true}

// filesCmd ferry files ls|preview|save|mkdir|mv|rm|upload|download <目标> …
func filesCmd(e *engine.Engine, c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	usage := errors.New("用法：ferry files ls|preview|save|mkdir|mv|rm|upload|download <目标> …（ferry help 查看）")
	if len(c.pos) < 2 {
		return fail(usage)
	}
	sub, target, rest := c.pos[0], c.pos[1], c.pos[2:]
	done := func(data any, msg string) int {
		if js != nil {
			return ok0(js, data)
		}
		if msg != "" {
			con.OK("%s", msg)
		}
		return 0
	}
	switch sub {
	case "ls":
		dir := ""
		if len(rest) > 0 {
			dir = rest[0]
		}
		l, err := e.ListDir(target, dir)
		if err != nil {
			return fail(err)
		}
		if js == nil {
			for _, f := range l.Entries {
				fmt.Fprintf(os.Stdout, "%s %10d %s %s\n", f.Mode, f.Size, f.MTime.Format("2006-01-02 15:04"), f.Name)
			}
		}
		return done(l, "")
	case "preview":
		if len(rest) != 1 {
			return fail(usage)
		}
		p, err := e.PreviewFile(target, rest[0])
		if err != nil {
			return fail(err)
		}
		if js == nil {
			if p.Unsupported != "" {
				return fail(errors.New(p.Unsupported))
			}
			fmt.Fprint(cio.stdout, p.Content)
			if p.Truncated {
				con.Warn("文件较大，仅显示前 1 MiB；下载后可查看完整内容")
			}
		}
		return done(p, "")
	case "save":
		if len(rest) != 1 {
			return fail(usage)
		}
		var value struct {
			Revision string  `json:"revision"`
			Content  *string `json:"content"`
		}
		if err := json.Unmarshal([]byte(cio.getenv("FERRY_FILE_VALUE")), &value); err != nil || value.Content == nil {
			return fail(errors.New("请用 FERRY_FILE_VALUE 提供包含 revision 和 content 的 JSON"))
		}
		p, err := e.SaveFile(target, rest[0], remote.FileEdit{Revision: value.Revision, Content: *value.Content})
		if err != nil {
			return fail(err)
		}
		return done(p, "文件已保存")
	case "mkdir":
		if len(rest) != 1 {
			return fail(usage)
		}
		if err := e.MakeDir(target, rest[0]); err != nil {
			return fail(err)
		}
		return done(map[string]any{"path": rest[0]}, "已新建 "+rest[0])
	case "mv":
		if len(rest) != 2 {
			return fail(usage)
		}
		if err := e.RenameFile(target, rest[0], rest[1]); err != nil {
			return fail(err)
		}
		return done(map[string]any{"from": rest[0], "to": rest[1]}, "已改为 "+rest[1])
	case "rm":
		if len(rest) == 0 {
			return fail(usage)
		}
		if err := e.RemoveFiles(target, rest); err != nil {
			return fail(err)
		}
		return done(map[string]any{"removed": rest}, "已删除")
	case "upload":
		if len(rest) == 0 || c.flags["dir"] == "" {
			return fail(usage)
		}
		if err := e.Upload(target, rest, c.flags["dir"], c.has("overwrite")); err != nil {
			return fail(err)
		}
		return done(map[string]any{"dir": c.flags["dir"], "files": rest}, "上传完成")
	case "download":
		if len(rest) == 0 || c.flags["to"] == "" {
			return fail(usage)
		}
		if err := e.Download(target, rest, c.flags["to"], c.has("overwrite")); err != nil {
			return fail(err)
		}
		return done(map[string]any{"to": c.flags["to"], "files": rest}, "下载完成")
	}
	return fail(usage)
}

func ok0(js *ui.JSON, data any) int {
	if js != nil {
		js.Result(true, data, nil)
	}
	return 0
}

// invalidate 凭据、主机公钥、数据目录等变了：连接池里的连接下次用到时重新建立。
func (cio *cliIO) invalidate() {
	if cio.pool != nil {
		cio.pool.InvalidateAll()
	}
}

func isCanceled(err error) bool {
	return errors.Is(err, engine.ErrCanceled) || errors.Is(err, remote.ErrCanceled) || errors.Is(err, build.ErrCanceled)
}

// ---------------------------------------------------------------- 输出

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func resultColor(con *ui.Console, s string) string {
	switch s {
	case "OK", "active", "健康":
		return con.Paint("ok", s)
	case "FAILED", "ROLLBACK_FAILED", "failed", "不健康":
		return con.Paint("bad", s)
	case "ROLLED_BACK", "SKIPPED", "activating", "auto-restart":
		return con.Paint("warn", s)
	}
	return s
}

func printOutcome(con *ui.Console, apps []proto.AppOutcome) {
	t := &ui.Table{Header: []string{"应用", "结果", "版本", "说明"}}
	for _, a := range apps {
		t.Rows = append(t.Rows, []string{a.App, a.Result, a.Release, a.Msg})
	}
	t.Color = func(_, col int, s string) string {
		if col == 1 {
			return resultColor(con, s)
		}
		return s
	}
	fmt.Fprintln(os.Stderr)
	t.Write(os.Stderr)
}

func printStatus(con *ui.Console, st []proto.AppStatus) {
	t := &ui.Table{Header: []string{"应用", "状态", "PID", "主端口", "附加端口", "健康", "当前版本", "提交", "部署时间", "部署人", "版本数", "问题"}}
	var notes []string // 附加端口的用途，表格下面列出
	for _, s := range st {
		state := s.Active
		if s.Sub != "" && s.Sub != "running" {
			state += "/" + s.Sub
		}
		if s.Type == proto.TypeStatic {
			state = "static"
		}
		pid := ""
		if s.PID > 0 {
			pid = fmt.Sprint(s.PID)
		}
		health := ""
		if s.Healthy != nil {
			health = map[bool]string{true: "健康", false: "不健康"}[*s.Healthy]
		}
		cur, commit, at, by := "-", "", "", ""
		if s.Current != nil {
			cur = s.Current.ID
			commit = short(s.Current.Commit, s.Current.Dirty)
			if !s.Current.DeployedAt.IsZero() {
				at = s.Current.DeployedAt.Local().Format("01-02 15:04")
			}
			by = s.Current.DeployedBy
		}
		ports := strings.Join(s.Listen, " ")
		if ports == "" && s.Port != "" {
			ports = "(" + s.Port + ")" // 没在监听：配置里的端口
		}
		var extra []string
		for _, e := range s.Extra {
			extra = append(extra, e.Addr)
			use := e.Use
			if use == "" {
				use = "用途未识别（不是配置里的业务端口）"
			}
			notes = append(notes, fmt.Sprintf("  %s %s：%s", s.Name, e.Addr, use))
		}
		problem := s.Problem
		if problem == "" && s.Serving != "" {
			problem = s.Serving // 静态站点没接入 nginx 的原因
		}
		t.Rows = append(t.Rows, []string{s.Name, state, pid, ports, strings.Join(extra, " "), health, cur, commit, at, by, fmt.Sprint(s.Releases), problem})
	}
	t.Color = func(_, col int, s string) string {
		if col == 1 || col == 5 {
			return resultColor(con, strings.SplitN(s, "/", 2)[0])
		}
		if col == 11 && s != "" {
			return con.Paint("warn", s)
		}
		return s
	}
	t.Write(os.Stdout)
	if len(notes) > 0 {
		fmt.Println("\n附加端口：")
		for _, n := range notes {
			fmt.Println(n)
		}
	}
}

func short(commit string, dirty bool) string {
	if commit == "" {
		return ""
	}
	if len(commit) > 8 {
		commit = commit[:8]
	}
	if dirty {
		commit += "*"
	}
	return commit
}

func printReleases(con *ui.Console, rs []proto.AppReleases) {
	for _, ar := range rs {
		fmt.Printf("\n%s\n", ar.App)
		t := &ui.Table{Header: []string{"", "版本", "提交", "分支", "部署时间", "部署人", "大小", "sha256", "状态"}}
		for _, r := range ar.Releases {
			mark := " "
			if r.Current {
				mark = "*"
			}
			state := ""
			if r.Failed {
				state = "失败"
			}
			at := ""
			if !r.DeployedAt.IsZero() {
				at = r.DeployedAt.Local().Format("2006-01-02 15:04")
			}
			sha := r.SHA256
			if len(sha) > 12 {
				sha = sha[:12]
			}
			t.Rows = append(t.Rows, []string{mark, r.ID, short(r.Commit, r.Dirty), r.Branch, at, r.DeployedBy, human(r.Size), sha, state})
		}
		if len(t.Rows) == 0 {
			fmt.Println("  （还没有版本）")
		} else {
			t.Color = func(row, col int, s string) string {
				if col == 8 && s != "" {
					return con.Paint("bad", s)
				}
				return s
			}
			t.Write(os.Stdout)
		}
		if ar.Legacy {
			fmt.Println(con.Paint("dim", "  可回到接管前的服务：ferry rollback <目标> "+ar.App+" --to legacy"))
		}
	}
}

func printLogRuns(w io.Writer, rs []proto.LogRun) {
	t := &ui.Table{Header: []string{"版本", "归档名（--run）", "开始", "结束", "文件", "大小"}}
	for _, r := range rs {
		id, end := r.ID, r.End.Local().Format("2006-01-02 15:04:05")
		if r.Current {
			id, end = "（当前运行）", "-"
		}
		start := "-"
		if !r.Start.IsZero() {
			start = r.Start.Local().Format("2006-01-02 15:04:05")
		}
		t.Rows = append(t.Rows, []string{r.Release, id, start, end, fmt.Sprint(len(r.Files)), human(r.Size)})
	}
	t.Write(w)
}

func printSoftware(con *ui.Console, rep *proto.SoftwareReport) {
	if len(rep.Items) == 0 {
		con.Info("没有找到认得的软件")
	} else {
		t := &ui.Table{Header: []string{"软件", "版本", "状态", "运行方式", "端口", "PID", "程序 / 镜像"}}
		for _, it := range rep.Items {
			state := map[string]string{"running": "运行中", "stopped": "已停止"}[it.State]
			via := map[string]string{"systemd": "系统服务 " + it.Unit, "docker": "容器 " + it.Container, "process": "直接运行的进程"}[it.Via]
			if it.Manager != "" {
				via += "（" + it.Manager + "）"
			}
			ports := strings.Join(it.Listen, " ")
			if len(it.Inner) > 0 {
				ports = strings.TrimSpace(ports + " 容器内 " + strings.Join(it.Inner, " "))
			}
			pid := ""
			if it.PID > 0 {
				pid = fmt.Sprint(it.PID)
			}
			where := it.Path
			if it.Image != "" {
				where = it.Image
			}
			t.Rows = append(t.Rows, []string{it.Name, it.Version, state, via, ports, pid, where})
		}
		t.Color = func(_, col int, s string) string {
			if col == 2 && s == "运行中" {
				return con.Paint("ok", s)
			}
			return s
		}
		t.Write(os.Stdout)
	}
	switch rep.Docker {
	case "", "ok":
	default:
		con.Warn("%s", rep.Docker)
	}
	for _, n := range rep.Notes {
		con.Warn("%s", n)
	}
}

func printSearch(w io.Writer, res *proto.LogSearch) {
	if len(res.Matches) == 0 {
		fmt.Fprintf(w, "没有找到（搜了 %d 个文件）\n", res.Files)
		return
	}
	for _, m := range res.Matches {
		where := "服务输出"
		if m.File != "" {
			where = fmt.Sprintf("%s:%d", m.File, m.Line)
		}
		fmt.Fprintf(w, "── %s\n", where)
		for _, l := range m.Before {
			fmt.Fprintf(w, "   %s\n", l)
		}
		fmt.Fprintf(w, " > %s\n", m.Text)
		for _, l := range m.After {
			fmt.Fprintf(w, "   %s\n", l)
		}
	}
	more := ""
	if res.Truncated {
		more = "，结果太多只显示了前面的，可以用 -n 调大或换个更具体的关键字"
	}
	fmt.Fprintf(w, "\n共 %d 处%s\n", len(res.Matches), more)
}

func printHistory(con *ui.Console, hs []proto.HistoryEntry) {
	t := &ui.Table{Header: []string{"时间", "操作", "应用", "结果", "版本", "提交", "操作人", "说明"}}
	for _, h := range hs {
		t.Rows = append(t.Rows, []string{h.Time.Local().Format("2006-01-02 15:04:05"), h.Action, h.App, h.Result, h.Release,
			short(h.Commit, false), h.Actor, h.Msg})
	}
	t.Color = func(_, col int, s string) string {
		if col == 3 {
			return resultColor(con, s)
		}
		return s
	}
	t.Write(os.Stdout)
}

func printPreflight(con *ui.Console, p *proto.Preflight) {
	fmt.Printf("系统      %s（内核 %s，%s）\n", p.OS, p.Kernel, p.Arch)
	fmt.Printf("systemd   %d\n", p.Systemd)
	if p.SELinux != "" {
		fmt.Printf("SELinux   %s\n", p.SELinux)
	}
	fmt.Printf("剩余空间  %d MB\n", p.RootFreeMB)
	for _, r := range p.AppRoots {
		fmt.Printf("          %s：%d MB（%s）\n", r.Path, r.FreeMB, strings.Join(r.Apps, "、"))
	}
	var users []string
	for u, ok := range p.Users {
		users = append(users, fmt.Sprintf("%s(%s)", u, map[bool]string{true: "存在", false: "不存在"}[ok]))
	}
	sort.Strings(users)
	if len(users) > 0 {
		fmt.Printf("运行用户  %s\n", strings.Join(users, " "))
	}
	var units []string
	for u, k := range p.ExistingUnits {
		units = append(units, u+"("+k+")")
	}
	sort.Strings(units)
	if len(units) > 0 {
		fmt.Printf("已有单元  %s\n", strings.Join(units, " "))
	}
	for _, j := range p.Javas {
		if j.Major > 0 {
			fmt.Printf("java      %s：Java %d（%s）\n", j.Path, j.Major, j.Version)
		} else {
			fmt.Printf("java      %s：%s\n", j.Path, j.Error)
		}
	}
	for n, r := range p.Runtimes {
		fmt.Printf("JDK %-3d   Ferry 提供：%s\n", n, r.ID)
	}
	if len(p.JavaFound) > 0 {
		var js []string
		for _, j := range p.JavaFound {
			js = append(js, fmt.Sprintf("%s（%d）", j.Path, j.Major))
		}
		fmt.Printf("其他 JDK  %s\n", strings.Join(js, "  "))
	}
	for _, w := range p.Warnings {
		con.Warn("%s", w)
	}
	for _, f := range p.Fixable {
		con.Info("部署时会自动修正：%s", f)
	}
	for _, pr := range p.Problems {
		con.Error("%s", pr)
	}
	if len(p.Problems) == 0 {
		con.OK("预检通过")
	}
}

func printPlan(e *engine.Engine, target string, names []string, units bool) error {
	f := e.File
	t, err := f.Target(target)
	if err != nil {
		return err
	}
	apps, err := f.Select(t, names)
	if err != nil {
		return err
	}
	fmt.Printf("目标 %s：%s@%s:%d，部署目录 %s\n", t.Name, t.User, t.Host, t.Port, t.Root)
	steps, err := build.Plan(f, t, apps)
	if err != nil {
		return err
	}
	if len(steps) > 0 {
		fmt.Println("\n构建（按顺序，同一条构建只执行一次）：")
		for _, s := range steps {
			var an []string
			for _, a := range s.Apps {
				an = append(an, a.Name)
			}
			fmt.Printf("  %s [%s]\n    目录 %s\n    命令 %s\n", s.Name, strings.Join(an, ", "), s.Workdir, s.Command)
		}
	}
	fmt.Println("\n应用（按部署顺序）：")
	tb := &ui.Table{Header: []string{"应用", "类型", "产物", "部署目录", "单元", "健康检查", "保留版本"}}
	var specs []proto.AppSpec
	for _, a := range apps {
		spec, err := engine.Spec(f, t, a)
		if err != nil {
			return err
		}
		specs = append(specs, spec)
		local, err := engine.LocalArtifact(f, t, a)
		if err != nil {
			return err
		}
		h := spec.Health.TCP
		if spec.Health.HTTP != "" {
			h = spec.Health.HTTP
		}
		if h == "" && spec.Health.StableSec > 0 {
			h = fmt.Sprintf("稳定运行 %ds", spec.Health.StableSec)
		}
		unit := spec.Unit
		if a.Type == proto.TypeStatic {
			unit = spec.Link
		}
		root, _ := f.AppRoot(t, a)
		tb.Rows = append(tb.Rows, []string{a.Name, a.Type, local, root + "/apps/" + a.Name, unit, h, fmt.Sprint(spec.KeepReleases)})
	}
	tb.Write(os.Stdout)
	for _, a := range apps {
		files, err := engine.LocalFiles(f, t, a)
		if err != nil {
			return err
		}
		if len(files) > 0 {
			fmt.Printf("\n%s 的附带文件（和程序放在一起）：\n", a.Name)
			for _, x := range files {
				fmt.Printf("  %s → %s\n", x.Local, x.To)
			}
		}
	}
	for _, s := range specs {
		if s.Hooks == nil {
			continue
		}
		fmt.Printf("\n%s 部署时在服务器上执行的命令：\n", s.Name)
		for _, g := range []struct {
			label string
			hooks []proto.Hook
		}{{"切换前（失败时不切换）", s.Hooks.BeforeSwitch}, {"切换后（失败时回滚）", s.Hooks.AfterSwitch}} {
			for _, h := range g.hooks {
				extra := ""
				if h.Optional {
					extra = "，失败只提示"
				}
				fmt.Printf("  %s，以 %s 执行%s：%s\n", g.label, h.User, extra, h.Run)
			}
		}
	}
	for _, s := range specs {
		if s.ZD == nil {
			continue
		}
		fmt.Printf("\n%s 不停机发布：槽位 a 端口 %d（%s），槽位 b 端口 %d（%s），新版本接上后等 %d 秒再停旧版本",
			s.Name, s.ZD.Slots[0].Port, s.ZD.Slots[0].Unit, s.ZD.Slots[1].Port, s.ZD.Slots[1].Unit, s.ZD.DrainSec)
		if s.ZD.ProxyUnit != "" {
			fmt.Printf("；前面由 Ferry 的 nginx（%s）转发", s.ZD.ProxyUnit)
		}
		fmt.Println()
	}
	if units {
		for _, s := range specs {
			if s.ZD != nil {
				for _, sl := range s.ZD.Slots {
					fmt.Printf("\n# ---- %s/%s（槽位 %s）\n%s", agentUnitDir, sl.Unit, sl.Name, sl.UnitText)
				}
				if s.ZD.ProxyUnit != "" {
					fmt.Printf("\n# ---- %s/%s\n%s", agentUnitDir, s.ZD.ProxyUnit, s.ZD.ProxyUnitText)
				}
				continue
			}
			if s.UnitText == "" {
				continue
			}
			fmt.Printf("\n# ---- %s/%s\n%s", agentUnitDir, s.Unit, s.UnitText)
		}
	}
	return nil
}

const agentUnitDir = "/etc/systemd/system"

// trust 记录主机公钥。expect 非空时只有指纹与它一致才记录（GUI 先用 hostkey 取指纹给用户核对，再带 --expect 调用）。
func trust(e *engine.Engine, target, expect string, con *ui.Console) (string, error) {
	t, key, err := e.HostKey(target)
	if err != nil {
		return "", err
	}
	fp := ssh.FingerprintSHA256(key)
	if con != nil {
		fmt.Printf("%s:%d 的主机公钥：%s %s\n", t.Host, t.Port, key.Type(), fp)
	}
	if t.HostKey != "" {
		if t.HostKey == fp {
			if con != nil {
				con.OK("与配置的 hostKey 一致")
			}
			return fp, nil
		}
		return "", fmt.Errorf("与配置的 hostKey %s 不一致", t.HostKey)
	}
	if expect != "" && expect != fp {
		return "", fmt.Errorf("服务器当前的指纹 %s 与核对过的 %s 不一致，未记录", fp, expect)
	}
	if expect == "" && !e.Yes {
		if con == nil {
			return "", errors.New("需要确认：先用 hostkey 取指纹核对，再用 trust --expect <指纹>")
		}
		fmt.Println("请通过可信渠道（服务器控制台：ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub）核对指纹。")
		if !con.Confirm("指纹一致，记录到 " + remote.KnownHostsFile() + "？") {
			return "", engine.ErrCanceled
		}
	}
	if remote.HostKeyState(t.Host, t.Port, key) != "known" {
		if err := remote.TrustHostKey(t.Host, t.Port, key); err != nil {
			return "", err
		}
	}
	if con != nil {
		con.OK("已记录。也可以把 hostKey: %s 写进目标配置，固定指纹", fp)
	}
	return fp, nil
}

// inspect 规范化后的配置（GUI 用）。
func inspect(f *config.File) map[string]any {
	var targets []map[string]any
	for _, n := range f.TargetNames() {
		t := f.Targets[n]
		var apps []string
		appRoots := map[string]string{} // 单独设置了部署目录的应用 → 在这个目标上的实际目录
		for _, a := range f.Ordered() {
			if t.Allows(a.Name) {
				apps = append(apps, a.Name)
				if r, err := f.AppRoot(t, a); err == nil && r != t.Root {
					appRoots[a.Name] = r
				}
			}
		}
		targets = append(targets, map[string]any{
			"name": t.Name, "host": t.Host, "port": t.Port, "user": t.User, "sudo": t.Sudo, "root": t.Root,
			"runAs": t.RunAs, "arch": t.Arch, "keepReleases": t.KeepReleases, "confirm": t.NeedConfirm(),
			"hostKey": t.HostKey, "apps": apps, "auth": t.Auth, "identityFile": t.IdentityFile, "appRoots": appRoots,
			"production": t.Production, "aiChanges": t.AllowAIChanges(),
			"local": t.Local, "localVars": t.LocalVars, "keyMissing": f.KeyMissing(t) != "",
		})
	}
	var apps []map[string]any
	for _, a := range f.Ordered() {
		m := map[string]any{"name": a.Name, "type": a.Type, "order": a.Order, "artifact": a.Artifact.Path}
		if len(a.Artifact.Files) > 0 {
			m["files"] = a.Artifact.Files
		}
		if b := f.ResolveBuild(a); b != nil {
			m["build"] = b.Name
		}
		if a.Type != "static" {
			m["unit"] = a.Service.Unit
		}
		if a.Root != "" {
			m["root"] = a.Root // 原样（可能含变量）；各目标上的实际目录见 targets[].appRoots
		}
		apps = append(apps, m)
	}
	problems := f.MachineProblems()
	if problems == nil {
		problems = []config.MachineProblem{}
	}
	return map[string]any{"project": f.Project, "path": f.Path, "dir": f.Dir, "targets": targets, "apps": apps,
		"localVars": f.LocalVars, "problems": problems}
}

func cmdInit() int {
	if _, err := os.Stat(config.DefaultFileName); err == nil {
		fmt.Fprintln(os.Stderr, "ferry: 当前目录已有 "+config.DefaultFileName)
		return 1
	}
	if err := os.WriteFile(config.DefaultFileName, []byte(initTemplate), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "ferry:", err)
		return 1
	}
	fmt.Println("已生成 " + config.DefaultFileName + "，按注释修改后运行 ferry validate、ferry plan <目标>")
	return 0
}

// secretCmd ferry secret set|delete|status <目标> | --passphrase <私钥>
func secretCmd(f *config.File, c *cli, cio *cliIO, con *ui.Console) (map[string]any, error) {
	if len(c.pos) == 0 {
		return nil, errors.New("用法：ferry secret set|delete|status <目标> 或 --passphrase <私钥>")
	}
	action := c.pos[0]
	var account, what string
	if key := c.flags["passphrase"]; key != "" {
		account, what = secret.PassphraseAccount(f.LocalPath(key)), "私钥 "+key+" 的密码"
	} else {
		if len(c.pos) < 2 {
			return nil, errors.New("用法：ferry secret " + action + " <目标>")
		}
		t, err := f.Target(c.pos[1])
		if err != nil {
			return nil, err
		}
		account, what = secret.PasswordAccount(t.User, t.Host, t.Port), fmt.Sprintf("%s@%s:%d 的登录密码", t.User, t.Host, t.Port)
	}
	switch action {
	case "set":
		v := cio.getenv("FERRY_SECRET_VALUE")
		if v == "" && con != nil {
			v, _ = con.Secret(what + "：")
		}
		if v == "" {
			return nil, errors.New("没有输入：在终端中运行，或通过环境变量 FERRY_SECRET_VALUE 提供")
		}
		if err := secret.Set(account, v); err != nil {
			return nil, err
		}
		if con != nil {
			con.OK("已把%s保存到系统凭据库", what)
		}
		return map[string]any{"stored": true}, nil
	case "delete":
		if err := secret.Delete(account); err != nil {
			return nil, err
		}
		if con != nil {
			con.OK("已删除%s", what)
		}
		return map[string]any{"stored": false}, nil
	case "status":
		_, ok, err := secret.Get(account)
		if err != nil {
			return nil, err
		}
		if con != nil {
			fmt.Printf("%s：%s\n", what, map[bool]string{true: "已保存", false: "未保存"}[ok])
		}
		return map[string]any{"stored": ok}, nil
	}
	return nil, errors.New("secret 只支持 set、delete、status")
}

// settingsCmd ferry settings [save]
func settingsCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	if len(c.pos) > 0 && c.pos[0] == "save" {
		raw := cio.getenv("FERRY_SETTINGS_VALUE")
		if raw == "" {
			return fail(errors.New("用环境变量 FERRY_SETTINGS_VALUE 提供 JSON"))
		}
		next := settings.Defaults()
		if err := json.Unmarshal([]byte(raw), &next); err != nil {
			return fail(fmt.Errorf("设置格式错误：%w", err))
		}
		if err := settings.Save(next); err != nil {
			return fail(err)
		}
		st, _ = settings.Load()
		if cio.pool != nil {
			cio.pool.SetIdle(st.IdleTimeout())
		}
		if con != nil {
			con.OK("已保存 %s", appdir.SettingsFile())
		}
	}
	out := map[string]any{"settings": st, "dataDir": appdir.Current(), "file": appdir.SettingsFile(),
		"keyDir": st.KeyDir(), "legacyDir": appdir.LegacyDir()}
	if js != nil {
		js.Result(true, out, nil)
		return 0
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Printf("数据目录  %s（%s）\n设置文件  %s\n%s\n", appdir.Dir(), appdir.Current().Mode, appdir.SettingsFile(), b)
	return 0
}

// datadirCmd ferry datadir [set <目录> | reset] [--move]
func datadirCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	cur := appdir.Current()
	if len(c.pos) > 0 {
		var dst string
		switch c.pos[0] {
		case "set":
			if len(c.pos) < 2 {
				return fail(errors.New("用法：ferry datadir set <目录> [--move]"))
			}
			dst = c.pos[1]
		case "reset":
			dst = cur.DefaultDir
		default:
			return fail(errors.New("datadir 只支持 set、reset"))
		}
		if cur.Mode == appdir.ModeEnv {
			return fail(errors.New("当前数据目录由环境变量 FERRY_HOME 决定，不能在这里修改"))
		}
		if abs, err := filepath.Abs(dst); err == nil {
			dst = abs
		}
		if c.has("move") {
			if err := appdir.Move(cur.Dir, dst); err != nil {
				return fail(err)
			}
		}
		if err := appdir.SetLocation(dst); err != nil {
			return fail(err)
		}
		cur = appdir.Current()
		cio.invalidate()
		if con != nil {
			con.OK("数据目录：%s", cur.Dir)
		}
	}
	if js != nil {
		js.Result(true, cur, nil)
		return 0
	}
	if len(c.pos) == 0 {
		fmt.Printf("%s（%s；默认位置 %s）\n", cur.Dir, cur.Mode, cur.DefaultDir)
	}
	return 0
}

// runtimeCmd ferry runtime list | download | import | remove：本机的 JDK 库。
func runtimeCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	dir := st.RuntimeDir()
	action := "list"
	if len(c.pos) > 0 {
		action = c.pos[0]
	}
	_, nginxFlag := c.flags["nginx"]
	switch action {
	case "list":
		list := jdk.List(dir)
		if list == nil {
			list = []jdk.Meta{}
		}
		ndir := st.NginxDir()
		nlist := nginxrt.List(ndir)
		if nlist == nil {
			nlist = []nginxrt.Meta{}
		}
		if js != nil {
			source := st.Runtime.Source
			if source == "" {
				source = jdk.DefaultSource
			}
			nsource := st.Runtime.NginxSource
			if nsource == "" {
				nsource = nginxrt.DefaultSource
			}
			return ok0(js, map[string]any{"dir": dir, "source": source, "jdks": list,
				"nginxDir": ndir, "nginxSource": nsource, "nginx": nlist})
		}
		if len(list) == 0 {
			fmt.Fprintf(cio.stdout, "%s 里还没有 JDK\n", dir)
		}
		for _, m := range list {
			fmt.Fprintf(cio.stdout, "%-28s Java %-3d %-8s %8s  %s\n", m.ID, m.Java, m.Arch, human(m.Size), m.Version)
		}
		if len(nlist) == 0 {
			fmt.Fprintf(cio.stdout, "%s 里还没有 nginx\n", ndir)
		}
		for _, m := range nlist {
			kind := "稳定版"
			if !m.Stable() {
				kind = "开发版"
			}
			fmt.Fprintf(cio.stdout, "%-28s nginx %-6s %-8s %8s  %s\n", m.ID, m.Series, m.Arch, human(m.Size), kind)
		}
		return 0
	case "download":
		if nginxFlag {
			return nginxDownload(c, cio, js, con, st, fail)
		}
		n, _ := strconv.Atoi(c.flags["java"])
		if n < 8 {
			return fail(errors.New("用 --java 指定 JDK 大版本，如 --java 17"))
		}
		arch := c.flags["arch"]
		if arch == "" {
			arch = "x64"
		}
		if arch != "x64" && arch != "aarch64" {
			return fail(errors.New("--arch 只能是 x64 或 aarch64"))
		}
		src := st.Runtime.Source
		if src == "" {
			src = jdk.DefaultSource
		}
		var rep engine.Reporter = js
		if js == nil {
			rep = con
		}
		rep.Step("下载 JDK %d（linux/%s，%s）", n, arch, src)
		m, err := jdk.Download(cio.ctx, src, n, arch, dir, func(done, total int64) { rep.Progress(fmt.Sprintf("JDK %d", n), done, total) })
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("已下载 %s（%s）到 %s", m.Version, human(m.Size), dir)
		}
		return ok0(js, m)
	case "import":
		if nginxFlag {
			file := c.flags["nginx"]
			if file == "" && len(c.pos) > 1 {
				file = c.pos[1]
			}
			if file == "" {
				return fail(errors.New("用法：ferry runtime import --nginx <nginx 程序，如 nginx-1.30.5-x86_64-linux> [--nginx-version 1.30.5] [--arch x86_64]"))
			}
			m, err := nginxrt.Import(file, st.NginxDir(), c.flags["nginx-version"], c.flags["arch"])
			if err != nil {
				return fail(err)
			}
			if con != nil {
				con.OK("已导入 nginx %s（%s）", m.Version, m.Arch)
			}
			return ok0(js, m)
		}
		if len(c.pos) < 2 {
			return fail(errors.New("用法：ferry runtime import <Linux 版 JDK 的 tar.gz>"))
		}
		m, err := jdk.Import(c.pos[1], dir)
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("已导入 %s（Java %d，%s）", m.ID, m.Java, m.Arch)
		}
		return ok0(js, m)
	case "remove":
		if len(c.pos) < 2 {
			return fail(errors.New("用法：ferry runtime remove <ID>"))
		}
		rm := jdk.Remove
		if strings.HasPrefix(c.pos[1], "nginx-") {
			rm = nginxrt.Remove
			dir = st.NginxDir()
		}
		if err := rm(dir, c.pos[1]); err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("已删除 %s（服务器上已安装的不受影响）", c.pos[1])
		}
		return ok0(js, map[string]any{"removed": c.pos[1]})
	}
	return fail(errors.New("runtime 只支持 list、download、import、remove、use"))
}

// nginxDownload ferry runtime download --nginx <1.30 | stable> [--arch x86_64|aarch64]
func nginxDownload(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	series := c.flags["nginx"]
	if series == "stable" || series == "latest" {
		series = ""
	}
	if series != "" && !nginxrt.ValidSeries(series) {
		return fail(errors.New("--nginx 写版本系列（如 1.30）或 stable（最新稳定版）"))
	}
	arch := c.flags["arch"]
	if arch == "" {
		arch = "x86_64"
	}
	if arch != "x86_64" && arch != "aarch64" {
		return fail(errors.New("--arch 只能是 x86_64 或 aarch64"))
	}
	src := st.Runtime.NginxSource
	if src == "" {
		src = nginxrt.DefaultSource
	}
	var rep engine.Reporter = js
	if js == nil {
		rep = con
	}
	what := series
	if what == "" {
		what = "最新稳定版"
	}
	rep.Step("下载 nginx %s（linux/%s，%s）", what, arch, src)
	dir := st.NginxDir()
	m, err := nginxrt.Download(cio.ctx, src, series, arch, dir, func(done, total int64) { rep.Progress("nginx", done, total) })
	if err != nil {
		return fail(err)
	}
	if con != nil {
		con.OK("已下载 nginx %s（%s）到 %s", m.Version, human(m.Size), dir)
	}
	return ok0(js, m)
}

// fixMovedKeys 数据目录搬走后，项目里指向旧位置 keys 下私钥（「配置免密登录」生成的）的 identityFile 改成新位置。
func fixMovedKeys(rep engine.Reporter, st settings.Settings, from string) {
	oldKeys := filepath.Join(from, "keys")
	newKeys := appdir.KeysDir()
	for _, p := range st.Desktop.Projects {
		f, err := config.Load(p.Path)
		if err != nil {
			continue
		}
		for name, t := range f.Targets {
			if t.IdentityFile == "" {
				continue
			}
			rel, err := filepath.Rel(oldKeys, f.LocalPath(t.IdentityFile))
			if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
				continue
			}
			np := filepath.ToSlash(filepath.Join(newKeys, rel))
			if err := config.SetTargetIdentity(f.Path, name, np); err != nil {
				rep.Warn("私钥已搬到 %s，但没能更新 %s 里 %s 的 identityFile：%v", np, f.Path, name, err)
			} else {
				rep.Info("已更新 %s：targets.%s.identityFile = %s（私钥随数据搬到了安装目录）", f.Path, name, np)
			}
		}
	}
}

// createCmd ferry create -c <路径> [--dry-run]：按向导填写的内容生成 ferry.yaml，先校验再写入，不覆盖已有文件。
func createCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	path := c.flags["config"]
	if path == "" {
		return fail(errors.New("用 -c 指定要生成的 ferry.yaml 路径"))
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if c.has("check") { // 向导第一步：只看这个位置是不是已经有文件
		return ok0(js, map[string]any{"path": filepath.ToSlash(path), "exists": fileExists(path)})
	}
	var spec scaffold.Spec
	if err := json.Unmarshal([]byte(cio.getenv("FERRY_CREATE_VALUE")), &spec); err != nil {
		return fail(fmt.Errorf("用环境变量 FERRY_CREATE_VALUE 提供向导的 JSON：%w", err))
	}
	content, err := scaffold.Generate(spec, path)
	if err != nil {
		return fail(err)
	}
	_, verr := config.LoadContent([]byte(content), path)
	out := map[string]any{"path": filepath.ToSlash(path), "content": content, "exists": fileExists(path)}
	if verr != nil {
		out["problems"] = strings.Split(verr.Error(), "\n")
	}
	if c.has("dry-run") {
		return ok0(js, out)
	}
	if verr != nil {
		return fail(fmt.Errorf("生成的配置有问题：%w", verr))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(fmt.Errorf("%s 已存在，不会覆盖：换一个位置，或直接添加这个文件", path))
		}
		return fail(err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(path)
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if con != nil {
		con.OK("已生成 %s", path)
	}
	return ok0(js, out)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// secretAccountCmd ferry secret list | delete --account <账户>；show 只供桌面版在 serve 中单独查看。
func secretAccountCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	if len(c.pos) != 1 {
		return fail(errors.New("用法：ferry secret list | delete --account <账户>；查看内容请在桌面版设置 → 凭据里点「查看」"))
	}
	switch c.pos[0] {
	case "show":
		// 内容只经本机 sidecar 的响应交给查看对话框，不输出到普通命令行或任务记录。
		if cio.pool == nil || js == nil {
			return fail(errors.New("请在桌面版设置 → 凭据里点「查看」"))
		}
		if c.flags["account"] == "" {
			return fail(errors.New("用 --account 指定要查看的凭据"))
		}
		value, found, err := secret.Get(c.flags["account"])
		if err != nil {
			return fail(err)
		}
		if !found {
			return fail(errors.New("这条凭据在系统凭据库中已不存在，可能已被删除"))
		}
		return ok0(js, map[string]string{"value": value})
	case "delete":
		if c.flags["account"] == "" {
			return fail(errors.New("用 --account 指定要删除的凭据"))
		}
		if err := secret.Delete(c.flags["account"]); err != nil {
			return fail(err)
		}
		cio.invalidate()
		if js != nil {
			js.Result(true, map[string]any{"stored": false}, nil)
		} else {
			con.OK("已删除 %s", c.flags["account"])
		}
		return 0
	case "list":
		if c.flags["account"] != "" {
			return fail(errors.New("secret list 不接受 --account；查看内容请在桌面版设置 → 凭据里点「查看」"))
		}
	default:
		return fail(errors.New("不支持的凭据操作"))
	}
	es, err := secret.List()
	if err != nil {
		return fail(err)
	}
	if js != nil {
		js.Result(true, es, nil)
		return 0
	}
	if len(es) == 0 {
		fmt.Println("没有保存的凭据")
	}
	for _, e := range es {
		fmt.Printf("%-10s  %-40s  %s\n", e.Kind, e.Label, e.SavedAt.Local().Format("2006-01-02 15:04"))
	}
	return 0
}

// versionInfo 版本信息（ferry version、桌面版的「关于」）。
func versionInfo() map[string]any {
	var agents []map[string]string
	for _, arch := range []string{"amd64", "arm64"} {
		a := map[string]string{"arch": arch, "sha256": "未内置"}
		if b, err := agentbin.Get(arch); err == nil {
			a["sha256"] = b.SHA256[:12]
			a["size"] = human(int64(len(b.Data)))
		}
		agents = append(agents, a)
	}
	return map[string]any{
		"version": Version, "proto": proto.Version, "go": runtime.Version(),
		"os": runtime.GOOS, "arch": runtime.GOARCH, "agents": agents,
	}
}
