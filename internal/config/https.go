package config

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

var reDomain = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// checkHTTPS 自动申请证书：域名、验证方式、端口。port 是 static.port 按 rePort 解析出来的分组（nil = 没写对）。
func checkHTTPS(h *HTTPS, port []string, where string, bad func(string, ...any)) {
	at := where + "：static.https"
	checkTLS(h, at, false, bad)
	if h.DNS == "" && port != nil && port[2] != "80" {
		bad("%s：HTTP 验证要从外网访问 80 端口：把 static.port 设成 80，或改用 DNS 验证（https.dns）", at)
	}
	if port != nil && h.HTTPSPort() == port[2] {
		bad("%s.port 要写一个和 static.port 不一样的端口（默认 443），当前 %q", at, h.Port)
	}
}

func checkTLS(h *HTTPS, at string, custom bool, bad func(string, ...any)) {
	if len(h.Domains) == 0 {
		bad("%s.domains 要写证书里的域名，如 [www.example.com]", at)
	}
	for i, d := range h.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		h.Domains[i] = d
		switch {
		case !reDomain.MatchString(d):
			bad("%s.domains：%q 不是域名", at, d)
		case strings.HasPrefix(d, "*.") && h.DNS == "" && !custom:
			bad("%s.domains：通配符域名 %s 只能用 DNS 验证（https.dns）", at, d)
		}
	}
	seen := map[string]bool{}
	h.Domains = slices.DeleteFunc(h.Domains, func(domain string) bool {
		duplicate := seen[domain]
		seen[domain] = true
		return duplicate
	})
	switch h.DNS {
	case "", "aliyun", "dnspod", "cloudflare", "huaweicloud", "route53":
	default:
		bad("%s.dns 只能是 aliyun、dnspod、cloudflare、huaweicloud、route53（当前 %q）", at, h.DNS)
	}
	if n, err := strconv.Atoi(h.HTTPSPort()); err != nil || n < 1 || n > 65535 {
		bad("%s.port 要写 1～65535（默认 443），当前 %q", at, h.Port)
	}
	if h.Email != "" && !strings.Contains(h.Email, "@") {
		bad("%s.email 不是邮箱：%q", at, h.Email)
	}
	if c := h.CA; c != "" && c != "letsencrypt" && c != "letsencrypt-staging" && !strings.HasPrefix(c, "https://") {
		bad("%s.ca 写 letsencrypt、letsencrypt-staging 或以 https:// 开头的 ACME 目录地址（当前 %q）", at, c)
	}
}

func (h *AppHTTPS) HasHTTP() bool {
	return h.Cert == "" && h.DNS == "" || h.Redirect != nil && *h.Redirect
}

func (a *App) HTTPSUnit() string {
	suffix := "-https.service"
	if a.Service.ZeroDowntime != nil {
		suffix = "-proxy.service"
	}
	return strings.TrimSuffix(a.Service.Unit, ".service") + suffix
}

var httpsInstanceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
var httpsHost = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?$`)

func checkAppHTTPS(a *App, where string, units map[string]string, bad func(string, ...any)) {
	h := a.HTTPS
	at := where + "：https"
	custom := h.Cert != "" || h.Key != ""
	checkTLS(&h.HTTPS, at, custom, bad)
	if a.Job != nil {
		bad("%s 只用于常驻后端应用", at)
	}
	if custom {
		for _, file := range []string{h.Cert, h.Key} {
			if !path.IsAbs(file) || path.Clean(file) != file || strings.ContainsAny(file, "\r\n\x00") {
				bad("%s.cert 和 https.key 要成对填写服务器上的绝对路径", at)
				break
			}
		}
		if h.DNS != "" || h.CA != "" || h.Email != "" {
			bad("%s 使用自带证书时无需设置 dns、ca 或 email", at)
		}
	} else if h.DNS == "" && h.HTTPSPort() != "443" {
		bad("%s 使用 HTTP 验证时 https.port 要用 443", at)
	}
	if h.HasHTTP() && h.HTTPSPort() == "80" {
		bad("%s.port 不能与 HTTP 端口 80 一样", at)
	}
	if h.MaxBody != "" && !reNgxSize.MatchString(h.MaxBody) {
		bad("%s.maxBody 要写大小，如 100m", at)
	}
	if h.NginxInstance != "" && h.NginxInstance != proto.SystemNginxInstance && !httpsInstanceName.MatchString(h.NginxInstance) {
		bad("%s.nginxInstance 必须是已安装的 nginx 实例名", at)
	}
	if z := a.Service.ZeroDowntime; z != nil {
		if h.Upstream != "" {
			bad("%s 不停机发布时会自动转发到运行中的槽位，无需填写 upstream", at)
		}
		port, _ := strconv.Atoi(h.HTTPSPort())
		for _, slot := range z.Ports {
			if slot == port || (h.HasHTTP() && slot == 80) {
				bad("%s 的监听端口和 zeroDowntime.ports 冲突：%d", at, slot)
			}
		}
		if m := rePort.FindStringSubmatch(z.Listen); m != nil &&
			(m[2] == h.HTTPSPort() || (h.HasHTTP() && m[2] == "80" && (h.NginxInstance != "" || m[1] != ""))) {
			bad("%s 的监听端口与 zeroDowntime.listen 冲突：%s", at, z.Listen)
		}
		if z.Listen != "" && !rePort.MatchString(z.Listen) {
			bad("%s：zeroDowntime.listen 要写端口或 IP:端口", at)
		}
	} else if h.Upstream == "" && a.Service.Health.TCP == "" && a.Service.Health.HTTP == "" {
		bad("%s 请填写 upstream，或在 service.health 里填写后端端口", at)
	}
	if h.Upstream != "" && !strings.Contains(h.Upstream, "${") {
		if _, err := HTTPSUpstream(h.Upstream, "", ""); err != nil {
			bad("%s：%v", at, err)
		}
	}
	if h.NginxInstance == "" {
		unit := a.HTTPSUnit()
		if other := units[unit]; other != "" {
			bad("%s 的服务 %s 与应用 %s 冲突", at, unit, other)
		}
		units[unit] = a.Name
	}
	if slices.Contains(a.Service.Conflicts, a.HTTPSUnit()) {
		bad("%s 的 HTTPS 单元不能写入 service.conflicts", at)
	}
}

// HTTPSUpstream 从显式地址或健康检查推断 nginx 的 HTTP 上游。
func HTTPSUpstream(explicit, tcp, http string) (string, error) {
	address := explicit
	if address == "" {
		address = tcp
		if http != "" {
			u, err := url.Parse(http)
			if err == nil {
				port := u.Port()
				if port == "" {
					port = "80"
					if u.Scheme == "https" {
						port = "443"
					}
				}
				address = net.JoinHostPort(u.Hostname(), port)
			}
		}
	}
	host, port, err := net.SplitHostPort(address)
	n, _ := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || (net.ParseIP(host) == nil && !httpsHost.MatchString(host)) {
		return "", fmt.Errorf("https.upstream 请填写 HTTP 后端的地址和端口，如 127.0.0.1:8080")
	}
	if host == "0.0.0.0" || host == "::" {
		return "", fmt.Errorf("https.upstream 请指向实际地址，如 127.0.0.1:%s；后端可设置为只监听本机", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}
