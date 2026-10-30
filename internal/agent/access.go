package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

// 预检：服务的运行用户能不能访问单元文件里用到的程序、脚本和目录。
// 用运行用户的身份实际测试（runuser … test），ACL、安全模块的限制也算在内；访问不了时找出卡在哪一级目录。

// accessNeed 一个要检查的路径。
type accessNeed struct {
	path   string
	mode   string // r / w / x 的组合
	where  string // ExecStart、ExecStartPre、ExecStartPost、WorkingDirectory
	soft   bool   // 这一行失败不影响启动（systemd 的 - 前缀）：只提示
	reason string // program（要执行的程序）/ arg（参数里的路径）/ dir（工作目录）
	tmpdir bool   // -Djava.io.tmpdir 的目录
	// writable 运行时要往里写的目录（日志、临时文件、快照……）：目录本身和里面已有的文件都要能写
	writable bool
	prop     string // 来自哪个参数 / 环境变量（说明用），如 -DJM.LOG.PATH
	creates  string // 目录还不存在：path 是最近的已有上级，creates 是要创建的目录
	// must 参数里一定得有的文件（没有时启动直接失败）：script（交给 bash、python 等执行的脚本）、
	// program（交给包装脚本执行的程序，如 /opt/jdk/bin/java）；runner 是执行它的程序
	must, runner string
}

// checkServiceAccess 检查一个服务应用；问题、提示记到 p 里。
func (a *Agent) checkServiceAccess(s *proto.AppSpec, p *proto.Preflight, cache map[string]error) {
	a.checkUnitSupport(s, p)
	a.checkSandboxPaths(s, p)
	a.checkEnvFiles(s, p)
	needs, protectHome := unitAccessNeeds(s.UnitText)
	own := a.appDir(s.Name)    // 本次部署才放好的版本、shared：不检查
	managed := a.runtimesDir() // Ferry 提供的 JDK：部署时才安装，由 Ferry 保证权限
	nginx := a.nginxDir()      // Ferry 提供的 nginx：同上
	seen := map[string]bool{}
	for _, n := range needs {
		if n.path == own || strings.HasPrefix(n.path, own+"/") || strings.HasPrefix(n.path, managed+"/") ||
			strings.HasPrefix(n.path, nginx+"/") || isPseudoFS(n.path) {
			continue
		}
		key := n.path + "|" + n.mode
		if seen[key] {
			continue
		}
		seen[key] = true
		report := func(msg, fix string) {
			if n.soft {
				msg += "（这条" + cmdName(n.where) + "允许失败，不影响启动，但建议修复）"
			}
			if fix != "" {
				msg += "\n修复：" + fix
			}
			if n.soft {
				p.Warnings = append(p.Warnings, msg)
			} else {
				p.Problems = append(p.Problems, msg)
			}
		}
		fi, err := os.Stat(n.path)
		if err != nil && n.writable {
			// 还不存在：程序启动时自己创建，最近的已有上级要能写
			if anc := existingParent(n.path); anc != "" && anc != "/" {
				n.creates, n.path, n.mode = n.path, anc, "wx"
				fi, err = os.Stat(anc)
			}
		}
		if err != nil {
			switch {
			case n.reason == "program":
				report(fmt.Sprintf("%s 的%s要执行 %s，但服务器上没有这个文件（启动会以 status=203/EXEC 退出）", s.Name, cmdName(n.where), n.path),
					"检查配置里的路径（service.java、service.exec、execStartPre 等）是否写对")
			case n.must != "":
				report(missingMust(s.Name, n), "把这个文件放到服务器上（跟着程序一起发布的脚本可以写进 artifact.files），或改配置里的路径")
			case n.reason == "dir":
				report(fmt.Sprintf("%s 的工作目录 %s 不存在", s.Name, n.path),
					fmt.Sprintf("在服务器上创建：mkdir -p %s && chown %s: %s，或修改 service.workdir", n.path, s.User, n.path))
			}
			continue // 参数里的路径可能由程序自己创建
		}
		if protectHome != "" && underHome(n.path) {
			ro := protectHome == "read-only"
			if !ro || strings.Contains(n.mode, "w") {
				report(fmt.Sprintf("%s 的单元设置了 ProtectHome=%s，服务里%s %s", s.Name, protectHome,
					map[bool]string{true: "不能写入", false: "看不到"}[ro], n.path),
					"去掉 unitExtra 里的 ProtectHome，或把文件放到 /home 之外")
				continue
			}
		}
		mode := n.mode
		if n.reason == "arg" && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0 {
			mode += "x" // 参数里的可执行文件（如交给包装脚本执行的 java）
		}
		if n.reason == "program" && !fi.IsDir() && isScript(n.path) {
			mode += "r" // 脚本要能读
		}
		if fi.IsDir() {
			mode = strings.ReplaceAll(mode, "r", "rx")
		}
		allowed := true
	flags:
		for _, f := range dedupe(mode) {
			// 自动修正时可能要改好几级目录：每改一处重新检查
			for attempt := 0; ; attempt++ {
				ok, err := a.accessAs(s.User, n.path, f, cache)
				if err != nil {
					p.Warnings = append(p.Warnings, fmt.Sprintf("无法检查运行用户 %s 的访问权限：%v", s.User, err))
					return
				}
				if ok {
					break
				}
				if a.req.FixPerms == "" || attempt >= 6 {
					report(a.explainDenied(s.Name, s.User, n, f, fi.IsDir(), cache))
					allowed = false
					break flags
				}
				target, tflag, tdir := n.path, f, fi.IsDir()
				if b := a.blocker(s.User, n.path, cache); b != "" {
					target, tflag, tdir = b, 'x', true
				}
				apply := a.req.FixPerms == FixApply
				desc, fixable := a.fixAccess(s.User, target, tflag, tdir, apply)
				if !fixable {
					msg, fix := a.explainDenied(s.Name, s.User, n, f, fi.IsDir(), cache)
					if desc != "" { // 修改失败
						msg += "（自动修正：" + desc + "）"
					}
					report(msg, fix)
					allowed = false
					break flags
				}
				line := fmt.Sprintf("%s：%s", s.Name, desc)
				if !apply {
					if !slices.Contains(p.Fixable, line) {
						p.Fixable = append(p.Fixable, line)
					}
					continue flags // 预计改好；部署时再真正修改并检查
				}
				p.Fixed = append(p.Fixed, line)
				clear(cache)
			}
		}
		// 要写入的已有目录：里面已有的文件也要能写（以前用 root 运行时留下的日志之类）
		if allowed && n.writable && n.creates == "" && fi.IsDir() {
			a.checkTree(s, n, p, report, cache)
		}
	}
}

