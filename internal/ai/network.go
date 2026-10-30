package ai

import (
	"net"
	"net/url"
	"path"
	"strings"
)

// 网络访问：只读查看只放行访问本机和内网（内网 IP、localhost、不带点的主机名）；
// 访问其他地址的命令要用户确认，而且每次都要确认（不能「同类不再询问」）。

// ExternalHost 命令要访问的第一个「本机、内网以外」的地址；没有时返回空。
func ExternalHost(cmd string) string {
	for _, seg := range splitCommands(strings.TrimSpace(cmd)) {
		w, _ := stripPrefix(words(strings.TrimSpace(seg)))
		if len(w) == 0 {
			continue
		}
		for _, h := range netTargets(path.Base(w[0]), w[1:]) {
			if !localHost(h) {
				return h
			}
		}
	}
	return ""
}

// curl 这些选项后面跟的是值，不是地址
var curlValueFlags = map[string]bool{"-H": true, "--header": true, "-A": true, "--user-agent": true, "-u": true, "--user": true,
	"-m": true, "--max-time": true, "--connect-timeout": true, "-w": true, "--write-out": true, "-b": true, "--cookie": true,
	"-e": true, "--referer": true, "-r": true, "--range": true, "--retry": true, "--retry-delay": true, "-y": true, "-Y": true,
	"--limit-rate": true, "--cacert": true, "--cert": true, "--key": true, "-E": true, "--interface": true, "-o": true, "--output": true,
	"--max-filesize": true, "-z": true, "--time-cond": true}

// netTargets 命令要连接的主机（不认识的命令返回空）。
func netTargets(name string, args []string) []string {
	var hosts []string
	switch name {
	case "curl", "wget":
		for i := 0; i < len(args); i++ {
			a := args[i]
			if curlValueFlags[a] {
				i++
				continue
			}
			// 改连接目标的选项：值当成要访问的地址
			if a == "-x" || a == "--proxy" || a == "--resolve" || a == "--connect-to" || a == "--preproxy" {
				if i+1 < len(args) {
					hosts = append(hosts, args[i+1])
				}
				i++
				continue
			}
			if !strings.HasPrefix(a, "-") {
				hosts = append(hosts, a)
			}
		}
	case "ping", "ping6", "nc", "ncat", "telnet", "traceroute", "tracepath", "nslookup", "dig", "host", "ssh", "scp", "sftp", "rsync":
		for _, a := range args {
			if strings.HasPrefix(a, "-") || isNumber(a) {
				continue
			}
			hosts = append(hosts, a)
		}
	case "openssl":
		for i, a := range args {
			if (a == "-connect" || a == "-proxy") && i+1 < len(args) {
				hosts = append(hosts, args[i+1])
			}
		}
	case "getent":
		if len(args) > 1 && strings.HasPrefix(args[0], "ahosts") || len(args) > 1 && (args[0] == "hosts" || args[0] == "networks") {
			hosts = append(hosts, args[1:]...)
		}
	}
	return hosts
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && c != '.' && c != '-' {
			return false
		}
	}
	return !strings.Contains(s, "..")
}

// localHost 地址是不是本机或内网：localhost、内网 / 回环 / 链路本地 IP、不带点的主机名（内网服务名）。
func localHost(target string) bool {
	h := strings.TrimSpace(target)
	if h == "" {
		return true
	}
	if strings.Contains(h, "://") {
		u, err := url.Parse(h)
		if err != nil {
			return false
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return false // file://、ftp:// 等一律确认
		}
		h = u.Hostname()
	} else {
		// host:port/path、[ipv6]:port、user@host
		if i := strings.LastIndex(h, "@"); i >= 0 {
			h = h[i+1:]
		}
		if i := strings.IndexAny(h, "/?#"); i >= 0 {
			h = h[:i]
		}
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		} else if strings.Count(h, ":") == 1 {
			h = h[:strings.Index(h, ":")]
		}
		h = strings.Trim(h, "[]")
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return !strings.Contains(h, ".")
}
