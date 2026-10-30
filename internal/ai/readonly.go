package ai

import (
	"fmt"
	"path"
	"strings"
)

// ReadOnly 检查模型要执行的命令是不是只读的：只看、不改服务器。不是的返回原因（交回给模型）。
//
// 规则偏严：只认识的命令、只认识的用法才放行；拿不准的一律拒绝，由用户自己在终端里执行。
//   - 可以用 | && || ; 连接多条命令，每一条都要通过；
//   - 不能有输出重定向（2>/dev/null、2>&1 除外）、命令替换 $( ) ` `、后台 &、换行；
//   - 可以在前面加 sudo（只读命令以 root 执行，看得到更多日志）；
//   - 不能读私钥、/etc/shadow 等敏感文件（内容会发给模型服务商）。
func ReadOnly(cmd string) error {
	s := strings.TrimSpace(cmd)
	if s == "" {
		return fmt.Errorf("命令是空的")
	}
	for _, ok := range []string{"2>/dev/null", "2> /dev/null", "2>&1", ">/dev/null 2>&1"} {
		s = strings.ReplaceAll(s, ok, " ")
	}
	for _, bad := range []string{"\n", "\r", "`", "$(", ">", "<(", "<<"} {
		if strings.Contains(s, bad) {
			return fmt.Errorf("不能用 %q（只允许只读命令，不能重定向输出、嵌套命令）", strings.TrimSpace(bad))
		}
	}
	if lower := strings.ToLower(s); containsAny(lower, sensitive) || patternHitsSensitive(s) {
		return fmt.Errorf("不能读取私钥、密码文件等敏感内容（内容会发给模型服务商）")
	}
	for _, seg := range splitCommands(s) {
		if err := checkOne(seg); err != nil {
			return err
		}
	}
	if h := ExternalHost(s); h != "" {
		return fmt.Errorf("要访问 %s：自动执行的查看命令只能访问本机和内网地址，访问其他地址要用户确认", h)
	}
	return nil
}

var sensitive = []string{"/etc/shadow", "/etc/gshadow", "/etc/sudoers", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
	"/.ssh/", ".pem", ".key", ".p12", ".pfx", ".keystore", ".jks", "authorized_keys", "known_hosts", ".bash_history", ".mysql_history"}

// 敏感文件的样例路径：用来判断通配符、{a,b} 写法会不会展开成它们
var sensitivePaths = []string{"/etc/shadow", "/etc/gshadow", "/etc/sudoers", "/etc/ssh/ssh_host_rsa_key", "/etc/ssh/ssh_host_ecdsa_key",
	"/etc/ssh/ssh_host_ed25519_key", "/root/.ssh/id_rsa", "/root/.ssh/id_ed25519", "/root/.ssh/id_ecdsa", "/root/.ssh/authorized_keys",
	"/home/u/.ssh/id_rsa", "/home/u/.ssh/id_ed25519", "/root/.bash_history", "/home/u/.bash_history", "/root/.mysql_history"}