// accessAs 以 user 的身份测试 path 的 r / w / x 权限（结果缓存）。
func (a *Agent) accessAs(user, path string, flag byte, cache map[string]error) (bool, error) {
	key := user + "|" + string(flag) + "|" + path
	if err, ok := cache[key]; ok {
		return err == nil, nil
	}
	ok, err := a.Sys.AccessAs(user, path, flag)
	if err != nil {
		return false, err
	}
	if ok {
		cache[key] = nil
	} else {
		cache[key] = os.ErrPermission
	}
	return ok, nil
}

// missingMust 参数里一定得有的文件不存在时的说明。
func missingMust(app string, n accessNeed) string {
	switch n.must {
	case "script":
		return fmt.Sprintf("%s 的%s要用 %s 执行脚本 %s，但服务器上没有这个文件（启动会以 status=127 退出）", app, cmdName(n.where), n.runner, n.path)
	}
	return fmt.Sprintf("%s 的%s要通过 %s 执行 %s，但服务器上没有这个文件（启动会以 status=127 退出）", app, cmdName(n.where), n.runner, n.path)
}

func cmdName(where string) string {
	switch where {
	case "ExecStartPre":
		return "启动前命令"
	case "ExecStartPost":
		return "启动后命令"
	case "WorkingDirectory":
		return "工作目录"
	case "Environment":
		return "环境变量"
	}
	return "启动命令"
}

