package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/ui"
)

const auditUsage = "用法：ferry audit <目标> [--kind 类型] [--actor 人] [--since 24h|2026-11-01] [--limit 200] [--local]；ferry audit --local（这台电脑上做过的，所有服务器）"

// auditQuery 命令行的筛选条件。--since 可以写时长（24h、7d）或日期。
func auditQuery(c *cli) (proto.AuditQuery, error) {
	q := proto.AuditQuery{Kind: c.flags["kind"], Actor: c.flags["actor"]}
	if v := c.flags["limit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return q, fmt.Errorf("--limit 要写正整数：%s", v)
		}
		q.Limit = n
	}
	if v := strings.TrimSpace(c.flags["since"]); v != "" {
		if strings.HasSuffix(v, "d") {
			if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && n > 0 {
				q.Since = time.Now().Add(-time.Duration(n) * 24 * time.Hour)
				return q, nil
			}
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			q.Since = time.Now().Add(-d)
			return q, nil
		}
		t, err := parseLogTime(v)
		if err != nil {
			return q, fmt.Errorf("--since 写成 24h、7d 或 2026-11-01：%s", v)
		}
		q.Since = t
	}
	return q, nil
}

// auditLocalCmd ferry audit --local：这台电脑上记的（不用读项目配置）。
func auditLocalCmd(c *cli, js *ui.JSON, fail func(error) int) int {
	q, err := auditQuery(c)
	if err != nil {
		return fail(err)
	}
	list, err := engine.LocalAuditLog(q, c.flags["project"], c.flags["target"])
	if err != nil {
		return fail(err)
	}
	if out := c.flags["out"]; out != "" {
		if err := writeAuditCSV(out, list); err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, map[string]any{"path": out, "count": len(list)})
		}
		fmt.Printf("已导出 %d 条到 %s\n", len(list), out)
		return 0
	}
	if js != nil {
		return ok0(js, list)
	}
	printAudit(list)
	return 0
}

// auditCmd ferry audit <目标>：服务器上的审计记录（团队里所有人做的）；加 --local 只看这台电脑上记的这台服务器的。
func auditCmd(c *cli, e *engine.Engine, js *ui.JSON, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(errors.New(auditUsage))
	}
	q, err := auditQuery(c)
	if err != nil {
		return fail(err)
	}
	var list []proto.AuditEntry
	if c.has("local") {
		list, err = engine.LocalAuditLog(q, e.File.Project, c.pos[0])
	} else {
		list, err = e.AuditLog(c.pos[0], q)
	}
	if err != nil {
		return fail(err)
	}
	if out := c.flags["out"]; out != "" {
		if err := writeAuditCSV(out, list); err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, map[string]any{"path": out, "count": len(list)})
		}
		fmt.Printf("已导出 %d 条到 %s\n", len(list), out)
		return 0
	}
	if js != nil {
		return ok0(js, list)
	}
	printAudit(list)
	return 0
}

func printAudit(list []proto.AuditEntry) {
	t := &ui.Table{Header: []string{"时间", "操作人", "做了什么", "对象", "细节", "结果"}}
	for _, a := range list {
		d := strings.Join(strings.Fields(a.Detail), " ")
		if r := []rune(d); len(r) > 60 {
			d = string(r[:60]) + "…"
		}
		res := "成功"
		if a.Result == "failed" {
			res = "失败"
			if a.Error != "" {
				res += "：" + a.Error
			}
		}
		t.Rows = append(t.Rows, []string{a.Time.Local().Format("2006-01-02 15:04:05"), a.Actor, proto.AuditKindText(a.Kind), a.Object, d, res})
	}
	if len(list) == 0 {
		fmt.Println("没有审计记录")
		return
	}
	t.Write(os.Stdout)
}

// writeAuditCSV 导出成 CSV（带 BOM，Excel 直接打开不乱码）。
func writeAuditCSV(p string, list []proto.AuditEntry) error {
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	f.WriteString("\ufeff")
	w := csv.NewWriter(f)
	w.Write([]string{"时间", "操作人", "项目", "服务器", "做了什么", "对象", "细节", "结果", "失败原因", "会话"})
	for _, a := range list {
		res := "成功"
		if a.Result == "failed" {
			res = "失败"
		}
		w.Write([]string{a.Time.Local().Format("2006-01-02 15:04:05"), a.Actor, a.Project, a.Target, proto.AuditKindText(a.Kind), a.Object, a.Detail, res, a.Error, a.Session})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
