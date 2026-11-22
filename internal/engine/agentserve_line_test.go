package engine

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// 读常驻 agent 的输出：超长的一行丢掉接着读，不能把整个常驻 agent 判成退出。
func TestReadServeLine(t *testing.T) {
	in := "a 1\n" + strings.Repeat("x", 5000) + "\n" + "b 2\nlast"
	r := bufio.NewReaderSize(strings.NewReader(in), 64)

	line, over, err := readServeLine(r, 1024)
	if line != "a 1\n" || over || err != nil {
		t.Fatalf("第一行：%q over=%v err=%v", line, over, err)
	}
	line, over, err = readServeLine(r, 1024)
	if !over || line != "" || err != nil {
		t.Fatalf("超长行应当丢掉：%q over=%v err=%v", line, over, err)
	}
	line, over, err = readServeLine(r, 1024)
	if line != "b 2\n" || over || err != nil {
		t.Fatalf("超长行之后的行：%q over=%v err=%v", line, over, err)
	}
	line, over, err = readServeLine(r, 1024)
	if line != "last" || over || err != io.EOF {
		t.Fatalf("最后没有换行的一段：%q over=%v err=%v", line, over, err)
	}
}
