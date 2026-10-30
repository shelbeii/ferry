// Package safe 保护后台 goroutine：ferry serve 是桌面版唯一的后台进程，其中任何一个 goroutine panic
// 都会让整个进程退出（终端全部断开，进行中的部署也断了线）。这里把 panic 拦下来写到标准错误，
// 只让出错的那一处失败。
package safe

import (
	"fmt"
	"os"
	"runtime/debug"
)

// Go 在新的 goroutine 里执行 fn；fn panic 时写到标准错误，进程照常运行。
func Go(what string, fn func()) {
	go func() {
		defer Recover(what, nil)
		fn()
	}()
}

// Recover 直接 defer 它（defer safe.Recover(…)）：拦下 panic，写到标准错误；err 不为空时把 panic 变成 *err。
func Recover(what string, err *error) {
	r := recover()
	if r == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "ferry：%s 出现内部错误：%v\n%s", what, r, debug.Stack())
	if err != nil {
		*err = fmt.Errorf("%s 出现内部错误：%v", what, r)
	}
}
