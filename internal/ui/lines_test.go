package ui

import (
	"bytes"
	"strings"
	"testing"
)

// 构建输出：\r 刷新的进度只留最后一次；一直不换行的不会无限攒着
func TestSplitLines(t *testing.T) {
	var got []string
	emit := func(l string) { got = append(got, l) }
	rest := splitLines([]byte("a\r\nb 10%\rb 50%\rb 100%\nhalf"), emit)
	if strings.Join(got, "|") != "a|b 100%" || string(rest) != "half" {
		t.Errorf("%q %q", got, rest)
	}
	got = nil
	var buf []byte
	for i := 0; i < 100000; i++ {
		buf = splitLines(append(buf, []byte("\rprogress ....")...), emit)
	}
	if len(buf) > lineMax || len(got) != 0 {
		t.Errorf("进度刷新：剩 %d 字节，输出 %d 行", len(buf), len(got))
	}
	buf = splitLines(bytes.Repeat([]byte("x"), lineMax+10), emit)
	if len(buf) != 0 || len(got) != 1 {
		t.Errorf("太长不换行：剩 %d，输出 %d 行", len(buf), len(got))
	}
}
