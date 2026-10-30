package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/taskhist"
	"github.com/captain/ferry/internal/ui"
)

// taskHistoryDir 任务记录的目录。
func taskHistoryDir() string { return appdir.SubDir("tasks") }

// cleanTaskHistory 按设置（设置 → 界面的任务记录）清理，keep 是正在保存的任务。返回删掉的任务 ID。
func cleanTaskHistory(st settings.Settings, keep string) []string {
	return taskhist.Clean(taskHistoryDir(), st.UI.TaskKeepDays, int64(st.UI.TaskMaxMB)<<20, st.UI.TaskKeep, keep)
}

// taskHistoryCmd ferry task-history list [--interrupt] | show <任务> | save | delete <任务…> | clean [--all]：
// 桌面版任务面板的任务记录。save 从环境变量 FERRY_TASK_VALUE 读 JSON。
func taskHistoryCmd(c *cli, cio *cliIO, js *ui.JSON, st settings.Settings, fail func(error) int) int {
	dir := taskHistoryDir()
	action := "list"
	if len(c.pos) > 0 {
		action = c.pos[0]
	}
	usage := func() map[string]any {
		n, b := taskhist.Usage(dir)
		return map[string]any{"count": n, "bytes": b, "keepDays": st.UI.TaskKeepDays, "maxMB": st.UI.TaskMaxMB, "keep": st.UI.TaskKeep}
	}
	var res map[string]any
	switch action {
	case "list":
		items, err := taskhist.List(dir, c.has("interrupt"))
		if err != nil {
			return fail(err)
		}
		res = usage()
		res["items"] = items
		if js == nil {
			for _, s := range items {
				fmt.Fprintf(cio.stdout, "%s  %s  %-11s %s\n", s.ID, time.UnixMilli(s.StartedAt).Format("01-02 15:04"), s.Status, s.Title)
			}
			fmt.Fprintf(cio.stdout, "共 %d 个任务，%.1f MB（保留 %d 天、最多 %d MB、最多 %d 个）\n", res["count"],
				float64(res["bytes"].(int64))/(1<<20), st.UI.TaskKeepDays, st.UI.TaskMaxMB, st.UI.TaskKeep)
			return 0
		}
	case "show":
		if len(c.pos) < 2 {
			return fail(errors.New("用法：ferry task-history show <任务 ID>"))
		}
		r, err := taskhist.Load(dir, c.pos[1])
		if err != nil {
			return fail(err)
		}
		if js == nil {
			b, _ := json.MarshalIndent(r, "", "  ")
			fmt.Fprintln(cio.stdout, string(b))
			return 0
		}
		js.Result(true, r, nil)
		return 0
	case "save":
		raw := cio.getenv("FERRY_TASK_VALUE")
		if raw == "" {
			return fail(errors.New("用环境变量 FERRY_TASK_VALUE 提供 JSON"))
		}
		var r taskhist.Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return fail(fmt.Errorf("任务记录格式错误：%w", err))
		}
		if err := taskhist.Save(dir, &r); err != nil {
			return fail(err)
		}
		res = map[string]any{"removed": cleanTaskHistory(st, r.ID)}
	case "delete":
		if len(c.pos) < 2 {
			return fail(errors.New("用法：ferry task-history delete <任务 ID…>"))
		}
		n, err := taskhist.Delete(dir, c.pos[1:]...)
		if err != nil {
			return fail(err)
		}
		res = usage()
		res["removed"] = n
	case "clean":
		var removed []string
		if c.has("all") {
			n := taskhist.CleanAll(dir)
			res = usage()
			res["removed"] = n
		} else {
			// 设置里「清理」用表单上（可能还没保存）的值
			if v, err := strconv.Atoi(c.flags["days"]); err == nil && v > 0 {
				st.UI.TaskKeepDays = v
			}
			if v, err := strconv.Atoi(c.flags["max-mb"]); err == nil && v > 0 {
				st.UI.TaskMaxMB = v
			}
			if v, err := strconv.Atoi(c.flags["keep"]); err == nil && v > 0 {
				st.UI.TaskKeep = v
			}
			removed = cleanTaskHistory(st, "")
			res = usage()
			res["removed"] = len(removed)
			res["ids"] = removed
		}
		if js == nil {
			fmt.Fprintf(cio.stdout, "删除了 %d 个任务记录\n", res["removed"])
			return 0
		}
	default:
		return fail(errors.New("用法：ferry task-history list|show|save|delete|clean"))
	}
	if js != nil {
		js.Result(true, res, nil)
	}
	return 0
}
