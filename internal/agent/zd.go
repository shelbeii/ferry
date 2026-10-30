package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 不停机发布（service.zeroDowntime）：两个槽位 a、b 轮流用，各有自己的端口和 systemd 单元。
//
//	apps/<app>/slots/a → ../versions/<id>    槽位的程序目录（单元里的路径都写它，进程运行期间不会变）
//	apps/<app>/running → versions/<id>       运行中的版本（链接；普通应用的 running 是真实目录）
//	.ferry/apps/<app>/slot                   正在用哪个槽位（JSON：slot、两个单元、前面的 nginx 单元）
//
// 部署：新版本在空着的槽位启动 → 就绪 → 前面的 nginx 改为转发到它 → 切换后的命令 → 等一会儿（drain）→ 停旧版本。
// 任何一步失败都停掉新版本：旧版本一直没停过，不用回滚。

// zdState .ferry/apps/<app>/slot 的内容。
type zdState struct {
	Slot  string   `json:"slot"`            // a | b
	Units []string `json:"units"`           // 两个槽位的单元（换回普通部署时要停掉、删掉）
	Proxy string   `json:"proxy,omitempty"` // 前面的 nginx 的单元
}

func (a *Agent) zdFile(app string) string { return filepath.Join(a.appMetaDir(app), "slot") }

func (a *Agent) slotDir(app, slot string) string { return filepath.Join(a.appDir(app), "slots", slot) }

func (a *Agent) readZD(app string) (zdState, bool) {
	var st zdState
	b, err := os.ReadFile(a.zdFile(app))
	if err != nil || json.Unmarshal(b, &st) != nil || (st.Slot != "a" && st.Slot != "b") {
		return st, false
	}
	return st, true
}

// activeSlot 正在用的槽位（0 = a，1 = b）；还没开始轮流时为 -1。
func (a *Agent) activeSlot(s *proto.AppSpec) int {
	st, ok := a.readZD(s.Name)
	if !ok {
		return -1
	}
	if st.Slot == "b" {
		return 1
	}
	return 0
}

// applySlot 让 AppSpec 的单元、单元文件、健康检查是正在用的槽位的：状态、启停、日志都按它来。
func (a *Agent) applySlot(s *proto.AppSpec) {
	if s.ZD == nil {
		return
	}
	i := max(a.activeSlot(s), 0)
	sl := s.ZD.Slots[i]
	s.Unit, s.UnitText, s.Health = sl.Unit, sl.UnitText, sl.Health
}

func validateZD(s *proto.AppSpec) error {
	zd := s.ZD
	if s.Type != proto.TypeJava && s.Type != proto.TypeBinary && !proto.DirService(s.Type) {
		return fmt.Errorf("%s：不停机发布只用于 java、binary", s.Name)
	}
	for i, sl := range zd.Slots {
		if sl.Name != []string{"a", "b"}[i] || sl.Port <= 0 || sl.Port > 65535 {
			return fmt.Errorf("%s：槽位 %d 无效", s.Name, i)
		}
		if !strings.HasSuffix(sl.Unit, ".service") || !reSafe.MatchString(sl.Unit) || !strings.HasPrefix(sl.UnitText, ManagedMarker) {
			return fmt.Errorf("%s：槽位 %s 的单元无效 %q", s.Name, sl.Name, sl.Unit)
		}
	}
	if zd.Slots[0].Unit == zd.Slots[1].Unit || zd.Slots[0].Port == zd.Slots[1].Port {
		return fmt.Errorf("%s：两个槽位的单元、端口不能一样", s.Name)
	}
	if zd.ProxyUnit != "" {
		if !strings.HasSuffix(zd.ProxyUnit, ".service") || !reSafe.MatchString(zd.ProxyUnit) || !strings.HasPrefix(zd.ProxyUnitText, ManagedMarker) ||
			!strings.Contains(zd.ProxyConf, proto.ProxyPort) || !reNginxLink.MatchString(s.RuntimeNginx) {
			return fmt.Errorf("%s：前面的 nginx 配置无效", s.Name)
		}
	}
	if zd.DrainSec < 0 || zd.DrainSec > 600 {
		return fmt.Errorf("%s：drain 无效", s.Name)
	}
	return nil
}

