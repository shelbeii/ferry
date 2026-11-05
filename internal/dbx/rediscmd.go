package dbx

// Redis 命令行：像 redis-cli 一样一行一条命令。同一个窗口用同一条连接（SELECT、MULTI 都保留）。
// 只读的连接只许执行只读的命令（按 Redis 自己的 COMMAND INFO 判断）；危险的命令（FLUSHALL、KEYS……）先要用户确认。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCmdTimeout 命令行里一条命令最多执行这么久。
var RedisCmdTimeout = 60 * time.Second

const (
	redisReplyMax   = 1 << 20 // 回复里的文字一共最多这么多字节
	redisReplyItems = 10000   // 一层数组最多显示这么多项
)

// rsession 命令行窗口的连接：单独的一条（SELECT、CLIENT SETNAME 这些改了连接状态的，关的时候整条断开，不还给共用的连接池）。
type rsession struct {
	cli      *redis.Client
	conn     *redis.Conn
	db       int
	lastUsed time.Time
}

func (s *rsession) close() {
	s.conn.Close()
	s.cli.Close()
}

// SplitRedisArgs 把一行命令拆成参数（redis-cli 的规则）：空格隔开；"…" 里认 \n \t \" \\ \xHH；'…' 里只认 \'。
func SplitRedisArgs(line string) ([]string, error) {
	var out []string
	i := 0
	for {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
			i++
		}
		if i >= len(line) {
			return out, nil
		}
		var b strings.Builder
		inDQ, inSQ := false, false
		done := false
		for !done {
			if i >= len(line) {
				if inDQ || inSQ {
					return nil, errors.New("引号没有配对")
				}
				break
			}
			c := line[i]
			switch {
			case inDQ:
				switch {
				case c == '\\' && i+3 < len(line) && line[i+1] == 'x' && isHex(line[i+2]) && isHex(line[i+3]):
					n, _ := strconv.ParseUint(line[i+2:i+4], 16, 8)
					b.WriteByte(byte(n))
					i += 3
				case c == '\\' && i+1 < len(line):
					i++
					switch line[i] {
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					case 't':
						b.WriteByte('\t')
					case 'b':
						b.WriteByte('\b')
					case 'a':
						b.WriteByte('\a')
					default:
						b.WriteByte(line[i])
					}
				case c == '"':
					if i+1 < len(line) && !isSpace(line[i+1]) {
						return nil, errors.New("引号后面要有空格")
					}
					inDQ = false
					done = true
				default:
					b.WriteByte(c)
				}
			case inSQ:
				switch {
				case c == '\\' && i+1 < len(line) && line[i+1] == '\'':
					b.WriteByte('\'')
					i++
				case c == '\'':
					if i+1 < len(line) && !isSpace(line[i+1]) {
						return nil, errors.New("引号后面要有空格")
					}
					inSQ = false
					done = true
				default:
					b.WriteByte(c)
				}
			default:
				switch {
				case isSpace(c):
					done = true
				case c == '"' && b.Len() == 0:
					inDQ = true
				case c == '\'' && b.Len() == 0:
					inSQ = true
				default:
					b.WriteByte(c)
				}
			}
			i++
		}
		out = append(out, b.String())
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// RedisReply 命令的回复：str（文字）、int、nil、err、arr（Items；More 是没显示的项数）。
type RedisReply struct {
	T     string       `json:"t"`
	V     string       `json:"v,omitempty"`
	Bin   bool         `json:"bin,omitempty"` // V 是转义过的二进制
	Items []RedisReply `json:"items,omitempty"`
	More  int          `json:"more,omitempty"`
	Cut   bool         `json:"cut,omitempty"` // 文字太长，只显示了前面一部分
}

// RedisCmdResult 执行一条命令的结果。Confirm 不为空时没有执行：要用户确认（说明在 Confirm 里）后带上确认再执行。
type RedisCmdResult struct {
	Reply   *RedisReply `json:"reply,omitempty"`
	DB      int         `json:"db"`
	Ms      int64       `json:"ms"`
	Confirm string      `json:"confirm,omitempty"`
}

// 命令行里不能用的：会一直占着连接、等推送
var redisBlocked = map[string]string{
	"SUBSCRIBE": "订阅", "PSUBSCRIBE": "订阅", "SSUBSCRIBE": "订阅", "MONITOR": "监视", "SYNC": "主从同步", "PSYNC": "主从同步",
}

// 只读连接里另外允许的（Redis 没有把它们标成只读，但只是查看）
var redisReadSafe = map[string]bool{
	"PING": true, "ECHO": true, "INFO": true, "TIME": true, "SELECT": true, "COMMAND": true, "LASTSAVE": true, "ROLE": true,
	"CONFIG GET": true, "SLOWLOG GET": true, "SLOWLOG LEN": true, "CLIENT LIST": true, "CLIENT INFO": true, "CLIENT GETNAME": true, "CLIENT ID": true,
	"MEMORY USAGE": true, "MEMORY STATS": true, "MEMORY DOCTOR": true, "LATENCY LATEST": true, "LATENCY HISTORY": true, "LATENCY DOCTOR": true,
	"OBJECT ENCODING": true, "OBJECT FREQ": true, "OBJECT IDLETIME": true, "OBJECT REFCOUNT": true, "ACL WHOAMI": true, "ACL CAT": true,
	"XINFO STREAM": true, "XINFO GROUPS": true, "XINFO CONSUMERS": true, "PUBSUB CHANNELS": true, "PUBSUB NUMSUB": true, "PUBSUB NUMPAT": true,
}

// 有子命令的命令（判断只读、危险时按「命令 子命令」）
var redisContainers = map[string]bool{
	"CONFIG": true, "CLIENT": true, "OBJECT": true, "MEMORY": true, "XINFO": true, "ACL": true, "CLUSTER": true, "SCRIPT": true,
	"FUNCTION": true, "COMMAND": true, "MODULE": true, "SLOWLOG": true, "LATENCY": true, "PUBSUB": true, "XGROUP": true,
}

// RedisDanger 执行前要多问一句的命令：返回说明；不危险时为空。
func RedisDanger(args []string) string {
	if len(args) == 0 {
		return ""
	}
	name := strings.ToUpper(args[0])
	sub := ""
	if len(args) > 1 {
		sub = strings.ToUpper(args[1])
	}
	switch name {
	case "FLUSHALL":
		return "会删掉这个 Redis 里所有库的全部键，删了就找不回来"
	case "FLUSHDB":
		return "会删掉这个库的全部键，删了就找不回来"
	case "SHUTDOWN":
		return "会关掉 Redis 服务，用它的程序都会连不上"
	case "DEBUG":
		return "调试命令，可能让 Redis 卡住或者崩溃"
	case "SWAPDB":
		return "会把两个库的数据整个对调"
	case "KEYS":
		return "键很多时会让 Redis 卡住一阵子，这期间别的程序都要等。左边的键列表用的是不会卡的 SCAN"
	case "SAVE":
		return "存盘期间 Redis 会停下来，别的程序都要等（BGSAVE 在后台存，不会卡）"
	case "REPLICAOF", "SLAVEOF", "FAILOVER":
		return "会改主从关系，这台 Redis 的数据可能被主库的覆盖"
	case "MIGRATE":
		return "会把键搬到另一台 Redis，并从这里删掉"
	case "CONFIG":
		if sub == "SET" || sub == "REWRITE" || sub == "RESETSTAT" {
			return "会改 Redis 的配置"
		}
	case "ACL":
		switch sub {
		case "SETUSER", "DELUSER", "LOAD", "SAVE", "DRYRUN":
			return "会改 Redis 的账号和权限"
		}
	case "CLIENT":
		if sub == "KILL" {
			return "会断开别的程序的连接"
		}
		if sub == "PAUSE" {
			return "会让所有程序的请求都停下来等"
		}
	case "SCRIPT":
		if sub == "FLUSH" {
			return "会删掉 Redis 里缓存的全部脚本"
		}
	case "FUNCTION":
		if sub == "FLUSH" || sub == "DELETE" || sub == "RESTORE" {
			return "会删掉或替换 Redis 里的函数"
		}
	case "MODULE":
		if sub == "LOAD" || sub == "UNLOAD" || sub == "LOADEX" {
			return "会加载或卸载 Redis 的模块"
		}
	case "CLUSTER":
		switch sub {
		case "RESET", "FAILOVER", "FORGET", "MEET", "ADDSLOTS", "DELSLOTS", "SETSLOT", "FLUSHSLOTS", "REPLICATE", "ADDSLOTSRANGE", "DELSLOTSRANGE":
			return "会改集群的节点或槽"
		}
	}
	return ""
}

// redisReadOnlyOK 只读连接能不能执行：Redis 标了只读（COMMAND INFO 的 readonly），或者在 redisReadSafe 里。
func (cl *Client) redisReadOnlyOK(ctx context.Context, r *redis.Client, args []string) bool {
	name := strings.ToUpper(args[0])
	full := name
	if redisContainers[name] && len(args) > 1 {
		full = name + " " + strings.ToUpper(args[1])
	}
	if redisReadSafe[full] || redisReadSafe[name] && (full == name || name == "COMMAND") {
		return true
	}
	cl.mu.Lock()
	v, ok := cl.rcmdRO[full]
	cl.mu.Unlock()
	if ok {
		return v
	}
	ro := false
	names := []string{strings.ToLower(name)}
	if full != name {
		names = []string{strings.ToLower(name) + "|" + strings.ToLower(args[1]), strings.ToLower(name)}
	}
	for _, n := range names {
		res, err := r.Do(ctx, "COMMAND", "INFO", n).Slice()
		if err != nil || len(res) == 0 || res[0] == nil {
			continue
		}
		info, _ := res[0].([]any)
		if len(info) < 3 {
			continue
		}
		flags, _ := info[2].([]any)
		hasRO, hasWrite := false, false
		for _, f := range flags {
			s, _ := f.(string)
			hasRO = hasRO || s == "readonly"
			hasWrite = hasWrite || s == "write" || s == "admin"
		}
		ro = hasRO && !hasWrite
		break
	}
	cl.mu.Lock()
	if cl.rcmdRO == nil {
		cl.rcmdRO = map[string]bool{}
	}
	cl.rcmdRO[full] = ro
	cl.mu.Unlock()
	return ro
}

// RedisCommand 在命令行窗口 sid 里执行一行命令。db 是窗口现在的库（SELECT 后跟着变，结果里的 DB 是执行后的库）。
func (cl *Client) RedisCommand(ctx context.Context, sid string, db int, line string, confirmed bool) (*RedisCmdResult, error) {
	start := time.Now()
	args, err := SplitRedisArgs(line)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, errors.New("没有命令")
	}
	name := strings.ToUpper(args[0])
	if what, ok := redisBlocked[name]; ok {
		return nil, fmt.Errorf("命令行窗口不支持 %s（%s会一直占着连接）", name, what)
	}
	r, err := cl.Redis(ctx, db)
	if err != nil {
		return nil, err
	}
	if cl.Conn.ReadOnly && !cl.redisReadOnlyOK(ctx, r, args) {
		return nil, fmt.Errorf("这个连接是只读的，不能执行 %s", name)
	}
	if d := RedisDanger(args); d != "" && !confirmed {
		return &RedisCmdResult{DB: db, Confirm: d}, nil
	}
	if name == "QUIT" || name == "RESET" {
		cl.dropRedisSession(sid)
		return &RedisCmdResult{Reply: &RedisReply{T: "str", V: "OK"}, DB: cl.redisDefault(), Ms: time.Since(start).Milliseconds()}, nil
	}
	var conn *redis.Conn
	if sid == "" {
		// 不在窗口里：临时连一条，用完就断
		cli := cl.newRedis(db, 1)
		defer cli.Close()
		conn = cli.Conn()
		defer conn.Close()
	} else if conn, err = cl.redisSession(ctx, sid, db); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, RedisCmdTimeout)
	defer cancel()
	xs := make([]any, len(args))
	for i, a := range args {
		xs[i] = a
	}
	v, err := conn.Do(cctx, xs...).Result()
	out := &RedisCmdResult{DB: db}
	var re redis.Error
	switch {
	case err == nil:
		budget := redisReplyMax
		out.Reply = toReplyN(v, &budget, redisReplyItems)
		if name == "SELECT" && len(args) > 1 {
			if n, err := strconv.Atoi(args[1]); err == nil {
				out.DB = n
				cl.mu.Lock()
				if s := cl.rsess[sid]; s != nil {
					s.db = n
				}
				cl.mu.Unlock()
			}
		}
	case errors.Is(err, redis.Nil):
		out.Reply = &RedisReply{T: "nil"}
	case errors.As(err, &re) && !authFailed(err):
		out.Reply = &RedisReply{T: "err", V: err.Error()}
	default:
		// 连接断了、超时：这个窗口的连接不要了（下次重新连，库回到窗口记着的库）
		cl.dropRedisSession(sid)
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("执行了 %d 秒还没结束，已经断开；Redis 可能还在执行", int(RedisCmdTimeout.Seconds()))
		}
		return nil, cl.explain(err)
	}
	out.Ms = time.Since(start).Milliseconds()
	return out, nil
}

