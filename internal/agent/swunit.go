package agent

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 不是 Ferry 装的软件（系统软件包、1Panel、Docker 等装的）：按它的系统服务或 Docker 容器启停、看日志（swunit）。
// Ferry 部署的应用的单元不在这里管（在「应用」里），Ferry 装的软件用 swcontrol。

var (
	reAnyUnit     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]*\.service$`)
	reContainerNm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

func (a *Agent) swunit() int {
	unit, ctr, action := a.req.SwUnit, a.req.SwContainer, a.req.Action
	switch action {
	case "start", "stop", "restart", "logs":
	default:
		return a.result(proto.CodeBadInput, "未知操作："+action, nil)
	}
	switch {
	case (unit == "") == (ctr == ""):
		return a.result(proto.CodeBadInput, "要指定一个系统服务或一个容器", nil)
	case unit != "" && !reAnyUnit.MatchString(unit):
		return a.result(proto.CodeBadInput, "服务名无效："+unit, nil)
	case ctr != "" && !reContainerNm.MatchString(ctr):
		return a.result(proto.CodeBadInput, "容器名无效："+ctr, nil)
	}
	n := a.req.Lines
	if n <= 0 || n > maxLogLines {
		n = 300
	}
	if unit != "" {
		return a.swunitService(unit, action, n)
	}
	return a.swunitContainer(ctr, action, n)
}

func (a *Agent) swunitService(unit, action string, n int) int {
	if a.ferryUnitFile(unit) && !strings.HasPrefix(unit, "ferry-") {
		return a.result(proto.CodeBadInput, unit+" 是 Ferry 部署的应用：请在「应用」里启停", nil)
	}
	p := a.Sys.UnitProps(unit)
	if p.Load != "loaded" {
		return a.result(proto.CodeFailed, "服务器上没有系统服务 "+unit, nil)
	}
	if action == "logs" {
		lines := a.Sys.Journal(unit, n, time.Time{})
		return a.result(proto.CodeOK, "journalctl -u "+unit, proto.LogText{Lines: lines, Truncated: len(lines) >= n})
	}
	label := map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}[action]
	a.info(proto.EvSoftware, unit, "%s %s", label, unit)
	if err := a.Sys.Systemctl(action, unit); err != nil {
		a.fail(proto.EvFail, unit, "%v", err)
		a.serviceDiag(unit)
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	// 等它进入目标状态（最多 30 秒）：启动后马上退出的要报出来
	deadline := a.Sys.Now().Add(30 * time.Second)
	for {
		p = a.Sys.UnitProps(unit)
		switch {
		case action == "stop" && !isRunning(p):
			a.info(proto.EvStop, unit, "已停止")
			return a.result(proto.CodeOK, "", nil)
		case action != "stop" && p.Active == "active" && p.Sub != "auto-restart":
			a.info(proto.EvReady, unit, "运行中（%s）", p.Sub)
			return a.result(proto.CodeOK, "", nil)
		case action != "stop" && (p.Active == "failed" || p.Active == "inactive" || p.Sub == "auto-restart"): // auto-restart：起来又退出，正在反复重启
			a.fail(proto.EvCrash, unit, "没有起来（%s/%s）", p.Active, p.Sub)
			a.serviceDiag(unit)
			return a.result(proto.CodeFailed, unit+" 没有起来", nil)
		}
		if !a.Sys.Now().Before(deadline) {
			a.warn(proto.EvWarn, unit, "30 秒后还是 %s/%s，稍后刷新看看", p.Active, p.Sub)
			return a.result(proto.CodeOK, "", nil)
		}
		a.Sys.Sleep(time.Second)
	}
}

// serviceDiag 启动失败时输出 journald 的最后几行。
func (a *Agent) serviceDiag(unit string) {
	lines := a.Sys.Journal(unit, 12, a.Sys.Now().Add(-time.Minute))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			a.fail(proto.EvDiag, unit, "%s", clipLine(l))
		}
	}
}

func (a *Agent) swunitContainer(ctr, action string, n int) int {
	if _, err := a.Sys.Shell("command -v docker", 10*time.Second); err != nil {
		return a.result(proto.CodeFailed, "服务器上没有 docker 命令", nil)
	}
	state, err := a.containerState(ctr)
	if err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	if action == "logs" {
		out, err := a.Sys.Shell(fmt.Sprintf("docker logs --tail %d %s 2>&1", n, shellQuote(ctr)), time.Minute)
		if err != nil {
			return a.result(proto.CodeFailed, "读容器日志失败："+strings.TrimSpace(out), nil)
		}
		lines := splitLines(out)
		return a.result(proto.CodeOK, "docker logs "+ctr, proto.LogText{Lines: lines, Truncated: len(lines) >= n})
	}
	label := map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}[action]
	a.info(proto.EvSoftware, ctr, "%s容器 %s（现在 %s）", label, ctr, state)
	if out, err := a.Sys.Shell(fmt.Sprintf("docker %s %s 2>&1", action, shellQuote(ctr)), 3*time.Minute); err != nil {
		msg := strings.TrimSpace(out)
		a.fail(proto.EvFail, ctr, "%s", msg)
		return a.result(proto.CodeFailed, label+"失败："+msg, nil)
	}
	// 启动后马上退出的（配置错误等）要报出来
	if action != "stop" {
		a.Sys.Sleep(3 * time.Second)
	}
	state, _ = a.containerState(ctr)
	switch {
	case action == "stop":
		a.info(proto.EvStop, ctr, "已停止（%s）", state)
	case state == "running":
		a.info(proto.EvReady, ctr, "运行中")
	default:
		a.fail(proto.EvCrash, ctr, "没有起来（%s）", state)
		if out, err := a.Sys.Shell("docker logs --tail 12 "+shellQuote(ctr)+" 2>&1", time.Minute); err == nil {
			for _, l := range splitLines(out) {
				a.fail(proto.EvDiag, ctr, "%s", clipLine(l))
			}
		}
		return a.result(proto.CodeFailed, ctr+" 没有起来（"+state+"）", nil)
	}
	return a.result(proto.CodeOK, "", nil)
}

// containerState 容器的状态（running、exited……）；没有这个容器时报错。
func (a *Agent) containerState(ctr string) (string, error) {
	out, err := a.Sys.Shell("docker inspect -f '{{.State.Status}}' "+shellQuote(ctr)+" 2>&1", 30*time.Second)
	if err != nil {
		msg := strings.TrimSpace(out)
		if strings.Contains(msg, "No such") {
			return "", errors.New("服务器上没有容器 " + ctr)
		}
		return "", errors.New("读容器状态失败：" + msg)
	}
	return strings.TrimSpace(out), nil
}
