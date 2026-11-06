package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/dbx"
)

// 「软件」页连数据库：先从服务器上读出账号密码（只读，不改动服务器），连接对话框里替用户填好；
// 读到的密码不一定还对（容器的环境变量是建容器时的，后来改过 root 密码的话就不对了），
// 所以每个读到的密码都经这台服务器真的登录试一下，能登录的才填；都不对时告诉用户、请用户填。
//   - Ferry 装的：安装时保存在本机凭据库里的密码
//   - Docker 容器：1Panel 记的（应用目录的 .env，1Panel 里改密码时会跟着改）、容器的环境变量
//     （MYSQL_ROOT_PASSWORD、POSTGRES_PASSWORD……）、*_FILE 指的文件、MySQL 随机生成的 root 密码（容器日志）、
//     Redis 的 --requirepass 和它的配置文件
//   - 系统装的 Redis：redis.conf 里的 requirepass
//   - 系统装的 MySQL / MariaDB：root 和登录用户的 ~/.my.cnf；PostgreSQL：~/.pgpass

// DBCredQuery 哪个数据库（「软件」页的一行）。
type DBCredQuery struct {
	Kind      string `json:"kind"` // mysql | mariadb | postgresql | redis
	Container string `json:"container,omitempty"`
	Unit      string `json:"unit,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Port      int    `json:"port,omitempty"`      // 从服务器上连的端口（主机上的）
	InnerPort int    `json:"innerPort,omitempty"` // 容器里的端口：试密码时直接连容器
	Instance  string `json:"instance,omitempty"`  // Ferry 装的软件
	Account   string `json:"account,omitempty"`   // Ferry 装的：安装时的账号
}

// DBCred 读到的账号密码。
type DBCred struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	// Found 有可以填的密码（可能是空密码：没设密码）；Verified 试过，能登录
	Found    bool   `json:"found"`
	Verified bool   `json:"verified,omitempty"`
	From     string `json:"from,omitempty"` // 从哪里读到的
	Note     string `json:"note,omitempty"` // 没读到、没试成、都不对时：为什么、该怎么办
}

// dbCand 读到的一个账号密码。
type dbCand struct{ user, password, from string }

var reSafeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// DBCredentials 读数据库的账号密码，并试哪个能登录。读不到、都不对不算错误（Note 里说明）；连不上服务器才返回错误。
func (e *Engine) DBCredentials(target string, q DBCredQuery) (*DBCred, error) {
	q.Kind = strings.ToLower(q.Kind)
	if q.Container != "" && !reSafeName.MatchString(q.Container) {
		return nil, fmt.Errorf("容器名 %q 不合法", q.Container)
	}
	defUser := map[string]string{"mysql": "root", "mariadb": "root", "postgresql": "postgres"}[q.Kind]
	var cands []dbCand
	note := ""
	if q.Instance != "" {
		if pw, err := e.SoftwarePassword(target, q.Instance); err == nil {
			cands = append(cands, dbCand{firstNonEmptyStr(q.Account, defUser), pw, "Ferry 安装时保存的密码"})
		} else {
			note = "这台电脑上没有保存它的密码（可能是在别的电脑上装的）：请填写"
		}
	}
	var cred *DBCred
	err := e.withTarget(target, func(s *session) error {
		run := func(cmd string) (string, bool) {
			out, r, err := s.c.Output("timeout 15 " + s.sudo + cmd)
			return out, err == nil && r.Code == 0
		}
		if q.Instance == "" {
			var c []dbCand
			if q.Container != "" {
				c, note = dockerCands(q, run)
			} else {
				c, note = hostCands(q, s.t.User, run)
			}
			cands = append(cands, c...)
		}
		reach := newReach(q.Container, q.InnerPort, q.Port)
		cred = pickCred(cands, defUser, note, func(c dbCand) (bool, error) { return e.tryLogin(s, q, reach, c) })
		return nil
	})
	if err != nil {
		if len(cands) > 0 { // Ferry 保存的密码：连不上服务器也能填，只是没试
			c := cands[0]
			return &DBCred{User: c.user, Password: c.password, Found: true, From: c.from, Note: "没能连上服务器试这个密码对不对"}, nil
		}
		return nil, err
	}
	return cred, nil
}

// pickCred 按顺序试，第一个能登录的就用；try 返回 (能登录, 连不上数据库时的错误)。
func pickCred(cands []dbCand, defUser, note string, try func(dbCand) (bool, error)) *DBCred {
	if len(cands) == 0 {
		if note == "" {
			note = "请填写账号密码"
		}
		return &DBCred{User: defUser, Note: note}
	}
	var netErr error
	reached := false
	var froms []string
	for _, c := range cands {
		ok, err := try(c)
		if ok {
			return &DBCred{User: c.user, Password: c.password, Found: true, Verified: true, From: c.from}
		}
		if err != nil {
			netErr = err
			continue
		}
		reached = true
		froms = append(froms, c.from)
	}
	if !reached { // 一次都没连上数据库：说不准密码对不对，先填第一个
		c := cands[0]
		msg := "没能试这个密码对不对"
		if netErr != nil {
			msg += "（从服务器上连不上数据库：" + netErr.Error() + "）"
		}
		return &DBCred{User: c.user, Password: c.password, Found: true, From: c.from, Note: msg}
	}
	return &DBCred{User: cands[0].user, Note: "读到的密码都登录不上（" + strings.Join(froms, "、") + "），可能后来改过：请填写正确的密码"}
}

// serverReach 从服务器上连一个软件：容器的先连容器的地址，再连主机上的端口。
// 连不上的地址记下来，后面的密码不再试它（被防火墙丢包时每次要等十几秒）；连通过的（不管密码对不对）以后只用它。
type serverReach struct {
	eps  []reachEP
	dead map[int]bool
	good int // 连通过的地址；-1 = 还不知道
}

type reachEP struct {
	host, container string
	port            int
}

func newReach(container string, inner, port int) *serverReach {
	r := &serverReach{dead: map[int]bool{}, good: -1}
	if container != "" && inner > 0 {
		r.eps = append(r.eps, reachEP{container: container, port: inner})
	}
	if port > 0 {
		r.eps = append(r.eps, reachEP{host: "127.0.0.1", port: port})
	}
	return r
}

// try 用能用的地址试：fn 返回 (成功, 连不上时的错误)。都连不上时返回最后一个错误。
func (r *serverReach) try(fn func(ep reachEP) (bool, error)) (bool, error) {
	var last error
	for i, ep := range r.eps {
		if r.dead[i] || (r.good >= 0 && i != r.good) {
			continue
		}
		ok, err := fn(ep)
		if err != nil {
			r.dead[i] = true
			last = err
			continue
		}
		r.good = i
		return ok, nil
	}
	if last == nil {
		last = errors.New("不知道从服务器上连哪个地址（端口没在监听？）")
	}
	return false, last
}

// tryLogin 经这台服务器登录数据库试一下（和「经服务器连」一样走 SSH 转发）。
// 密码不对返回 (false, nil)；连不上数据库返回 (false, 错误)。
func (e *Engine) tryLogin(s *session, q DBCredQuery, r *serverReach, c dbCand) (bool, error) {
	kind := map[string]string{"mysql": dbx.MySQL, "mariadb": dbx.MySQL, "postgresql": dbx.Postgres, "redis": dbx.Redis}[q.Kind]
	if kind == "" {
		return false, errors.New("不认识的数据库")
	}
	tn := keepTunnel{&Tunnel{s: s}}
	open := func(*dbx.Via) (dbx.Tunnel, error) { return tn, nil }
	return r.try(func(p reachEP) (bool, error) {
		m := &dbx.Manager{}
		defer m.Close("")
		cn := &dbx.Conn{ID: "cred-try", Name: "试密码", Kind: kind, Host: p.host, Port: p.port, Container: p.container,
			User: c.user, Via: &dbx.Via{Config: "-", Target: "-"}}
		ctx, cancel := context.WithTimeout(e.ctx(), 10*time.Second)
		defer cancel()
		_, release, err := m.Open(ctx, cn, c.password, open)
		if err == nil {
			release()
			return true, nil
		}
		var ne *dbx.NeedPasswordError
		if errors.As(err, &ne) {
			return false, nil // 连上了，密码不对
		}
		return false, err
	})
}

// keepTunnel 试密码用的通道：用完不关到服务器的连接（withTarget 结束时关）。
type keepTunnel struct{ *Tunnel }

func (keepTunnel) Close() {}

// ---------------------------------------------------------------- 容器

type dockerInspect struct {
	Path   string   `json:"Path"`
	Args   []string `json:"Args"`
	Config struct {
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func dockerCands(q DBCredQuery, run func(string) (string, bool)) ([]dbCand, string) {
	out, ok := run("docker inspect " + shQuote(q.Container))
	var list []dockerInspect
	if !ok || json.Unmarshal([]byte(out), &list) != nil || len(list) == 0 {
		return nil, "读不到容器 " + q.Container + " 的配置：请填写"
	}
	in := list[0]
	env := map[string]string{}
	for _, kv := range in.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	// 1Panel、Docker Compose 的应用目录里的 .env：1Panel 里改密码时会改它（容器的环境变量还是建容器时的）
	dotenv := map[string]string{}
	envFile := ""
	if dir := in.Config.Labels["com.docker.compose.project.working_dir"]; strings.HasPrefix(dir, "/") && !strings.ContainsAny(dir, "\n\x00") {
		envFile = strings.TrimRight(dir, "/") + "/.env"
		if text, ok := run("cat " + shQuote(envFile)); ok {
			dotenv = parseDotenv(text)
		}
	}
	// 容器里的文件（*_FILE、Redis 配置文件）
	cat := func(file string) (string, bool) {
		if !strings.HasPrefix(file, "/") || strings.ContainsAny(file, "\n\x00") {
			return "", false
		}
		return run("docker exec " + shQuote(q.Container) + " head -c 65536 " + shQuote(file))
	}
	var cands []dbCand
	add := func(user, pw, from string) {
		for _, c := range cands {
			if c.user == user && c.password == pw {
				return
			}
		}
		cands = append(cands, dbCand{user, pw, from})
	}
	fromEnv := func(user string, names ...string) {
		for _, n := range names {
			if v, ok := env[n]; ok {
				add(user, v, "容器的环境变量 "+n)
			}
			if f := env[n+"_FILE"]; f != "" {
				if v, ok := cat(f); ok {
					add(user, strings.TrimRight(v, "\r\n"), "容器里的文件 "+f+"（"+n+"_FILE）")
				}
			}
		}
	}
	fromDotenv := func(user string, names ...string) {
		for _, n := range names {
			if v, ok := dotenv[n]; ok && v != "" {
				add(user, v, envFile+" 里的 "+n)
			}
		}
	}
	switch q.Kind {
	case "mysql", "mariadb":
		fromDotenv("root", "PANEL_DB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD")
		fromEnv("root", "MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD")
		if env["MYSQL_ALLOW_EMPTY_PASSWORD"] != "" || env["MARIADB_ALLOW_EMPTY_ROOT_PASSWORD"] != "" || env["MARIADB_ALLOW_EMPTY_PASSWORD"] != "" {
			add("root", "", "容器设置了 root 不要密码")
		}
		random := env["MYSQL_RANDOM_ROOT_PASSWORD"] != "" || env["MARIADB_RANDOM_ROOT_PASSWORD"] != ""
		if random {
			// 第一次启动时打在日志里：GENERATED ROOT PASSWORD: xxx
			if logs, ok := run("sh -c " + shQuote("docker logs "+shQuote(q.Container)+" 2>&1 | grep -m 1 'GENERATED ROOT PASSWORD'")); ok {
				if _, pw, ok := strings.Cut(strings.TrimSpace(logs), "GENERATED ROOT PASSWORD:"); ok && strings.TrimSpace(pw) != "" {
					add("root", strings.TrimSpace(pw), "容器第一次启动时的日志（随机生成的 root 密码）")
				}
			}
		}
		if user, ok := firstEnv(env, "MYSQL_USER", "MARIADB_USER"); ok {
			fromEnv(user, "MYSQL_PASSWORD", "MARIADB_PASSWORD")
		}
		if len(cands) == 0 {
			if random {
				return nil, "root 密码是容器第一次启动时随机生成的，日志里已经找不到了：请填写"
			}
			return nil, "容器的环境变量里没有 root 密码：请填写"
		}
	case "postgresql":
		user := firstNonEmptyStr(dotenv["PANEL_DB_ROOT_USER"], env["POSTGRES_USER"], "postgres")
		fromDotenv(user, "PANEL_DB_ROOT_PASSWORD", "POSTGRES_PASSWORD")
		fromEnv(user, "POSTGRES_PASSWORD")
		if strings.EqualFold(env["POSTGRES_HOST_AUTH_METHOD"], "trust") {
			add(user, "", "容器设置了不用密码登录（POSTGRES_HOST_AUTH_METHOD=trust）")
		}
		if len(cands) == 0 {
			return nil, "容器的环境变量里没有密码：请填写"
		}
	case "redis":
		fromDotenv("", "PANEL_REDIS_ROOT_PASSWORD", "REDIS_PASSWORD")
		args := append([]string{in.Path}, in.Args...)
		if pw, ok := redisArgPass(args); ok {
			add("", pw, "容器的启动命令（--requirepass）")
		}
		fromEnv("", "REDIS_PASSWORD")
		if conf := redisConfArg(args); conf != "" {
			if text, ok := cat(conf); ok {
				for _, c := range redisConfCands(text, "容器里的配置文件 "+conf, cat) {
					add(c.user, c.password, c.from)
				}
			} else if len(cands) == 0 {
				return nil, "读不到容器里的配置文件 " + conf + "：没设密码就留空，设了请填写"
			}
		}
		if len(cands) == 0 {
			add("", "", "启动命令和环境变量里都没有设密码")
		}
	default:
		return nil, "请填写账号密码"
	}
	return cands, ""
}

func firstEnv(env map[string]string, names ...string) (string, bool) {
	for _, n := range names {
		if v := env[n]; v != "" {
			return v, true
		}
	}
	return "", false
}

// parseDotenv KEY=VALUE（可以加引号、export 开头、# 注释）。
func parseDotenv(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// ---------------------------------------------------------------- 系统装的

var reExecArgv = regexp.MustCompile(`argv\[\]=([^;]*)`)

func hostCands(q DBCredQuery, sshUser string, run func(string) (string, bool)) ([]dbCand, string) {
	homes := []string{"/root"}
	if sshUser != "" && sshUser != "root" && reSafeName.MatchString(sshUser) {
		homes = append(homes, "/home/"+sshUser)
	}
	switch q.Kind {
	case "redis":
		var args []string
		if q.Unit != "" && reSafeName.MatchString(strings.TrimSuffix(q.Unit, ".service")) {
			if out, ok := run("systemctl show -p ExecStart --no-pager " + shQuote(q.Unit)); ok {
				if m := reExecArgv.FindStringSubmatch(out); m != nil {
					args = strings.Fields(m[1])
				}
			}
		}
		if q.PID > 0 { // redis-server 会改自己的进程标题，命令行里不一定还有配置文件
			if out, ok := run("cat /proc/" + strconv.Itoa(q.PID) + "/cmdline"); ok {
				args = append(args, strings.Split(strings.TrimRight(out, "\x00\n"), "\x00")...)
			}
		}
		var cands []dbCand
		if pw, ok := redisArgPass(args); ok {
			cands = append(cands, dbCand{"", pw, "启动命令（--requirepass）"})
		}
		files := []string{}
		if c := redisConfArg(args); c != "" {
			files = append(files, c)
		}
		port := q.Port
		if port <= 0 {
			port = 6379
		}
		p := strconv.Itoa(port)
		files = append(files, "/etc/redis/"+p+".conf", "/etc/redis/redis-"+p+".conf", "/etc/redis/redis_"+p+".conf",
			"/etc/redis.conf", "/etc/redis/redis.conf", "/usr/local/etc/redis.conf", "/www/server/redis/redis.conf")
		for _, f := range files {
			if text, ok := run("cat " + shQuote(f)); ok {
				return append(cands, redisConfCands(text, "配置文件 "+f, func(f string) (string, bool) { return run("cat " + shQuote(f)) })...), ""
			}
		}
		if len(cands) > 0 {
			return cands, ""
		}
		return nil, "没找到 Redis 的配置文件（或没有权限读）：没设密码就留空，设了请填写"
	case "mysql", "mariadb":
		var cands []dbCand
		for _, h := range homes {
			if text, ok := run("cat " + shQuote(h+"/.my.cnf")); ok {
				if user, pw, ok := parseMyCnf(text); ok {
					cands = append(cands, dbCand{firstNonEmptyStr(user, "root"), pw, h + "/.my.cnf"})
				}
			}
		}
		if len(cands) > 0 {
			return cands, ""
		}
		note := "服务器上没有保存 root 密码（~/.my.cnf）：请填写"
		if q.Kind == "mariadb" {
			note += "。系统装的 MariaDB 的 root 常常只能在服务器上登录（unix_socket，不用密码），这时要填一个能从网络登录的账号"
		}
		return nil, note
	case "postgresql":
		var cands []dbCand
		for _, h := range append(homes, "/var/lib/pgsql", "/var/lib/postgresql") {
			if text, ok := run("cat " + shQuote(h+"/.pgpass")); ok {
				if user, pw, ok := parsePgpass(text, q.Port); ok {
					cands = append(cands, dbCand{user, pw, h + "/.pgpass"})
				}
			}
		}
		if len(cands) > 0 {
			return cands, ""
		}
		return nil, "PostgreSQL 的 postgres 账号默认只能在服务器上登录、没有密码：请填写一个设了密码的账号"
	}
	return nil, "请填写账号密码"
}

// ---------------------------------------------------------------- 解析

// redisArgPass 启动命令里的 --requirepass。
func redisArgPass(args []string) (string, bool) {
	for i, a := range args {
		if a == "--requirepass" && i+1 < len(args) {
			return strings.Trim(args[i+1], `"'`), true
		}
		if v, ok := strings.CutPrefix(a, "--requirepass="); ok {
			return strings.Trim(v, `"'`), true
		}
	}
	return "", false
}