// patternHitsSensitive 命令里带通配符（* ? [ ]）或 {a,b} 的参数，展开后可能是敏感文件。
func patternHitsSensitive(cmd string) bool {
	for _, w := range words(cmd) {
		if !strings.ContainsAny(w, "*?[{") {
			continue
		}
		for _, alt := range expandBraces(w, 0) {
			if containsAny(strings.ToLower(alt), sensitive) {
				return true
			}
			pats := []string{alt}
			if strings.HasPrefix(alt, "~") {
				pats = []string{"/root" + alt[1:], "/home/u" + alt[1:]}
			}
			for _, p := range pats {
				for _, c := range sensitivePaths {
					segs := strings.Split(c, "/")
					tail2 := strings.Join(segs[max(len(segs)-2, 0):], "/")
					for _, cand := range []string{c, path.Base(c), tail2} {
						if ok, _ := path.Match(p, cand); ok {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// expandBraces 展开 {a,b}（像 bash 那样），最多展开几层。
func expandBraces(w string, depth int) []string {
	i := strings.Index(w, "{")
	j := strings.Index(w, "}")
	if depth > 3 || i < 0 || j < i || !strings.Contains(w[i:j], ",") {
		return []string{w}
	}
	var out []string
	for _, part := range strings.Split(w[i+1:j], ",") {
		out = append(out, expandBraces(w[:i]+part+w[j+1:], depth+1)...)
	}
	return out
}

func containsAny(s string, list []string) bool {
	for _, x := range list {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// splitCommands 按 | && || ; 拆成单条命令（引号里的不拆）；单独的 & 当成一条以 & 结尾的命令（后面会被拒绝）。
func splitCommands(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == '|' || c == ';':
			out = append(out, cur.String())
			cur.Reset()
			if i+1 < len(s) && s[i+1] == '|' {
				i++
			}
		case c == '&' && (i > 0 && s[i-1] == '>' || i+1 < len(s) && s[i+1] == '>'):
			cur.WriteByte(c) // 2>&1、&> 是重定向，不是放到后台
		case c == '&':
			if i+1 < len(s) && s[i+1] == '&' {
				out = append(out, cur.String())
				cur.Reset()
				i++
			} else {
				cur.WriteString(" &")
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// words 简单按空白拆参数（去掉引号）。
func words(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	has := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, has = c, true
		case c == ' ' || c == '\t':
			if cur.Len() > 0 || has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 || has {
		out = append(out, cur.String())
	}
	return out
}

// 任何用法都只读的命令
var plain = map[string]bool{
	"ls": true, "cat": true, "head": true, "wc": true, "grep": true, "egrep": true, "fgrep": true, "zgrep": true, "zcat": true,
	"df": true, "du": true, "free": true, "uptime": true, "uname": true, "hostname": true, "whoami": true, "id": true, "date": true,
	"ps": true, "pgrep": true, "pidof": true, "ss": true, "netstat": true, "lsof": true, "which": true, "whereis": true, "type": true,
	"file": true, "stat": true, "sort": true, "uniq": true, "cut": true, "tr": true, "column": true, "nl": true, "jq": true,
	"echo": true, "printf": true, "basename": true, "dirname": true, "readlink": true, "realpath": true, "md5sum": true,
	"sha256sum": true, "sha1sum": true, "vmstat": true, "iostat": true, "mpstat": true, "lscpu": true, "lsblk": true, "blkid": true,
	"findmnt": true, "getent": true, "nproc": true, "w": true, "who": true, "last": true, "lastlog": true, "getenforce": true,
	"sestatus": true, "lsb_release": true, "true": true, "test": true, "[": true, "env": false, "printenv": false,
	"arch": true, "tree": true, "diff": true, "cmp": true, "strings": true, "od": true, "hexdump": true, "xxd": true,
	"pstree": true, "lsmod": true, "lspci": true, "lsusb": true, "dmidecode": true, "ulimit": true, "locale": true,
	"rpm": false, "timedatectl": false, "hostnamectl": false,
}

// subcommands 只读的子命令（第一个非选项参数）
var subcommands = map[string][]string{
	"systemctl":   {"status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files", "list-timers", "list-sockets", "list-dependencies", "show", "cat", "get-default"},
	"docker":      {"ps", "images", "logs", "inspect", "stats", "version", "info", "top", "port", "diff", "history", "network", "volume", "image", "container", "compose", "system"},
	"podman":      {"ps", "images", "logs", "inspect", "stats", "version", "info", "top", "port"},
	"yum":         {"list", "info", "search", "repolist", "provides", "whatprovides", "deplist", "history", "check-update"},
	"dnf":         {"list", "info", "search", "repolist", "provides", "repoquery", "history", "check-update"},
	"apt":         {"list", "show", "search", "policy"},
	"apt-cache":   {"show", "search", "policy", "depends", "rdepends", "showpkg", "madison"},
	"ip":          {"a", "addr", "address", "r", "route", "link", "l", "neigh", "n", "rule", "-br", "-4", "-6", "-s"},
	"timedatectl": {"status", "show", "list-timezones"},
	"hostnamectl": {"status"},
	"firewall-cmd": {"--state", "--list-all", "--list-ports", "--list-services", "--get-active-zones", "--get-default-zone",
		"--list-all-zones", "--get-zones", "--query-port", "--query-service"},
	"crontab":   {"-l"},
	"nginx":     {"-t", "-T", "-v", "-V"},
	"java":      {"-version", "--version"},
	"mysql":     {"--version", "-V"},
	"redis-cli": {"--version", "-v"},
	"openssl":   {"version", "x509", "s_client"},
}

// 这些选项会改东西
var badFlags = map[string][]string{
	"journalctl": {"--vacuum", "--rotate", "--flush", "--sync", "--relinquish", "--smart-relinquish", "--setup-keys", "-f", "--follow", "--update-catalog"},
	"tail":       {"-f", "-F", "--follow", "--retry"},
	"dmesg":      {"-c", "-C", "--clear", "--read-clear", "-n", "--console", "-w", "--follow", "-D", "-E"},
	"find":       {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls"},
	"sort":       {"-o", "--output", "--compress-program"},
	"xxd":        {"-r", "-revert"},
	"tree":       {"-o"},
	"lastlog":    {"-C", "--clear", "-S", "--set"},
	"file":       {"-C", "--compile"},
	"blkid":      {"-g", "--garbage-collect"},
	"curl": {"-X", "--request", "-d", "--data", "--data-binary", "--data-raw", "--data-urlencode", "--json", "-F", "--form", "-T",
		"--upload-file", "-o", "--output", "-O", "--remote-name", "-K", "--config"},
	"nginx":        {"-s"},
	"crontab":      {"-r", "-e", "-i"},
	"firewall-cmd": {"--add", "--remove", "--reload", "--complete-reload", "--permanent", "--set", "--change", "--new", "--delete", "--runtime-to-permanent", "--panic", "--lockdown"},
	"top":          {},
	"sysctl":       {"-w", "--write", "-p", "--load", "--system"},
	"iptables":     {"-A", "-D", "-I", "-R", "-F", "-X", "-P", "-N", "-E", "-Z", "--append", "--delete", "--insert", "--replace", "--flush", "--policy", "--new-chain", "--delete-chain", "--zero"},
	"ip6tables":    {"-A", "-D", "-I", "-R", "-F", "-X", "-P", "-N", "-E", "-Z"},
	"openssl":      {"-out", "genrsa", "genpkey", "req"},
	"rpm":          {"-i", "-U", "-F", "-e", "--install", "--upgrade", "--freshen", "--erase", "--import", "--rebuilddb", "--initdb", "--setperms", "--setugids", "--restore"},
	"dpkg":         {"-i", "-r", "-P", "--install", "--remove", "--purge", "--configure", "--unpack", "--set-selections", "--clear-selections"},
}

// 这些命令只有在用了指定选项时才只读
var needFlag = map[string][]string{
	"top":       {"-b", "-bn1", "-bn 1", "-n"},
	"ping":      {"-c"},
	"nc":        {"-z"},
	"sysctl":    {"-a", "-n", "--all"},
	"tar":       {"-t", "-tf", "-tzf", "-tvf", "-tvzf", "tf", "tzf", "tvf", "--list"},
	"unzip":     {"-l", "-Z"},
	"dpkg":      {"-l", "-L", "-s", "-S", "--list", "--listfiles", "--status", "--search", "-p", "--print-avail", "--get-selections"},
	"rpm":       {"-q", "--query", "-V", "--verify"},
	"iptables":  {"-L", "-S", "--list", "--list-rules", "-nL", "-nvL", "-vnL"},
	"ip6tables": {"-L", "-S", "--list", "-nL"},
	"mount":     {},
}

// docker / podman 的管理命令组，以及组里只读的操作
var (
	dockerGroups = []string{"container", "image", "network", "volume", "system", "compose", "builder", "plugin", "context"}
	dockerViews  = []string{"ls", "list", "ps", "inspect", "logs", "top", "port", "diff", "history", "df", "info", "images", "config", "version", "stats"}
)

// strictArgs 有些查看命令换个参数就会改东西：只放行查看的用法。
func strictArgs(name string, args []string) error {
	switch name {
	case "date": // 只能显示（+格式、-d 指定时间、-u、-I、-R……），不能设置时间
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case strings.HasPrefix(a, "+"):
			case a == "-d" || a == "--date" || a == "-r" || a == "--reference" || a == "-f" || a == "--file":
				i++ // 后面是值
			case strings.HasPrefix(a, "--set") || strings.HasPrefix(a, "-s"):
				return fmt.Errorf("date 只能查看时间，不能设置")
			case strings.HasPrefix(a, "-"):
			default:
				return fmt.Errorf("date 只能查看时间，不能设置")
			}
		}
	case "hostname": // 只能查看
		ok := []string{"-f", "-s", "-i", "-I", "-d", "-A", "-a", "-y", "-v", "--fqdn", "--long", "--short", "--ip-address",
			"--all-ip-addresses", "--all-fqdns", "--domain", "--alias", "--yp", "--nis"}
		for _, a := range args {
			if !contains(ok, a) {
				return fmt.Errorf("hostname 只能查看主机名，不能修改")
			}
		}
	case "sysctl": // 带 = 的参数是改值
		for _, a := range args {
			if strings.Contains(a, "=") {
				return fmt.Errorf("sysctl 只能查看，不能设置（%s）", a)
			}
		}
	case "uniq": // 第二个文件名是输出文件
		n := 0
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "-f" || a == "-s" || a == "-w" {
				i++
				continue
			}
			if !strings.HasPrefix(a, "-") {
				n++
			}
		}
		if n > 1 {
			return fmt.Errorf("uniq 只能读一个文件（第二个文件名会被写入）")
		}
	case "tar": // 只能列出内容：只认 -t v z j J f 这几个选项和 --list
		for i, a := range args {
			switch {
			case a == "--list" || a == "--verbose" || a == "--gzip" || a == "--bzip2" || a == "--xz" || strings.HasPrefix(a, "--file="):
			case strings.HasPrefix(a, "--"):
				return fmt.Errorf("tar 列出内容时不能用 %s", a)
			case strings.HasPrefix(a, "-") || i == 0 && !strings.Contains(a, "/") && !strings.Contains(a, "."):
				if strings.Trim(strings.TrimPrefix(a, "-"), "tvzjJf") != "" {
					return fmt.Errorf("tar 只能用 -t 列出内容（%s 不行）", a)
				}
			}
		}
	}
	return nil
}

func checkOne(seg string) error {
	w := words(strings.TrimSpace(seg))
	if len(w) == 0 {
		return fmt.Errorf("命令不完整")
	}
	if w[len(w)-1] == "&" {
		return fmt.Errorf("不能放到后台执行（&）")
	}
	// 前缀：sudo（只读命令以 root 执行）、timeout N
	for len(w) > 0 {
		switch {
		case w[0] == "sudo":
			w = w[1:]
			for len(w) > 0 && (w[0] == "-n" || w[0] == "-E") {
				w = w[1:]
			}
			if len(w) > 0 && strings.HasPrefix(w[0], "-") {
				return fmt.Errorf("sudo 只能直接跟命令（不能用 %s）", w[0])
			}
			continue
		case w[0] == "timeout" && len(w) > 2:
			w = w[2:]
			continue
		}
		break
	}
	if len(w) == 0 {
		return fmt.Errorf("命令不完整")
	}
	name := path.Base(w[0])
	args := w[1:]
	if name == "curl" { // -o /dev/null 只是丢掉响应内容
		var keep []string
		for i := 0; i < len(args); i++ {
			if (args[i] == "-o" || args[i] == "--output") && i+1 < len(args) && args[i+1] == "/dev/null" {
				i++
				continue
			}
			keep = append(keep, args[i])
		}
		args = keep
	}
	if strings.Contains(w[0], "=") {
		return fmt.Errorf("不能设置环境变量（%s）", w[0])
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, f := range badFlags[name] {
		if hasFlag(args, joined, f) || (name == "firewall-cmd" && strings.Contains(joined, " "+f)) {
			return fmt.Errorf("%s 不能用 %s（会改动服务器、或一直不结束）", name, strings.TrimSpace(f))
		}
	}
	switch name {
	case "service": // service <名字> status
		if len(args) == 2 && args[1] == "status" || len(args) == 1 && args[0] == "--status-all" {
			return nil
		}
		return fmt.Errorf("service 只能用来看状态（service <名字> status）")
	case "ip":
		for _, a := range args {
			if contains([]string{"add", "del", "delete", "set", "flush", "change", "replace", "append", "save", "restore"}, a) {
				return fmt.Errorf("ip 不能用 %s（会改动网络配置）", a)
			}
		}
	}
	if err := strictArgs(name, args); err != nil {
		return err
	}
	if subs, ok := subcommands[name]; ok {
		first := ""
		for _, a := range args {
			if !strings.HasPrefix(a, "-") || contains(subs, a) {
				first = a
				break
			}
		}
		if (first == "" && name != "systemctl" && name != "docker") || (first != "" && !contains(subs, first)) {
			return fmt.Errorf("%s %s 不是只读的用法（只能用 %s）", name, first, strings.Join(subs, "、"))
		}
		if name == "docker" || name == "podman" {
			if contains(args, "-f") || contains(args, "--follow") {
				return fmt.Errorf("docker logs 不能用 -f（会一直不结束）")
			}
			// docker container / image / network … 后面的操作也只放行查看类的
			var pos []string
			for _, a := range args {
				if !strings.HasPrefix(a, "-") {
					pos = append(pos, a)
				}
			}
			if len(pos) > 0 && contains(dockerGroups, pos[0]) {
				if len(pos) < 2 || !contains(dockerViews, pos[1]) {
					op := ""
					if len(pos) > 1 {
						op = pos[1]
					}
					return fmt.Errorf("%s %s %s 不是只读的用法（只能用 %s）", name, pos[0], op, strings.Join(dockerViews, "、"))
				}
			}
		}
		return nil
	}
	if flags, ok := needFlag[name]; ok {
		if name == "mount" {
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf("mount 只能不带参数（列出已挂载的）")
		}
		for _, f := range flags {
			if hasFlag(args, joined, f) {
				return nil
			}
		}
		return fmt.Errorf("%s 只有加 %s 时才放行", name, strings.Join(flags, " / "))
	}
	switch name {
	case "journalctl", "tail", "dmesg", "find", "curl", "less", "more":
		if name == "less" || name == "more" {
			return fmt.Errorf("不能用 %s（交互式），用 cat、head、tail", name)
		}
		return nil
	case "sed", "awk":
		// 它们是完整的脚本语言，能写文件、执行命令，很难逐一检查：一律先给用户确认
		return fmt.Errorf("%s 能写文件、执行命令，不作为只读命令自动执行（查看文件用 grep、head、tail、cut）", name)
	}
	if v, ok := plain[name]; ok && v {
		return nil
	}
	return fmt.Errorf("%s 不在只读命令的名单里：可能会改动服务器，先不执行（需要的话请在终端里自己执行）", name)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// hasFlag 参数里有没有这个选项：完全相同，或 -X 后面直接跟值（-Xfoo）、--x=foo；含空格的按子串找
func hasFlag(args []string, joined, f string) bool {
	if strings.Contains(f, " ") || !strings.HasPrefix(f, "-") {
		return strings.Contains(joined, f) || contains(args, f)
	}
	for _, a := range args {
		if a == f || strings.HasPrefix(a, f+"=") || strings.HasPrefix(f, "--") && strings.HasPrefix(a, f) {
			return true
		}
		// 单字母选项合写：-tf、-bn1；或直接跟值：-ofile
		if len(f) == 2 && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a[1:], f[1:]) {
			return true
		}
	}
	return false
}
