package main

import (
	"errors"
	"fmt"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

func appSecretsCmd(e *engine.Engine, c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	usage := errors.New("用法：ferry secrets <目标> [list [应用…] | set <应用> <变量> [--all-targets] | unset <应用> <变量> [--all-targets] | push [应用…] [--no-restart] | keygen | team [status|init|add <公钥>|remove <公钥>]]")
	if len(c.pos) == 0 {
		return fail(usage)
	}
	target, action := c.pos[0], "list"
	var args []string
	if len(c.pos) > 1 {
		action, args = c.pos[1], c.pos[2:]
	}
	switch action {
	case "keygen":
		if len(args) != 0 {
			return fail(usage)
		}
		if _, err := e.File.Target(target); err != nil {
			return fail(err)
		}
		key, err := engine.EnsureAppAgeIdentity()
		if err != nil {
			return fail(err)
		}
		if js == nil {
			con.Info("%s", key)
		}
		return ok0(js, map[string]string{"publicKey": key})
	case "team":
		return teamSecretsCmd(e, target, args, js, con, fail)
	case "list":
		rows, err := e.AppSecrets(target, args)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, rows)
		}
		if len(rows) == 0 {
			con.Info("没有声明应用密钥")
		}
		for _, row := range rows {
			state := map[string]string{"env": "环境变量", "target": "当前目标", "shared": "所有目标",
				"team": "团队文件", "team-unavailable": "团队文件不可用",
				"unavailable": "凭据库不可用", "invalid": "值格式无效", "": "缺少"}[row.Source]
			if row.Optional {
				state += "（可选）"
			}
			con.Info("%s / %s：%s %s", row.App, row.Name, state, row.Doc)
		}
		return 0
	case "set", "unset":
		if len(args) != 2 {
			return fail(usage)
		}
		value := ""
		if action == "set" {
			value = cio.getenv("FERRY_SECRET_VALUE")
			if value == "" && js == nil {
				value, _ = con.Secret(fmt.Sprintf("%s / %s 的值：", args[0], args[1]))
			}
			if value == "" {
				return fail(errors.New("请在交互终端输入应用密钥，或通过 FERRY_SECRET_VALUE 提供"))
			}
		}
		if err := e.SaveAppSecret(target, args[0], args[1], value, c.has("all-targets")); err != nil {
			return fail(err)
		}
		if js == nil {
			con.OK("%s / %s 已%s", args[0], args[1], map[bool]string{true: "保存", false: "删除"}[value != ""])
		}
		return ok0(js, map[string]bool{"stored": value != ""})
	case "push":
		out, err := e.PushAppSecrets(target, args, !c.has("no-restart"))
		if err != nil && len(out) == 0 {
			return fail(err)
		}
		if js != nil {
			js.Result(err == nil, out, err)
		}
		if err != nil {
			return 1
		}
		return 0
	}
	return fail(usage)
}

func teamSecretsCmd(e *engine.Engine, target string, args []string, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	usage := errors.New("用法：ferry secrets <目标> team [status | init | add <公钥> | remove <公钥>]")
	if len(args) > 0 {
		switch args[0] {
		case "status":
			if len(args) != 1 {
				return fail(usage)
			}
		case "init":
			if len(args) != 1 {
				return fail(usage)
			}
			if err := e.InitTeamSecrets(target); err != nil {
				return fail(err)
			}
		case "add", "remove":
			if len(args) != 2 {
				return fail(usage)
			}
			if err := e.EditTeamRecipient(target, args[1], args[0] == "add"); err != nil {
				return fail(err)
			}
		default:
			return fail(usage)
		}
	}
	status, err := e.TeamSecrets(target)
	if err != nil {
		return fail(err)
	}
	if js == nil {
		if status.Path == "" {
			con.Info("此目标使用本机凭据库")
			return 0
		}
		con.Info("团队密钥文件：%s", status.Path)
		if status.PublicKey != "" {
			con.Info("本机公钥：%s", status.PublicKey)
		}
		for _, key := range status.Recipients {
			con.Info("收件人：%s", key)
		}
		if status.Problem != "" {
			con.Warn("%s", status.Problem)
		}
	}
	return ok0(js, status)
}