// purpose 服务要拿这个路径做什么（说明用）。
func purpose(n accessNeed, flag byte, isDir bool) string {
	switch {
	case n.reason == "program":
		return fmt.Sprintf("%s要执行 %s", cmdName(n.where), n.path)
	case n.reason == "dir":
		return fmt.Sprintf("要进入工作目录 %s", n.path)
	case n.tmpdir:
		return fmt.Sprintf("要往 %s 写临时文件（启动参数 -Djava.io.tmpdir）", n.path)
	case n.creates != "":
		return fmt.Sprintf("运行时要创建 %s 并往里写入（%s），需要能写它的上级 %s", n.creates, n.prop, n.path)
	case n.writable:
		return fmt.Sprintf("运行时要往 %s 写入（%s）", n.path, n.prop)
	case isDir:
		return fmt.Sprintf("%s要访问目录 %s", cmdName(n.where), n.path)
	case flag == 'x':
		return fmt.Sprintf("%s要执行 %s", cmdName(n.where), n.path)
	}
	return fmt.Sprintf("%s要读取 %s", cmdName(n.where), n.path)
}

// permWord 权限的说法：目录的 r/w/x 是「列出 / 写入 / 进入」，文件是「读 / 写 / 执行」。
func permWord(flag byte, isDir bool) string {
	if isDir {
		return map[byte]string{'r': "列出", 'w': "写入", 'x': "进入"}[flag]
	}
	return map[byte]string{'r': "读", 'w': "写", 'x': "执行"}[flag]
}

// explainDenied 用直白的话说明为什么访问不了（哪一级目录进不去、属主与权限怎样），并给出修复命令。
// blocker 从根往下第一级运行用户进不去的目录（没有则为空）。
func (a *Agent) blocker(user, p string, cache map[string]error) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	dir := ""
	for _, part := range parts[:len(parts)-1] {
		dir += "/" + part
		if ok, err := a.accessAs(user, dir, 'x', cache); err == nil && !ok {
			return dir
		}
	}
	return ""
}

func (a *Agent) explainDenied(app, user string, n accessNeed, flag byte, isDir bool, cache map[string]error) (msg, fix string) {
	head := fmt.Sprintf("%s 以 %s 用户运行，%s", app, user, purpose(n, flag, isDir))
	// 从根往下找第一级进不去的目录
	parts := strings.Split(strings.Trim(n.path, "/"), "/")
	dir := ""
	for _, part := range parts[:len(parts)-1] {
		dir += "/" + part
		if ok, err := a.accessAs(user, dir, 'x', cache); err == nil && !ok {
			why, fix := a.permExplain(user, dir, 'x', true)
			return fmt.Sprintf("%s，但进不去上级目录 %s：%s", head, dir, why), fix
		}
	}
	why, fix := a.permExplain(user, n.path, flag, isDir)
	return fmt.Sprintf("%s，但没有%s的权限：%s", head, permWord(flag, isDir), why), fix
}

// permExplain 说明 user 对 path 的权限从哪来（属主 / 同组 / 其他用户）、缺了什么，以及怎么改。
func (a *Agent) permExplain(user, path string, flag byte, isDir bool) (why, fix string) {
	kind := map[bool]string{true: "目录", false: "文件"}[isDir]
	o, err := a.Sys.Ownership(path)
	uid, gids, uerr := a.Sys.UserGroups(user)
	if err != nil || uerr != nil {
		return "无法读取它的属主与权限", fmt.Sprintf("chown %s: %s（交给运行用户），或 chmod o+%c %s", user, path, flag, path)
	}
	perm := uint32(o.Perm.Perm())
	var bits uint32
	var who, class string
	switch {
	case uid == o.UID:
		bits, class = perm>>6&7, "u"
		who = fmt.Sprintf("这个%s属于 %s 自己，属主的权限是「%s」", kind, user, bitsWords(perm>>6&7, isDir))
	case slices.Contains(gids, o.GID):
		bits, class = perm>>3&7, "g"
		who = fmt.Sprintf("这个%s属于 %s（组 %s），%s 在这个组里，组的权限是「%s」", kind, o.User, o.Group, user, bitsWords(perm>>3&7, isDir))
	default:
		bits, class = perm&7, "o"
		who = fmt.Sprintf("这个%s属于 %s（组 %s），%s 既不是属主也不在这个组里，其他用户的权限是「%s」", kind, o.User, o.Group, user, bitsWords(perm&7, isDir))
	}
	need := map[byte]uint32{'r': 4, 'w': 2, 'x': 1}[flag]
	if bits&need != 0 {
		return who + "，按权限位是允许的，可能被 ACL 或安全策略（如麒麟安全 KYSEC、SELinux）拦截了",
			fmt.Sprintf("用 getfacl %s 查看 ACL，再检查麒麟安全（getstatus）或 SELinux 的策略", path)
	}
	switch {
	case flag == 'w' || class == "u":
		// 要写入，或本来就是运行用户的：交给运行用户最合适
		if class == "u" {
			fix = fmt.Sprintf("chmod u+%c %s", flag, path)
		} else {
			fix = fmt.Sprintf("chown %s: %s（把它交给运行用户）", user, path)
		}
	case class == "g":
		fix = fmt.Sprintf("chmod g+%c %s（让组 %s 可以%s）", flag, path, o.Group, permWord(flag, isDir))
	default:
		fix = fmt.Sprintf("chown %s: %s（交给运行用户），或 chmod o+%c %s（所有用户都可以%s）", user, path, flag, path, permWord(flag, isDir))
	}
	return who, fix
}

