package dbx

// AI 用 Redis（编程软件的 MCP、Ferry 的 AI 助手）：
//   - 查：一次一条只读的命令（Redis 自己标成只读的，见 redisReadOnlyOK），最多 15 秒，回复最多约 48 KB；
//   - 改：一次最多 100 条命令，用户看过同意后用 MULTI/EXEC 一起执行（中间不会插进别人的命令）。
//
// 不管查还是改，都不给 AI 用的：看密码和配置（CONFIG、ACL）、别的连接（CLIENT）、清库、关服务、执行脚本、切库、会一直等的命令……

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// AIRedisTimeout AI 的一条查询最多执行这么久。
var AIRedisTimeout = 15 * time.Second

// AIRedisWriteTimeout AI 的一批改动最多执行这么久。
var AIRedisWriteTimeout = 60 * time.Second

const (
	aiRedisReply = 48 << 10 // 回复里的文字一共最多这么多字节
	aiRedisItems = 500      // 一层数组最多这么多项
)

// 不给 AI 用的命令和原因
var aiRedisDeny = map[string]string{
	"KEYS":         "键多时会卡住 Redis，用 SCAN（如 SCAN 0 MATCH user:* COUNT 200）",
	"CONFIG":       "配置里有密码",
	"ACL":          "账号和权限",
	"AUTH":         "登录",
	"HELLO":        "登录",
	"CLIENT":       "别的程序的连接",
	"SLOWLOG":      "里面有别的程序执行的命令",
	"MONITOR":      "会一直占着连接",
	"SUBSCRIBE":    "会一直占着连接",
	"PSUBSCRIBE":   "会一直占着连接",
	"SSUBSCRIBE":   "会一直占着连接",
	"SYNC":         "主从同步",
	"PSYNC":        "主从同步",
	"REPLCONF":     "主从同步",
	"SELECT":       "用 database 参数选库",
	"SWAPDB":       "会对调两个库",
	"MOVE":         "不能跨库",
	"MIGRATE":      "会把键搬到别的 Redis",
	"FLUSHALL":     "会清空全部库",
	"FLUSHDB":      "会清空整个库；要清空请用户在 Ferry 的「数据库」里做",
	"MULTI":        "Ferry 会把这批改动放在一起执行，不用写 MULTI、EXEC",
	"EXEC":         "Ferry 会把这批改动放在一起执行，不用写 MULTI、EXEC",
	"DISCARD":      "Ferry 管事务",
	"WATCH":        "Ferry 管事务",
	"UNWATCH":      "Ferry 管事务",
	"EVAL":         "不能执行脚本",
	"EVALSHA":      "不能执行脚本",
	"EVAL_RO":      "不能执行脚本",
	"EVALSHA_RO":   "不能执行脚本",
	"FCALL":        "不能执行函数",
	"FCALL_RO":     "不能执行函数",
	"SCRIPT":       "不能执行脚本",
	"FUNCTION":     "不能执行函数",
	"MODULE":       "模块",
	"CLUSTER":      "集群",
	"REPLICAOF":    "主从关系",
	"SLAVEOF":      "主从关系",
	"FAILOVER":     "主从关系",
	"SHUTDOWN":     "会关掉 Redis",
	"DEBUG":        "调试命令",
	"SAVE":         "会让 Redis 停下来存盘",
	"BGSAVE":       "存盘",
	"BGREWRITEAOF": "存盘",
	"WAIT":         "会一直等",
	"WAITAOF":      "会一直等",
	"RESET":        "会重置连接",
	"QUIT":         "会断开连接",
	"BLPOP":        "会一直等",
	"BRPOP":        "会一直等",
	"BRPOPLPUSH":   "会一直等",
	"BLMOVE":       "会一直等",
	"BLMPOP":       "会一直等",
	"BZPOPMIN":     "会一直等",
	"BZPOPMAX":     "会一直等",
	"BZMPOP":       "会一直等",
}

// aiRedisArgs 拆一条命令并检查是不是给 AI 用的。
func aiRedisArgs(line string) ([]string, error) {
	args, err := SplitRedisArgs(line)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", line, err)
	}
	if len(args) == 0 {
		return nil, errors.New("没有命令")
	}
	name := strings.ToUpper(args[0])
	if why, ok := aiRedisDeny[name]; ok {
		return nil, fmt.Errorf("不能用 %s（%s）", name, why)
	}
	for _, a := range args[1:] {
		u := strings.ToUpper(a)
		if (name == "XREAD" || name == "XREADGROUP") && u == "BLOCK" {
			return nil, fmt.Errorf("%s 不能带 BLOCK（会一直等）", name)
		}
		if name == "COPY" && u == "DB" {
			return nil, errors.New("COPY 不能复制到别的库")
		}
	}
	return args, nil
}