// redisConfArg 启动命令里的配置文件（redis-server /etc/redis.conf）。
func redisConfArg(args []string) string {
	for _, a := range args {
		if strings.HasSuffix(a, ".conf") && strings.HasPrefix(a, "/") {
			return a
		}
	}
	return ""
}

// redisConfCands 配置文件里最后一个 requirepass（include 的文件也看，一层）；没有 requirepass = 没设密码。
func redisConfCands(text, from string, read func(string) (string, bool)) []dbCand {
	pw, ok, includes := parseRedisConf(text)
	for _, inc := range includes {
		if t, rok := read(inc); rok {
			if p, ok2, _ := parseRedisConf(t); ok2 {
				pw, ok, from = p, true, from+" 引用的 "+inc
			}
		}
	}
	if ok {
		return []dbCand{{"", pw, from}}
	}
	return []dbCand{{"", "", from + "（没有设密码）"}}
}

func parseRedisConf(text string) (pass string, found bool, includes []string) {
	for _, line := range strings.Split(text, "\n") {
		f := redisConfFields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue
		}
		switch strings.ToLower(f[0]) {
		case "requirepass":
			pass, found = f[1], true
		case "include":
			if strings.HasPrefix(f[1], "/") && !strings.ContainsAny(f[1], "*?") {
				includes = append(includes, f[1])
			}
		}
	}
	return pass, found, includes
}

