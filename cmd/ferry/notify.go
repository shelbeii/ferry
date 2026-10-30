package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/notify"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/ui"
)

const notifyUsage = "用法：ferry notify [list] | save <ID> [--sign] | forget <ID> [--sign] | test [<ID>]"

// notifyCmd 部署通知（设置 → 通知）：
//
//	ferry notify [list]           各通知方式，有没有保存地址、加签密钥
//	ferry notify save <ID>        保存机器人地址（含令牌）；--sign 保存加签密钥。值从 FERRY_SECRET_VALUE 或终端输入
//	ferry notify forget <ID>      删除地址和加签密钥；--sign 只删加签密钥
//	ferry notify test [<ID>]      发一条测试消息。设置里「测试」还没保存的值用 FERRY_NOTIFY_TEST_KIND / _URL / _SIGN 传
func notifyCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	action := "list"
	if len(c.pos) > 0 {
		action = c.pos[0]
	}
	id := ""
	if len(c.pos) > 1 {
		id = c.pos[1]
	}
	_, sign := c.flags["sign"]
	account := func() string {
		if sign {
			return secret.NotifySignAccount(id)
		}
		return secret.NotifyURLAccount(id)
	}
	switch action {
	case "list":
		type row struct {
			settings.NotifyChannel
			URL  bool `json:"url"`
			Sign bool `json:"sign"`
		}
		rows := []row{}
		for _, ch := range st.Notify.Channels {
			_, u, err := secret.Get(secret.NotifyURLAccount(ch.ID))
			if err != nil {
				return fail(err)
			}
			_, s, _ := secret.Get(secret.NotifySignAccount(ch.ID))
			rows = append(rows, row{ch, u, s})
		}
		env, err := notify.EnvTargets(cio.getenv)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, map[string]any{"channels": rows, "env": len(env)})
		}
		if len(rows) == 0 && len(env) == 0 {
			fmt.Fprintln(cio.stdout, "还没有部署通知：在桌面版「设置 → 通知」里添加，或设置环境变量 FERRY_NOTIFY")
		}
		for _, r := range rows {
			state := "地址未保存"
			if r.URL {
				state = "已保存地址"
			}
			if r.Sign {
				state += "、加签密钥"
			}
			if r.Off {
				state += "（暂停）"
			}
			fmt.Fprintf(cio.stdout, "%-12s %-10s %s：%s\n", r.ID, settings.NotifyKinds[r.Kind], r.Name, state)
		}
		for _, t := range env {
			fmt.Fprintf(cio.stdout, "%-12s %-10s 环境变量 FERRY_NOTIFY\n", t.Channel.ID, settings.NotifyKinds[t.Channel.Kind])
		}
		return 0
	case "save":
		if id == "" {
			return fail(errors.New(notifyUsage))
		}
		v := strings.TrimSpace(cio.getenv("FERRY_SECRET_VALUE"))
		if v == "" && con != nil {
			v, _ = con.Secret(map[bool]string{true: "加签密钥：", false: "机器人地址（含令牌）："}[sign])
		}
		if v == "" {
			return fail(errors.New("没有输入：在终端中运行，或通过环境变量 FERRY_SECRET_VALUE 提供"))
		}
		if !sign && !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
			return fail(errors.New("地址要以 https:// 开头"))
		}
		if err := secret.Set(account(), v); err != nil {
			return fail(err)
		}
	case "forget":
		if id == "" {
			return fail(errors.New(notifyUsage))
		}
		if err := secret.Delete(account()); err != nil {
			return fail(err)
		}
		if !sign {
			if err := secret.Delete(secret.NotifySignAccount(id)); err != nil {
				return fail(err)
			}
		}
	case "test":
		t, err := notifyTestTarget(cio, st, id)
		if err != nil {
			return fail(err)
		}
		if err := engine.TestNotify(t); err != nil {
			return fail(err)
		}
		if js == nil {
			fmt.Fprintf(cio.stdout, "已发送测试消息到「%s」\n", t.Channel.Name)
		}
	default:
		return fail(errors.New(notifyUsage))
	}
	return ok0(js, map[string]any{"ok": true})
}

// notifyTestTarget 测试发给谁：环境变量里给了地址就用它（设置界面里还没保存的值），否则用保存的。
func notifyTestTarget(cio *cliIO, st settings.Settings, id string) (notify.Target, error) {
	var t notify.Target
	for _, ch := range st.Notify.Channels {
		if ch.ID == id {
			t.Channel = ch
		}
	}
	if kind := cio.getenv("FERRY_NOTIFY_TEST_KIND"); kind != "" {
		if _, ok := settings.NotifyKinds[kind]; !ok {
			return t, fmt.Errorf("不认识的方式：%s", kind)
		}
		t.Channel.Kind = kind
		if t.Channel.Name == "" {
			t.Channel.Name = settings.NotifyKinds[kind]
		}
	}
	if t.Channel.Kind == "" {
		if id == "" {
			return t, errors.New(notifyUsage)
		}
		return t, fmt.Errorf("没有通知方式 %s", id)
	}
	t.URL = strings.TrimSpace(cio.getenv("FERRY_NOTIFY_TEST_URL"))
	t.Sign = strings.TrimSpace(cio.getenv("FERRY_NOTIFY_TEST_SIGN"))
	if id != "" {
		if t.URL == "" {
			v, _, err := secret.Get(secret.NotifyURLAccount(id))
			if err != nil {
				return t, err
			}
			t.URL = v
		}
		if t.Sign == "" && cio.getenv("FERRY_NOTIFY_TEST_NO_SIGN") == "" {
			t.Sign, _, _ = secret.Get(secret.NotifySignAccount(id))
		}
	}
	if t.URL == "" {
		return t, errors.New("还没有填机器人地址")
	}
	return t, nil
}
