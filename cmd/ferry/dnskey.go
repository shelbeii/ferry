package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/ui"
)

const dnsKeyUsage = "用法：ferry dns-key [list] | set aliyun|dnspod --id <ID> | set cloudflare | delete <服务商>"

var dnsKeyNames = map[string]string{"aliyun": "阿里云 DNS", "dnspod": "腾讯云 DNSPod", "cloudflare": "Cloudflare"}

// dnsKeyCmd HTTPS 证书 DNS 验证用的 DNS 服务商密钥（存在系统凭据库）：
//
//	ferry dns-key [list]                     各家有没有保存
//	ferry dns-key set aliyun --id <AccessKey ID>       AccessKey Secret 从 FERRY_SECRET_VALUE 或终端输入
//	ferry dns-key set dnspod --id <SecretId>           SecretKey 同上
//	ferry dns-key set cloudflare                       API Token 同上
//	ferry dns-key delete <服务商>
func dnsKeyCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	action := "list"
	if len(c.pos) > 0 {
		action = c.pos[0]
	}
	p := ""
	if len(c.pos) > 1 {
		p = c.pos[1]
	}
	if action != "list" {
		if _, ok := agent.DNSProviders[p]; !ok {
			return fail(errors.New(dnsKeyUsage))
		}
	}
	switch action {
	case "list":
		out := map[string]bool{}
		names := make([]string, 0, len(agent.DNSProviders))
		for name, fields := range agent.DNSProviders {
			names = append(names, name)
			ok := true
			for _, f := range fields {
				if _, has, err := secret.Get(secret.DNSAccount(name, f)); err != nil {
					return fail(err)
				} else if !has {
					ok = false
				}
			}
			out[name] = ok
		}
		if js != nil {
			return ok0(js, out)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(cio.stdout, "%-11s %s：%s\n", n, dnsKeyNames[n], map[bool]string{true: "已保存", false: "未保存"}[out[n]])
		}
		return 0
	case "set":
		v := strings.TrimSpace(cio.getenv("FERRY_SECRET_VALUE"))
		label := map[string]string{"aliyun": "AccessKey Secret：", "dnspod": "SecretKey：", "cloudflare": "API Token："}[p]
		if v == "" && con != nil {
			v, _ = con.Secret(label)
		}
		if v == "" {
			return fail(errors.New("没有输入：在终端中运行，或通过环境变量 FERRY_SECRET_VALUE 提供"))
		}
		if p == "cloudflare" {
			if err := secret.Set(secret.DNSAccount(p, "token"), v); err != nil {
				return fail(err)
			}
			break
		}
		id := strings.TrimSpace(c.flags["id"])
		if id == "" {
			return fail(errors.New(dnsKeyUsage))
		}
		if err := secret.Set(secret.DNSAccount(p, "id"), id); err != nil {
			return fail(err)
		}
		if err := secret.Set(secret.DNSAccount(p, "secret"), v); err != nil {
			return fail(err)
		}
	case "delete":
		for _, f := range agent.DNSProviders[p] {
			if err := secret.Delete(secret.DNSAccount(p, f)); err != nil {
				return fail(err)
			}
		}
	default:
		return fail(errors.New(dnsKeyUsage))
	}
	if js == nil {
		fmt.Fprintf(cio.stdout, "%s的密钥：%s\n", dnsKeyNames[p], map[string]string{"set": "已保存", "delete": "已删除"}[action])
	}
	return ok0(js, map[string]any{"ok": true})
}
