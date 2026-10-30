package engine

import (
	"strings"
	"testing"
)

func TestShGlob(t *testing.T) {
	if got := shGlob("/opt/my app/logs/app-*.log"); got != `'/opt/my app/logs/app-'*'.log'` {
		t.Errorf("%s", got)
	}
	if got := shGlob("a?b"); got != `'a'?'b'` {
		t.Errorf("%s", got)
	}
}

func TestFollowCmd(t *testing.T) {
	plain := followCmd(LogOptions{Lines: 100, Follow: true}, "/x/logs/app.log")
	if plain != "tail -n 100 -F -- '/x/logs/app.log'" {
		t.Errorf("普通文件：%s", plain)
	}
	if gz := followCmd(LogOptions{Lines: 50}, "/x/logs/a.log.gz"); !strings.Contains(gz, "gzip -dc") {
		t.Errorf(".gz：%s", gz)
	}
	once := followCmd(LogOptions{Lines: 100}, "/x/logs/app-*.log")
	if !strings.Contains(once, "ls -1t") || strings.Contains(once, "while sleep") {
		t.Errorf("通配符、不持续：%s", once)
	}
	loop := followCmd(LogOptions{Lines: 100, Follow: true}, "/x/logs/app-*.log")
	if !strings.Contains(loop, "while sleep 5") || !strings.Contains(loop, "换到新的日志文件") {
		t.Errorf("通配符、持续：%s", loop)
	}
}

func TestParseGrep(t *testing.T) {
	// grep -n -C 1 的输出：两组之间是 --
	out := parseGrep("3-前一行\n4:第一条 ERROR\n5-后一行\n--\n9-前\n10:第二条 error\n", 1)
	if len(out.Matches) != 2 {
		t.Fatalf("%+v", out)
	}
	a, b := out.Matches[0], out.Matches[1]
	if a.Text != "第一条 ERROR" || strings.Join(a.Before, "|") != "前一行" || strings.Join(a.After, "|") != "后一行" {
		t.Errorf("第一条：%+v", a)
	}
	if b.Text != "第二条 error" || strings.Join(b.Before, "|") != "前" || len(b.After) != 0 {
		t.Errorf("第二条：%+v", b)
	}
	// 内容里带冒号、横线
	out = parseGrep("12:2026-10-19T10:00:00+0800 host app[1]: a-b: c\n", 0)
	if len(out.Matches) != 1 || out.Matches[0].Text != "2026-10-19T10:00:00+0800 host app[1]: a-b: c" {
		t.Errorf("%+v", out)
	}
}
