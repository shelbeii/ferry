package dbx

// Redis：库（db0、db1……）、键的浏览（SCAN，不卡 Redis）、看和改值、命令行（rediscmd.go）。
// 键和值都是字节串：能当文字显示的原样给界面；不能的（二进制）转义后显示，另带 base64 用来指定它（删、改别的字段时）。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

const (
	redisKeysDefault = 500
	redisKeysMax     = 2000
	redisPageDefault = 200
	redisPageMax     = 1000
	// 字符串值超过这么长只显示前面一部分（不能在界面上改）
	redisStringMax = 1 << 20
	// 集合、哈希……里的一个元素超过这么长只显示前面一部分
	redisItemMax = 64 << 10
	redisTimeout = 30 * time.Second
	// 列键、翻元素时一次最多扫这么久（键很多、大多不匹配时分几次扫）
	redisScanBudget = 3 * time.Second
)

// redisIndex 库的编号："" → 连接的默认库（没填是 0）；也认 "db3"。
func (cl *Client) redisIndex(name string) (int, error) {
	if name == "" {
		name = cl.Conn.Database
	}
	return ParseRedisDB(name)
}

func ParseRedisDB(name string) (int, error) {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "db")
	if name == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(name)
	if err != nil || n < 0 || n > 100000 {
		return 0, fmt.Errorf("Redis 的库是编号（0、1、2……），不是 %q", name)
	}
	return n, nil
}

// Redis 某个库的连接池。
func (cl *Client) Redis(ctx context.Context, db int) (*redis.Client, error) {
	if cl.Conn.Kind != Redis {
		return nil, errors.New("不是 Redis 连接")
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.closed {
		return nil, errors.New("连接已关闭，请重试")
	}
	if r := cl.rdbs[db]; r != nil {
		return r, nil
	}
	r := cl.newRedis(db, 4)
	if cl.rdbs == nil {
		cl.rdbs = map[int]*redis.Client{}
	}
	cl.rdbs[db] = r
	return r, nil
}

// newRedis 连某个库的连接池（pool 条连接）。
func (cl *Client) newRedis(db, pool int) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     cl.addr,
		Username: cl.Conn.User,
		Password: cl.password,
		DB:       db,
		Dialer: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := cl.dial(ctx)
			if err != nil {
				return nil, err
			}
			return cl.redisTLS(ctx, c)
		},
		// RESP2：老版本也认；不发 CLIENT SETINFO、不开维护通知（托管的 Redis 常把这些命令禁掉）
		Protocol:                 2,
		DisableIdentity:          true,
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
		// 不自动重试：LPUSH 这类命令重试会做两次
		MaxRetries:            -1,
		DialTimeout:           dialTimeout,
		ReadTimeout:           redisTimeout,
		WriteTimeout:          redisTimeout,
		ContextTimeoutEnabled: true,
		PoolSize:              pool,
		MaxIdleConns:          min(pool, 2),
		ConnMaxIdleTime:       5 * time.Minute,
	})
}

// redisPing 打开连接时连一次：密码不对、连不上时现在就报错；顺便读版本。
func (cl *Client) redisPing(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout+5*time.Second)
	defer cancel()
	db, err := cl.redisIndex("")
	if err != nil {
		return err
	}
	r, err := cl.Redis(ctx, db)
	if err != nil {
		return err
	}
	cl.Encrypted = cl.tlsMode() != TLSOff
	info, err := r.Info(ctx, "server").Result()
	if err != nil {
		var re redis.Error
		if !errors.As(err, &re) || authFailed(err) {
			return cl.explain(err)
		}
		// INFO 被禁用了（托管的 Redis 会改名、禁掉一些命令）：能 PING 通就行
		if err := r.Ping(ctx).Err(); err != nil {
			return cl.explain(err)
		}
		cl.Version = "Redis"
		return nil
	}
	m := parseInfo(info)
	name, v := "Redis", m["redis_version"]
	if m["server_name"] == "valkey" {
		name = "Valkey"
		if x := m["valkey_version"]; x != "" {
			v = x
		}
	}
	cl.Version = strings.TrimSpace(name + " " + v)
	if m["redis_mode"] == "cluster" {
		cl.redisCluster = true
		cl.Version += "（集群模式，只看这一个节点）"
	}
	return nil
}

