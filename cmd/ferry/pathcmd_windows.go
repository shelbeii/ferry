package main

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const userPathSupported = true

// userEnvKey 当前用户的环境变量（测试时换成别的键）。
var userEnvKey = `Environment`

// userPath 当前用户的 PATH（注册表里的原始值，%变量% 不展开）。
func userPath() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, userEnvKey, registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("读取用户环境变量失败：%v", err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取用户 PATH 失败：%v", err)
	}
	return v, nil
}

// setUserPath 写回当前用户的 PATH（保持可展开 %变量% 的类型），并通知其他程序环境变量变了。
func setUserPath(v string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, userEnvKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开用户环境变量失败：%v", err)
	}
	defer k.Close()
	if _, typ, err := k.GetValue("Path", nil); err == nil && typ == registry.SZ {
		err = k.SetStringValue("Path", v)
	} else {
		err = k.SetExpandStringValue("Path", v)
	}
	if err != nil {
		return fmt.Errorf("写入用户 PATH 失败：%v", err)
	}
	if userEnvKey == `Environment` {
		broadcastEnvChange()
	}
	return nil
}

// setPathChoice 记下用户要不要把 ferry 加入 PATH（安装程序读它，见 desktop/src-tauri/windows/hooks.nsh）。
func setPathChoice(yes bool) {
	if userEnvKey != `Environment` {
		return
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Ferry`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetStringValue("PathChoice", map[bool]string{true: "yes", false: "no"}[yes])
}

// broadcastEnvChange 让资源管理器等重新读取环境变量：之后新打开的命令行窗口就能用。
func broadcastEnvChange() {
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	env, _ := windows.UTF16PtrFromString("Environment")
	var res uintptr
	windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW").Call(
		hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 3000, uintptr(unsafe.Pointer(&res)))
}
