package safe

import (
	"strings"
	"sync"
	"testing"
)

func TestGoRecovers(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	Go("测试", func() {
		defer wg.Done()
		panic("出错了")
	})
	wg.Wait() // 进程还在：走到这里就说明 panic 被拦下了
}

func TestRecoverToError(t *testing.T) {
	f := func() (err error) {
		defer Recover("上传", &err)
		var m map[string]int
		m["x"] = 1 // 向 nil map 写入：panic
		return nil
	}
	if err := f(); err == nil || !strings.Contains(err.Error(), "上传 出现内部错误") {
		t.Fatalf("panic 应变成错误：%v", err)
	}
}