// bitsWords 一组 rwx 位的说法，如「列出、进入」；一个都没有时为「无」。
func bitsWords(b uint32, isDir bool) string {
	var w []string
	for _, f := range []struct {
		bit  uint32
		flag byte
	}{{4, 'r'}, {2, 'w'}, {1, 'x'}} {
		if b&f.bit != 0 {
			w = append(w, permWord(f.flag, isDir))
		}
	}
	if len(w) == 0 {
		return "无"
	}
	return strings.Join(w, "、")
}

func isScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 2)
	n, _ := f.Read(b)
	return n == 2 && string(b) == "#!"
}

func dedupe(mode string) []byte {
	var out []byte
	for _, f := range []byte("rwx") {
		if strings.IndexByte(mode, f) >= 0 {
			out = append(out, f)
		}
	}
	return out
}

func isPseudoFS(p string) bool {
	for _, d := range []string{"/proc", "/sys", "/dev", "/run/systemd"} {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

func underHome(p string) bool {
	for _, d := range []string{"/home", "/root", "/run/user"} {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// checkSandboxPaths ReadWritePaths 等列出的目录必须存在（前面加 - 的除外），否则 systemd 准备不了服务的隔离环境，
// 服务一启动就退出（status=226/NAMESPACE）。部署目录下的由 Ferry 部署时创建，不算。
func (a *Agent) checkSandboxPaths(s *proto.AppSpec, p *proto.Preflight) {
	roots := []string{a.req.Root, a.appRoot(s.Name)}
	for _, sp := range unitSandboxPaths(s.UnitText) {
		if !sdSupports(p.Systemd, sp.key) {
			continue // 这台的 systemd 不认识这条设置，直接忽略，不会因此启动失败（见 checkUnitSupport）
		}
		ferryMakes := false
		for _, r := range roots {
			ferryMakes = ferryMakes || sp.path == r || strings.HasPrefix(sp.path, r+"/") || strings.HasPrefix(r, sp.path+"/")
		}
		if ferryMakes {
			continue
		}
		if _, err := os.Stat(sp.path); err == nil {
			continue
		}
		p.Problems = append(p.Problems, fmt.Sprintf("%s 的单元里 %s 写的 %s 在服务器上不存在：systemd 会拒绝启动这个服务（status=226/NAMESPACE）"+
			"\n修复：在服务器上建好这个目录，或改配置（unitExtra）里的路径；不一定存在的路径前面加 -（如 %s=-%s）",
			s.Name, sp.key, sp.path, sp.key, sp.path))
	}
}

// checkEnvFiles EnvironmentFile 写的文件必须存在（前面加 - 的除外），否则 systemd 不启动这个服务
// （journal 里是 Failed to load environment files）。systemd 以 root 读它，不用检查运行用户的权限。
func (a *Agent) checkEnvFiles(s *proto.AppSpec, p *proto.Preflight) {
	own := filepath.ToSlash(a.appDir(s.Name)) // 跟着版本发布的文件（artifact.files）部署时才放好
	for _, f := range unitValues(s.UnitText, "EnvironmentFile") {
		if strings.HasPrefix(f, "-") {
			continue
		}
		f, ok := plainPath(f)
		if !ok || f == own || strings.HasPrefix(f, own+"/") {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			continue
		}
		p.Problems = append(p.Problems, fmt.Sprintf("%s 的环境变量文件 %s 在服务器上不存在：systemd 不会启动这个服务（Failed to load environment files）"+
			"\n修复：把这个文件放到服务器上，或改配置里的 service.envFile；可以没有的文件前面加 -", s.Name, f))
	}
}

// sdDirectiveSince 较新的 systemd 才认识的单元设置：开始支持的版本。更旧的 systemd 遇到不认识的设置只记一条警告，
// 然后忽略它（服务照常启动，只是这条限制不起作用）。
var sdDirectiveSince = map[string]int{
	"ReadWritePaths": 231, "ReadOnlyPaths": 231, "InaccessiblePaths": 231,
	"MemoryDenyWriteExecute": 231, "RestrictRealtime": 231,
	"ProtectKernelTunables": 232, "ProtectKernelModules": 232, "ProtectControlGroups": 232,
	"DynamicUser": 232, "PrivateUsers": 232, "RemoveIPC": 232,
	"RestrictNamespaces": 233, "BindPaths": 233, "BindReadOnlyPaths": 233,
	"LockPersonality": 235, "StateDirectory": 235, "CacheDirectory": 235, "LogsDirectory": 235, "ConfigurationDirectory": 235,
	"TemporaryFileSystem": 238,
	"ProtectHostname":     242, "RestrictSUIDSGID": 242,
	"ProtectKernelLogs": 244,
	"ProtectClock":      247, "ProtectProc": 247, "ProcSubset": 247,
}

// sdSupports systemd 版本 v 认识 key 这条设置（版本未知时按认识算）。
func sdSupports(v int, key string) bool {
	since, ok := sdDirectiveSince[key]
	return v == 0 || !ok || v >= since
}

// checkUnitSupport 单元里有这台服务器的 systemd 还不认识的设置：提醒会被忽略。
func (a *Agent) checkUnitSupport(s *proto.AppSpec, p *proto.Preflight) {
	if p.Systemd == 0 {
		return
	}
	var keys []string
	for _, l := range strings.Split(s.UnitText, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		k = strings.TrimSpace(k)
		if !ok || slices.Contains(keys, k) {
			continue
		}
		if !sdSupports(p.Systemd, k) || (k == "ProtectSystem" && strings.TrimSpace(v) == "strict" && p.Systemd < 232) {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s 的单元里 %s 是较新的 systemd 才有的设置，这台服务器是 systemd %d，会忽略它们"+
			"（服务照常启动，只是这些限制不起作用）", s.Name, strings.Join(keys, "、"), p.Systemd))
	}
}

// unitValues [Service] 里某个设置的各个值（同一个设置写了几行的都算）。
func unitValues(unit, key string) []string {
	var out []string
	section := ""
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if section == "[Service]" && ok && strings.TrimSpace(k) == key {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

type sandboxPath struct{ key, path string }

// unitSandboxPaths 单元里 ReadWritePaths、ReadOnlyPaths 等要求必须存在的路径（前面加 - 的不要求）。
func unitSandboxPaths(unit string) []sandboxPath {
	var out []sandboxPath
	section := ""
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section != "[Service]" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		switch k {
		case "ReadWritePaths", "ReadOnlyPaths", "InaccessiblePaths", "ExecPaths", "NoExecPaths":
		default:
			continue
		}
		for _, f := range strings.Fields(v) {
			f = strings.TrimPrefix(f, "+")
			if strings.HasPrefix(f, "-") || !strings.HasPrefix(f, "/") || strings.ContainsAny(f, "%$") {
				continue
			}
			out = append(out, sandboxPath{key: k, path: path.Clean(f)})
		}
	}
	return out
}

// unitAccessNeeds 从单元文件里找出要检查的路径，以及 ProtectHome 的设置（no / 空表示不限制）。
func unitAccessNeeds(unit string) (needs []accessNeed, protectHome string) {
	section := ""
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section != "[Service]" || line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "ProtectHome":
			switch strings.ToLower(v) {
			case "yes", "true", "on", "1", "tmpfs":
				protectHome = v
			case "read-only":
				protectHome = "read-only"
			default:
				protectHome = ""
			}
		case "WorkingDirectory":
			soft := strings.HasPrefix(v, "-")
			v = strings.TrimPrefix(v, "-")
			if v == "~" || !strings.HasPrefix(v, "/") || strings.ContainsAny(v, "%$") {
				continue
			}
			needs = append(needs, accessNeed{path: path.Clean(v), mode: "x", where: k, soft: soft, reason: "dir"})
		case "ExecStartPre", "ExecStart", "ExecStartPost":
			needs = append(needs, execNeeds(k, v)...)
		case "Environment":
			// 环境变量里的日志、临时目录（如 LOG_PATH=/data/logs）
			for _, kv := range sdSplit(v) {
				name, val, ok := strings.Cut(kv, "=")
				if !ok || !writableKey(name) {
					continue
				}
				if p, ok := plainPath(val); ok {
					n := accessNeed{path: p, mode: "rw", where: k, reason: "arg", writable: true, prop: "环境变量 " + name}
					if fileKey(name) {
						n.path = path.Dir(p)
					}
					needs = append(needs, n)
				}
			}
		}
	}
	return needs, protectHome
}

// propKey -Dname=值 / --name=值 里的 name，以及说明用的写法（-Dname / --name）。
func propKey(arg string) (key, prop string) {
	k, _, ok := strings.Cut(arg, "=")
	if !ok {
		return "", ""
	}
	switch {
	case strings.HasPrefix(k, "-D"):
		return k[2:], k
	case strings.HasPrefix(k, "--"):
		return k[2:], k
	}
	return "", ""
}

// writableKey 属性 / 环境变量名看起来是程序运行时要写入的位置：日志、临时文件、快照、缓存、上传、数据目录，
// 如 JM.LOG.PATH、JM.SNAPSHOT.PATH、logging.file.path、csp.sentinel.log.dir、LOG_HOME。配置文件（logging.config 等）不算。
func writableKey(key string) bool {
	k := strings.ToLower(key)
	if k == "" || strings.Contains(k, "conf") {
		return false
	}
	topic, kind := false, false
	for _, w := range []string{"log", "tmp", "temp", "snapshot", "cache", "upload", "data", "dump"} {
		topic = topic || strings.Contains(k, w)
	}
	for _, w := range []string{"path", "dir", "home", "location", "folder", "file"} {
		kind = kind || strings.Contains(k, w)
	}
	return topic && kind
}

// fileKey 值是文件而不是目录（如 logging.file.name）。
func fileKey(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "file") && !strings.Contains(k, "path") && !strings.Contains(k, "dir")
}

// execNeeds 一行 Exec*= 里的程序与参数中的路径。
func execNeeds(where, v string) []accessNeed {
	var out []accessNeed
	for _, cmd := range splitCommands(sdSplit(v)) {
		if len(cmd) == 0 {
			continue
		}
		prog := cmd[0]
		soft, root, at := false, false, false
		for len(prog) > 0 && strings.ContainsRune("-@:+!", rune(prog[0])) {
			switch prog[0] {
			case '-':
				soft = true
			case '+':
				root = true // 以完整权限（root）执行：不受运行用户限制
			case '@':
				at = true
			}
			prog = prog[1:]
		}
		if root {
			continue
		}
		args := cmd[1:]
		if at && len(args) > 0 {
			args = args[1:] // @ 时第二项是 argv[0] 的名字
		}
		if p, ok := plainPath(prog); ok {
			out = append(out, accessNeed{path: p, mode: "x", where: where, soft: soft, reason: "program"})
		}
		interp := isInterpreter(path.Base(prog))
		runner := prog // 执行参数里的程序的：包装脚本，没有脚本时是程序本身
		for _, arg := range args {
			key, prop := propKey(arg)
			must, by := "", prog
			switch {
			case interp && (arg == "-c" || arg == "-m" || arg == "-e"):
				interp = false // 后面是命令、模块名，不是脚本文件
			case strings.HasPrefix(arg, "-"):
			case interp:
				must, interp = "script", false
				if p, ok := plainPath(arg); ok {
					runner = p
				}
			case strings.Contains(arg, "/bin/"):
				must, by = "program", runner // 包装脚本后面要执行的程序（${java} 等）
			}
			for _, p := range argPaths(arg) {
				n := accessNeed{path: p, mode: "r", where: where, soft: soft, reason: "arg", must: must, runner: by}
				if key == "java.io.tmpdir" {
					n.mode, n.tmpdir, n.writable, n.prop = "rw", true, true, prop
				} else if writableKey(key) {
					n.mode, n.writable, n.prop = "rw", true, prop
					if fileKey(key) {
						n.path = path.Dir(p) // 日志文件：所在目录要能写
					}
				}
				out = append(out, n)
			}
		}
	}
	return out
}

// isInterpreter 把第一个参数当脚本文件执行的程序（bash x.sh、python3 app.py）。
func isInterpreter(name string) bool {
	switch name {
	case "sh", "bash", "dash", "ksh", "zsh", "perl", "ruby", "php", "node":
		return true
	}
	return strings.HasPrefix(name, "python")
}

// argPaths 参数里的绝对路径：/x/y、-Dkey=/x/y、--opt=/x/y；冒号分隔的多个路径（类路径）逐个算。
func argPaths(arg string) []string {
	v := arg
	if strings.HasPrefix(arg, "-") {
		_, after, ok := strings.Cut(arg, "=")
		if !ok {
			return nil
		}
		v = after
	}
	var out []string
	for _, part := range strings.Split(v, ":") {
		if p, ok := plainPath(part); ok {
			out = append(out, p)
		} else {
			return nil
		}
	}
	return out
}

// plainPath 不含变量、通配符、说明符的绝对路径。
func plainPath(s string) (string, bool) {
	s = strings.ReplaceAll(s, "%%", "\x00")
	if !strings.HasPrefix(s, "/") || len(s) < 2 || strings.ContainsAny(s, "$%*?[{}") {
		return "", false
	}
	return path.Clean(strings.ReplaceAll(s, "\x00", "%")), true
}

// splitCommands 按单独的 ; 分成多条命令（旧版 systemd 允许一行写多条）。
func splitCommands(tokens []string) [][]string {
	var out [][]string
	cur := []string{}
	for _, t := range tokens {
		if t == ";" {
			out = append(out, cur)
			cur = []string{}
			continue
		}
		cur = append(cur, t)
	}
	return append(out, cur)
}

// sdSplit 按 systemd 的规则切分命令行：空白分隔，支持双引号、单引号与反斜杠转义。
func sdSplit(s string) []string {
	var out []string
	var b strings.Builder
	inTok, quote := false, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
			inTok = true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote, inTok = c, true
		case c == ' ' || c == '\t':
			if inTok {
				out = append(out, b.String())
				b.Reset()
				inTok = false
			}
		default:
			b.WriteByte(c)
			inTok = true
		}
	}
	if inTok {
		out = append(out, b.String())
	}
	return out
}

