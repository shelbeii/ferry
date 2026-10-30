package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/ui"
)

// pathCmd ferry path [status] | add | remove [--keep-choice] | check：把 ferry 所在目录加进（或移出）当前用户的 PATH，
// 在 cmd、PowerShell、编程软件里可以直接敲 ferry。只改当前用户的，不需要管理员权限。只支持 Windows。
// check 给安装程序用：已在 PATH 里退出码 0，不在 1，不支持 2。
// 加入、移除时记下用户的选择（安装程序升级、重装后按它决定加不加）；--keep-choice 不记（卸载时用）。
func pathCmd(c *cli, cio *cliIO) int {
	action := "status"
	if len(c.pos) > 0 {
		action = c.pos[0]
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(cio.stderr, "ferry: 找不到程序位置：", err)
		return 1
	}
	dir := filepath.Dir(exe)
	result := func(in bool) map[string]any {
		return map[string]any{"supported": userPathSupported, "exe": exe, "dir": dir, "inPath": in}
	}
	var js *ui.JSON
	if c.has("json") {
		js = ui.NewJSON(cio.stdout)
	}
	fail := func(err error) int {
		if js != nil {
			js.Result(false, nil, err)
		} else {
			fmt.Fprintln(cio.stderr, "ferry:", err)
		}
		return 1
	}
	if !userPathSupported {
		if action == "check" {
			return 2
		}
		if action == "add" || action == "remove" {
			return fail(errors.New("只支持 Windows；其他系统请自己把 " + dir + " 加进 PATH，或建一个指向 " + exe + " 的链接"))
		}
		if js == nil {
			fmt.Fprintf(cio.stdout, "只支持 Windows；其他系统请自己把 %s 加进 PATH\n", dir)
		}
		return ok0(js, result(false))
	}
	switch action {
	case "status", "check":
		cur, err := userPath()
		if err != nil {
			if action == "check" {
				return 1
			}
			return fail(err)
		}
		in := pathHas(cur, dir)
		if action == "check" {
			return map[bool]int{true: 0, false: 1}[in]
		}
		if js == nil {
			fmt.Fprintf(cio.stdout, "%s：%s\n", dir, map[bool]string{true: "已在当前用户的 PATH 里", false: "不在 PATH 里（ferry path add 加入）"}[in])
			return 0
		}
		return ok0(js, result(in))
	case "add", "remove":
		cur, err := userPath()
		if err != nil {
			return fail(err)
		}
		next := pathAdd(cur, dir)
		if action == "remove" {
			next = pathRemove(cur, dir)
		}
		if next != cur {
			if err := setUserPath(next); err != nil {
				return fail(err)
			}
		}
		if !c.has("keep-choice") { // 卸载时移除：记下的选择不变，重装后按它加回来
			setPathChoice(action == "add")
		}
		if js == nil {
			if action == "add" {
				fmt.Fprintf(cio.stdout, "已把 %s 加入当前用户的 PATH，重新打开命令行窗口后可以直接用 ferry\n", dir)
			} else {
				fmt.Fprintf(cio.stdout, "已从当前用户的 PATH 移除 %s\n", dir)
			}
			return 0
		}
		return ok0(js, result(action == "add"))
	}
	return fail(errors.New("用法：ferry path [status] | add | remove"))
}

// samePathDir 两个目录是否相同（Windows 不区分大小写，忽略结尾的斜杠）。
func samePathDir(a, b string) bool {
	clean := func(s string) string {
		return strings.TrimRight(strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"`)), `\/`)
	}
	return strings.EqualFold(clean(a), clean(b))
}

// pathHas PATH（分号分隔）里有没有 dir。
func pathHas(path, dir string) bool {
	for _, p := range strings.Split(path, ";") {
		if samePathDir(p, dir) {
			return true
		}
	}
	return false
}

// pathAdd 在 PATH 末尾加上 dir（已有时原样返回）。
func pathAdd(path, dir string) string {
	if pathHas(path, dir) {
		return path
	}
	path = strings.TrimRight(path, ";")
	if path == "" {
		return dir
	}
	return path + ";" + dir
}

// pathRemove 去掉 PATH 里所有的 dir，其他的原样保留。
func pathRemove(path, dir string) string {
	if !pathHas(path, dir) {
		return path
	}
	var keep []string
	for _, p := range strings.Split(path, ";") {
		if !samePathDir(p, dir) {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, ";")
}