// CheckAIRedisRead 查的命令：只能一条；是不是只读的执行前再按 Redis 的标记查（AIRedisQuery）。
func CheckAIRedisRead(line string) ([]string, error) {
	line = strings.TrimSpace(line)
	if strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("一次只能执行一条命令")
	}
	return aiRedisArgs(line)
}

// CheckAIRedisWrite 改的命令：一行一条，最多 AIMaxStmts 条。返回去掉空行后的命令。
func CheckAIRedisWrite(src string) ([]string, error) {
	if len(src) > aiMaxSQL {
		return nil, fmt.Errorf("命令太长了（最多 %d KB）", aiMaxSQL>>10)
	}
	var lines []string
	for _, l := range strings.Split(src, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, CheckAIRedisWriteLines(lines)
}

// CheckAIRedisWriteLines 检查一批改动的命令。
func CheckAIRedisWriteLines(lines []string) error {
	if len(lines) == 0 {
		return errors.New("没有要执行的命令")
	}
	if len(lines) > AIMaxStmts {
		return fmt.Errorf("一次最多 %d 条命令", AIMaxStmts)
	}
	for i, l := range lines {
		if strings.ContainsAny(l, "\r\n") {
			return fmt.Errorf("第 %d 条：一行一条命令", i+1)
		}
		if _, err := aiRedisArgs(l); err != nil {
			return fmt.Errorf("第 %d 条：%w", i+1, err)
		}
	}
	return nil
}

// 会删掉键或里面的数据的命令
var aiRedisRemove = map[string]bool{
	"DEL": true, "UNLINK": true, "GETDEL": true, "HDEL": true, "SREM": true, "SPOP": true, "ZREM": true, "ZREMRANGEBYSCORE": true,
	"ZREMRANGEBYRANK": true, "ZREMRANGEBYLEX": true, "ZPOPMIN": true, "ZPOPMAX": true, "ZMPOP": true, "LREM": true, "LTRIM": true,
	"LPOP": true, "RPOP": true, "LMPOP": true, "XDEL": true, "XTRIM": true, "HGETDEL": true,
}

// AIRedisDanger 确认框里要标出来的危险的地方；没有时为空。
func AIRedisDanger(lines []string) string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, l := range lines {
		args, err := SplitRedisArgs(l)
		if err != nil || len(args) == 0 {
			continue
		}
		name := strings.ToUpper(args[0])
		switch {
		case aiRedisRemove[name]:
			add("会删掉键或里面的数据（" + name + "）")
		case name == "RENAME":
			add("RENAME 会覆盖已有的同名键")
		}
	}
	return strings.Join(out, "；")
}

// AIRedisNotes 确认框里的说明。
func AIRedisNotes() []string {
	return []string{"这批命令用 MULTI/EXEC 一起按顺序执行，中间不会插进别人的命令",
		"Redis 没有回滚：有命令写错时一条都不执行；执行时出错的那条不生效，别的照常生效"}
}

// AIRedisResult AI 查询的结果。
type AIRedisResult struct {
	Reply *RedisReply `json:"-"`
	// Text 像 redis-cli 那样排好的回复（给 AI 看，比 JSON 省）
	Text string `json:"reply"`
	Note string `json:"note,omitempty"`
	Ms   int64  `json:"ms"`
}

