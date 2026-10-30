package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/ui"
)

const monitorUsage = "用法：ferry monitor <目标> [status | on [--notify ID,…] [--interval 5] [--disk 90] [--cert-days 14] | off | check | test]"

// monitorCmd 服务器监控：服务器上每隔几分钟检查应用、磁盘、证书、端口，有问题时发到设置 → 通知里的地方。
func monitorCmd(c *cli, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(errors.New(monitorUsage))
	}
	action := "status"
	if len(c.pos) > 1 {
		action = c.pos[1]
	}
	switch action {
	case "status", "on", "off", "check", "test":
	default:
		return fail(errors.New(monitorUsage))
	}
	num := func(k string) int {
		n, _ := strconv.Atoi(c.flags[k])
		return n
	}
	var chans []string
	if v := strings.TrimSpace(c.flags["notify"]); v != "" {
		chans = strings.Split(v, ",")
	}
	st, err := e.Monitor(c.pos[0], action, engine.MonitorOptions{Channels: chans, IntervalMin: num("interval"), DiskPct: num("disk"), CertDays: num("cert-days")})
	if err != nil {
		return fail(err)
	}
	if js != nil {
		return ok0(js, st)
	}
	printMonitor(con, action, st)
	return 0
}

func printMonitor(con *ui.Console, action string, st proto.MonitorStatus) {
	switch action {
	case "test":
		for name, err := range st.Sent {
			if err == "" {
				con.OK("测试告警已发到「%s」", name)
			} else {
				con.Error("没有发到「%s」：%s", name, err)
			}
		}
		return
	case "check":
		if len(st.Problems) == 0 {
			con.OK("没有发现问题")
		}
		for _, p := range st.Problems {
			con.Warn("%s", p.Text)
		}
		return
	}
	if !st.On {
		fmt.Println("监控：没开")
		return
	}
	fmt.Printf("监控：开着，每 %d 分钟检查一次（磁盘用到 %d%%、证书剩 %d 天时告警），发到 %s\n", st.IntervalMin, st.DiskPct, st.CertDays, strings.Join(st.Channels, "、"))
	if !st.LastRun.IsZero() {
		fmt.Printf("上次检查：%s\n", st.LastRun.Local().Format("2006-01-02 15:04"))
	}
	if len(st.Problems) == 0 {
		con.OK("现在没有问题")
	}
	for _, p := range st.Problems {
		con.Warn("%s（%s 起）", p.Text, p.Since.Local().Format("01-02 15:04"))
	}
}
