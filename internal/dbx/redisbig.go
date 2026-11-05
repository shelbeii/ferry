package dbx

// AI 查 Redis 时不许一次读整个大键：Redis 是单线程的，HGETALL 一个几百万字段的哈希、SCAN … COUNT 一千万
// 会让它卡住几秒，这期间用这个 Redis 的程序全都要等；回复也要整个读进 Ferry 才能截断。
// 执行前先看大小（HLEN、LLEN……），太大时拒绝，并告诉 AI 怎么分批读。

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

const (
	// aiRedisBigItems 一次读整个键（或一个范围）最多这么多个元素
	aiRedisBigItems = 10000
	// aiRedisBigBytes GET 最多读这么大的值
	aiRedisBigBytes = 1 << 20
	// aiRedisMaxCount SCAN、HSCAN 这类的 COUNT 最多
	aiRedisMaxCount = 1000
)

// aiRedisTooBig 这条命令会不会一次读太多；会的话返回原因和分批读的办法。看不了大小时（键不存在、类型不对）照常执行，命令自己会报错。
func aiRedisTooBig(ctx context.Context, r *redis.Client, args []string) error {
	name := strings.ToUpper(args[0])
	has := func(w string) bool {
		for _, a := range args[1:] {
			if strings.EqualFold(a, w) {
				return true
			}
		}
		return false
	}
	switch name {
	case "SCAN", "HSCAN", "SSCAN", "ZSCAN":
		for i := 1; i+1 < len(args); i++ {
			if strings.EqualFold(args[i], "COUNT") {
				if n, err := strconv.ParseInt(args[i+1], 10, 64); err == nil && n > aiRedisMaxCount {
					return fmt.Errorf("%s 的 COUNT 最多 %d（太大时 Redis 一次要看太多元素，会卡住别的程序）", name, aiRedisMaxCount)
				}
			}
		}
		return nil
	}
	if len(args) < 2 {
		return nil
	}
	key := args[1]
	big := func(n int64, how string) error {
		if n > aiRedisBigItems {
			return fmt.Errorf("%s 有 %d 个元素，%s 一次读这么多会让 Redis 卡住、回复也太长：%s", key, n, name, how)
		}
		return nil
	}
	size := func(c *redis.IntCmd) int64 {
		n, err := c.Result()
		if err != nil {
			return 0
		}
		return n
	}
	switch name {
	case "HGETALL", "HKEYS", "HVALS":
		return big(size(r.HLen(ctx, key)), "用 HSCAN "+key+" 0 COUNT 200 分批读，或者用 HGET、HMGET 读要的字段")
	case "SMEMBERS":
		return big(size(r.SCard(ctx, key)), "用 SSCAN "+key+" 0 COUNT 200 分批读")
	case "SINTER", "SUNION", "SDIFF":
		var sum int64
		for _, k := range args[1:] {
			sum += size(r.SCard(ctx, k))
		}
		if sum > aiRedisBigItems {
			return fmt.Errorf("这几个集合一共有 %d 个元素，%s 会让 Redis 卡住：先用 SCARD 看大小，或者用 SSCAN 分批读", sum, name)
		}
	case "LRANGE":
		if len(args) >= 4 {
			return big(rangeLen(size(r.LLen(ctx, key)), args[2], args[3]), "缩小范围，比如 LRANGE "+key+" 0 199")
		}
	case "ZRANGE", "ZREVRANGE":
		switch {
		case has("BYSCORE") || has("BYLEX"):
			if !has("LIMIT") {
				return big(size(r.ZCard(ctx, key)), "加上 LIMIT 0 200")
			}
		case len(args) >= 4:
			return big(rangeLen(size(r.ZCard(ctx, key)), args[2], args[3]), "缩小范围，比如 "+name+" "+key+" 0 199")
		}
	case "ZRANGEBYSCORE", "ZREVRANGEBYSCORE", "ZRANGEBYLEX", "ZREVRANGEBYLEX":
		if !has("LIMIT") {
			return big(size(r.ZCard(ctx, key)), "加上 LIMIT 0 200")
		}
	case "XRANGE", "XREVRANGE":
		if !has("COUNT") {
			return big(size(r.XLen(ctx, key)), "加上 COUNT 200")
		}
	case "SORT_RO":
		if !has("LIMIT") {
			var n int64
			switch t, _ := r.Type(ctx, key).Result(); t {
			case "list":
				n = size(r.LLen(ctx, key))
			case "set":
				n = size(r.SCard(ctx, key))
			case "zset":
				n = size(r.ZCard(ctx, key))
			}
			return big(n, "加上 LIMIT 0 200")
		}
	case "GET", "GETRANGE":
		n := size(r.StrLen(ctx, key))
		if name == "GETRANGE" && len(args) >= 4 {
			n = rangeLen(n, args[2], args[3])
		}
		if n > aiRedisBigBytes {
			return fmt.Errorf("%s 的值有 %.1f MB，太大了：用 GETRANGE %s 0 4095 读前面一部分", key, float64(n)/(1<<20), key)
		}
	}
	return nil
}

// rangeLen LRANGE、ZRANGE 这类 start stop（可以是负数）在 n 个元素里取到多少个。
func rangeLen(n int64, start, stop string) int64 {
	s, err1 := strconv.ParseInt(start, 10, 64)
	e, err2 := strconv.ParseInt(stop, 10, 64)
	if err1 != nil || err2 != nil || n <= 0 {
		return 0
	}
	if s < 0 {
		s += n
	}
	if e < 0 {
		e += n
	}
	s = max(s, 0)
	e = min(e, n-1)
	if s > e {
		return 0
	}
	return e - s + 1
}