// existingParent 最近的已存在的上级目录。
func existingParent(p string) string {
	for d := path.Dir(p); ; d = path.Dir(d) {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
		if d == "/" || d == "." {
			return ""
		}
	}
}

// badEntry 运行用户写不了的文件或目录。
type badEntry struct {
	path  string
	uid   int
	owner string
	perm  os.FileMode
	dir   bool
}

const (
	treeMaxEntries = 5000 // 最多看这么多个（日志目录可能很大）
	treeMaxDepth   = 6
	treeMaxConfirm = 30 // 按权限位判断写不了的，最多再实际测试这么多个（ACL 可能允许）
)

// unwritable 目录里运行用户写不了的文件、写不了或进不去的子目录（不跟随符号链接）。
func (a *Agent) unwritable(dir, user string, uid int, gids []int, cache map[string]error) []badEntry {
	var bad []badEntry
	seen, confirmed := 0, 0
	base := strings.Count(filepath.ToSlash(dir), "/")
	filepath.WalkDir(filepath.FromSlash(dir), func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		p := filepath.ToSlash(fp)
		if p == dir {
			return nil
		}
		if seen++; seen > treeMaxEntries {
			return fs.SkipAll
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() && strings.Count(p, "/")-base >= treeMaxDepth {
			return fs.SkipDir
		}
		o, err := a.Sys.Ownership(p)
		if err != nil {
			return nil
		}
		need := uint32(2) // 文件：写
		if d.IsDir() {
			need = 3 // 目录：写 + 进入
		}
		if permits(o, uid, gids, need) {
			return nil
		}
		if confirmed < treeMaxConfirm {
			confirmed++
			ok, err := a.accessAs(user, p, 'w', cache)
			if err == nil && ok && (!d.IsDir() || func() bool { ok, _ := a.accessAs(user, p, 'x', cache); return ok }()) {
				return nil // ACL 等允许
			}
		}
		bad = append(bad, badEntry{path: p, uid: o.UID, owner: o.User, perm: o.Perm, dir: d.IsDir()})
		if d.IsDir() {
			return fs.SkipDir // 进不去的目录里面不用再看
		}
		return nil
	})
	return bad
}