// zdSwitch 不停机切到版本 id（已经放在 versions/ 下）。prev 是现在运行的版本（第一次部署为空）。
// hooks：执行切换后的命令（部署时；手动回滚时不执行）。
func (a *Agent) zdSwitch(s *proto.AppSpec, id, prev string, hooks bool) (string, string) {
	zd := s.ZD
	act := a.activeSlot(s)
	// 旧版本在哪个单元里：轮流过的是正在用的槽位；还没轮流过的（以前普通方式部署的、接管前的服务）是槽位 a 的单元
	oldUnit := ""
	switch {
	case act >= 0 && isRunning(a.Sys.UnitProps(zd.Slots[act].Unit)):
		oldUnit = zd.Slots[act].Unit
	case act < 0 && isRunning(a.Sys.UnitProps(zd.Slots[0].Unit)):
		oldUnit = zd.Slots[0].Unit
	}
	free := 0
	if act == 0 || (act < 0 && oldUnit != "") {
		free = 1
	}
	sl := zd.Slots[free]
	spec := *s
	spec.Unit, spec.UnitText, spec.Health = sl.Unit, sl.UnitText, sl.Health
	// 启动失败时看的日志：running 还指着旧版本，换成新版本的槽位
	spec.DiagLogs = nil
	for _, d := range s.DiagLogs {
		spec.DiagLogs = append(spec.DiagLogs, strings.Replace(d, a.runningDir(s.Name)+string(filepath.Separator), a.slotDir(s.Name, sl.Name)+string(filepath.Separator), 1))
	}
	failed := func(msg string) (string, string) {
		a.Sys.Systemctl("stop", sl.Unit)
		a.markFailed(s.Name, id)
		if oldUnit != "" {
			a.info(proto.EvRollback, s.Name, "新版本 %s 已停止；旧版本一直在运行，没有中断", id)
		} else {
			a.info(proto.EvRollback, s.Name, "首次部署失败，保持停止")
		}
		return "ROLLED_BACK", msg
	}

	// 空着的槽位：上次留下的（停着的旧版本）先停稳，端口不能被别的程序占着
	a.Sys.Systemctl("stop", sl.Unit)
	if a.Sys.Probe(proto.Health{TCP: "127.0.0.1:" + strconv.Itoa(sl.Port)}) == nil {
		msg := fmt.Sprintf("端口 %d 已经被别的程序占用：不停机发布要用它启动新版本（zeroDowntime.ports）", sl.Port)
		a.fail(proto.EvFail, s.Name, "%s", msg)
		a.markFailed(s.Name, id)
		return "FAILED", msg
	}
	// 第一次轮到槽位 a、它的单元文件不是 Ferry 生成的：接管，原文件留着（回到接管前）
	if before, err := os.ReadFile(a.unitPath(sl.Unit)); err == nil && !strings.HasPrefix(string(before), ManagedMarker) {
		if err := a.saveLegacy(sl.Unit, before, s.Name); err != nil {
			return failed("保存接管前的单元文件失败：" + err.Error())
		}
	}
	if err := os.MkdirAll(filepath.Join(a.appDir(s.Name), "slots"), 0o755); err != nil {
		return failed(err.Error())
	}
	if err := a.Sys.SwitchLink(a.slotDir(s.Name, sl.Name), filepath.Join("..", dirVersions, id)); err != nil {
		return failed("槽位 " + sl.Name + "：" + err.Error())
	}
	a.beginRun(&spec, id)
	if err := a.installUnit(s.Name, sl.Unit, []byte(sl.UnitText)); err != nil {
		return failed(err.Error())
	}
	a.info(proto.EvStart, s.Name, "%s（槽位 %s，端口 %d）", sl.Unit, sl.Name, sl.Port)
	started := a.Sys.Now() // 这次运行从这时开始（看日志从这里看起；不是切过去的时间）
	if err := a.Sys.Systemctl("restart", sl.Unit); err != nil {
		a.diag(&spec, sl.Unit)
		return failed(err.Error())
	}
	if err := a.waitReady(&spec, sl.Unit); err != nil {
		a.diag(&spec, sl.Unit)
		return failed(err.Error())
	}
	// 前面的 nginx 改为转发到新版本
	oldPort := 0
	if act >= 0 {
		oldPort = zd.Slots[act].Port
	} else if oldUnit != "" {
		oldPort = zd.Slots[0].Port
	}
	if zd.ProxyUnit != "" {
		if err := a.zdProxy(s, sl.Port); err != nil {
			if oldPort != 0 {
				a.zdProxy(s, oldPort)
			}
			return failed("前面的 nginx：" + err.Error())
		}
	}
	if hooks && s.Hooks != nil {
		ph := hookPhase{name: "afterSwitch", label: "切换后的命令", dir: a.slotDir(s.Name, sl.Name)}
		if err := a.runHooks(s, ph, s.Hooks.AfterSwitch, id, prev); err != nil {
			a.fail(proto.EvFail, s.Name, "%v", err)
			if zd.ProxyUnit != "" && oldPort != 0 {
				a.zdProxy(s, oldPort)
			}
			return failed(err.Error())
		}
	}
	// 让还在处理的请求做完、注册中心摘掉旧版本，再停旧的
	if oldUnit != "" && zd.DrainSec > 0 {
		a.info(proto.EvWait, s.Name, "新版本已接上，等 %d 秒再停旧版本（还在处理的请求做完、注册中心摘掉它）", zd.DrainSec)
		a.Sys.Sleep(time.Duration(zd.DrainSec) * time.Second)
	}
	if oldUnit != "" {
		a.info(proto.EvStop, s.Name, "%s（旧版本 %s）", oldUnit, orNone(prev))
		if err := a.Sys.Systemctl("stop", oldUnit); err != nil {
			a.warn(proto.EvWarn, s.Name, "%v", err)
		}
		a.Sys.Systemctl("disable", oldUnit)
	}
	for _, c := range s.Conflict {
		if isRunning(a.Sys.UnitProps(c)) {
			a.info(proto.EvStop, s.Name, "停止冲突的服务 %s", c)
			a.Sys.Systemctl("stop", c)
		}
		if err := a.Sys.Systemctl("disable", c); err == nil {
			a.info(proto.EvInfo, s.Name, "已取消 %s 的开机自启", c)
		}
	}
	if err := a.Sys.Systemctl("enable", sl.Unit); err != nil {
		a.warn(proto.EvWarn, s.Name, "设置开机自启失败：%v", err)
	}
	if err := a.zdFinish(s, id, prev, sl.Name, started); err != nil {
		a.warn(proto.EvWarn, s.Name, "%v", err)
	}
	a.info(proto.EvSwitch, s.Name, "running → %s（槽位 %s）", id, sl.Name)
	return "OK", ""
}