// parseInfo INFO 的「名字:值」。
func parseInfo(s string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			m[k] = v
		}
	}
	return m
}

// redisDatabases db0…dbN 和各自的键数（INFO keyspace）。
func (cl *Client) redisDatabases(ctx context.Context) ([]Database, error) {
	db, err := cl.redisIndex("")
	if err != nil {
		return nil, err
	}
	r, err := cl.Redis(ctx, db)
	if err != nil {
		return nil, err
	}
	counts := map[int]int64{}
	if s, err := r.Info(ctx, "keyspace").Result(); err == nil {
		for k, v := range parseInfo(s) {
			n, err := ParseRedisDB(k)
			if err != nil || !strings.HasPrefix(k, "db") {
				continue
			}
			for _, part := range strings.Split(v, ",") {
				if x, ok := strings.CutPrefix(part, "keys="); ok {
					counts[n], _ = strconv.ParseInt(x, 10, 64)
				}
			}
		}
	} else if authFailed(err) {
		return nil, cl.explain(err)
	}
	n := 16
	if cl.redisCluster {
		n = 1
	} else if v, err := r.ConfigGet(ctx, "databases").Result(); err == nil {
		if x, err := strconv.Atoi(v["databases"]); err == nil && x > 0 {
			n = x
		}
	}
	for i := range counts {
		if i >= n {
			n = i + 1
		}
	}
	if db >= n {
		n = db + 1
	}
	n = min(n, 1024)
	out := make([]Database, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Database{Name: strconv.Itoa(i), Keys: counts[i]})
	}
	return out, nil
}

// ---------------------------------------------------------------- 字节串

// RStr 界面传来的键、字段、成员：文字，或二进制时的 base64（优先）。
type RStr struct {
	T   string `json:"t"`
	B64 string `json:"b64,omitempty"`
}

func (s RStr) raw() (string, error) {
	if s.B64 != "" {
		b, err := base64.StdEncoding.DecodeString(s.B64)
		if err != nil {
			return "", errors.New("二进制的键或值格式不对")
		}
		return string(b), nil
	}
	return s.T, nil
}

// RedisVal 显示用的值。T 是文字（二进制时是转义后的，如 \x00）；Size 不为 0 时值被截断了，原来有 Size 字节。
type RedisVal struct {
	T    string `json:"t"`
	B64  string `json:"b64,omitempty"`
	Bin  bool   `json:"bin,omitempty"`
	Size int    `json:"size,omitempty"`
}

func rval(s string, max int) *RedisVal {
	v := &RedisVal{}
	show := s
	if len(s) > max {
		show = s[:max]
		v.Size = len(s)
		// 截在一个字的中间：去掉残缺的尾巴
		for i := 0; i < 3 && len(show) > 0 && !utf8.ValidString(show); i++ {
			show = show[:len(show)-1]
		}
	}
	if printable(show) {
		v.T = show
		return v
	}
	v.Bin = true
	v.T = escapeBytes(show)
	if v.Size == 0 {
		v.B64 = base64.StdEncoding.EncodeToString([]byte(s))
	}
	return v
}

