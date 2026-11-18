package agent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// SELinux（RHEL 系默认是 Enforcing）：从软件源装的程序（redis-server、mariadbd、postgres、nginx）带着自己的 SELinux 标签，
// 由 systemd 启动后进入受限制的运行域，只能读写打了对应标签的目录、监听对应标签的端口。安装目录在 /opt 下（标签是 usr_t）时
// 它写不进去，不是默认端口时也监听不了：按模板声明的类型给安装目录、端口加上标签（semanage），卸载、撤销时去掉。
// 官方安装包里的程序也可能继承安装目录的标签（如 /home 下的 user_home_t）。

var reSELinuxType = regexp.MustCompile(`^[a-z][a-z0-9_]*_t$`)

// selinuxEnforcing SELinux 开着并且在拦截（Enforcing）。
func (a *Agent) selinuxEnforcing() bool {
	out, err := a.Sys.Shell("getenforce 2>/dev/null", 10*time.Second)
	return err == nil && strings.TrimSpace(out) == "Enforcing"
}

// swSELinux 给安装目录、端口加上模板声明的标签；返回加上了的（写进安装记录，卸载时去掉）。
// 做不到时只提示，不算安装失败：服务起不来时会说明可能是 SELinux 拦截了。
func (a *Agent) swSELinux(pl *proto.SoftwarePlan) *proto.SwSELinux {
	se := pl.SELinux
	if se == nil || (se.Files == "" && se.Port == "" && se.Exec == "") || !a.selinuxEnforcing() {
		return nil
	}
	if _, err := a.Sys.Shell("command -v semanage", 10*time.Second); err != nil {
		if pl.System != nil && pl.System.Bundle != "" {
			a.warn(proto.EvWarn, pl.Name, "离线安装缺少 semanage，请将 SELinux 标签工具加入离线软件包")
			return nil
		}
		a.info(proto.EvSoftware, pl.Name, "SELinux 开着：安装设置标签用的 semanage")
		allow := pl.System != nil && pl.System.AllowChanges
		tools := &proto.PlanSystem{DNF: []string{"policycoreutils-python-utils|policycoreutils-python"}, APT: []string{"policycoreutils-python-utils"}, AllowChanges: allow}
		if _, err := a.systemInstall(pl.Name, tools, proxyEnvPrefix(pl), true); err != nil {
			a.warn(proto.EvWarn, pl.Name, "装不上 semanage（%v）：没有给 %s 设置 SELinux 标签，服务可能起不来", err, pl.Dir)
			return nil
		}
		if _, err := a.Sys.Shell("command -v semanage", 10*time.Second); err != nil {
			a.warn(proto.EvWarn, pl.Name, "服务器上没有 semanage：没有给 %s 设置 SELinux 标签，服务可能起不来", pl.Dir)
			return nil
		}
	}
	done := &proto.SwSELinux{}
	if se.Exec != "" {
		executable := commandExecutable(sdSplit(pl.Exec))
		if resolved, err := filepath.EvalSymlinks(executable); err == nil && inDir(pl.Dir, resolved) && !a.hasSELinuxType(resolved, se.Exec) {
			spec := shellQuote(regexp.QuoteMeta(resolved))
			if _, err := a.Sys.Shell("semanage fcontext -a -t "+se.Exec+" "+spec+" 2>&1", 2*time.Minute); err != nil {
				a.warn(proto.EvWarn, pl.Name, "无法为启动程序 %s 添加 SELinux 标签 %s：%v", resolved, se.Exec, err)
			} else {
				done.ExecPath = resolved
				if _, err := a.Sys.Shell("restorecon "+shellQuote(resolved)+" 2>&1", time.Minute); err != nil {
					a.warn(proto.EvWarn, pl.Name, "应用 SELinux 启动程序标签失败：%v", err)
				}
			}
		}
	}
	if se.Files != "" {
		spec := shellQuote(regexp.QuoteMeta(pl.Dir) + "(/.*)?")
		cmd := fmt.Sprintf("semanage fcontext -a -t %s %s 2>&1 || semanage fcontext -m -t %s %s 2>&1", se.Files, spec, se.Files, spec)
		if out, err := a.Sys.Shell(cmd, 2*time.Minute); err != nil {
			a.warn(proto.EvWarn, pl.Name, "给 %s 设置 SELinux 标签 %s 失败：%v %s", pl.Dir, se.Files, err, strings.TrimSpace(out))
		} else {
			a.Sys.Shell("restorecon -R "+shellQuote(pl.Dir)+" 2>&1", 5*time.Minute)
			done.Files = pl.Dir
			a.info(proto.EvSoftware, pl.Name, "SELinux 开着：给 %s 加上标签 %s（程序才能读写）", pl.Dir, se.Files)
		}
	}
	if se.Port != "" {
		list, _ := a.Sys.Shell("semanage port -l 2>/dev/null", 2*time.Minute)
		for _, p := range pl.Ports {
			types, exact := portTypes(list, "tcp", p)
			if contains(types, se.Port) {
				continue // 本来就是（如 6379 是 redis_port_t）
			}
			op := "-a" // 落在范围里（如 unreserved_port_t 1024-32767）时单独加上
			if exact {
				op = "-m" // 这个端口单独定义成了别的类型：改成这个软件的
			}
			cmd := fmt.Sprintf("semanage port %s -t %s -p tcp %d 2>&1", op, se.Port, p)
			if out, err := a.Sys.Shell(cmd, 2*time.Minute); err != nil {
				a.warn(proto.EvWarn, pl.Name, "给端口 %d 设置 SELinux 标签 %s 失败：%v %s", p, se.Port, err, strings.TrimSpace(out))
				continue
			}
			done.Ports = append(done.Ports, p)
			a.info(proto.EvSoftware, pl.Name, "SELinux 开着：给端口 %d 加上标签 %s（程序才能监听）", p, se.Port)
		}
	}
	if done.Files == "" && done.ExecPath == "" && len(done.Ports) == 0 {
		return nil
	}
	return done
}

