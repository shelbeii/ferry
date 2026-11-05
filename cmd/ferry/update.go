package main

import (
	"errors"
	"fmt"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/ui"
	"github.com/captain/ferry/internal/update"
)

// updateCmd ferry update [check] | download | install [--restart]：检查、下载、安装新版本。
func updateCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	sub := "check"
	if len(c.pos) > 0 {
		sub = c.pos[0]
	}
	src := update.Source{Mirror: st.Runtime.GitHubMirror}.FromEnv(cio.getenv)
	dir := appdir.SubDir("updates")
	var rep engine.Reporter = js
	if js == nil {
		rep = con
	}

	info, err := update.Check(cio.ctx, src, Version, dir)
	if err != nil {
		return fail(err)
	}
	if sub == "check" {
		if js != nil {
			return ok0(js, info)
		}
		switch {
		case info.Newer:
			fmt.Fprintf(cio.stdout, "有新版本：%s（这台电脑上是 %s）\n%s\n", info.Latest, info.Current, info.Page)
			if info.Asset != "" {
				fmt.Fprintln(cio.stdout, "更新：ferry update install")
			}
		case update.IsNewer(info.Current, info.Latest):
			fmt.Fprintf(cio.stdout, "这台电脑上的 %s 比 GitHub 上最新发布的 %s 还新\n", info.Current, info.Latest)
		default:
			fmt.Fprintf(cio.stdout, "已是最新版本（%s）\n", info.Latest)
		}
		return 0
	}
	if sub != "download" && sub != "install" {
		return fail(errors.New("用法：ferry update [check] | download | install [--restart]"))
	}
	if !info.Newer {
		return fail(fmt.Errorf("已是最新版本（GitHub 上最新发布的是 %s，这台电脑上是 %s）", info.Latest, info.Current))
	}
	if info.Asset == "" {
		return fail(fmt.Errorf("%s 版本没有这种系统用的安装包，请到发布页下载：%s", info.Latest, info.Page))
	}

	rep.Step("下载 Ferry %s（%s）", info.Latest, info.Asset)
	path, err := update.DownloadFile(cio.ctx, src, info.Latest, info.Asset, dir, func(done, total int64) {
		rep.Progress("Ferry "+info.Latest, done, total)
	})
	if err != nil {
		if cio.ctx.Err() != nil {
			return fail(engine.ErrCanceled)
		}
		if src.Mirror == "" {
			err = fmt.Errorf("%w（下载慢或打不开时，可以在「设置 → 运行环境」填 GitHub 加速）", err)
		}
		return fail(err)
	}
	info.Downloaded = path
	if sub == "download" {
		if con != nil {
			con.OK("已下载并校验：%s", path)
		}
		return ok0(js, info)
	}

	rep.Step("安装 Ferry %s", info.Latest)
	msg, err := update.Install(path, c.has("restart"))
	if err != nil {
		return fail(fmt.Errorf("启动安装失败：%w（安装包在 %s，可以手动运行）", err, path))
	}
	if con != nil {
		con.OK("%s", msg)
	}
	return ok0(js, map[string]any{"info": info, "message": msg})
}