// zdFinish 旧版本停下之后：归档它这次运行的日志，记下新的运行中的版本和槽位，running 指向新版本。
func (a *Agent) zdFinish(s *proto.AppSpec, id, prev, slot string, started time.Time) error {
	app := s.Name
	if prev != "" && prev != id {
		a.rotateLogs(s, prev)
	}
	running := a.runningDir(app)
	// 以前按普通方式部署的：running/ 是真实目录（旧版本），放回 versions/
	if realDir(running) {
		if prev == "" || exists(filepath.Join(a.versionsDir(app), prev)) {
			return fmt.Errorf("%s 是目录，没法换成指向新版本的链接", running)
		}
		if err := os.Rename(running, filepath.Join(a.versionsDir(app), prev)); err != nil {
			return err
		}
	}
	a.Sys.RemoveLink(a.oldCurrentLink(app))
	if err := a.Sys.SwitchLink(running, filepath.Join(dirVersions, id)); err != nil {
		return err
	}
	if err := a.writeRunning(app, id); err != nil {
		return err
	}
	st := zdState{Slot: slot, Units: []string{s.ZD.Slots[0].Unit, s.ZD.Slots[1].Unit}, Proxy: s.ZD.ProxyUnit}
	b, _ := json.Marshal(st)
	if err := writeFileAtomic(a.zdFile(app), b, 0o644); err != nil {
		return err
	}
	// 当前运行的开始时间（日志目录在启动前已经准备好了，这里不再动它）
	m, _ := json.Marshal(runMark{Release: id, Start: started.Add(-time.Second)})
	return writeFileAtomic(a.runFilePath(app), m, 0o644)
}

