package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/rundist"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/swpkg"
	"github.com/captain/ferry/internal/swtpl"
	"github.com/captain/ferry/internal/ui"
)

var softwareUsage = errors.New(`用法：
  ferry software <目标>                              查看服务器上已有的软件
  ferry software jdk list <目标>                     查看服务器上 Ferry 管理的 JDK
  ferry software jdk install <目标> --java N | --version V [--java-dir 目录]    预装 JDK，大版本或完整版本二选一
  ferry software jdk remove <目标> <Java 绝对路径> --yes    卸载该路径对应的 JDK
  ferry software runtime list <目标>                   查看全部 Ferry 管理的运行环境
  ferry software runtime install <目标> node|python|dotnet --channel 系列 | --version V
      --dir 目录  指定安装父目录  --package 本机.tar.gz  使用离线安装包（需指定完整版本）
      --component aspnetcore|runtime|sdk  .NET 组件（默认 aspnetcore）
  ferry software runtime remove <目标> <程序绝对路径> --yes
  ferry software status <目标> <任务ID>                查询服务器上的安装任务
  ferry software plan <目标> <模板> [安装选项]         预检并显示安装计划
  ferry software install <目标> <模板> [选项]          按模板安装（模板见 ferry software templates）
      --name 名称  --version V  --user 运行用户  --variant N（第几种装法，默认按服务器自动选）
      --in 目录  装到这个目录下的「名称-版本」文件夹（默认 /opt/ferry-software）；--dir 目录  直接指定安装目录
      --java N  --java-dir 目录（JDK 装到这里，默认装到 Ferry 的公共目录）
      --package 文件名（本机库里的安装包）  --set 参数=值,参数=值
      --conf 名称=值  附加配置：模板参数没有的设置（如 --conf innodb_buffer_pool_size=1G），可以写几次；
                     模板有几处附加配置时写成 --conf 附加配置ID:名称=值。能写哪些见 ferry software template show <ID> 的 extras；
                     也可以用环境变量 FERRY_SOFTWARE_EXTRA 给 JSON：{"附加配置ID": {"名称": "值"}} 或 {"名称": "值"}
      --fix-parents  运行用户进不了安装目录的上级目录时，给它加上进入的权限
      --allow-changes  从软件源装包会顺带升级、删除已装的包时也继续（不加时停下来说明会改动哪些）
      --network auto|local|server  服务器要下载东西（从软件源装包等）时走谁的网络：local 经这台电脑，
                     server 用服务器自己的；默认 auto（服务器连不上软件源的经这台电脑）
      常用参数的简写：--port N  --console-port N  --public  --memory 512m  --lower-case-tables
      密码从环境变量 FERRY_SOFTWARE_PASSWORD 读，没有就随机生成；
      模板写 - 时从环境变量 FERRY_SOFTWARE_TEMPLATE 读模板内容（自定义安装）
  ferry software start|stop|restart <目标> <名称>
  ferry software start|stop|restart|logs <目标> --unit <服务> | --container <容器>   不是 Ferry 装的软件
  ferry software remove <目标> <名称> [--purge]        --purge 连安装目录（数据）一起删除；不加时保留数据，以后能恢复
  ferry software restore <目标> <名称>               恢复卸载时保留了数据的软件
  ferry software upgrade <目标> <名称> [--version V] [--package 文件名] [--major]
                     升级用安装包装的软件（不填版本 = 最新）：换掉程序，配置、数据带过去；新版本起不来时换回旧的。
                     --major：跨大版本升级（可能不兼容，先备份）
  ferry software backup <目标> <名称> [--database 库名]  给 Ferry 装的数据库设置数据备份（写好密码文件，在 ferry.yaml 的 backups 里加上一项；不填库名 = 全部库）
  ferry software logs <目标> <名称> [-n 200] [--main-log]
  ferry software password <目标> <名称>              安装时保存的密码
  ferry software config list <目标> [名称] [--unit 服务 | --container 容器 | --pid PID]
  ferry software config read|save|restore|apply <目标> [名称] --id 配置ID [--unit 服务 | --container 容器 | --pid PID]
      save、restore 从 FERRY_FILE_VALUE 读取 JSON {revision, content}，restore 只需 revision
  ferry software config associate <目标> [名称] [--unit 服务 | --container 容器 | --pid PID]
      从 FERRY_FILE_VALUE 读取 JSON {path}，关联此软件目录或持久化挂载内的配置
  ferry software templates                           全部模板（内置 + 自己的）
  ferry software template show <ID> | delete <ID> | import <文件> | save | check   save、check 从 FERRY_SOFTWARE_TEMPLATE 读
  ferry software latest <模板> [--variant N]          查最新版本
  ferry software lib [list] | import <文件> [rpm|deb|auto] | inspect <文件名> [剥离层数] | remove <文件名> | download <模板> [--version V]`)

