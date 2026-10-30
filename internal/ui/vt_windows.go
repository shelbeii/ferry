package ui

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT 打开 Windows 控制台的 ANSI 转义支持（Windows 10 起）。
func enableVT() bool {
	h := windows.Handle(os.Stderr.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
