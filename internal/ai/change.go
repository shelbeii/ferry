package ai

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// 改动服务器的命令（第二步）：用户逐条确认后才执行。这里判断一条命令属于哪「类」改动（「本次对话同类不再询问」按它记），
// 以及它危不危险（危险的不管怎么设置都要确认，并标红）。

// stripPrefix 去掉 sudo、timeout N、环境变量赋值等前缀，返回真正的命令与参数，以及是否用了 sudo。
func stripPrefix(w []string) ([]string, bool) {
	sudo := false
	for len(w) > 0 {
		switch {
		case w[0] == "sudo":
			sudo = true
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				if w[0] == "-u" && len(w) > 1 {
					w = w[1:]
				}
				w = w[1:]
			}
			continue
		case w[0] == "timeout" && len(w) > 2:
			w = w[2:]
			continue
		case w[0] == "env" || w[0] == "nohup" || w[0] == "nice":
			w = w[1:]
			continue
		case strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") && !strings.HasPrefix(w[0], "/"):
			w = w[1:]
			continue
		}
		break
	}
	return w, sudo
}

// UsesSudo 命令里有没有用 sudo。
func UsesSudo(cmd string) bool {
	for _, seg := range splitCommands(cmd) {
		if _, sudo := stripPrefix(words(strings.TrimSpace(seg))); sudo {
			return true
		}
	}
	return false
}

// 这些命令按「命令 + 子命令」分类（yum install 和 yum remove 是两类）
var withSub = map[string]bool{"systemctl": true, "yum": true, "dnf": true, "apt": true, "apt-get": true, "docker": true,
	"podman": true, "firewall-cmd": true, "git": true, "npm": true, "pip": true, "pip3": true, "snap": true, "service": true,
	"ufw": true, "nmcli": true, "hostnamectl": true, "timedatectl": true, "crontab": true, "kubectl": true, "helm": true}

var (
	shellLead = map[string]bool{"do": true, "then": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true, "{": true}
	shellSkip = map[string]bool{"for": true, "case": true, "done": true, "fi": true, "esac": true, "}": true, "true": true, "false": true, ":": true, "[": true, "[[": true, "test": true}
)

// CommandKind 一条要改动服务器的命令属于哪类，如「yum install」「systemctl restart」「mkdir」；多条命令时把各条的类合起来。
func CommandKind(cmd string) string {
	set := map[string]bool{}
	for _, seg := range splitCommands(strings.TrimSpace(cmd)) {
		if strings.TrimSpace(seg) == "" || checkOne(seg) == nil {
			continue // 只读的部分不算
		}
		w, _ := stripPrefix(words(strings.TrimSpace(seg)))
		// shell 的关键字不算命令：for i in …; do 命令; done 只算里面的命令
		for len(w) > 0 && shellLead[w[0]] {
			w = w[1:]
		}
		if len(w) == 0 || shellSkip[w[0]] {
			continue
		}
		w, _ = stripPrefix(w)
		if len(w) == 0 {
			continue
		}
		k := path.Base(w[0])
		switch {
		case k == "service": // service nginx restart：动作在最后
			if len(w) > 2 {
				k += " " + w[len(w)-1]
			}
		case k == "tee": // 写到哪个目录也算进类别：放行写 A 目录不等于放行写 B 目录
			for _, a := range w[1:] {
				if !strings.HasPrefix(a, "-") {
					k += " " + WriteScope(a)
				}
			}
		case withSub[k]:
			for _, a := range w[1:] {
				if !strings.HasPrefix(a, "-") {
					k += " " + a
					break
				}
			}
		}
		set[k] = true
	}
	for _, f := range redirectTargets(cmd) {
		set["写 "+WriteScope(f)] = true
	}
	var ks []string
	for k := range set {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	if len(ks) == 0 {
		return "其他命令"
	}
	return strings.Join(ks, " + ")
}

// redirectTargets 命令里用 > 或 >> 写入的文件（引号里的不算；2>&1、/dev/null 不算）。
func redirectTargets(cmd string) []string {
	var out []string
	quote := byte(0)
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '>':
			j := i + 1
			if j < len(cmd) && cmd[j] == '>' {
				j++
			}
			if j < len(cmd) && cmd[j] == '&' { // 2>&1
				i = j
				continue
			}
			for j < len(cmd) && cmd[j] == ' ' {
				j++
			}
			k := j
			for k < len(cmd) && !strings.ContainsRune(" ;|&<>", rune(cmd[k])) {
				k++
			}
			if f := strings.Trim(cmd[j:k], `"'`); f != "" && f != "/dev/null" {
				out = append(out, f)
			}
			i = k - 1
		}
	}
	return out
}