// 不需要 ferry.yaml 的子命令
var softwareLocal = map[string]bool{"templates": true, "template": true, "latest": true, "lib": true}

// softwareCmd 需要目标的：ferry software <目标> | install | start | stop | restart | remove | logs | password。
func softwareCmd(c *cli, cio *cliIO, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	// 独立连接也能管理软件；该操作明确需要 agent 和 root/sudo。
	for _, t := range e.File.Targets {
		if t.Plain {
			t.Plain = false
			t.Sudo = t.User != "root" && t.SudoAuth != "disabled"
			if t.Root == "" {
				t.Root = "/opt/ferry"
			}
			if t.FerryDir == "" {
				t.FerryDir = "/usr/local/lib/ferry"
			}
		}
	}
	ok := func(data any) int { return ok0(js, data) }
	if len(c.pos) == 0 {
		return fail(softwareUsage)
	}
	if c.pos[0] == "config" {
		return softwareConfigCmd(c, cio, e, js, fail)
	}
	sub, args := c.pos[0], c.pos[1:]
	need := func(n int) error {
		if len(args) < n {
			return softwareUsage
		}
		return nil
	}
	// 不是 Ferry 装的软件：按系统服务或容器
	if unit, ctr := c.flags["unit"], c.flags["container"]; unit != "" || ctr != "" {
		if len(args) < 1 || (sub != "start" && sub != "stop" && sub != "restart" && sub != "logs") {
			return fail(softwareUsage)
		}
		n := 300
		if v := c.flags["n"]; v != "" {
			n, _ = strconv.Atoi(v)
		}
		text, from, err := e.SoftwareUnit(args[0], unit, ctr, sub, n)
		if err != nil {
			return fail(err)
		}
		if sub == "logs" {
			if js != nil {
				return ok(map[string]any{"from": from, "lines": text.Lines, "truncated": text.Truncated})
			}
			con.Info("%s", from)
			for _, l := range text.Lines {
				fmt.Fprintln(cio.stdout, l)
			}
			return 0
		}
		if con != nil {
			con.OK("%s 完成", sub)
		}
		return ok(nil)
	}
	switch sub {
	case "jdk":
		if len(args) > 0 {
			return softwareJDKCmd(c, args, e, js, con, fail)
		}
	case "runtime":
		if len(args) > 0 {
			return softwareRuntimeCmd(c, args, e, js, con, fail)
		}
	case "status":
		if err := need(2); err != nil {
			return fail(err)
		}
		task, err := e.SoftwareTask(args[0], args[1])
		if err != nil {
			return fail(err)
		}
		if con != nil {
			if task == nil {
				con.Info("服务器上没有该安装任务的记录")
			} else {
				con.Info("%s：%s · %s", task.Name, task.Status, task.Phase)
			}
		}
		return ok(task)
	case "install", "plan":
		if err := need(2); err != nil {
			return fail(err)
		}
		o, err := installOptions(c, cio, args[1])
		if err != nil {
			return fail(err)
		}
		o.Preview = sub == "plan"
		res, err := e.SoftwareInstall(args[0], o)
		if err != nil {
			return fail(err)
		}
		if res.Plan != nil {
			if con != nil {
				p := res.Plan
				route := map[string]string{"none": "无需远端下载", "server": "服务器直连", "local": "经本机代理", "offline": "离线安装包"}[p.Network]
				con.OK("%s %s · %s · %s · %s", p.Template, p.Version, p.Variant, p.Dir, route)
				for _, x := range p.Extras {
					where := x.File
					if where == "" {
						where = map[string]string{"env": "环境变量", "args": "启动参数"}[x.Format]
					}
					con.Info("附加配置「%s」（%s）：", x.Label, where)
					for _, it := range x.Items {
						k := it.Key
						if it.Section != "" {
							k = "[" + it.Section + "] " + k
						}
						con.Info("  %s = %s", k, it.Value)
					}
				}
			}
			return ok(res.Plan)
		}
		if js != nil {
			return ok(res)
		}
		con.OK("%s 上已安装 %s（%s，系统服务 %s）", res.Target, strings.TrimSpace(res.Name+" "+res.Version), res.Dir, res.Unit)
		if res.Account != "" {
			con.Info("账号：%s", res.Account)
		}
		if res.Password != "" {
			pw := "密码：" + res.Password
			if res.Saved {
				pw += "（已存进系统凭据库，以后用 ferry software password 查看）"
			}
			con.Info("%s", pw)
		}
		for _, n := range res.Notes {
			con.Info("%s", n)
		}
		return 0

	case "start", "stop", "restart":
		if err := need(2); err != nil {
			return fail(err)
		}
		if err := e.SoftwareControl(args[0], args[1], sub); err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("%s 完成", sub)
		}
		return ok(nil)

	case "remove":
		if err := need(2); err != nil {
			return fail(err)
		}
		if err := e.SoftwareRemove(args[0], args[1], c.has("purge")); err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("已卸载 %s", args[1])
		}
		return ok(nil)

	case "restore":
		if err := need(2); err != nil {
			return fail(err)
		}
		in, err := e.SoftwareRestore(args[0], args[1])
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("已恢复 %s（%s）", in.Name, in.Dir)
		}
		return ok(in)

	case "upgrade":
		if err := need(2); err != nil {
			return fail(err)
		}
		in, err := e.SoftwareUpgrade(args[0], args[1], engine.UpgradeOptions{Version: c.flags["version"], PackageFile: c.flags["package"], Major: c.has("major")})
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("%s 已升级到 %s", in.Name, in.Version)
		}
		return ok(in)

	case "backup":
		if err := need(2); err != nil {
			return fail(err)
		}
		item, err := e.SoftwareBackup(args[0], args[1], c.flags["database"])
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("已加上数据备份 %s：用 ferry backup %s on 打开", item, args[0])
		}
		return ok(map[string]string{"backup": item})

	case "logs":
		if err := need(2); err != nil {
			return fail(err)
		}
		n := 200
		if v := c.flags["n"]; v != "" {
			n, _ = strconv.Atoi(v)
		}
		text, from, err := e.SoftwareLogs(args[0], args[1], n, c.has("main-log"))
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(map[string]any{"from": from, "lines": text.Lines, "truncated": text.Truncated})
		}
		con.Info("%s", from)
		for _, l := range text.Lines {
			fmt.Fprintln(cio.stdout, l)
		}
		return 0

	case "password":
		if err := need(2); err != nil {
			return fail(err)
		}
		pw, err := e.SoftwarePassword(args[0], args[1])
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(map[string]any{"password": pw})
		}
		fmt.Fprintln(cio.stdout, pw)
		return 0
	}
	// 其余：ferry software <目标>
	rep, err := e.Software(sub)
	if err != nil {
		return fail(err)
	}
	if js != nil {
		return ok(rep)
	}
	printSoftware(con, rep)
	return 0
}