// zdProxy 前面的 nginx 转发到 port：写配置，没在运行就启动，在运行就重新加载（已有的连接照常做完）。
func (a *Agent) zdProxy(s *proto.AppSpec, port int) error {
	zd := s.ZD
	conf := filepath.Join(a.appMetaDir(s.Name), "proxy.conf")
	text := strings.ReplaceAll(zd.ProxyConf, proto.ProxyPort, strconv.Itoa(port))
	if err := os.MkdirAll(a.appMetaDir(s.Name), 0o755); err != nil {
		return err
	}
	changed := !sameFile(conf, []byte(text))
	if changed {
		if err := writeFileAtomic(conf, []byte(text), 0o644); err != nil {
			return err
		}
	}
	if err := a.installUnit(s.Name, zd.ProxyUnit, []byte(zd.ProxyUnitText)); err != nil {
		return err
	}
	a.Sys.Systemctl("enable", zd.ProxyUnit)
	if !isRunning(a.Sys.UnitProps(zd.ProxyUnit)) {
		a.info(proto.EvStart, s.Name, "%s（转发到 %d）", zd.ProxyUnit, port)
		if err := a.Sys.Systemctl("start", zd.ProxyUnit); err != nil {
			return err
		}
		ps := *s
		ps.Health = proto.Health{TimeoutSec: 30, StableSec: 2}
		keep, keepErr := a.readySec, a.errorLines // 记下的是应用的启动用时、错误行数，不是 nginx 的
		defer func() { a.readySec, a.errorLines = keep, keepErr }()
		return a.waitReady(&ps, zd.ProxyUnit)
	}
	if !changed {
		return nil
	}
	a.info(proto.EvSwitch, s.Name, "%s 转发到 %d", zd.ProxyUnit, port)
	return a.Sys.Systemctl("reload", zd.ProxyUnit)
}

// leaveZD 以前用过不停机发布、现在不用了：停掉两个槽位的单元和前面的 nginx，删掉它们，running 链接、slots 都去掉，
// 接下来按普通方式部署（会停机）。单元名在 slot 文件里（现在的配置里已经没有了）。
func (a *Agent) leaveZD(s *proto.AppSpec) {
	st, ok := a.readZD(s.Name)
	if !ok || s.ZD != nil {
		return
	}
	a.info(proto.EvInfo, s.Name, "去掉了 zeroDowntime：停掉两个槽位，按普通方式部署")
	for _, u := range append(st.Units, st.Proxy) {
		if u == "" {
			continue
		}
		a.Sys.Systemctl("stop", u)
		if u != s.Unit {
			a.Sys.Systemctl("disable", u)
			os.Remove(a.unitPath(u))
		}
	}
	a.Sys.Systemctl("daemon-reload")
	if !realDir(a.runningDir(s.Name)) {
		a.Sys.RemoveLink(a.runningDir(s.Name))
	}
	os.RemoveAll(filepath.Join(a.appDir(s.Name), "slots"))
	os.Remove(a.zdFile(s.Name))
}
