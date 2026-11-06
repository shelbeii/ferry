package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

var forwardUsage = errors.New(`用法：
  ferry forward <目标> <端口> [--host 127.0.0.1] [--container 名字]
                                  这台电脑上开一个端口（只监听 127.0.0.1），经 SSH 转到服务器那边的地址；
                                  --container：端口只在容器里、没映射到主机时，连容器的 IP。
                                  命令行下一直转发到按 Ctrl+C；桌面版里空闲 30 分钟后自动关掉
  ferry forward list              开着的转发（桌面版）
  ferry forward close [<本机端口>]  关掉转发（不写端口 = 全部）
  ferry forward probe <目标> <端口>…  从这台电脑直接连服务器的这些端口，看能不能连上
  ferry forward reach <域名> <端口>  这台电脑能不能解析这个域名、连上端口
  ferry forward <目标> <端口> --vhost <域名> [--tls]   按域名转发网站（nginx 一个端口上有几个网站时）：请求里的 Host 换成域名
  ferry forward accounts | save-account | forget-account <目标>   打开网页时自动登录用的账号（桌面版用；参数在 FERRY_WEB_REQ）`)

var reVHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// forwardCmd ferry forward …（「软件」页「打开网页」经 SSH 转发用）。
func forwardCmd(c *cli, cio *cliIO, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	ok := func(data any) int { return ok0(js, data) }
	if len(c.pos) == 0 {
		return fail(forwardUsage)
	}
	switch c.pos[0] {
	case "list":
		return ok(engine.Forwards())
	case "close":
		local := 0
		if len(c.pos) > 1 {
			n, err := strconv.Atoi(c.pos[1])
			if err != nil {
				return fail(forwardUsage)
			}
			local = n
		}
		n := engine.CloseForwards(local)
		if js == nil {
			con.OK("关掉了 %d 个转发", n)
		}
		return ok(map[string]int{"closed": n})
	case "accounts", "save-account", "forget-account":
		// 网页登录用的账号（带密码）：只给桌面版
		if !cio.served {
			return fail(errors.New("ferry forward " + c.pos[0] + " 只能由 Ferry 桌面版调用"))
		}
		if len(c.pos) < 2 {
			return fail(forwardUsage)
		}
		var req struct {
			engine.WebQuery
			User string `json:"user"`
		}
		if err := json.Unmarshal([]byte(cio.getenv("FERRY_WEB_REQ")), &req); err != nil {
			return fail(fmt.Errorf("FERRY_WEB_REQ 格式不对：%w", err))
		}
		switch c.pos[0] {
		case "accounts":
			list, err := e.WebAccounts(c.pos[1], req.WebQuery)
			if err != nil {
				return fail(err)
			}
			return ok(list)
		case "save-account":
			if strings.TrimSpace(req.User) == "" {
				return fail(errors.New("要填账号"))
			}
			if err := e.SaveWebAccount(c.pos[1], req.WebQuery, strings.TrimSpace(req.User), cio.getenv("FERRY_WEB_PASSWORD")); err != nil {
				return fail(err)
			}
		default:
			if err := e.ForgetWebAccount(c.pos[1], req.WebQuery, req.User); err != nil {
				return fail(err)
			}
		}
		return ok(map[string]any{})
	case "reach":
		// 用域名直接打开网站前：这台电脑能不能解析这个域名、连上端口
		if len(c.pos) < 3 {
			return fail(forwardUsage)
		}
		port, err := strconv.Atoi(c.pos[2])
		if err != nil {
			return fail(forwardUsage)
		}
		return ok(engine.Reach(c.pos[1], port, 4*time.Second))
	case "probe":
		if len(c.pos) < 3 {
			return fail(forwardUsage)
		}
		var ports []int
		for _, a := range c.pos[2:] {
			for _, s := range strings.Split(a, ",") {
				p, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil || p <= 0 || p > 65535 {
					return fail(errors.New("端口不对：" + s))
				}
				ports = append(ports, p)
			}
		}
		res, err := e.PortCheck(c.pos[1], ports, 3*time.Second)
		if err != nil {
			return fail(err)
		}
		return ok(res)
	}
	if len(c.pos) < 2 {
		return fail(forwardUsage)
	}
	port, err := strconv.Atoi(c.pos[1])
	if err != nil {
		return fail(forwardUsage)
	}
	spec := engine.ForwardSpec{Target: c.pos[0], Host: c.flags["host"], Port: port, Container: c.flags["container"],
		VHost: strings.ToLower(strings.TrimSpace(c.flags["vhost"])), TLS: c.has("tls")}
	if spec.VHost != "" && !reVHost.MatchString(spec.VHost) {
		return fail(errors.New("--vhost 要写域名"))
	}
	if spec.Host != "" && spec.Container != "" {
		return fail(errors.New("--host 和 --container 只能用一个"))
	}
	info, err := e.OpenForward(spec)
	if err != nil {
		return fail(err)
	}
	if cio.served {
		return ok(info)
	}
	// 命令行：一直转发到被打断
	con.OK("已转发：127.0.0.1:%d → %s（经 %s），按 Ctrl+C 停止", info.Local, info.To, info.Server)
	<-cio.ctx.Done()
	engine.CloseForwards(info.Local)
	return 0
}