// redisConfFields 按空格拆开一行；"…" 和 '…' 里的空格不拆。
func redisConfFields(line string) []string {
	if line == "" || line[0] == '#' {
		return nil
	}
	var out []string
	var b strings.Builder
	quote := byte(0)
	in := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0 && ch == '\\' && quote == '"' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
		case quote != 0 && ch == quote:
			quote = 0
		case quote == 0 && (ch == '"' || ch == '\''):
			quote, in = ch, true
		case quote == 0 && (ch == ' ' || ch == '\t'):
			if in {
				out = append(out, b.String())
				b.Reset()
				in = false
			}
		default:
			b.WriteByte(ch)
			in = true
		}
	}
	if in {
		out = append(out, b.String())
	}
	return out
}

// parseMyCnf [client]、[mysql] 里的 user、password。
func parseMyCnf(text string) (user, pass string, ok bool) {
	section := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			continue
		}
		if section != "client" && section != "mysql" && section != "mysqladmin" {
			continue
		}
		k, v, has := strings.Cut(line, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		switch {
		case k == "user" && has:
			user = v
		case k == "password" && has:
			pass, ok = v, true
		}
	}
	return user, pass, ok
}

// parsePgpass host:port:db:user:password，端口对得上（或 *）的第一行；: 和 \ 用 \ 转义。
func parsePgpass(text string, port int) (user, pass string, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		var f []string
		var b strings.Builder
		for i := 0; i < len(line); i++ {
			switch {
			case line[i] == '\\' && i+1 < len(line):
				i++
				b.WriteByte(line[i])
			case line[i] == ':' && len(f) < 4:
				f = append(f, b.String())
				b.Reset()
			default:
				b.WriteByte(line[i])
			}
		}
		f = append(f, b.String())
		if len(f) != 5 || f[3] == "*" {
			continue
		}
		if port > 0 && f[1] != "*" && f[1] != strconv.Itoa(port) {
			continue
		}
		if h := f[0]; h != "*" && h != "localhost" && h != "127.0.0.1" && h != "::1" && !strings.HasPrefix(h, "/") {
			continue
		}
		return f[3], f[4], true
	}
	return "", "", false
}