// 这些命令用来执行别的命令或代码：放行一次不能代表放行以后的（每次内容都不一样）
var runsCode = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true,
	"tcsh": true, "eval": true, "exec": true, "source": true, ".": true, "xargs": true, "env": true, "su": true, "setsid": true,
	"nohup": true, "expect": true, "at": true, "batch": true, "parallel": true, "busybox": true, "systemd-run": true, "watch": true}

// WriteScope 写文件的「同类」范围：同一个目录下的文件。
func WriteScope(file string) string {
	d := path.Dir(strings.TrimSpace(file))
	if d == "." {
		return "当前目录下的文件"
	}
	return strings.TrimSuffix(d, "/") + "/ 下的文件"
}

// SensitiveWrite 写这个文件要每次确认的原因：隐藏的配置文件（如 .bashrc）、SSH 登录、定时任务、系统服务等；没有时返回空。
func SensitiveWrite(file string) string {
	f := strings.TrimSpace(file)
	base := path.Base(f)
	switch {
	case strings.Contains(f, "/.ssh/") || strings.HasPrefix(f, ".ssh/") || base == "authorized_keys":
		return "改 SSH 登录相关的文件"
	case strings.Contains(f, "cron") || strings.HasPrefix(f, "/var/spool/"):
		return "改定时任务"
	case strings.Contains(f, "/systemd/") || strings.HasSuffix(f, ".service") || strings.HasSuffix(f, ".timer"):
		return "改系统服务"
	case strings.HasPrefix(f, "/etc/") || strings.HasPrefix(f, "/boot/") || strings.HasPrefix(f, "/usr/") || strings.HasPrefix(f, "/lib"):
		return "改系统文件"
	case strings.HasPrefix(base, ".") && base != "." && base != "..":
		return "改隐藏的配置文件（如 .bashrc）"
	}
	return ""
}

// AlwaysAsk 每次都要用户确认的原因（不能「同类不再询问」）；没有时返回空。
// 执行脚本、代码或别的命令的，以及访问本机和内网以外地址的。
func AlwaysAsk(cmd string) string {
	if h := ExternalHost(cmd); h != "" {
		return "访问外部地址 " + h
	}
	for _, f := range redirectTargets(cmd) {
		if r := SensitiveWrite(f); r != "" {
			return r
		}
	}
	for _, seg := range splitCommands(strings.TrimSpace(cmd)) {
		raw := words(strings.TrimSpace(seg))
		for i, a := range raw {
			if a == "sudo" && i+1 < len(raw) && (raw[i+1] == "-s" || raw[i+1] == "-i") {
				return "打开 root 的 shell"
			}
		}
		w, _ := stripPrefix(raw)
		for len(w) > 0 && shellLead[w[0]] {
			w = w[1:]
		}
		if len(w) == 0 {
			continue
		}
		name := path.Base(w[0])
		switch {
		case runsCode[name]:
			return "用 " + name + " 执行别的命令或脚本"
		case strings.HasPrefix(name, "python") || strings.HasPrefix(name, "perl") || strings.HasPrefix(name, "ruby") ||
			strings.HasPrefix(name, "php") || strings.HasPrefix(name, "node") || strings.HasPrefix(name, "lua"):
			return "用 " + name + " 执行代码"
		case name == "find":
			for _, a := range w[1:] {
				if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" || a == "-delete" {
					return "对找到的文件执行操作"
				}
			}
		case name == "docker" || name == "podman" || name == "kubectl" || name == "crictl" || name == "nerdctl":
			for _, a := range w[1:] {
				if a == "exec" || a == "run" {
					return "在容器里执行命令"
				}
			}
		case name == "crontab" || name == "at":
			return "安排以后自动执行的任务"
		case name == "tee" || name == "cp" || name == "mv" || name == "install" || name == "ln":
			// 写到的文件：tee 的参数、cp / mv / install / ln 的最后一个参数
			targets := w[1:]
			if name != "tee" && len(targets) > 0 {
				targets = targets[len(targets)-1:]
			}
			for _, a := range targets {
				if !strings.HasPrefix(a, "-") {
					if r := SensitiveWrite(a); r != "" {
						return r
					}
				}
			}
		case strings.HasSuffix(name, ".sh") || strings.HasPrefix(w[0], "./") || strings.HasPrefix(w[0], "/tmp/"):
			return "执行脚本 " + w[0]
		}
	}
	return ""
}

