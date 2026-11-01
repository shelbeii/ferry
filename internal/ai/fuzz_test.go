package ai

import (
	"strings"
	"testing"
)

// 命令由模型给出：判断只读、危险程度的函数不管给什么都不能 panic。
func FuzzCommand(f *testing.F) {
	f.Add("sudo -u root sh -c 'cat /etc/passwd | grep x > /tmp/a' && echo {a,b}{c,d} &")
	f.Add("service nginx restart; tee -a /etc/x <<EOF\nx\nEOF")
	f.Fuzz(func(t *testing.T, cmd string) {
		ReadOnly(cmd)
		UsesSudo(cmd)
		CommandKind(cmd)
		AlwaysAsk(cmd)
		Danger(cmd)
	})
}

// {a,b} 每层很多项、连写几层：不展开，不会一下子生成上亿个词
func TestBraceBomb(t *testing.T) {
	part := "{" + strings.Repeat("a,", 300) + "a}"
	if got := expandBraces("x"+part+part+part+part, 0); len(got) > maxBraceWords {
		t.Errorf("展开了 %d 个", len(got))
	}
	if got := expandBraces("/etc/{passwd,shadow}", 0); len(got) != 2 {
		t.Errorf("%v", got)
	}
}