// permits 按权限位，uid（所属组 gids）对 o 是否有 need 这几位（4 读 2 写 1 执行）。
func permits(o Ownership, uid int, gids []int, need uint32) bool {
	perm := uint32(o.Perm.Perm())
	var cls uint32
	switch {
	case uid == o.UID:
		cls = perm >> 6 & 7
	case slices.Contains(gids, o.GID):
		cls = perm >> 3 & 7
	default:
		cls = perm & 7
	}
	return cls&need == need
}

// checkTree 运行时要写入的目录里，已有的文件、子目录运行用户也要能写（如以前用 root 运行时留下的日志，
// 程序打开它们会「权限不够」）。都属于 root 或运行用户自己时可以自动修正：交给运行用户。
func (a *Agent) checkTree(s *proto.AppSpec, n accessNeed, p *proto.Preflight, report func(msg, fix string), cache map[string]error) {
	uid, gids, err := a.Sys.UserGroups(s.User)
	if err != nil {
		return
	}
	bad := a.unwritable(n.path, s.User, uid, gids, cache)
	if len(bad) == 0 {
		return
	}
	describe := func(bad []badEntry) string {
		owners := map[string]int{}
		var names []string
		for _, b := range bad {
			if owners[b.owner] == 0 {
				names = append(names, b.owner)
			}
			owners[b.owner]++
		}
		var parts []string
		for _, o := range names {
			parts = append(parts, fmt.Sprintf("属于 %s 的 %d 个", o, owners[o]))
		}
		eg := strings.TrimPrefix(bad[0].path, n.path+"/")
		return fmt.Sprintf("%s（如 %s）", strings.Join(parts, "、"), eg)
	}
	fixable := a.req.FixPerms != "" && !protectedPath(n.path) && !otherHome(n.path, s.User) && !a.isRootOrAncestor(n.path)
	for _, b := range bad {
		if b.uid != 0 && b.uid != uid {
			fixable = false // 属于别的用户：可能是别的服务在用，不动
		}
	}
	msg := fmt.Sprintf("%s 运行时要往 %s 写入（%s），但里面有运行用户 %s 写不了的文件：%s，程序打开它们时会「权限不够」",
		s.Name, n.path, n.prop, s.User, describe(bad))
	fix := fmt.Sprintf("chown -R %s: %s", s.User, n.path)
	if !fixable {
		report(msg, fix)
		return
	}
	line := fmt.Sprintf("%s：把 %s 里%s交给 %s（%s 运行时要写入）", s.Name, n.path, describe(bad), s.User, n.prop)
	if a.req.FixPerms != FixApply {
		if !slices.Contains(p.Fixable, line) {
			p.Fixable = append(p.Fixable, line)
		}
		return
	}
	_, gid, err := a.Sys.LookupUser(s.User)
	if err != nil {
		report(msg, fix)
		return
	}
	for _, b := range bad {
		if err := a.Sys.Lchown(b.path, uid, gid); err != nil {
			report(msg+"（自动修正失败："+err.Error()+"）", fix)
			return
		}
		add := os.FileMode(0o200)
		if b.dir {
			add = 0o300
		}
		if b.perm.Perm()&add != add {
			a.Sys.Chmod(b.path, modeBits(b.perm)|add)
		}
	}
	p.Fixed = append(p.Fixed, line)
	clear(cache)
	if still := a.unwritable(n.path, s.User, uid, gids, cache); len(still) > 0 {
		report(fmt.Sprintf("%s 运行时要往 %s 写入（%s），自动修正后仍有写不了的文件：%s", s.Name, n.path, n.prop, describe(still)), fix)
	}
}
