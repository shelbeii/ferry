package config

import (
	"regexp"
	"strconv"
	"strings"
)

var reDomain = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// checkHTTPS 自动申请证书：域名、验证方式、端口。port 是 static.port 按 rePort 解析出来的分组（nil = 没写对）。
func checkHTTPS(h *HTTPS, port []string, where string, bad func(string, ...any)) {
	at := where + "：static.https"
	if len(h.Domains) == 0 {
		bad("%s.domains 要写证书里的域名，如 [www.example.com]", at)
	}
	for i, d := range h.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		h.Domains[i] = d
		switch {
		case !reDomain.MatchString(d):
			bad("%s.domains：%q 不是域名", at, d)
		case strings.HasPrefix(d, "*.") && h.DNS == "":
			bad("%s.domains：通配符域名 %s 只能用 DNS 验证（https.dns）", at, d)
		}
	}
	switch h.DNS {
	case "", "aliyun", "dnspod", "cloudflare":
	default:
		bad("%s.dns 只能是 aliyun、dnspod、cloudflare（当前 %q）", at, h.DNS)
	}
	if h.DNS == "" && port != nil && port[2] != "80" {
		bad("%s：HTTP 验证要从外网访问 80 端口：把 static.port 设成 80，或改用 DNS 验证（https.dns: aliyun / dnspod / cloudflare）", at)
	}
	if n, err := strconv.Atoi(h.HTTPSPort()); err != nil || n < 1 || n > 65535 || (port != nil && h.HTTPSPort() == port[2]) {
		bad("%s.port 要写一个和 static.port 不一样的端口（默认 443），当前 %q", at, h.Port)
	}
	if h.Email != "" && !strings.Contains(h.Email, "@") {
		bad("%s.email 不是邮箱：%q", at, h.Email)
	}
	if c := h.CA; c != "" && c != "letsencrypt" && c != "letsencrypt-staging" && !strings.HasPrefix(c, "https://") {
		bad("%s.ca 写 letsencrypt、letsencrypt-staging 或以 https:// 开头的 ACME 目录地址（当前 %q）", at, c)
	}
}