// 停掉会影响业务、断开连接的服务
var keyServices = []string{"sshd", "ssh", "mysql", "mysqld", "mariadb", "postgres", "redis", "mongod", "nacos", "nginx", "docker",
	"network", "NetworkManager", "firewalld", "kafka", "zookeeper", "rabbitmq", "elasticsearch", "minio"}

// 把输出交给 shell 或解释器执行（… | sh）
var rePipeShell = regexp.MustCompile(`\|\s*(sudo\s+(-\S+\s+)*)?(sh|bash|zsh|dash|ksh|python[0-9.]*|perl|ruby|node|php)(\s|$)`)

// 用代码删文件、执行命令的迹象（python -c、perl -e 等）
var codeDanger = []string{"rmtree", "os.remove", "os.unlink", "unlink(", "rmdir", "shutil", "os.system", "subprocess", "exec(", "rm -", "drop table", "truncate"}

// Danger 危险命令的原因（删除、格式化、关机重启、改防火墙、停关键服务、卸载、删数据、改登录或系统文件……）；不危险时返回空。
// 也看包在 bash -c、xargs、find -exec、eval 里的命令。
func Danger(cmd string) string { return danger(cmd, 0) }

func danger(cmd string, depth int) string {
	if depth > 3 {
		return ""
	}
	low := strings.ToLower(cmd)
	if rePipeShell.MatchString(low) {
		if strings.Contains(low, "curl") || strings.Contains(low, "wget") {
			return "直接执行网上下载的脚本"
		}
		return "把别的命令的输出当成命令执行"
	}
	for _, f := range redirectTargets(cmd) {
		for _, dev := range []string{"/dev/sd", "/dev/nvme", "/dev/vd", "/dev/xvd", "/dev/mapper/"} {
			if strings.HasPrefix(f, dev) {
				return "直接写磁盘"
			}
		}
		if r := SensitiveWrite(f); r != "" {
			return r
		}
	}
	for _, seg := range splitCommands(strings.TrimSpace(cmd)) {
		w, _ := stripPrefix(words(strings.TrimSpace(seg)))
		for len(w) > 0 && shellLead[w[0]] {
			w = w[1:]
		}
		if len(w) == 0 {
			continue
		}
		name, args := path.Base(w[0]), w[1:]
		switch {
		case name == "sh" || name == "bash" || name == "zsh" || name == "dash" || name == "ksh":
			for i, a := range args {
				if a == "-c" && i+1 < len(args) {
					if r := danger(args[i+1], depth+1); r != "" {
						return r
					}
				}
			}
		case name == "eval" || name == "setsid" || name == "watch":
			if r := danger(strings.Join(args, " "), depth+1); r != "" {
				return r
			}
		case name == "xargs":
			if r := danger(strings.Join(afterFlags(args, "-n", "-I", "-P", "-d", "-L", "-s", "-E", "-a", "--max-args", "--max-procs", "--delimiter"), " "), depth+1); r != "" {
				return r
			}
		case name == "find":
			for i, a := range args {
				if a == "-delete" {
					return "删除找到的文件"
				}
				if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" {
					var sub []string
					for _, b := range args[i+1:] {
						if b == ";" || b == `\;` || b == "+" {
							break
						}
						sub = append(sub, b)
					}
					if r := danger(strings.Join(sub, " "), depth+1); r != "" {
						return r
					}
				}
			}
		case strings.HasPrefix(name, "python") || name == "perl" || name == "ruby" || name == "node" || name == "php":
			code := strings.ToLower(strings.Join(args, " "))
			for _, k := range codeDanger {
				if strings.Contains(code, k) {
					return "用代码删除文件或执行命令"
				}
			}
		case name == "tee" || name == "cp" || name == "mv" || name == "install" || name == "ln" || name == "dd":
			targets := args
			if name != "tee" && name != "dd" && len(args) > 0 {
				targets = args[len(args)-1:]
			}
			for _, a := range targets {
				a = strings.TrimPrefix(a, "of=")
				if strings.HasPrefix(a, "-") {
					continue
				}
				if r := SensitiveWrite(a); r != "" {
					return r
				}
			}
		}
		if r := dangerOne(name, args); r != "" {
			return r
		}
	}
	return ""
}