func softwareRuntimeCmd(c *cli, args []string, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	if len(args) == 2 && args[0] == "list" {
		rows, err := e.ServerRuntimes(args[1])
		if err != nil {
			return fail(err)
		}
		if con != nil {
			if len(rows) == 0 {
				con.Info("%s 还没有 Ferry 管理的运行环境", args[1])
			}
			for _, row := range rows {
				con.Info("%s · %s", row.ID, row.Path)
			}
		}
		return ok0(js, rows)
	}
	if len(args) != 3 {
		return fail(softwareUsage)
	}
	target := args[1]
	switch args[0] {
	case "install":
		choice := rundist.Selection{Kind: args[2], Component: c.flags["component"], Channel: c.flags["channel"], Version: c.flags["version"]}
		row, err := e.InstallServerRuntime(target, choice, c.flags["dir"], c.flags["package"])
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("%s 上的 %s 已就绪（%s）", target, row.ID, row.Path)
		}
		return ok0(js, row)
	case "remove":
		if !c.has("yes") {
			return fail(errors.New("请确认程序路径，添加 --yes 确认卸载"))
		}
		row, err := e.RemoveServerRuntime(target, args[2])
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("%s 上已卸载 %s（%s）", target, row.ID, row.Path)
		}
		return ok0(js, row)
	}
	return fail(softwareUsage)
}

