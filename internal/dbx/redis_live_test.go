package dbx

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

// 对真实的 Redis 跑一遍（本机起的容器，要设密码）：
//
//	FERRY_REDIS_LIVE=127.0.0.1:63791 FERRY_REDIS_PASSWORD=… go test ./internal/dbx -run RedisLive
func TestRedisLive(t *testing.T) {
	addr := os.Getenv("FERRY_REDIS_LIVE")
	if addr == "" {
		t.Skip("没有设置 FERRY_REDIS_LIVE")
	}
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	pw := os.Getenv("FERRY_REDIS_PASSWORD")
	c := &Conn{ID: "live-redis", Name: "redis", Kind: Redis, Host: host, Port: pn, Database: "9"}
	m := &Manager{clients: map[string]*Client{}}

	// 没填密码：要用户填
	if _, _, err := m.Open(ctx, c, "", nil); err == nil {
		t.Fatal("没有密码也连上了（容器要设密码）")
	} else if ne := (*NeedPasswordError)(nil); !errors.As(err, &ne) {
		t.Fatalf("没有密码时应要求填密码：%v", err)
	}
	if _, _, err := m.Open(ctx, c, "wrong", nil); err == nil {
		t.Fatal("密码不对也连上了")
	} else if ne := (*NeedPasswordError)(nil); !errors.As(err, &ne) || !ne.Rejected {
		t.Fatalf("密码不对时应说密码不对：%v", err)
	}

	cl, release, err := m.Open(ctx, c, pw, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	t.Log("version", cl.Version)
	if !strings.HasPrefix(cl.Version, "Redis ") {
		t.Errorf("版本：%q", cl.Version)
	}
	db := "" // 连接的默认库 9
	edit := func(e RedisEdit) *RedisEditResult {
		t.Helper()
		r, err := cl.RedisApply(ctx, db, e)
		if err != nil {
			t.Fatalf("%s: %v", e.Op, err)
		}
		return r
	}
	get := func(key string, g RedisGet) *RedisValue {
		t.Helper()
		g.Key = RStr{T: key}
		v, err := cl.RedisGet(ctx, db, g)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		return v
	}
	cmd := func(sid string, n int, line string) *RedisCmdResult {
		t.Helper()
		r, err := cl.RedisCommand(ctx, sid, n, line, true)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		return r
	}
	edit(RedisEdit{Op: "flushdb"})

	// 新建各种类型的键
	edit(RedisEdit{Op: "new", Type: "string", Key: RStr{T: "app:name"}, Value: "Ferry", TTL: 1000})
	edit(RedisEdit{Op: "new", Type: "hash", Key: RStr{T: "app:user:1"}, Field: RStr{T: "name"}, Value: "张三"})
	edit(RedisEdit{Op: "new", Type: "list", Key: RStr{T: "app:queue"}, Value: "a"})
	edit(RedisEdit{Op: "new", Type: "set", Key: RStr{T: "app:tags"}, Field: RStr{T: "go"}})
	edit(RedisEdit{Op: "new", Type: "zset", Key: RStr{T: "rank"}, Field: RStr{T: "bob"}, Score: "3.5"})
	edit(RedisEdit{Op: "new", Type: "stream", Key: RStr{T: "events"}, Pairs: []string{"type", "login"}})
	if _, err := cl.RedisApply(ctx, db, RedisEdit{Op: "new", Type: "string", Key: RStr{T: "app:name"}, Value: "x"}); err == nil {
		t.Error("已有的键又新建成功了")
	}

	// 列键：按模式、按类型
	ks, err := cl.RedisKeys(ctx, db, RedisScan{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ks.Keys) != 6 || ks.Total != 6 || ks.Cursor != "0" {
		t.Fatalf("键：%+v", ks)
	}
	ks, _ = cl.RedisKeys(ctx, db, RedisScan{Pattern: "app:*", Type: "hash"})
	if len(ks.Keys) != 1 || ks.Keys[0].Name != "app:user:1" || ks.Keys[0].Type != "hash" {
		t.Fatalf("按模式和类型：%+v", ks.Keys)
	}
	dbs, err := cl.Databases(ctx)
	if err != nil || len(dbs) != 16 || dbs[9].Keys != 6 {
		t.Fatalf("库：%+v %v", dbs, err)
	}

	// 字符串：改值时过期时间不变
	edit(RedisEdit{Op: "set", Key: RStr{T: "app:name"}, Value: "Ferry 2"})
	v := get("app:name", RedisGet{})
	if v.Value.T != "Ferry 2" || v.TTL <= 990_000 || v.Size != 7 {
		t.Errorf("字符串：%+v %+v", v, v.Value)
	}
	edit(RedisEdit{Op: "persist", Key: RStr{T: "app:name"}})
	if v := get("app:name", RedisGet{}); v.TTL != -1 {
		t.Errorf("永不过期后 TTL=%d", v.TTL)
	}
	edit(RedisEdit{Op: "expire", Key: RStr{T: "app:name"}, TTL: 60})
	if v := get("app:name", RedisGet{}); v.TTL <= 0 || v.TTL > 60_000 {
		t.Errorf("设过期后 TTL=%d", v.TTL)
	}
	// 类型不对时不改
	if _, err := cl.RedisApply(ctx, db, RedisEdit{Op: "hset", Key: RStr{T: "app:name"}, Field: RStr{T: "f"}, Value: "v"}); err == nil || !strings.Contains(err.Error(), "string") {
		t.Errorf("类型不对时应报错：%v", err)
	}

	// 哈希：加字段、改名、删
	edit(RedisEdit{Op: "hset", Key: RStr{T: "app:user:1"}, Field: RStr{T: "age"}, Value: "20"})
	edit(RedisEdit{Op: "hset", Key: RStr{T: "app:user:1"}, Field: RStr{T: "years"}, Orig: &RStr{T: "age"}, Value: "21"})
	if _, err := cl.RedisApply(ctx, db, RedisEdit{Op: "hset", Key: RStr{T: "app:user:1"}, Field: RStr{T: "name"}, Orig: &RStr{T: "years"}, Value: "x"}); err == nil {
		t.Error("改名成已有的字段应报错")
	}
	v = get("app:user:1", RedisGet{})
	if v.Size != 2 || len(v.Items) != 2 || v.Items[0].F.T != "name" || v.Items[0].V.T != "张三" || v.Items[1].F.T != "years" || v.Items[1].V.T != "21" {
		t.Errorf("哈希：%+v", v.Items)
	}
	v = get("app:user:1", RedisGet{Match: "na*"})
	if len(v.Items) != 1 {
		t.Errorf("哈希按字段筛：%+v", v.Items)
	}
	edit(RedisEdit{Op: "hdel", Key: RStr{T: "app:user:1"}, Field: RStr{T: "years"}})

	// 列表：加、按位置改（核对原值）、删
	edit(RedisEdit{Op: "push", Key: RStr{T: "app:queue"}, Value: "b"})
	edit(RedisEdit{Op: "push", Key: RStr{T: "app:queue"}, Value: "z", Head: true}) // z a b
	edit(RedisEdit{Op: "lset", Key: RStr{T: "app:queue"}, Index: 1, Orig: &RStr{T: "a"}, Value: "A"})
	if _, err := cl.RedisApply(ctx, db, RedisEdit{Op: "lset", Key: RStr{T: "app:queue"}, Index: 1, Orig: &RStr{T: "a"}, Value: "x"}); !errors.Is(err, errListChanged) {
		t.Errorf("原值对不上时应报错：%v", err)
	}
	edit(RedisEdit{Op: "ldel", Key: RStr{T: "app:queue"}, Index: 0, Orig: &RStr{T: "z"}})
	v = get("app:queue", RedisGet{})
	if v.Size != 2 || v.Items[0].V.T != "A" || v.Items[1].V.T != "b" || *v.Items[1].I != 1 {
		t.Errorf("列表：%+v", v.Items)
	}
	for i := 0; i < 30; i++ {
		edit(RedisEdit{Op: "push", Key: RStr{T: "app:queue"}, Value: strconv.Itoa(i)})
	}
	v = get("app:queue", RedisGet{Offset: 10, Limit: 10})
	if len(v.Items) != 10 || *v.Items[0].I != 10 || !v.More {
		t.Errorf("列表翻页：%d 项 more=%v", len(v.Items), v.More)
	}

	// 集合、有序集合
	edit(RedisEdit{Op: "sadd", Key: RStr{T: "app:tags"}, Field: RStr{T: "redis"}})
	edit(RedisEdit{Op: "sadd", Key: RStr{T: "app:tags"}, Field: RStr{T: "golang"}, Orig: &RStr{T: "go"}})
	v = get("app:tags", RedisGet{})
	if v.Size != 2 || v.Items[0].F.T != "golang" || v.Items[1].F.T != "redis" {
		t.Errorf("集合：%+v", v.Items)
	}
	edit(RedisEdit{Op: "zadd", Key: RStr{T: "rank"}, Field: RStr{T: "amy"}, Score: "10"})
	edit(RedisEdit{Op: "zadd", Key: RStr{T: "rank"}, Field: RStr{T: "bob"}, Score: "-inf"})
	v = get("rank", RedisGet{Desc: true})
	if len(v.Items) != 2 || v.Items[0].F.T != "amy" || v.Items[0].S != "10" || v.Items[1].S != "-Inf" {
		t.Errorf("有序集合：%+v", v.Items)
	}

	// 流：从新到旧翻页
	for i := 0; i < 5; i++ {
		edit(RedisEdit{Op: "xadd", Key: RStr{T: "events"}, Pairs: []string{"n", strconv.Itoa(i), "type", "click"}})
	}
	v = get("events", RedisGet{Limit: 4})
	if v.Size != 6 || len(v.Items) != 4 || !v.More || v.Items[0].P[0].T != "n" || v.Items[0].P[1].T != "4" {
		t.Fatalf("流：%+v", v)
	}
	v2 := get("events", RedisGet{Limit: 4, Cursor: v.Cursor})
	if len(v2.Items) != 2 || v2.More || v2.Items[1].P[1].T != "login" {
		t.Errorf("流第二页：%+v", v2.Items)
	}
	edit(RedisEdit{Op: "xdel", Key: RStr{T: "events"}, Field: RStr{T: v2.Items[1].F.T}})

	// 改名、删除
	edit(RedisEdit{Op: "rename", Key: RStr{T: "rank"}, NewKey: RStr{T: "rank2"}})
	if _, err := cl.RedisApply(ctx, db, RedisEdit{Op: "rename", Key: RStr{T: "rank2"}, NewKey: RStr{T: "events"}}); err == nil {
		t.Error("改成已有的键名应报错")
	}
	if r := edit(RedisEdit{Op: "del", Keys: []RStr{{T: "rank2"}, {T: "nothing"}}}); r.N != 1 {
		t.Errorf("删了 %d 个", r.N)
	}

	// 二进制的键和值
	cmd("", 9, `SET "bin\x00key" "\x00\x01\xff"`)
	ks, _ = cl.RedisKeys(ctx, db, RedisScan{Pattern: "bin*"})
	if len(ks.Keys) != 1 || ks.Keys[0].B64 == "" || ks.Keys[0].Name != `bin\x00key` {
		t.Fatalf("二进制键：%+v", ks.Keys)
	}
	bv, err := cl.RedisGet(ctx, db, RedisGet{Key: RStr{T: ks.Keys[0].Name, B64: ks.Keys[0].B64}})
	if err != nil || !bv.Value.Bin || bv.Value.T != `\x00\x01\xff` {
		t.Fatalf("二进制值：%+v %v", bv, err)
	}

	// 很长的字符串只读前面一部分
	edit(RedisEdit{Op: "new", Type: "string", Key: RStr{T: "big"}, Value: strings.Repeat("x", redisStringMax+10)})
	if v := get("big", RedisGet{}); v.Value.Size != redisStringMax+10 || len(v.Value.T) != redisStringMax {
		t.Errorf("长字符串：size=%d len=%d", v.Value.Size, len(v.Value.T))
	}

	// 命令行：SELECT 后窗口跟着换库；MULTI 在同一条连接上
	r := cmd("w1", 9, "SELECT 3")
	if r.DB != 3 {
		t.Errorf("SELECT 后库是 %d", r.DB)
	}
	cmd("w1", 3, "SET only3 yes")
	if v := cmd("w1", 3, "GET only3"); v.Reply.T != "str" || v.Reply.V != "yes" {
		t.Errorf("GET：%+v", v.Reply)
	}
	if v := cmd("w2", 9, "GET only3"); v.Reply.T != "nil" {
		t.Errorf("别的库里不该有：%+v", v.Reply)
	}
	cmd("w1", 3, "MULTI")
	cmd("w1", 3, "INCR n")
	cmd("w1", 3, "INCR n")
	if v := cmd("w1", 3, "EXEC"); v.Reply.T != "arr" || len(v.Reply.Items) != 2 || v.Reply.Items[1].V != "2" {
		t.Errorf("EXEC：%+v", v.Reply)
	}
	if v := cmd("w1", 3, "HGET nothing"); v.Reply.T != "err" {
		t.Errorf("参数不对应返回错误：%+v", v.Reply)
	}
	// 关掉 SELECT 过的窗口：它的连接不能还给共用的连接池（否则别处读到的是 db3）
	cl.dropRedisSession("w1")
	for i := 0; i < 5; i++ {
		if v := get("app:queue", RedisGet{}); !v.Exists {
			t.Fatal("关掉命令行窗口后，共用的连接跑到别的库去了")
		}
	}
	cmd("w1", 3, "FLUSHDB")
	if r, err := cl.RedisCommand(ctx, "w1", 3, "KEYS *", false); err != nil || r.Confirm == "" || r.Reply != nil {
		t.Errorf("KEYS 应要确认：%+v %v", r, err)
	}
	if _, err := cl.RedisCommand(ctx, "w1", 3, "SUBSCRIBE x", true); err == nil {
		t.Error("SUBSCRIBE 应不支持")
	}

	// AI：只读的命令、改动一起执行
	edit(RedisEdit{Op: "new", Type: "string", Key: RStr{T: "ai:n"}, Value: "1"})
	if q, err := cl.AIRedisQuery(ctx, db, "GET ai:n"); err != nil || q.Reply.V != "1" {
		t.Fatalf("AI 查：%+v %v", q, err)
	}
	if _, err := cl.AIRedisQuery(ctx, db, "SET ai:n 2"); err == nil || !strings.Contains(err.Error(), "只读") {
		t.Errorf("AI 查询不能改：%v", err)
	}
	if q, _ := cl.AIRedisQuery(ctx, db, "LRANGE app:queue 0 -1"); q == nil || q.Reply.T != "arr" {
		t.Errorf("AI 查列表：%+v", q)
	}
	// 大键不许一次读完，要分批
	rc, _ := cl.redisOf(ctx, db)
	big := []any{"HSET", "ai:big"}
	for i := 0; i <= aiRedisBigItems; i++ {
		big = append(big, "f"+strconv.Itoa(i), "v")
	}
	if err := rc.Do(ctx, big...).Err(); err != nil {
		t.Fatal(err)
	}
	rc.RPush(ctx, "ai:bigl", big[2:]...)
	for _, c := range []struct{ cmd, want string }{
		{"HGETALL ai:big", "HSCAN"},
		{"HKEYS ai:big", "HSCAN"},
		{"LRANGE ai:bigl 0 -1", "LRANGE ai:bigl 0 199"},
		{"SCAN 0 COUNT 100000", "COUNT 最多"},
		{"DUMP ai:big", "不能用 DUMP"},
	} {
		if _, err := cl.AIRedisQuery(ctx, db, c.cmd); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 应该拒绝（%s）：%v", c.cmd, c.want, err)
		}
	}
	for _, cmd := range []string{"LRANGE ai:bigl 0 199", "HSCAN ai:big 0 COUNT 200", "HLEN ai:big", "HGET ai:big f1"} {
		if _, err := cl.AIRedisQuery(ctx, db, cmd); err != nil {
			t.Errorf("%s 应该可以：%v", cmd, err)
		}
	}
	rc.Del(ctx, "ai:big", "ai:bigl")
	x, err := cl.AIExec(ctx, db, []string{"SET ai:n 5", "INCR ai:n", "HSET ai:h f v"})
	if err != nil || !x.OK || !x.Committed || len(x.Results) != 3 || x.Results[1].ReplyText != "(integer) 6" {
		t.Fatalf("AI 改：%+v %v", x, err)
	}
	// 有一条写错（参数个数不对）：一条都不执行
	x, err = cl.AIExec(ctx, db, []string{"SET ai:n 100", "HSET ai:h onlyfield"})
	if err != nil || x.OK || x.Committed || !strings.Contains(x.Note, "一条都没有执行") {
		t.Fatalf("写错的：%+v %v", x, err)
	}
	if v := get("ai:n", RedisGet{}); v.Value.T != "6" {
		t.Errorf("写错时不该改：%s", v.Value.T)
	}
	// 执行时出错（类型不对）：那条不生效，别的生效
	x, err = cl.AIExec(ctx, db, []string{"SET ai:n 7", "LPUSH ai:h x", "SET ai:m 1"})
	if err != nil || x.OK || !x.Committed || x.Results[1].Error == "" || x.Results[0].Error != "" {
		t.Fatalf("执行出错的：%+v %v", x, err)
	}
	if v := get("ai:m", RedisGet{}); !v.Exists {
		t.Error("别的命令应已生效")
	}

	// 只读连接
	ro := *c
	ro.ID, ro.ReadOnly = "live-redis-ro", true
	rcl, rrel, err := m.Open(ctx, &ro, pw, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rrel()
	for _, ok := range []string{"GET app:name", "HGETALL app:user:1", "SCAN 0", "TTL app:name", "INFO memory", "CONFIG GET maxmemory", "PING", "DBSIZE"} {
		if _, err := rcl.RedisCommand(ctx, "r", 9, ok, true); err != nil {
			t.Errorf("只读连接应能执行 %s：%v", ok, err)
		}
	}
	for _, bad := range []string{"SET a b", "DEL app:name", "FLUSHALL", "CONFIG SET maxmemory 1", "EVAL \"return 1\" 0", "MULTI"} {
		if _, err := rcl.RedisCommand(ctx, "r", 9, bad, true); err == nil {
			t.Errorf("只读连接不应能执行 %s", bad)
		}
	}
	if _, err := rcl.AIExec(ctx, "", []string{"SET a 1"}); !errors.Is(err, ErrRedisReadOnly) {
		t.Errorf("只读连接 AI 不能改：%v", err)
	}
	if _, err := rcl.RedisApply(ctx, "", RedisEdit{Op: "del", Keys: []RStr{{T: "app:name"}}}); !errors.Is(err, ErrRedisReadOnly) {
		t.Errorf("只读连接不能删键：%v", err)
	}
	edit(RedisEdit{Op: "flushdb"})
}

// 没设密码的 Redis：不填密码能连。
//
//	FERRY_REDIS_NOPASS=127.0.0.1:63792 go test ./internal/dbx -run RedisNoPass
func TestRedisNoPass(t *testing.T) {
	addr := os.Getenv("FERRY_REDIS_NOPASS")
	if addr == "" {
		t.Skip("没有设置 FERRY_REDIS_NOPASS")
	}
	ctx := context.Background()
	host, port, _ := strings.Cut(addr, ":")
	pn, _ := strconv.Atoi(port)
	c := &Conn{ID: "live-nopass", Name: "nopass", Kind: Redis, Host: host, Port: pn}
	m := &Manager{clients: map[string]*Client{}}
	cl, release, err := m.Open(ctx, c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	t.Log("version", cl.Version)
	c2 := *c
	c2.ID = "live-nopass-2"
	// Redis 6 起没设密码时随便什么密码都认；更老的版本报错时 explain 说要清空密码
	if _, rel, err := m.Open(ctx, &c2, "something", nil); err == nil {
		rel()
	} else if !strings.Contains(err.Error(), "没有设密码") {
		t.Errorf("填了多余的密码：%v", err)
	}
}
