//go:build windows

package approve

import "golang.org/x/sys/windows"

const stillActive = 259 // STILL_ACTIVE

func osProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// 权限拒绝说明进程存在；其余错误（如 ERROR_INVALID_PARAMETER）说明进程已退出
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