func softwareJDKCmd(c *cli, args []string, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	if len(args) == 3 && args[0] == "remove" {
		if !c.has("yes") {
			return fail(errors.New("请确认 Java 路径，添加 --yes 确认卸载"))
		}
		r, err := e.RemoveServerJDK(args[1], args[2])
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("%s 上已卸载 JDK %s（%s）", args[1], r.Version, r.Path)
		}
		return ok0(js, r)
	}
	if len(args) != 2 {
		return fail(softwareUsage)
	}
	action, target := args[0], args[1]
	switch action {
	case "list":
		list, err := e.JDKs(target)
		if err != nil {
			return fail(err)
		}
		if con != nil {
			if len(list) == 0 {
				con.Info("%s 还没有 Ferry 管理的 JDK", target)
			}
			for _, r := range list {
				shared := ""
				if r.Shared {
					shared = fmt.Sprintf("（共享 java-%d）", r.Java)
				}
				con.Info("JDK %s · %s · %s%s", r.Version, r.Arch, r.Path, shared)
			}
		}
		return ok0(js, list)
	case "install":
		java := 0
		if raw, exists := c.flags["java"]; exists {
			var err error
			java, err = strconv.Atoi(raw)
			if err != nil || java < 8 || java > 99 {
				return fail(errors.New("--java 要填 8–99 之间的 JDK 大版本"))
			}
		}
		r, err := e.InstallServerJDK(target, java, c.flags["version"], c.flags["java-dir"])
		if err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("%s 上已安装 JDK %s（%s）", target, r.Version, r.Path)
		}
		return ok0(js, r)
	}
	return fail(softwareUsage)
}

// installOptions 命令行参数 → 安装选项。
func installOptions(c *cli, cio *cliIO, template string) (engine.SoftwareOptions, error) {
	o := engine.SoftwareOptions{Template: template, Variant: -1, Name: c.flags["name"], Version: c.flags["version"], Dir: c.flags["dir"], In: c.flags["in"],
		User: c.flags["user"], PackageFile: c.flags["package"], JavaDir: c.flags["java-dir"], FixParents: c.has("fix-parents"), AllowChanges: c.has("allow-changes"), Network: c.flags["network"], Params: map[string]string{}}
	if template == "-" {
		o.TemplateYAML = []byte(cio.getenv("FERRY_SOFTWARE_TEMPLATE"))
		if len(o.TemplateYAML) == 0 {
			return o, errors.New("模板写 - 时要用环境变量 FERRY_SOFTWARE_TEMPLATE 给出模板内容")
		}
	}
	for flag, dst := range map[string]*int{"java": &o.Java, "variant": &o.Variant} {
		if v := c.flags[flag]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return o, fmt.Errorf("--%s 要写数字：%q", flag, v)
			}
			*dst = n
		}
	}
	if c.flags["variant"] != "" {
		o.Variant-- // 命令行从 1 数
	}
	if v := cio.getenv("FERRY_SOFTWARE_PARAMS"); v != "" {
		if err := json.Unmarshal([]byte(v), &o.Params); err != nil {
			return o, fmt.Errorf("FERRY_SOFTWARE_PARAMS 不是 JSON 对象：%v", err)
		}
	}
	for flag, key := range map[string]string{"port": "port", "console-port": "console", "memory": "memory"} {
		if v := c.flags[flag]; v != "" {
			o.Params[key] = v
		}
	}
	for flag, key := range map[string]string{"public": "public", "lower-case-tables": "lowerCase"} {
		if c.has(flag) {
			o.Params[key] = "true"
		}
	}
	if v := c.flags["set"]; v != "" {
		for _, kv := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(kv, "=")
			if !ok {
				return o, fmt.Errorf("--set 要写成 参数=值：%q", kv)
			}
			o.Params[strings.TrimSpace(k)] = val
		}
	}
	if pw := cio.getenv("FERRY_SOFTWARE_PASSWORD"); pw != "" {
		o.Params["password"] = pw
	}
	extra, err := installExtras(cio.getenv("FERRY_SOFTWARE_EXTRA"), c.flags["conf"])
	if err != nil {
		return o, err
	}
	o.Extra = extra
	return o, nil
}

