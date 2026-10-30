package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

var whoUsage = errors.New(`用法：
  ferry who <目标>                    服务器上连着的 SSH：登录用户、来源 IP、连上的时间、在干什么
  ferry who kick <目标> <PID>…        断开这些连接（PID 是 ferry who 列出的），里面正在运行的命令会被结束`)

// whoCmd ferry who <目标> | kick。
func whoCmd(c *cli, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(whoUsage)
	}
	if c.pos[0] == "kick" {
		if len(c.pos) < 3 {
			return fail(whoUsage)
		}
		var pids []int
		for _, a := range c.pos[2:] {
			for _, s := range strings.Split(a, ",") {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil || n <= 1 {
					return fail(fmt.Errorf("PID 无效：%s", s))
				}
				pids = append(pids, n)
			}
		}
		rep, err := e.SSHKick(c.pos[1], pids)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, rep)
		}
		con.OK("已断开")
		return 0
	}
	rep, err := e.SSHSessions(c.pos[0])
	if err != nil {
		return fail(err)
	}
	if js != nil {
		return ok0(js, rep)
	}
	kinds := map[string]string{"terminal": "终端", "sftp": "传文件", "ferry": "Ferry", "exec": "执行命令", "idle": "空闲"}
	t := &ui.Table{Header: []string{"PID", "用户", "来源", "连上", "类型", "在运行"}}
	for _, s := range rep.Sessions {
		from := s.From
		if from == "" {
			from = "—"
		}
		kind := kinds[s.Kind]
		if len(s.TTYs) > 0 {
			kind += " " + strings.Join(s.TTYs, ",")
		}
		if s.Self {
			kind += "（本机 Ferry）"
		}
		t.Rows = append(t.Rows, []string{strconv.Itoa(s.PID), s.User, from, time.Unix(s.Since, 0).Format("01-02 15:04"), kind, strings.Join(s.Cmds, "；")})
	}
	t.Write(os.Stdout)
	if rep.Pending > 0 {
		con.Warn("另外有 %d 个连接正在登录（还没通过验证）", rep.Pending)
	}
	for _, n := range rep.Notes {
		con.Warn("%s", n)
	}
	return 0
}