// printable 能当文字显示（合法 UTF-8，除了换行、制表符没有控制字符）。
func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r == utf8.RuneError || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// escapeBytes 像 redis-cli 那样转义：\n、\t、\"、\\、\xHH；能显示的 UTF-8 字原样。
func escapeBytes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x80 && (r < 0x20 || r == 0x7f), unicode.IsControl(r):
			for _, c := range []byte(s[i : i+n]) {
				fmt.Fprintf(&b, `\x%02x`, c)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// ---------------------------------------------------------------- 列键

// RedisKey 键列表里的一项。
type RedisKey struct {
	Name string `json:"name"`
	B64  string `json:"b64,omitempty"` // 键不是文字时：原来的字节
	Type string `json:"type"`
}

func rkey(s, typ string) RedisKey {
	if printable(s) && !strings.ContainsAny(s, "\r\n\t") {
		return RedisKey{Name: s, Type: typ}
	}
	return RedisKey{Name: escapeBytes(s), B64: base64.StdEncoding.EncodeToString([]byte(s)), Type: typ}
}

// RedisScan 列键：按 Pattern（* ? [abc]，空 = 全部）、Type（空 = 全部类型），从 Cursor 接着扫。
type RedisScan struct {
	Pattern string `json:"pattern"`
	Type    string `json:"type"`
	Cursor  string `json:"cursor"`
	Count   int    `json:"count"`
}

// RedisKeys 扫到的键；Cursor 为 "0" 表示扫完了。
type RedisKeys struct {
	Keys    []RedisKey `json:"keys"`
	Cursor  string     `json:"cursor"`
	Scanned int        `json:"scanned"`
	Total   int64      `json:"total"` // 这个库一共多少个键（DBSIZE）
	Ms      int64      `json:"ms"`
}

// RedisKeys 用 SCAN 列键（不会像 KEYS 那样卡住 Redis）。一次大约 Count 个；没扫完时 Cursor 不是 "0"。
func (cl *Client) RedisKeys(ctx context.Context, dbName string, o RedisScan) (*RedisKeys, error) {
	start := time.Now()
	r, err := cl.redisOf(ctx, dbName)
	if err != nil {
		return nil, err
	}
	count := o.Count
	if count <= 0 {
		count = redisKeysDefault
	}
	count = min(count, redisKeysMax)
	pattern := o.Pattern
	if pattern == "" {
		pattern = "*"
	}
	var cursor uint64
	if o.Cursor != "" {
		if cursor, err = strconv.ParseUint(o.Cursor, 10, 64); err != nil {
			return nil, errors.New("游标格式不对")
		}
	}
	out := &RedisKeys{Keys: []RedisKey{}}
	deadline := time.Now().Add(redisScanBudget)
	for {
		keys, next, err := r.Scan(ctx, cursor, pattern, int64(min(count, 1000))).Result()
		if err != nil {
			return nil, cl.explain(err)
		}
		out.Scanned += len(keys)
		if len(keys) > 0 {
			types, err := redisTypes(ctx, r, keys)
			if err != nil {
				return nil, cl.explain(err)
			}
			for i, k := range keys {
				t := types[i]
				if t == "none" || (o.Type != "" && !strings.EqualFold(t, o.Type)) {
					continue
				}
				out.Keys = append(out.Keys, rkey(k, t))
			}
		}
		cursor = next
		if cursor == 0 || len(out.Keys) >= count || time.Now().After(deadline) {
			break
		}
	}
	sort.Slice(out.Keys, func(i, j int) bool { return out.Keys[i].Name < out.Keys[j].Name })
	out.Cursor = strconv.FormatUint(cursor, 10)
	out.Total, _ = r.DBSize(ctx).Result()
	out.Ms = time.Since(start).Milliseconds()
	return out, nil
}

func redisTypes(ctx context.Context, r *redis.Client, keys []string) ([]string, error) {
	pipe := r.Pipeline()
	cmds := make([]*redis.StatusCmd, len(keys))
	for i, k := range keys {
		cmds[i] = pipe.Type(ctx, k)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		var re redis.Error
		if !errors.As(err, &re) {
			return nil, err
		}
	}
	out := make([]string, len(keys))
	for i, c := range cmds {
		out[i] = c.Val()
		if out[i] == "" {
			out[i] = "none"
		}
	}
	return out, nil
}

func (cl *Client) redisOf(ctx context.Context, dbName string) (*redis.Client, error) {
	db, err := cl.redisIndex(dbName)
	if err != nil {
		return nil, err
	}
	return cl.Redis(ctx, db)
}

// ---------------------------------------------------------------- 看一个键

// RedisGet 看一个键：列表、有序集合按 Offset 翻页；哈希、集合（和带 Match 的有序集合）按 Cursor 接着扫；流按 Cursor（下一条的 ID）往前翻。
type RedisGet struct {
	Key    RStr   `json:"key"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
	Match  string `json:"match"`
	Desc   bool   `json:"desc"` // 有序集合：分数从大到小
}

// RedisItem 值里的一项：列表（I、V）、哈希（F 字段、V 值）、集合（F 成员）、有序集合（F 成员、S 分数）、流（F 是 ID、P 是字段和值交替）。
type RedisItem struct {
	I *int64     `json:"i,omitempty"`
	F *RedisVal  `json:"f,omitempty"`
	V *RedisVal  `json:"v,omitempty"`
	S string     `json:"s,omitempty"`
	P []RedisVal `json:"p,omitempty"`
}

// RedisValue 一个键的类型、过期时间、大小和值。
type RedisValue struct {
	Key      RedisKey    `json:"key"`
	Exists   bool        `json:"exists"`
	Type     string      `json:"type"`
	TTL      int64       `json:"ttl"`  // 毫秒；-1 永不过期
	Size     int64       `json:"size"` // 字符串的字节数；其他类型的元素个数
	Memory   int64       `json:"memory,omitempty"`
	Encoding string      `json:"encoding,omitempty"`
	Value    *RedisVal   `json:"value,omitempty"` // 字符串
	Items    []RedisItem `json:"items,omitempty"`
	Offset   int         `json:"offset,omitempty"`
	Cursor   string      `json:"cursor,omitempty"` // 没扫完时接着扫用；"" 或 "0" 表示没有了
	More     bool        `json:"more,omitempty"`
	Ms       int64       `json:"ms"`
}

// RedisGet 读一个键。
func (cl *Client) RedisGet(ctx context.Context, dbName string, g RedisGet) (*RedisValue, error) {
	start := time.Now()
	r, err := cl.redisOf(ctx, dbName)
	if err != nil {
		return nil, err
	}
	key, err := g.Key.raw()
	if err != nil {
		return nil, err
	}
	limit := g.Limit
	if limit <= 0 {
		limit = redisPageDefault
	}
	limit = min(limit, redisPageMax)
	offset := max(g.Offset, 0)

	typ, err := r.Type(ctx, key).Result()
	if err != nil {
		return nil, cl.explain(err)
	}
	v := &RedisValue{Key: rkey(key, typ), Type: typ, Exists: typ != "none", Offset: offset}
	if !v.Exists {
		v.Ms = time.Since(start).Milliseconds()
		return v, nil
	}
	if d, err := r.PTTL(ctx, key).Result(); err == nil {
		v.TTL = d.Milliseconds()
		if d < 0 {
			v.TTL = int64(d / time.Nanosecond) // go-redis 把 -1、-2 原样放在 Duration 里
		}
	}
	if n, err := r.MemoryUsage(ctx, key).Result(); err == nil {
		v.Memory = n
	}
	if s, err := r.ObjectEncoding(ctx, key).Result(); err == nil {
		v.Encoding = s
	}
	fail := func(err error) (*RedisValue, error) { return nil, cl.explain(err) }
	switch typ {
	case "string":
		n, err := r.StrLen(ctx, key).Result()
		if err != nil {
			return fail(err)
		}
		v.Size = n
		var s string
		if n > redisStringMax {
			s, err = r.GetRange(ctx, key, 0, redisStringMax).Result() // 多读一个字节：rval 据此知道被截断了
		} else {
			s, err = r.Get(ctx, key).Result()
		}
		if err != nil && !errors.Is(err, redis.Nil) {
			return fail(err)
		}
		v.Value = rval(s, redisStringMax)
		if n > redisStringMax {
			v.Value.Size = int(n)
		}
	case "list":
		if v.Size, err = r.LLen(ctx, key).Result(); err != nil {
			return fail(err)
		}
		vals, err := r.LRange(ctx, key, int64(offset), int64(offset+limit-1)).Result()
		if err != nil {
			return fail(err)
		}
		for i, s := range vals {
			idx := int64(offset + i)
			v.Items = append(v.Items, RedisItem{I: &idx, V: rval(s, redisItemMax)})
		}
		v.More = int64(offset+len(vals)) < v.Size
	case "hash", "set":
		if typ == "hash" {
			v.Size, err = r.HLen(ctx, key).Result()
		} else {
			v.Size, err = r.SCard(ctx, key).Result()
		}
		if err != nil {
			return fail(err)
		}
		cur, err := scanElems(ctx, g.Cursor, limit, func(c uint64) ([]string, uint64, error) {
			if typ == "hash" {
				return r.HScan(ctx, key, c, g.Match, int64(limit)).Result()
			}
			return r.SScan(ctx, key, c, g.Match, int64(limit)).Result()
		}, func(xs []string) int {
			if typ == "hash" {
				for i := 0; i+1 < len(xs); i += 2 {
					v.Items = append(v.Items, RedisItem{F: rval(xs[i], redisItemMax), V: rval(xs[i+1], redisItemMax)})
				}
			} else {
				for _, x := range xs {
					v.Items = append(v.Items, RedisItem{F: rval(x, redisItemMax)})
				}
			}
			return len(v.Items)
		})
		if err != nil {
			return fail(err)
		}
		v.Cursor = cur
		v.More = cur != "0"
		sort.SliceStable(v.Items, func(i, j int) bool { return v.Items[i].F.T < v.Items[j].F.T })
	case "zset":
		if v.Size, err = r.ZCard(ctx, key).Result(); err != nil {
			return fail(err)
		}
		if g.Match != "" {
			cur, err := scanElems(ctx, g.Cursor, limit, func(c uint64) ([]string, uint64, error) {
				return r.ZScan(ctx, key, c, g.Match, int64(limit)).Result()
			}, func(xs []string) int {
				for i := 0; i+1 < len(xs); i += 2 {
					v.Items = append(v.Items, RedisItem{F: rval(xs[i], redisItemMax), S: xs[i+1]})
				}
				return len(v.Items)
			})
			if err != nil {
				return fail(err)
			}
			v.Cursor = cur
			v.More = cur != "0"
			break
		}
		var zs []redis.Z
		if g.Desc {
			zs, err = r.ZRevRangeWithScores(ctx, key, int64(offset), int64(offset+limit-1)).Result()
		} else {
			zs, err = r.ZRangeWithScores(ctx, key, int64(offset), int64(offset+limit-1)).Result()
		}
		if err != nil {
			return fail(err)
		}
		for i, z := range zs {
			idx := int64(offset + i)
			m, _ := z.Member.(string)
			v.Items = append(v.Items, RedisItem{I: &idx, F: rval(m, redisItemMax), S: strconv.FormatFloat(z.Score, 'g', -1, 64)})
		}
		v.More = int64(offset+len(zs)) < v.Size
	case "stream":
		if v.Size, err = r.XLen(ctx, key).Result(); err != nil {
			return fail(err)
		}
		end := "+"
		if g.Cursor != "" {
			end = g.Cursor
		}
		xs, err := r.XRevRangeN(ctx, key, end, "-", int64(limit+1)).Result()
		if err != nil {
			return fail(err)
		}
		if len(xs) > limit {
			v.Cursor = xs[limit].ID
			v.More = true
			xs = xs[:limit]
		}
		for _, x := range xs {
			it := RedisItem{F: &RedisVal{T: x.ID}}
			names := make([]string, 0, len(x.Values))
			for k := range x.Values {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				s, _ := x.Values[k].(string)
				it.P = append(it.P, *rval(k, redisItemMax), *rval(s, redisItemMax))
			}
			v.Items = append(v.Items, it)
		}
	}
	v.Ms = time.Since(start).Milliseconds()
	return v, nil
}

// scanElems 用 HSCAN / SSCAN / ZSCAN 接着扫，直到够 limit 个、扫完或时间到。返回下次接着扫的游标（"0" 是扫完了）。
func scanElems(ctx context.Context, from string, limit int, scan func(uint64) ([]string, uint64, error), add func([]string) int) (string, error) {
	var cur uint64
	if from != "" {
		var err error
		if cur, err = strconv.ParseUint(from, 10, 64); err != nil {
			return "", errors.New("游标格式不对")
		}
	}
	deadline := time.Now().Add(redisScanBudget)
	for {
		xs, next, err := scan(cur)
		if err != nil {
			return "", err
		}
		n := add(xs)
		cur = next
		if cur == 0 || n >= limit || time.Now().After(deadline) || ctx.Err() != nil {
			return strconv.FormatUint(cur, 10), nil
		}
	}
}

// ---------------------------------------------------------------- 改

// RedisEdit 改一个键（Op 见 RedisApply）。
type RedisEdit struct {
	Op     string   `json:"op"`
	Key    RStr     `json:"key"`
	Keys   []RStr   `json:"keys"`   // del
	Type   string   `json:"type"`   // new：string | hash | list | set | zset | stream
	Field  RStr     `json:"field"`  // 哈希的字段、集合和有序集合的成员、流的 ID
	Orig   *RStr    `json:"orig"`   // 改名：原来的字段、成员；列表：这个位置原来的值（核对用）
	Value  string   `json:"value"`  // 新值
	Index  int64    `json:"index"`  // 列表的位置
	Score  string   `json:"score"`  // 有序集合的分数
	Pairs  []string `json:"pairs"`  // 流：字段、值、字段、值……
	Head   bool     `json:"head"`   // push：加在头上（否则加在末尾）
	TTL    int64    `json:"ttl"`    // 秒：expire；new 时大于 0 就设过期时间
	NewKey RStr     `json:"newKey"` // rename
}

// RedisEditResult 改的结果：N 是影响的个数（删了几个键……）。
type RedisEditResult struct {
	OK bool  `json:"ok"`
	N  int64 `json:"n"`
}

// ErrRedisReadOnly 只读连接不能改。
var ErrRedisReadOnly = errors.New("这个连接是只读的，不能改")

var errListChanged = errors.New("列表在这期间被改过了（这个位置上已经不是原来的值），请刷新后再改")

// 核对位置上的值再改，避免列表被别的程序改过后改错位置
var luaLSet = redis.NewScript(`if redis.call('LINDEX', KEYS[1], ARGV[1]) == ARGV[2] then return redis.call('LSET', KEYS[1], ARGV[1], ARGV[3]) else return redis.error_reply('FERRY_CHANGED') end`)
var luaLDel = redis.NewScript(`if redis.call('LINDEX', KEYS[1], ARGV[1]) == ARGV[2] then redis.call('LSET', KEYS[1], ARGV[1], ARGV[3]) return redis.call('LREM', KEYS[1], 1, ARGV[3]) else return redis.error_reply('FERRY_CHANGED') end`)

// RedisApply 改键：
//
//	set（字符串的新值，过期时间不变）、new（新建 Type 类型的键）、del（删 Keys）、rename（改名为 NewKey，已有同名的键时不改）、
//	expire（TTL 秒后过期）、persist（永不过期）、flushdb（清空这个库）、
//	hset / hdel（哈希的字段；Orig 不为空时是给字段改名）、
//	lset / ldel（列表某个位置；Orig 是那里原来的值）、push（列表加一项）、
//	sadd / srem（集合；sadd 带 Orig 是改成员）、zadd / zrem（有序集合；zadd 带 Orig 是改成员）、xadd / xdel（流）。
func (cl *Client) RedisApply(ctx context.Context, dbName string, e RedisEdit) (*RedisEditResult, error) {
	if cl.Conn.ReadOnly {
		return nil, ErrRedisReadOnly
	}
	r, err := cl.redisOf(ctx, dbName)
	if err != nil {
		return nil, err
	}
	res := &RedisEditResult{OK: true}
	fail := func(err error) (*RedisEditResult, error) {
		if err != nil && strings.Contains(err.Error(), "FERRY_CHANGED") {
			return nil, errListChanged
		}
		return nil, cl.explain(err)
	}
	if e.Op == "flushdb" {
		if err := r.FlushDB(ctx).Err(); err != nil {
			return fail(err)
		}
		return res, nil
	}
	if e.Op == "del" {
		keys := []string{}
		for _, k := range e.Keys {
			s, err := k.raw()
			if err != nil {
				return nil, err
			}
			keys = append(keys, s)
		}
		if len(keys) == 0 {
			return nil, errors.New("没有要删的键")
		}
		n, err := r.Unlink(ctx, keys...).Result()
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown command") {
			n, err = r.Del(ctx, keys...).Result() // Redis 4.0 以前没有 UNLINK
		}
		if err != nil {
			return fail(err)
		}
		res.N = n
		return res, nil
	}
	key, err := e.Key.raw()
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errors.New("没有指定键")
	}
	field, err := e.Field.raw()
	if err != nil {
		return nil, err
	}
	var orig *string
	if e.Orig != nil {
		s, err := e.Orig.raw()
		if err != nil {
			return nil, err
		}
		orig = &s
	}
	// 改之前核对类型：键被删了、换了类型时不要写成别的东西
	needType := map[string]string{"set": "string", "hset": "hash", "hdel": "hash", "lset": "list", "ldel": "list", "push": "list",
		"sadd": "set", "srem": "set", "zadd": "zset", "zrem": "zset", "xadd": "stream", "xdel": "stream"}
	if want := needType[e.Op]; want != "" {
		t, err := r.Type(ctx, key).Result()
		if err != nil {
			return fail(err)
		}
		if t == "none" {
			return nil, errors.New("这个键已经不在了（被删了或过期了），请刷新")
		}
		if t != want {
			return nil, fmt.Errorf("这个键现在是 %s 类型，不是 %s，请刷新", t, want)
		}
	}
	switch e.Op {
	case "set":
		pttl, err := r.PTTL(ctx, key).Result()
		if err != nil {
			return fail(err)
		}
		_, err = r.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Set(ctx, key, e.Value, 0)
			if pttl > 0 {
				p.PExpire(ctx, key, pttl)
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
	case "new":
		n, err := r.Exists(ctx, key).Result()
		if err != nil {
			return fail(err)
		}
		if n > 0 {
			return nil, errors.New("已经有这个键了")
		}
		var score float64
		if e.Type == "zset" {
			if score, err = parseScore(e.Score); err != nil {
				return nil, err
			}
		}
		if e.Type == "stream" && (len(e.Pairs) == 0 || len(e.Pairs)%2 != 0) {
			return nil, errors.New("流的一条消息要有字段和值")
		}
		_, err = r.TxPipelined(ctx, func(p redis.Pipeliner) error {
			switch e.Type {
			case "string":
				p.Set(ctx, key, e.Value, 0)
			case "hash":
				p.HSet(ctx, key, field, e.Value)
			case "list":
				p.RPush(ctx, key, e.Value)
			case "set":
				p.SAdd(ctx, key, field)
			case "zset":
				p.ZAdd(ctx, key, redis.Z{Score: score, Member: field})
			case "stream":
				p.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: e.Pairs})
			default:
				return fmt.Errorf("不认识的类型 %q", e.Type)
			}
			if e.TTL > 0 {
				p.Expire(ctx, key, time.Duration(e.TTL)*time.Second)
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
	case "rename":
		nk, err := e.NewKey.raw()
		if err != nil {
			return nil, err
		}
		if nk == "" {
			return nil, errors.New("请填新的键名")
		}
		ok, err := r.RenameNX(ctx, key, nk).Result()
		if err != nil {
			return fail(err)
		}
		if !ok {
			return nil, errors.New("已经有叫这个名字的键了")
		}
	case "expire":
		if e.TTL <= 0 {
			return nil, errors.New("过期时间要大于 0 秒")
		}
		ok, err := r.Expire(ctx, key, time.Duration(e.TTL)*time.Second).Result()
		if err != nil {
			return fail(err)
		}
		if !ok {
			return nil, errors.New("这个键已经不在了，请刷新")
		}
	case "persist":
		if err := r.Persist(ctx, key).Err(); err != nil {
			return fail(err)
		}
	case "hset":
		if orig != nil && *orig != field {
			ok, err := r.HExists(ctx, key, field).Result()
			if err != nil {
				return fail(err)
			}
			if ok {
				return nil, fmt.Errorf("已经有字段 %s 了", escapeBytes(field))
			}
			_, err = r.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.HSet(ctx, key, field, e.Value)
				p.HDel(ctx, key, *orig)
				return nil
			})
			if err != nil {
				return fail(err)
			}
			break
		}
		if err := r.HSet(ctx, key, field, e.Value).Err(); err != nil {
			return fail(err)
		}
	case "hdel":
		if res.N, err = r.HDel(ctx, key, field).Result(); err != nil {
			return fail(err)
		}
	case "lset", "ldel":
		if orig == nil {
			return nil, errors.New("没有这个位置原来的值")
		}
		if e.Op == "lset" {
			err = luaLSet.Run(ctx, r, []string{key}, e.Index, *orig, e.Value).Err()
		} else {
			err = luaLDel.Run(ctx, r, []string{key}, e.Index, *orig, deletedMarker()).Err()
		}
		if err != nil {
			return fail(err)
		}
	case "push":
		if e.Head {
			err = r.LPush(ctx, key, e.Value).Err()
		} else {
			err = r.RPush(ctx, key, e.Value).Err()
		}
		if err != nil {
			return fail(err)
		}
	case "sadd":
		if orig != nil && *orig != field {
			_, err = r.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.SRem(ctx, key, *orig)
				p.SAdd(ctx, key, field)
				return nil
			})
		} else {
			err = r.SAdd(ctx, key, field).Err()
		}
		if err != nil {
			return fail(err)
		}
	case "srem":
		if res.N, err = r.SRem(ctx, key, field).Result(); err != nil {
			return fail(err)
		}
	case "zadd":
		score, err := parseScore(e.Score)
		if err != nil {
			return nil, err
		}
		if orig != nil && *orig != field {
			_, err = r.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.ZRem(ctx, key, *orig)
				p.ZAdd(ctx, key, redis.Z{Score: score, Member: field})
				return nil
			})
		} else {
			err = r.ZAdd(ctx, key, redis.Z{Score: score, Member: field}).Err()
		}
		if err != nil {
			return fail(err)
		}
	case "zrem":
		if res.N, err = r.ZRem(ctx, key, field).Result(); err != nil {
			return fail(err)
		}
	case "xadd":
		if len(e.Pairs) == 0 || len(e.Pairs)%2 != 0 {
			return nil, errors.New("一条消息要有字段和值")
		}
		if err := r.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: e.Pairs}).Err(); err != nil {
			return fail(err)
		}
	case "xdel":
		if res.N, err = r.XDel(ctx, key, field).Result(); err != nil {
			return fail(err)
		}
	default:
		return nil, fmt.Errorf("不认识的操作 %q", e.Op)
	}
	return res, nil
}

func parseScore(s string) (float64, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "inf", "+inf":
		return math.Inf(1), nil
	case "-inf":
		return math.Inf(-1), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("分数要是数字，不是 %q", s)
	}
	return f, nil
}

// deletedMarker 删列表里的一项：先把它改成这个随机的值，再 LREM 掉（Redis 没有按位置删的命令）。
func deletedMarker() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "__ferry_deleted_" + hex.EncodeToString(b)
}