func (cl *Client) redisDefault() int {
	n, _ := cl.redisIndex("")
	return n
}

// redisSession 命令行窗口的连接；窗口的库变了（或者连接断过）时重新取。
func (cl *Client) redisSession(ctx context.Context, sid string, db int) (*redis.Conn, error) {
	if sid == "" {
		return nil, errors.New("没有命令行窗口")
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.closed {
		return nil, errors.New("连接已关闭，请重试")
	}
	s := cl.rsess[sid]
	if s != nil && s.db != db {
		s.close()
		delete(cl.rsess, sid)
		s = nil
	}
	if s != nil {
		s.lastUsed = time.Now()
		return s.conn, nil
	}
	cli := cl.newRedis(db, 1)
	s = &rsession{cli: cli, conn: cli.Conn(), db: db, lastUsed: time.Now()}
	if cl.rsess == nil {
		cl.rsess = map[string]*rsession{}
	}
	cl.rsess[sid] = s
	return s.conn, nil
}

func (cl *Client) dropRedisSession(sid string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if s := cl.rsess[sid]; s != nil {
		s.close()
		delete(cl.rsess, sid)
	}
}

// FormatRedisReply 像 redis-cli 那样把回复排成文字（和桌面版的 formatReply 一样）。
func FormatRedisReply(r *RedisReply) string {
	return strings.Join(formatReply(r), "\n")
}

func formatReply(r *RedisReply) []string {
	if r == nil {
		return nil
	}
	switch r.T {
	case "nil":
		return []string{"(nil)"}
	case "int":
		return []string{"(integer) " + r.V}
	case "err":
		return []string{"(error) " + r.V}
	case "str":
		cut := ""
		if r.Cut {
			cut = " …（太长，只给了前面一部分）"
		}
		if !r.Bin && strings.Contains(r.V, "\n") {
			return strings.Split(strings.TrimSuffix(strings.ReplaceAll(r.V, "\r\n", "\n"), "\n")+cut, "\n")
		}
		if !r.Bin && (r.V == "OK" || r.V == "PONG" || r.V == "QUEUED") {
			return []string{r.V}
		}
		v := r.V
		if !r.Bin {
			v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\t", `\t`, "\r", `\r`).Replace(v)
		}
		return []string{`"` + v + `"` + cut}
	case "arr":
		if len(r.Items) == 0 && r.More == 0 {
			return []string{"(empty array)"}
		}
		w := len(strconv.Itoa(len(r.Items)))
		var out []string
		for i := range r.Items {
			head := fmt.Sprintf("%*d) ", w, i+1)
			pad := strings.Repeat(" ", len(head))
			for j, l := range formatReply(&r.Items[i]) {
				if j == 0 {
					out = append(out, head+l)
				} else {
					out = append(out, pad+l)
				}
			}
		}
		if r.More > 0 {
			out = append(out, fmt.Sprintf("…（还有 %d 项没给）", r.More))
		}
		return out
	}
	return nil
}

// toReplyN 回复转成界面显示的样子：文字一共最多 *budget 字节，一层数组最多 items 项。
func toReplyN(v any, budget *int, items int) *RedisReply {
	switch x := v.(type) {
	case nil:
		return &RedisReply{T: "nil"}
	case int64:
		return &RedisReply{T: "int", V: strconv.FormatInt(x, 10)}
	case string:
		rv := rval(x, max(*budget, 0))
		*budget -= len(x)
		return &RedisReply{T: "str", V: rv.T, Bin: rv.Bin, Cut: rv.Size > 0}
	case []any:
		out := &RedisReply{T: "arr", Items: []RedisReply{}}
		for i, it := range x {
			if i >= items || *budget <= 0 {
				out.More = len(x) - i
				break
			}
			out.Items = append(out.Items, *toReplyN(it, budget, items))
		}
		return out
	case redis.Error:
		return &RedisReply{T: "err", V: x.Error()}
	case error:
		return &RedisReply{T: "err", V: x.Error()}
	default:
		return &RedisReply{T: "str", V: fmt.Sprint(x)}
	}
}
