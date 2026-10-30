package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/taskhist"
	"github.com/captain/ferry/internal/ui"
)

// MovedConfig ferry move-config 的结果。
type MovedConfig struct {
	From    string `json:"from"`
	To      string `json:"to"`
	DataDir string `json:"dataDir,omitempty"` // 本机设置、部署历史、构建日志搬到的项目目录；原来没有数据时为空
	Listed  bool   `json:"listed"`            // 桌面版项目列表里的位置改了
	Convs   int    `json:"convs"`             // 改了几个 AI 对话
	Tasks   int    `json:"tasks"`             // 改了几个任务记录
}

// moveConfigCmd ferry move-config <新位置> -c <ferry.yaml> [--force]：移动项目的配置文件。
// 这台电脑上按配置文件位置记的东西一起改过去：项目目录（本机设置、本机部署历史、构建日志）、
// 桌面版的项目列表、AI 对话和任务记录。服务器上的东西不受影响。
func moveConfigCmd(c *cli, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	if c.flags["config"] == "" || len(c.pos) != 1 {
		return fail(errors.New("用法：ferry move-config <新位置（文件或文件夹）> -c <现在的 ferry.yaml> [--force]"))
	}
	src, err := filepath.Abs(c.flags["config"])
	if err != nil {
		return fail(err)
	}
	dst, err := filepath.Abs(c.pos[0])
	if err != nil {
		return fail(err)
	}
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		dst = filepath.Join(dst, filepath.Base(src))
	}
	switch {
	case samePath(src, dst):
		return fail(errors.New("新位置和现在的一样"))
	case fileExists(dst):
		return fail(fmt.Errorf("%s 已经有文件了：不会覆盖", dst))
	case !dirExists(filepath.Dir(dst)):
		return fail(fmt.Errorf("文件夹 %s 不存在", filepath.Dir(dst)))
	}
	f, err := config.LoadWithLocal(src) // 本机设置里改了的（源码目录、私钥）按这台电脑上的算
	if err != nil {
		return fail(err)
	}
	if f.Kind == config.KindConnections {
		return fail(errors.New("这是「新建连接」保存的服务器列表，不是项目的配置文件"))
	}
	if !samePath(filepath.Dir(src), filepath.Dir(dst)) && !c.has("force") {
		if rel := f.RelativePaths(); len(rel) > 0 {
			return fail(fmt.Errorf("配置里这些本机路径是按 ferry.yaml 所在的文件夹写的，移到 %s 以后会指向那里：\n  %s\n先把它们改成绝对路径再移动（或加 --force 照样移动）",
				filepath.Dir(dst), strings.Join(rel, "\n  ")))
		}
	}

	if err := appdir.MoveFile(src, dst); err != nil {
		return fail(fmt.Errorf("移动 %s 失败：%v", src, err))
	}
	res := MovedConfig{From: src, To: dst}
	if res.DataDir, err = appdir.MoveProject(f.Project, src, dst); err != nil {
		if appdir.MoveFile(dst, src) == nil {
			return fail(fmt.Errorf("搬这台电脑上的项目数据失败，配置文件已经放回原处：%v", err))
		}
		return fail(fmt.Errorf("搬这台电脑上的项目数据失败：%v（配置文件在 %s，数据还在 %s）", err, dst, appdir.ProjectPath(f.Project, src)))
	}
	warn := func(format string, a ...any) {
		if con != nil {
			con.Warn(format, a...)
		}
	}
	// 重新读一次设置：读失败时不能拿默认设置覆盖掉用户的
	if st, err := settings.Load(); err != nil {
		warn("没能更新项目列表里的位置（%v）：在 Ferry 里重新添加 %s", err, dst)
	} else {
		for i := range st.Desktop.Projects {
			if samePath(st.Desktop.Projects[i].Path, src) {
				st.Desktop.Projects[i].Path = dst
				res.Listed = true
			}
		}
		cur := samePath(st.Desktop.Current, src)
		if cur {
			st.Desktop.Current = dst
		}
		if res.Listed || cur {
			if err := settings.Save(st); err != nil {
				warn("没能更新项目列表里的位置（%v）：在 Ferry 里重新添加 %s", err, dst)
				res.Listed = false
			}
		}
	}
	res.Convs = ai.RelinkConfig(engine.AIConvDir(), src, dst)
	res.Tasks = taskhist.RelinkConfig(taskHistoryDir(), src, dst)

	if js != nil {
		return ok0(js, res)
	}
	con.OK("已把 %s 移到 %s", src, dst)
	if res.DataDir != "" {
		con.Info("本机设置、部署历史、构建日志跟着搬到了 %s", res.DataDir)
	}
	if res.Listed {
		con.Info("Ferry 项目列表里的位置已经改好")
	}
	return 0
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