// AIRedisQuery 执行一条只读的命令。
func (cl *Client) AIRedisQuery(ctx context.Context, dbName, line string) (*AIRedisResult, error) {
	start := time.Now()
	args, err := CheckAIRedisRead(line)
	if err != nil {
		return nil, err
	}
	r, err := cl.redisOf(ctx, dbName)
	if err != nil {
		return nil, err
	}
	if !cl.redisReadOnlyOK(ctx, r, args) {
		return nil, fmt.Errorf("只能执行只读的命令（GET、HGETALL、LRANGE、SCAN、TYPE、TTL……），%s 会改数据：要改的话用 redis_execute，或者请用户在 Ferry 的「数据库」里改", strings.ToUpper(args[0]))
	}
	cctx, cancel := context.WithTimeout(ctx, AIRedisTimeout)
	defer cancel()
	v, err := r.Do(cctx, toAny(args)...).Result()
	out := &AIRedisResult{}
	var re redis.Error
	switch {
	case err == nil:
		budget := aiRedisReply
		out.Reply = toReplyN(v, &budget, aiRedisItems)
		if cutReply(out.Reply) {
			out.Note = "回复太长，只给了前面一部分：用 SCAN、HSCAN、LRANGE 这类分批的命令，或者加 COUNT、范围"
		}
	case errors.Is(err, redis.Nil):
		out.Reply = &RedisReply{T: "nil"}
	case errors.As(err, &re) && !authFailed(err):
		out.Reply = &RedisReply{T: "err", V: err.Error()}
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("执行了 %d 秒还没结束，已经断开", int(AIRedisTimeout.Seconds()))
	default:
		return nil, cl.explain(err)
	}
	out.Text = FormatRedisReply(out.Reply)
	out.Ms = time.Since(start).Milliseconds()
	return out, nil
}

// AIRedisExec 执行用户同意了的一批改动（MULTI/EXEC）。
func (cl *Client) AIRedisExec(ctx context.Context, dbName string, lines []string) (*AIExecResult, error) {
	start := time.Now()
	if cl.Conn.ReadOnly {
		return nil, ErrRedisReadOnly
	}
	if err := CheckAIRedisWriteLines(lines); err != nil {
		return nil, err
	}
	argv := make([][]string, len(lines))
	for i, l := range lines {
		argv[i], _ = aiRedisArgs(l)
	}
	db, err := cl.redisIndex(dbName)
	if err != nil {
		return nil, err
	}
	// 单独连一条：MULTI 的状态不留在共用的连接上
	cli := cl.newRedis(db, 1)
	defer cli.Close()
	cctx, cancel := context.WithTimeout(ctx, AIRedisWriteTimeout)
	defer cancel()
	var cmds []*redis.Cmd
	_, err = cli.TxPipelined(cctx, func(p redis.Pipeliner) error {
		for _, a := range argv {
			cmds = append(cmds, p.Do(cctx, toAny(a)...))
		}
		return nil
	})
	out := &AIExecResult{Results: []AIStmtResult{}}
	var re redis.Error
	if err != nil && !errors.As(err, &re) {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("执行了 %d 秒还没结束，已经断开；改动可能已经执行了，请查一下", int(AIRedisWriteTimeout.Seconds()))
		}
		return nil, cl.explain(err)
	}
	aborted := err != nil && strings.Contains(err.Error(), "EXECABORT")
	failed := 0
	for i, c := range cmds {
		res := AIStmtResult{SQL: lines[i]}
		v, err := c.Result()
		switch {
		case aborted:
			if err != nil && !strings.Contains(err.Error(), "EXECABORT") {
				res.Error = err.Error()
			}
		case err == nil:
			budget := aiRedisReply / max(len(cmds), 1)
			res.Reply = toReplyN(v, &budget, 50)
		case errors.Is(err, redis.Nil):
			res.Reply = &RedisReply{T: "nil"}
		default:
			res.Error = err.Error()
			failed++
		}
		if res.Reply != nil {
			res.ReplyText = FormatRedisReply(res.Reply)
		}
		out.Results = append(out.Results, res)
	}
	switch {
	case aborted:
		out.Note = "有命令写错了，Redis 一条都没有执行"
		// 标出是哪一条（排队时就报错的那条）
		for i := range out.Results {
			if out.Results[i].Error != "" {
				failed++
			}
		}
		if failed == 0 && len(out.Results) > 0 {
			out.Results[len(out.Results)-1].Error = err.Error()
		}
	case failed > 0:
		out.Committed = true
		out.Note = fmt.Sprintf("有 %d 条执行出错，没有生效；Redis 没有回滚，别的已经生效了", failed)
	default:
		out.OK, out.Committed = true, true
		out.Note = fmt.Sprintf("%d 条命令都执行了", len(lines))
	}
	out.Ms = time.Since(start).Milliseconds()
	return out, nil
}

func toAny(args []string) []any {
	xs := make([]any, len(args))
	for i, a := range args {
		xs[i] = a
	}
	return xs
}

func cutReply(r *RedisReply) bool {
	if r == nil {
		return false
	}
	if r.Cut || r.More > 0 {
		return true
	}
	for i := range r.Items {
		if cutReply(&r.Items[i]) {
			return true
		}
	}
	return false
}
