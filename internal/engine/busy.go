package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/captain/ferry/internal/build"
	"github.com/captain/ferry/internal/config"
)

// BusyItem 一个被本机程序占着的产物（Windows 锁住正在运行的 jar，构建时删不掉）。
type BusyItem struct {
	App     string       `json:"app"`
	Path    string       `json:"path"`
	Holders []BusyHolder `json:"holders"`
}

// BusyHolder 占着产物的进程。
type BusyHolder struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`          // 程序名，如 OpenJDK Platform binary
	Cmd  string `json:"cmd,omitempty"` // 命令行（看得出是哪个服务）
}

// BusyArtifactsError 构建前发现产物被占着（桌面版据此弹窗，可以结束这些进程后重新部署）。
type BusyArtifactsError struct {
	Target string
	Items  []BusyItem
}

func (e *BusyArtifactsError) Error() string {
	var lines []string
	for _, it := range e.Items {
		who := "本机的某个程序"
		if len(it.Holders) > 0 {
			var ps []string
			for _, h := range it.Holders {
				ps = append(ps, fmt.Sprintf("%s（进程号 %d）", h.Name, h.PID))
			}
			who = strings.Join(ps, "、")
		}
		lines = append(lines, fmt.Sprintf("  %s：%s 正在使用 %s", it.App, who, it.Path))
	}
	return "本机有程序正在运行要构建的产物，构建时删不掉旧文件（Windows 会锁住正在运行的 jar）：\n" + strings.Join(lines, "\n") +
		"\n先停掉它们再部署：关掉本机启动的这些服务（如 IDEA 里运行的、命令行 java -jar 启动的），或在任务管理器里按进程号结束"
}

// JSONDetail --json 输出的附加信息。
func (e *BusyArtifactsError) JSONDetail() map[string]any {
	return map[string]any{"kind": "busy-artifacts", "target": e.Target, "items": e.Items}
}

// busyItems 这些应用里有构建、产物又被本机程序占着的。
func (e *Engine) busyItems(t *config.Target, apps []*config.App) []BusyItem {
	var out []BusyItem
	var pids []int
	for _, a := range apps {
		if e.File.ResolveBuild(a) == nil {
			continue // 不构建的产物不会被删
		}
		p, err := LocalArtifact(e.File, t, a)
		if err != nil || !build.FileBusy(p) {
			continue
		}
		it := BusyItem{App: a.Name, Path: p}
		for _, h := range build.FileHolders(p) {
			name := h.Name
			if name == "" {
				name = "程序"
			}
			it.Holders = append(it.Holders, BusyHolder{PID: h.PID, Name: name})
			pids = append(pids, h.PID)
		}
		out = append(out, it)
	}
	if len(pids) > 0 {
		cmds := build.ProcessCommands(pids)
		for i := range out {
			for j := range out[i].Holders {
				out[i].Holders[j].Cmd = cmds[out[i].Holders[j].PID]
			}
		}
	}
	return out
}

// checkBusyArtifacts 构建前看产物有没有被本机的程序占着：Windows 上正在运行的 jar 删不掉，
// mvn clean 会失败、报一行看不懂的错。占着的话说清楚是哪个进程，先停掉再部署。
func (e *Engine) checkBusyArtifacts(t *config.Target, st *build.Step) error {
	if items := e.busyItems(t, st.Apps); len(items) > 0 {
		return &BusyArtifactsError{Target: t.Name, Items: items}
	}
	return nil
}

// BusyArtifacts 目标上要部署的应用里，产物正被本机程序占着的（names 为空 = 全部应用）。
func (e *Engine) BusyArtifacts(target string, names []string) ([]BusyItem, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	apps, err := e.File.Select(t, names)
	if err != nil {
		return nil, err
	}
	items := e.busyItems(t, apps)
	if items == nil {
		items = []BusyItem{}
	}
	return items, nil
}

// KillBusy 结束占着这些产物的进程 pid：只允许结束正占着它们的进程，别的进程号一律拒绝。返回结束后还被占着的。
func (e *Engine) KillBusy(target string, names []string, pid int) ([]BusyItem, error) {
	items, err := e.BusyArtifacts(target, names)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, it := range items {
		for _, h := range it.Holders {
			allowed = allowed || h.PID == pid
		}
	}
	if !allowed {
		return items, fmt.Errorf("进程 %d 没有占着要构建的产物（可能已经结束），不能由 Ferry 结束", pid)
	}
	if err := build.KillProcess(pid); err != nil {
		return items, fmt.Errorf("结束进程 %d 失败：%v", pid, err)
	}
	// 进程退出、文件句柄释放要一点时间
	for i := 0; i < 25; i++ {
		time.Sleep(200 * time.Millisecond)
		if items, err = e.BusyArtifacts(target, names); err != nil || !holds(items, pid) {
			break
		}
	}
	return items, err
}

func holds(items []BusyItem, pid int) bool {
	for _, it := range items {
		for _, h := range it.Holders {
			if h.PID == pid {
				return true
			}
		}
	}
	return false
}
