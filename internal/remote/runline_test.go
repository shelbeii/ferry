package remote

import (
	"strings"
	"testing"
)

// 一行输出超过上限：要明确报错，不能悄悄少半截还当作读完了。
func TestRunLineTooLong(t *testing.T) {
	d := newFakeSSHD(t, 4)
	d.out = strings.Repeat("x", 5000) + "\n"
	saved := runLineLimit
	runLineLimit = 1024
	defer func() { runLineLimit = saved }()
	c := d.dial(t)
	lines := 0
	_, err := c.Run("anything", nil, func(string) { lines++ })
	if err == nil || !strings.Contains(err.Error(), "没有读完") {
		t.Fatalf("应当报错说明没读完：%v", err)
	}
	if lines != 0 {
		t.Errorf("不该把不完整的内容交给回调：%d 行", lines)
	}
}

// 正常输出照旧逐行回调。
func TestRunLinesOK(t *testing.T) {
	d := newFakeSSHD(t, 4)
	d.out = "第一行\n第二行\n"
	c := d.dial(t)
	var got []string
	res, err := c.Run("anything", nil, func(l string) { got = append(got, l) })
	if err != nil || res.Code != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if len(got) != 2 || got[0] != "第一行" || got[1] != "第二行" {
		t.Fatalf("%q", got)
	}
}
