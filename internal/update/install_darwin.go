package update

import "os/exec"

// Install 打开下载好的 .dmg：把里面的 Ferry 拖到「应用程序」替换旧的（要先退出 Ferry），再打开。
func Install(path string, _ bool) (string, error) {
	if err := exec.Command("open", path).Run(); err != nil {
		return "", err
	}
	return "已打开安装包：退出 Ferry，把里面的 Ferry 拖到「应用程序」替换旧的，再打开 Ferry", nil
}