// installExtras 附加配置：环境变量 FERRY_SOFTWARE_EXTRA 的 JSON（{"ID": {"名称": "值"}}，或只有一处时 {"名称": "值"}），
// 加上命令行的 --conf [ID:]名称=值（一行一项）。
func installExtras(js, lines string) (map[string]map[string]string, error) {
	out, err := engine.ParseExtras([]byte(js))
	if err != nil {
		return nil, fmt.Errorf("FERRY_SOFTWARE_EXTRA：%w", err)
	}
	if out == nil {
		out = map[string]map[string]string{}
	}
	put := func(id, k, v string) error {
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		if _, dup := out[id][k]; dup {
			return fmt.Errorf("附加配置 %s 写了两次", k)
		}
		out[id][k] = v
		return nil
	}
	for _, l := range strings.Split(lines, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return nil, fmt.Errorf("--conf 要写成 名称=值：%q", l)
		}
		id := ""
		if i, key, found := strings.Cut(k, ":"); found {
			id, k = strings.TrimSpace(i), key
		}
		if err := put(id, strings.TrimSpace(k), v); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// softwareLocalCmd 不需要 ferry.yaml 的：templates、template、latest、lib。
func softwareLocalCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	ok := func(data any) int { return ok0(js, data) }
	tdir, lib := st.TemplateDir(), st.SoftwareDir()
	src := swpkg.Sources{GitHubMirror: st.Runtime.GitHubMirror, MySQL: st.Runtime.MySQLSource}
	sub, args := c.pos[0], c.pos[1:]
	switch sub {
	case "templates":
		list, errs := swtpl.All(tdir)
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		if js != nil {
			return ok(map[string]any{"templates": list, "errors": msgs, "dir": tdir, "library": swpkg.List(lib)})
		}
		for _, t := range list {
			from := "内置"
			if !t.Builtin {
				from = "自己的"
			}
			fmt.Fprintf(cio.stdout, "%-12s %-18s %-8s %-6s %s\n", t.ID, t.Name, t.Category, from, t.Desc)
		}
		for _, m := range msgs {
			con.Warn("%s", m)
		}
		return 0

	case "template":
		if len(args) == 0 {
			return fail(softwareUsage)
		}
		switch args[0] {
		case "show":
			if len(args) < 2 {
				return fail(softwareUsage)
			}
			t, err := swtpl.Find(tdir, args[1])
			if err != nil {
				return fail(err)
			}
			var b []byte
			if t.Builtin {
				b, err = swtpl.BuiltinSource(t.ID)
			} else {
				b, err = os.ReadFile(t.File)
			}
			if err != nil {
				return fail(err)
			}
			if js != nil {
				return ok(map[string]any{"id": t.ID, "builtin": t.Builtin, "content": string(b)})
			}
			cio.stdout.Write(b)
			return 0
		case "save", "check":
			content := []byte(cio.getenv("FERRY_SOFTWARE_TEMPLATE"))
			if len(content) == 0 {
				return fail(errors.New("用环境变量 FERRY_SOFTWARE_TEMPLATE 给出模板内容"))
			}
			var t *swtpl.Template
			var err error
			if args[0] == "save" {
				t, err = swtpl.Save(tdir, content)
			} else {
				t, err = swtpl.Parse(content)
			}
			if err != nil {
				return fail(err)
			}
			if con != nil {
				con.OK("模板 %s（%s）%s", t.ID, t.Name, map[string]string{"save": "已保存", "check": "没有问题"}[args[0]])
			}
			return ok(t)
		case "import":
			if len(args) < 2 {
				return fail(softwareUsage)
			}
			b, err := os.ReadFile(args[1])
			if err != nil {
				return fail(err)
			}
			t, err := swtpl.Save(tdir, b)
			if err != nil {
				return fail(err)
			}
			if con != nil {
				con.OK("已导入模板 %s（%s）", t.ID, t.Name)
			}
			return ok(t)
		case "delete":
			if len(args) < 2 {
				return fail(softwareUsage)
			}
			if err := swtpl.Delete(tdir, args[1]); err != nil {
				return fail(err)
			}
			return ok(nil)
		}
		return fail(softwareUsage)

	case "latest":
		if len(args) < 1 {
			return fail(softwareUsage)
		}
		e := &engine.Engine{Settings: st, Ctx: cio.ctx}
		variant := -1
		if v := c.flags["variant"]; v != "" {
			n, _ := strconv.Atoi(v)
			variant = n - 1
		}
		ctx, cancel := context.WithTimeout(cio.ctx, 30*time.Second)
		defer cancel()
		e.Ctx = ctx
		v, err := e.SoftwareLatest(args[0], variant)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok(map[string]any{"version": v})
		}
		fmt.Fprintln(cio.stdout, v)
		return 0

	case "lib":
		sub := "list"
		if len(args) > 0 {
			sub = args[0]
		}
		tpls, _ := swtpl.All(tdir)
		switch sub {
		case "list":
			list := swpkg.List(lib)
			if js != nil {
				return ok(list)
			}
			if len(list) == 0 {
				con.Info("本机还没有安装包（%s）", lib)
			}
			for _, m := range list {
				fmt.Fprintf(cio.stdout, "%-56s %-12s %-10s %-20s %10s\n", m.File, m.Template, m.Version, m.Desc, human(m.Size))
			}
			return 0
		case "import":
			if len(args) < 2 || len(args) > 3 {
				return fail(errors.New("用法：ferry software lib import <安装包文件> [rpm|deb|auto]"))
			}
			if len(args) == 3 {
				if err := swpkg.CheckSystemBundle(cio.ctx, args[1], args[2]); err != nil {
					return fail(err)
				}
			}
			m, err := swpkg.Import(args[1], lib, tpls)
			if err != nil {
				return fail(err)
			}
			if con != nil {
				if m.Bundle != "" {
					con.OK("已导入 %s 离线系统包 %s", strings.ToUpper(m.Bundle), m.File)
				} else if m.Template != "" {
					con.OK("已导入 %s %s", m.Template, m.Version)
				} else {
					con.OK("已导入 %s（没认出是哪个模板的官方安装包：自定义安装时可以选它）", m.File)
				}
			}
			return ok(m)
		case "remove":
			if len(args) < 2 {
				return fail(errors.New("用法：ferry software lib remove <文件名>"))
			}
			if err := swpkg.Remove(lib, args[1]); err != nil {
				return fail(err)
			}
			return ok(nil)
		case "inspect":
			if len(args) < 2 || len(args) > 3 {
				return fail(errors.New("用法：ferry software lib inspect <文件名> [剥离层数]"))
			}
			m := swpkg.FindFile(lib, args[1])
			if m == nil {
				return fail(fmt.Errorf("本机库里没有 %s", args[1]))
			}
			strip := 0
			if len(args) == 3 {
				var err error
				strip, err = strconv.Atoi(args[2])
				if err != nil || strip < 0 || strip > 64 {
					return fail(errors.New("剥离层数必须是 0 到 64"))
				}
			}
			preview, err := swpkg.InspectArchive(cio.ctx, filepath.Join(lib, m.File), swtpl.FormatOf("", m.File), strip)
			if err != nil {
				return fail(err)
			}
			if con != nil {
				con.Info("%d 个文件，离线包类型：%s", preview.Entries, preview.Bundle)
				for _, file := range preview.Files {
					con.Info("%s", file)
				}
			}
			return ok(preview)
		case "download":
			if len(args) < 2 {
				return fail(errors.New("用法：ferry software lib download <模板> [--version V] [--variant N]"))
			}
			t, err := swtpl.Find(tdir, args[1])
			if err != nil {
				return fail(err)
			}
			var p *swtpl.Package
			for i, v := range t.Variants {
				if (c.flags["variant"] == "" || strconv.Itoa(i+1) == c.flags["variant"]) && v.Package != nil && v.Package.URL != "" {
					p = v.Package
					break
				}
			}
			if p == nil {
				return fail(fmt.Errorf("%s 没有可以下载的安装包", t.Name))
			}
			ver := c.flags["version"]
			if ver == "" {
				if ver, err = swpkg.Latest(cio.ctx, src, p); err != nil {
					return fail(err)
				}
			}
			prog := func(done, total int64) {
				if con != nil {
					con.Progress(t.Name, done, total)
				} else {
					js.Progress(t.Name, done, total)
				}
			}
			m, err := swpkg.Download(cio.ctx, src, t.ID, p, ver, lib, prog)
			if err != nil {
				return fail(err)
			}
			if con != nil {
				con.OK("已下载 %s（%s）", m.File, human(m.Size))
			}
			return ok(m)
		}
		return fail(fmt.Errorf("未知的 software lib 命令 %q", sub))
	}
	return fail(softwareUsage)
}
