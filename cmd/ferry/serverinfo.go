package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

func serverInfoCmd(c *cli, cio *cliIO, e *engine.Engine, js *ui.JSON, fail func(error) int) int {
	if len(c.pos) != 1 {
		return fail(errors.New("用法：ferry server-info <目标>"))
	}
	r, err := e.ServerInfo(c.pos[0])
	if err != nil {
		return fail(err)
	}
	if js != nil {
		return ok0(js, r)
	}
	w := cio.stdout
	fmt.Fprintf(w, "%s · %s\n内核 %s · %s · %s\n", r.Hostname, r.OS, r.Kernel, r.Arch, r.Virtualization)
	fmt.Fprintf(w, "运行时间 %s\nCPU %s · %d 个逻辑 CPU · 使用率 %s\n", (time.Duration(r.Uptime) * time.Second).Truncate(time.Second), r.CPU.Model, r.CPU.Cores, serverPercentText(r.CPU.Usage))
	if len(r.CPU.Load) == 3 {
		fmt.Fprintf(w, "平均负载（1 / 5 / 15 分钟）：%.2f / %.2f / %.2f\n", r.CPU.Load[0], r.CPU.Load[1], r.CPU.Load[2])
	}
	if r.Memory.Total > 0 {
		fmt.Fprintf(w, "内存 %.2f / %.2f GiB，可用 %.2f GiB\n", float64(r.Memory.Used)/(1<<30), float64(r.Memory.Total)/(1<<30), float64(r.Memory.Available)/(1<<30))
		if r.Swap.Total == 0 {
			fmt.Fprintln(w, "Swap 未启用")
		} else {
			fmt.Fprintf(w, "Swap %.2f / %.2f GiB\n", float64(r.Swap.Used)/(1<<30), float64(r.Swap.Total)/(1<<30))
		}
	}
	t := &ui.Table{Header: []string{"挂载点", "设备", "文件系统", "容量 GiB", "已用 GiB", "可用 GiB", "使用率", "inode 使用率"}}
	for _, m := range r.Mounts {
		t.Rows = append(t.Rows, []string{m.Path, m.Device, m.FSType, fmt.Sprintf("%.2f", float64(m.Total)/(1<<30)),
			fmt.Sprintf("%.2f", float64(m.Used)/(1<<30)), fmt.Sprintf("%.2f", float64(m.Available)/(1<<30)), serverPercentText(m.Usage), serverPercentText(m.InodesUsage)})
	}
	t.Write(w)
	for _, a := range r.Addresses {
		fmt.Fprintf(w, "%s  %s\n", a.Interface, a.Address)
	}
	for _, note := range r.Notes {
		fmt.Fprintf(w, "提示：%s\n", note)
	}
	return 0
}

func serverPercentText(n *float64) string {
	if n == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", *n)
}
