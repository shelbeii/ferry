package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/ui"
)

const backupUsage = "用法：ferry backup <目标> [status | on | off | run [名字] | list [名字] | restore <名字> <文件> [--force] | delete <名字> <文件> | download <名字> <文件> [本机目录]]"

// backupCmd 数据备份：配置里的 backups 在服务器上打开后按时备份；手动备份、恢复、下载到本机。
func backupCmd(c *cli, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(errors.New(backupUsage))
	}
	action := "status"
	if len(c.pos) > 1 {
		action = c.pos[1]
	}
	arg := func(i int) string {
		if len(c.pos) > i {
			return c.pos[i]
		}
		return ""
	}
	o := engine.BackupOptions{Name: arg(2), File: arg(3), Force: c.has("force")}
	switch action {
	case "status", "on", "off":
		if len(c.pos) > 2 {
			return fail(errors.New(backupUsage))
		}
	case "run", "list":
		if len(c.pos) > 3 {
			return fail(errors.New(backupUsage))
		}
	case "restore", "delete":
		if o.Name == "" || o.File == "" || len(c.pos) > 4 {
			return fail(errors.New(backupUsage))
		}
	case "download":
		if o.Name == "" || o.File == "" || len(c.pos) > 5 {
			return fail(errors.New(backupUsage))
		}
		dir := arg(4)
		if dir == "" {
			dir, _ = os.Getwd()
		}
		local, err := e.BackupDownload(c.pos[0], o.Name, o.File, dir)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, map[string]string{"file": local})
		}
		con.OK("已下载到 %s", local)
		return 0
	default:
		return fail(errors.New(backupUsage))
	}
	st, err := e.Backup(c.pos[0], action, o)
	if err != nil {
		if js == nil && len(st.Items) > 0 {
			printBackup(con, st)
		}
		return fail(err)
	}
	if js != nil {
		return ok0(js, st)
	}
	if action == "list" || action == "delete" {
		printBackupFiles(st.Files)
		return 0
	}
	printBackup(con, st)
	return 0
}

func printBackup(con *ui.Console, st proto.BackupStatus) {
	if len(st.Items) == 0 {
		fmt.Println("配置里没有 backups（要备份什么）：见 docs/config.md 的「数据备份」")
		return
	}
	if st.On {
		fmt.Printf("数据备份：开着，备份文件在服务器的 %s\n", st.Dir)
	} else {
		fmt.Println("数据备份：没开（不定时备份、部署前也不备份；ferry backup <目标> on 打开。手动备份、恢复照样能用）")
	}
	for _, it := range st.Items {
		what := map[string]string{"mysql": "MySQL " + it.Database, "postgres": "PostgreSQL " + it.Database, "paths": strings.Join(it.Paths, "、"), "command": "命令"}[it.Kind]
		when := calendarText(it.OnCalendar)
		if it.BeforeDeploy {
			when += "、部署前"
		}
		fmt.Printf("\n%s（%s）：%s，留 %d 份；现在有 %d 份（%s）\n", it.Name, what, when, it.Keep, it.Count, human(it.Bytes))
		if it.Next != "" && st.On {
			fmt.Printf("  下次：%s\n", it.Next)
		}
		if it.Last != nil {
			r := it.Last
			if r.OK {
				con.OK("上次（%s，%s）：%s，%s", r.Reason, r.Time.Local().Format("01-02 15:04"), r.File, human(r.Size))
			} else {
				con.Error("上次（%s，%s）失败：%s", r.Reason, r.Time.Local().Format("01-02 15:04"), r.Error)
			}
		}
		switch {
		case it.Removed:
			con.Warn("配置里已经没有这一项了：关闭再打开（或部署一次）后去掉")
		case it.Changed:
			con.Warn("配置改过了，服务器上还是旧的：ferry backup <目标> on 或部署一次后同步")
		}
	}
}

func printBackupFiles(fs []proto.BackupFile) {
	if len(fs) == 0 {
		fmt.Println("还没有备份文件")
		return
	}
	for _, f := range fs {
		fmt.Printf("%-10s %-44s %10s  %s\n", f.Item, f.Name, human(f.Size), f.Time.Local().Format("2006-01-02 15:04"))
	}
}

func calendarText(c string) string {
	switch c {
	case "*-*-* 03:00:00":
		return "每天 3 点"
	case "hourly":
		return "每小时"
	case "Sun *-*-* 03:00:00":
		return "每周日 3 点"
	}
	return c
}