// afterFlags 去掉开头的选项（这些选项后面跟一个值），返回剩下的命令。
func afterFlags(args []string, valued ...string) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return args[i:]
		}
		for _, v := range valued {
			if a == v {
				i++
				break
			}
		}
	}
	return nil
}

func dangerOne(name string, args []string) string {
	sub := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			sub = a
			break
		}
	}
	joined := " " + strings.Join(args, " ") + " "
	hasArg := func(list ...string) bool {
		for _, a := range args {
			for _, x := range list {
				if a == x {
					return true
				}
			}
		}
		return false
	}
	hasRecursive := func() bool {
		for _, a := range args {
			if a == "--recursive" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR")) {
				return true
			}
		}
		return false
	}
	touchesKey := func() string {
		for _, a := range args {
			s := strings.TrimSuffix(a, ".service")
			for _, k := range keyServices {
				if strings.EqualFold(s, k) || strings.HasPrefix(strings.ToLower(s), strings.ToLower(k)+"@") {
					return s
				}
			}
		}
		return ""
	}
	if strings.HasPrefix(name, "mkfs") || strings.HasPrefix(name, "mke2fs") {
		return "格式化磁盘，数据会全部丢失"
	}
	switch name {
	case "rm", "rmdir", "unlink", "shred":
		return "删除文件，删了不能恢复"
	case "truncate":
		return "清空文件内容"
	case "dd", "fdisk", "sfdisk", "parted", "gdisk", "wipefs", "lvremove", "vgremove", "pvremove", "mkswap", "swapoff":
		return "改动磁盘、分区"
	case "shutdown", "reboot", "halt", "poweroff":
		return "关机或重启服务器"
	case "init", "telinit":
		return "切换运行级别（可能关机、重启）"
	case "iptables", "ip6tables", "nft", "ufw", "iptables-restore":
		return "改防火墙规则，可能断开连接"
	case "firewall-cmd":
		if strings.Contains(joined, "--add") || strings.Contains(joined, "--remove") || strings.Contains(joined, "--set") ||
			strings.Contains(joined, "--reload") || strings.Contains(joined, "--panic") || strings.Contains(joined, "--change") {
			return "改防火墙规则，可能断开连接"
		}
	case "systemctl":
		switch sub {
		case "reboot", "poweroff", "halt", "kexec", "rescue", "emergency", "isolate":
			return "关机、重启或切换系统状态"
		case "stop", "disable", "mask", "kill", "restart":
			if s := touchesKey(); s != "" {
				if sub == "restart" {
					if s == "sshd" || s == "ssh" || s == "network" || s == "NetworkManager" || s == "firewalld" {
						return "重启 " + s + "，可能断开连接"
					}
					return ""
				}
				return fmt.Sprintf("停止 %s，依赖它的服务会受影响", s)
			}
		}
	case "service":
		if hasArg("stop") {
			if s := touchesKey(); s != "" {
				return fmt.Sprintf("停止 %s，依赖它的服务会受影响", s)
			}
		}
	case "kill", "pkill", "killall":
		return "结束进程"
	case "userdel", "groupdel", "passwd", "chpasswd", "usermod", "chage":
		return "改动系统账号或密码"
	case "chmod", "chown", "chgrp", "setfacl":
		if hasRecursive() {
			return "递归修改权限或属主"
		}
	case "yum", "dnf", "zypper":
		if sub == "remove" || sub == "erase" || sub == "autoremove" || sub == "downgrade" || sub == "history" {
			return "卸载或回退软件包"
		}
	case "apt", "apt-get", "aptitude":
		if sub == "remove" || sub == "purge" || sub == "autoremove" {
			return "卸载软件包"
		}
	case "rpm":
		if hasArg("-e", "--erase") {
			return "卸载软件包"
		}
	case "dpkg":
		if hasArg("-r", "-P", "--remove", "--purge") {
			return "卸载软件包"
		}
	case "docker", "podman":
		if sub == "rm" || sub == "rmi" || sub == "prune" || strings.Contains(joined, " prune") ||
			(sub == "volume" && strings.Contains(joined, " rm")) || (sub == "compose" && strings.Contains(joined, " down") && hasArg("-v", "--volumes")) {
			return "删除容器、镜像或数据卷"
		}
	case "mysql", "mariadb", "psql", "redis-cli", "mongo", "mongosh":
		up := strings.ToUpper(joined)
		for _, k := range []string{"DROP ", "TRUNCATE", "DELETE ", "FLUSHALL", "FLUSHDB", "ALTER ", "UPDATE "} {
			if strings.Contains(up, k) {
				return "改动或删除数据库里的数据"
			}
		}
	case "crontab":
		if hasArg("-r") {
			return "删除全部定时任务"
		}
	case "mv":
		for _, a := range args {
			if a == "/" || a == "/etc" || a == "/usr" || a == "/boot" || a == "/var" || a == "/home" {
				return "移动系统目录"
			}
		}
	}
	return ""
}

