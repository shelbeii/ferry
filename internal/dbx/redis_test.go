package dbx

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitRedisArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		bad  bool
	}{
		{in: "GET a", want: []string{"GET", "a"}},
		{in: "  set   k  v  ", want: []string{"set", "k", "v"}},
		{in: `SET k "hello world"`, want: []string{"SET", "k", "hello world"}},
		{in: `SET k "a\nb\t\"c\"\\"`, want: []string{"SET", "k", "a\nb\t\"c\"\\"}},
		{in: `SET k "\x00\xff"`, want: []string{"SET", "k", "\x00\xff"}},
		{in: `SET k 'it\'s \n'`, want: []string{"SET", "k", `it's \n`}},
		{in: `SET k ""`, want: []string{"SET", "k", ""}},
		{in: `SET k a"b`, want: []string{"SET", "k", `a"b`}},
		{in: "", want: nil},
		{in: `SET k "abc`, bad: true},
		{in: `SET k "abc"def`, bad: true},
		{in: `SET k 'abc`, bad: true},
	}
	for _, c := range cases {
		got, err := SplitRedisArgs(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: 应该报错，得到 %q", c.in, got)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: 得到 %q, %v；应为 %q", c.in, got, err, c.want)
		}
	}
}

func TestRedisDanger(t *testing.T) {
	for _, line := range []string{"FLUSHALL", "flushdb async", "KEYS *", "CONFIG SET maxmemory 1", "shutdown nosave", "ACL SETUSER x on", "CLIENT KILL ID 3", "DEBUG SLEEP 1", "SWAPDB 0 1"} {
		args, _ := SplitRedisArgs(line)
		if RedisDanger(args) == "" {
			t.Errorf("%s 应该要确认", line)
		}
	}
	for _, line := range []string{"GET a", "SET a b", "DEL a", "CONFIG GET *", "ACL WHOAMI", "CLIENT LIST", "SCAN 0", "INFO"} {
		args, _ := SplitRedisArgs(line)
		if d := RedisDanger(args); d != "" {
			t.Errorf("%s 不用确认，得到 %q", line, d)
		}
	}
}

func TestRval(t *testing.T) {
	v := rval("你好 world\n", 100)
	if v.T != "你好 world\n" || v.Bin || v.Size != 0 || v.B64 != "" {
		t.Errorf("文字：%+v", v)
	}
	v = rval("a\x00b\xff", 100)
	if !v.Bin || v.T != `a\x00b\xff` || v.B64 != "YQBi/w==" {
		t.Errorf("二进制：%+v", v)
	}
	// 截在一个字中间：去掉残缺的尾巴，不当成二进制
	v = rval("你好你好", 4)
	if v.T != "你" || v.Bin || v.Size != len("你好你好") {
		t.Errorf("截断：%+v", v)
	}
	k := rkey("user:1", "string")
	if k.Name != "user:1" || k.B64 != "" {
		t.Errorf("键：%+v", k)
	}
	k = rkey("a\nb", "string")
	if k.Name != `a\nb` || k.B64 == "" {
		t.Errorf("带换行的键：%+v", k)
	}
	s, err := RStr{B64: k.B64}.raw()
	if err != nil || s != "a\nb" {
		t.Errorf("base64 还原：%q %v", s, err)
	}
}

func TestRedisConnCheck(t *testing.T) {
	c := Conn{Name: "r", Kind: Redis, Host: "127.0.0.1", Port: 6379, AI: true, AIWrite: true, Database: "db3"}
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
	if !c.AI || !c.AIWrite {
		t.Error("Redis 连接也能允许 AI 查询、修改")
	}
	c.Database = "x"
	if err := c.Check(); err == nil || !strings.Contains(err.Error(), "编号") {
		t.Errorf("库编号不对时应报错：%v", err)
	}
	if n, _ := ParseRedisDB("db3"); n != 3 {
		t.Errorf("db3 → %d", n)
	}
}

func TestAIRedisChecks(t *testing.T) {
	for _, ok := range []string{"GET a", "HGETALL user:1", "SCAN 0 MATCH x* COUNT 100", "XREAD COUNT 10 STREAMS s 0", "INFO memory"} {
		if _, err := CheckAIRedisRead(ok); err != nil {
			t.Errorf("%s 应能查：%v", ok, err)
		}
	}
	for _, bad := range []string{"KEYS *", "CONFIG GET requirepass", "ACL LIST", "CLIENT LIST", "SELECT 2", "EVAL \"return 1\" 0", "XREAD BLOCK 0 STREAMS s $", "GET a\nGET b", "MONITOR"} {
		if _, err := CheckAIRedisRead(bad); err == nil {
			t.Errorf("%q 不应能查", bad)
		}
	}
	lines, err := CheckAIRedisWrite("SET a 1\n\n  HSET h f v  \nDEL x\n")
	if err != nil || len(lines) != 3 || lines[1] != "HSET h f v" {
		t.Fatalf("改：%q %v", lines, err)
	}
	for _, bad := range []string{"FLUSHDB", "SET a 1\nFLUSHALL", "MULTI\nSET a 1\nEXEC", "BLPOP q 0", "COPY a b DB 2", "SET a \"x", strings.Repeat("SET a 1\n", 101)} {
		if _, err := CheckAIRedisWrite(bad); err == nil {
			t.Errorf("%q 不应能改", bad)
		}
	}
	if d := AIRedisDanger([]string{"SET a 1", "DEL a", "HDEL h f", "RENAME a b"}); !strings.Contains(d, "DEL") || !strings.Contains(d, "HDEL") || !strings.Contains(d, "RENAME") {
		t.Errorf("危险：%q", d)
	}
	if d := AIRedisDanger([]string{"SET a 1", "HSET h f v"}); d != "" {
		t.Errorf("不危险：%q", d)
	}
	if _, err := CheckAIWrite(Redis, "x", "SET a 1"); err == nil {
		t.Error("库编号不对应报错")
	}
}

func TestFormatRedisReply(t *testing.T) {
	r := &RedisReply{T: "arr", Items: []RedisReply{{T: "str", V: "name"}, {T: "str", V: `张"三`}, {T: "arr", Items: []RedisReply{{T: "int", V: "1"}, {T: "nil"}}}}}
	want := "1) \"name\"\n2) \"张\\\"三\"\n3) 1) (integer) 1\n   2) (nil)"
	if got := FormatRedisReply(r); got != want {
		t.Errorf("得到\n%s\n应为\n%s", got, want)
	}
	if got := FormatRedisReply(&RedisReply{T: "str", V: "OK"}); got != "OK" {
		t.Errorf("OK：%q", got)
	}
}