// undoSELinux 去掉安装时加上的标签（卸载、撤销安装时）。
func (a *Agent) undoSELinux(name string, se *proto.SwSELinux) {
	if se == nil {
		return
	}
	if se.ExecPath != "" {
		if _, err := a.Sys.Shell("semanage fcontext -d "+shellQuote(regexp.QuoteMeta(se.ExecPath))+" 2>&1", 2*time.Minute); err != nil {
			a.warn(proto.EvWarn, name, "移除启动程序 SELinux 标签规则失败：%v", err)
		}
	}
	if se.Files != "" {
		if out, err := a.Sys.Shell("semanage fcontext -d "+shellQuote(regexp.QuoteMeta(se.Files)+"(/.*)?")+" 2>&1", 2*time.Minute); err != nil {
			a.warn(proto.EvWarn, name, "去掉 %s 的 SELinux 标签失败：%v %s", se.Files, err, strings.TrimSpace(out))
		}
	}
	for _, p := range se.Ports {
		if out, err := a.Sys.Shell(fmt.Sprintf("semanage port -d -p tcp %d 2>&1", p), 2*time.Minute); err != nil {
			a.warn(proto.EvWarn, name, "去掉端口 %d 的 SELinux 标签失败：%v %s", p, err, strings.TrimSpace(out))
		}
	}
}

// portTypes semanage port -l 里包含这个端口的类型（单个端口或范围）；exact：有类型单独定义了这个端口。
func portTypes(list, proto string, port int) (out []string, exact bool) {
	for _, l := range splitLines(list) {
		f := strings.Fields(l)
		if len(f) < 3 || f[1] != proto {
			continue
		}
		for _, item := range strings.Split(strings.Join(f[2:], ""), ",") {
			lo, hi, isRange := strings.Cut(item, "-")
			a, err1 := strconv.Atoi(lo)
			b, err2 := strconv.Atoi(hi)
			if !isRange {
				b, err2 = a, err1
			}
			if err1 == nil && err2 == nil && a <= port && port <= b {
				out = append(out, f[0])
				exact = exact || !isRange
				break
			}
		}
	}
	return out, exact
}

// selinuxHint 服务起不来、SELinux 又开着时的提示。
func (a *Agent) selinuxHint(name string) {
	if a.selinuxEnforcing() {
		a.warn(proto.EvWarn, name, "这台服务器的 SELinux 开着（Enforcing），服务可能被它拦截了：用 ausearch -m avc -ts recent 查看被拦截的操作")
	}
}

func (a *Agent) hasSELinuxType(path, expected string) bool {
	out, err := a.Sys.Shell("ls -Zd -- "+shellQuote(path)+" 2>/dev/null", 10*time.Second)
	fields := strings.Fields(out)
	return err == nil && len(fields) > 0 && strings.Contains(fields[0], ":"+expected+":")
}

var auditStamp = regexp.MustCompile(`audit\(([0-9]+)\.`)

func softwareAVC(lines []string, executable string, since time.Time) []string {
	var out []string
	base := filepath.Base(executable)
	for _, line := range lines {
		if !strings.HasPrefix(line, "type=AVC ") || !strings.Contains(line, "denied") || !strings.Contains(line, "permissive=0") {
			continue
		}
		if !strings.Contains(line, `comm="`+base+`"`) && !strings.Contains(line, `comm="(`+base+`)"`) {
			continue
		}
		stamp := auditStamp.FindStringSubmatch(line)
		if stamp == nil {
			continue
		}
		seconds, _ := strconv.ParseInt(stamp[1], 10, 64)
		if seconds >= since.Unix() {
			out = append(out, line)
		}
	}
	return out
}

func (a *Agent) softwareSELinuxDiag(pl *proto.SoftwarePlan, unit string) {
	if a.Sys.UnitProps(unit).ExecMainStatus != 203 {
		return
	}
	since := a.waitStart[unit].Add(-3 * time.Second)
	if lines, err := tailFile("/var/log/audit/audit.log", 1500); err == nil {
		denials := softwareAVC(lines, commandExecutable(sdSplit(pl.Exec)), since)
		for _, line := range denials[max(0, len(denials)-3):] {
			a.fail(proto.EvDiag, pl.Name, "SELinux 拒绝启动程序执行：%s", line)
		}
		if len(denials) > 0 {
			return
		}
	}
	a.selinuxHint(pl.Name)
}