// ShellQuote 把一个参数用单引号包起来，交给 sh。
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// LineDiff 两段文本按行比较，返回统一格式的差异（- 删掉的行、+ 加上的行，前后各留 3 行上下文）。
// 太大的文件不比较，返回空。
func LineDiff(old, new string) string {
	a, b := strings.Split(old, "\n"), strings.Split(new, "\n")
	if len(a)*len(b) > 4_000_000 {
		return ""
	}
	// 最长公共子序列
	n, m := len(a), len(b)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	type line struct {
		op   byte // ' ' '-' '+'
		text string
	}
	var ops []line
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, line{' ', a[i]})
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]): // 先列删掉的行，再列加上的
			ops = append(ops, line{'-', a[i]})
			i++
		default:
			ops = append(ops, line{'+', b[j]})
			j++
		}
	}
	// 只留改动处前后 3 行
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.op != ' ' {
			for x := max(0, k-3); x <= min(len(ops)-1, k+3); x++ {
				keep[x] = true
			}
		}
	}
	var out strings.Builder
	gap := false
	for k, o := range ops {
		if !keep[k] {
			gap = true
			continue
		}
		if gap && out.Len() > 0 {
			out.WriteString("…\n")
		}
		gap = false
		out.WriteByte(o.op)
		out.WriteByte(' ')
		out.WriteString(o.text)
		out.WriteByte('\n')
	}
	return out.String()
}

// 输出写到了文件（> 文件、>> 文件、&> 文件；2>&1 这种只是合并不算）
var reStdoutRedirect = regexp.MustCompile(`(^|[^0-9&>])(>>?|&>)\s*[^\s&>]`)

// BackgroundProblem 放到后台（&）的命令没有把输出重定向到文件：它会一直占着这次执行的输出，让执行等到超时。
// 有这个问题时返回给模型的说明；没有时返回空。
func BackgroundProblem(cmd string) string {
	for _, seg := range splitCommands(strings.TrimSpace(cmd)) {
		s := strings.TrimSpace(seg)
		if !strings.HasSuffix(s, " &") && s != "&" {
			continue
		}
		if !reStdoutRedirect.MatchString(strings.TrimSuffix(s, "&")) {
			return "放到后台运行（&）的命令要把输出重定向到文件，否则这次执行会一直等它、直到超时。" +
				"长期运行的程序请做成 systemd 服务；只是临时在后台跑，就写成：命令 > /tmp/xxx.log 2>&1 &"
		}
	}
	return ""
}
