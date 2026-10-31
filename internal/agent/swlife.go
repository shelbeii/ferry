package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// Ferry 装的软件装好以后：日志轮转、卸载时保留数据（以后能恢复）、升级、最近一次安装的结果。
//
//	<ferryDir>/software/.kept/<名称>.json      卸载时保留了数据的：原来的记录、单元、日志轮转配置
//	<ferryDir>/software/.last-install.json   最近一次安装的结果（连接断开时本机没拿到，回来能看到）
//	/etc/logrotate.d/ferry-<名称>            日志按天轮转（模板写了 logrotate 的）

// logrotateDir 日志轮转配置的目录（测试时替换）。
var logrotateDir = "/etc/logrotate.d"

func (a *Agent) keptDir() string             { return filepath.Join(a.swDir(), ".kept") }
func (a *Agent) keptFile(name string) string { return filepath.Join(a.keptDir(), name+".json") }
func (a *Agent) lastInstallFile() string     { return filepath.Join(a.swDir(), ".last-install.json") }
func swLogrotateFile(name string) string     { return filepath.Join(logrotateDir, "ferry-"+name) }
func swOldDir(dir string) string             { return filepath.Join(dir, ".ferry-old") }
func (a *Agent) loadKept(name string) (*proto.KeptSoftware, error) {
	if !reInstName.MatchString(name) {
		return nil, fmt.Errorf("名称无效：%q", name)
	}
	b, err := os.ReadFile(a.keptFile(name))
	if err != nil {
		return nil, fmt.Errorf("服务器上没有卸载时保留了数据的 %s", name)
	}
	var k proto.KeptSoftware
	if err := json.Unmarshal(b, &k); err != nil || k.Name != name || validInstallDir(k.Dir) != nil {
		return nil, fmt.Errorf("%s 的保留记录无效", name)
	}
	return &k, nil
}

// keptList 卸载时保留了数据的软件（目录已经被手动删掉的不列）。
func (a *Agent) keptList() []proto.KeptSoftware {
	ents, _ := os.ReadDir(a.keptDir())
	var out []proto.KeptSoftware
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if k, err := a.loadKept(name); err == nil {
			if _, err := os.Stat(k.Dir); err == nil {
				k.UnitText, k.LogrotateText = "", ""
				out = append(out, *k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// saveLastInstall 记下这次安装的结果。
func (a *Agent) saveLastInstall(name string, ok bool, msg string) {
	if !reInstName.MatchString(name) {
		return
	}
	b, _ := json.Marshal(proto.SwLastInstall{Name: name, OK: ok, Msg: msg, Time: a.Sys.Now()})
	writeFileAtomic(a.lastInstallFile(), b, 0o644)
}

func (a *Agent) lastInstall() *proto.SwLastInstall {
	b, err := os.ReadFile(a.lastInstallFile())
	if err != nil {
		return nil
	}
	var l proto.SwLastInstall
	if json.Unmarshal(b, &l) != nil {
		return nil
	}
	return &l
}

// ---------------------------------------------------------------- 日志轮转

// logrotateText 日志按天轮转：留 14 天、压缩，超过 100 MB 提前轮转；copytruncate 不用通知软件重新打开日志。
func logrotateText(name string, logs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s（软件 %s）：日志按天轮转，卸载时删除\n", ManagedMarker, name)
	b.WriteString(strings.Join(logs, " "))
	b.WriteString(" {\n    daily\n    rotate 14\n    maxsize 100M\n    missingok\n    notifempty\n    compress\n    delaycompress\n    copytruncate\n}\n")
	return b.String()
}

// swLogrotate 写日志轮转配置；服务器上没有 logrotate 时跳过。返回写好的文件。
func (a *Agent) swLogrotate(name string, logs []string) string {
	if len(logs) == 0 {
		return ""
	}
	if fi, err := os.Stat(logrotateDir); err != nil || !fi.IsDir() {
		a.info(proto.EvSoftware, name, "服务器上没有 logrotate：日志不会自动轮转，留意 %s 的大小", strings.Join(logs, " "))
		return ""
	}
	f := swLogrotateFile(name)
	if err := writeFileAtomic(f, []byte(logrotateText(name, logs)), 0o644); err != nil {
		a.warn(proto.EvWarn, name, "写日志轮转配置 %s 失败：%v", f, err)
		return ""
	}
	a.info(proto.EvSoftware, name, "日志按天轮转（留 14 天）：%s", f)
	return f
}

// removeLogrotate 删掉 Ferry 写的日志轮转配置。
func removeLogrotate(f string) {
	if f == "" || filepath.Dir(f) != logrotateDir {
		return
	}
	if b, err := os.ReadFile(f); err == nil && strings.HasPrefix(string(b), "# "+ManagedMarker) {
		os.Remove(f)
	}
}

// ---------------------------------------------------------------- 看记录、恢复

// swinfo Ferry 装的软件的记录（升级、设置备份时用）；没有时看卸载时保留了数据的。
func (a *Agent) swinfo() int {
	if in, err := a.loadInstance(a.req.Instance); err == nil {
		return a.result(proto.CodeOK, "", in)
	}
	if k, err := a.loadKept(a.req.Instance); err == nil {
		return a.result(proto.CodeOK, "kept", k.SoftwareInstance)
	}
	return a.result(proto.CodeBadInput, "服务器上没有 Ferry 安装的 "+a.req.Instance, nil)
}

// swrestore 恢复卸载时保留了数据的软件：照原样写回系统服务，启动、等就绪，写回安装记录。
func (a *Agent) swrestore() int {
	name := a.req.Instance
	k, err := a.loadKept(name)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	unlock, err := a.Sys.Lock(a.swLock(), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "另一个安装 / 卸载正在进行："+err.Error(), nil)
	}
	defer unlock()
	fail := func(format string, args ...any) int {
		msg := fmt.Sprintf(format, args...)
		a.fail(proto.EvFail, name, "%s", msg)
		return a.result(proto.CodeFailed, msg, nil)
	}
	unit := swUnit(name)
	if _, err := os.Stat(a.instFile(name)); err == nil {
		return fail("已经有名为 %s 的软件了：先卸载它再恢复", name)
	}
	if _, err := os.Stat(a.unitPath(unit)); err == nil {
		return fail("服务器上已有系统服务 %s：先删掉它再恢复", unit)
	}
	if _, err := os.Stat(k.Dir); err != nil {
		return fail("保留的安装目录 %s 已经不在了：恢复不了", k.Dir)
	}
	if !strings.HasPrefix(k.UnitText, ManagedMarker) {
		return fail("%s 的保留记录里没有系统服务：恢复不了", name)
	}
	if _, _, err := a.Sys.LookupUser(k.User); err != nil {
		return fail("运行用户 %s 已经不在了：恢复不了（%v）", k.User, err)
	}
	inUse, udpInUse, users := listeningPorts(), listeningUDP(), a.swPortUsers(name)
	for _, p := range k.AllPorts {
		if who := users["tcp/"+strconv.Itoa(p)]; who != "" || inUse[p] {
			return fail("端口 %d 现在被%s占用了：先停掉它再恢复", p, orText(who, "别的程序"))
		}
	}
	for _, p := range k.UDPPorts {
		if who := users["udp/"+strconv.Itoa(p)]; who != "" || udpInUse[p] {
			return fail("UDP 端口 %d 现在被%s占用了：先停掉它再恢复", p, orText(who, "别的程序"))
		}
	}
	a.info(proto.EvSoftware, name, "恢复 %s（%s）", strings.TrimSpace(k.Title+" "+k.Version), k.Dir)
	in := k.SoftwareInstance
	ok := false
	defer func() {
		if ok {
			return
		}
		a.Sys.Systemctl("disable", "--now", unit)
		a.undoSELinux(name, in.SELinux)
		removeLogrotate(in.Logrotate)
		if a.ferryUnitFile(unit) {
			os.Remove(a.unitPath(unit))
			a.Sys.Systemctl("daemon-reload")
		}
	}()
	if t := k.SELinux; t != nil && t.Types != nil {
		pl := &proto.SoftwarePlan{Name: name, Dir: k.Dir, Ports: k.AllPorts, SELinux: t.Types}
		in.SELinux = a.swSELinux(pl)
	}
	in.Logrotate = ""
	if k.LogrotateText != "" {
		in.Logrotate = a.swLogrotate(name, k.Logs)
	}
	if err := writeFileAtomic(a.unitPath(unit), []byte(k.UnitText), 0o644); err != nil {
		return fail("写系统服务失败：%v", err)
	}
	if err := a.Sys.Systemctl("daemon-reload"); err != nil {
		return fail("%v", err)
	}
	a.info(proto.EvStart, name, "启动 %s", unit)
	if err := a.Sys.Systemctl("enable", "--now", unit); err != nil {
		return fail("%v", err)
	}
	spec := readySpec(name, k.Logs, k.ReadyPort, 300)
	if err := a.waitReady(spec, unit); err != nil {
		a.diag(spec, unit)
		a.selinuxHint(name)
		return fail("恢复后启动失败：%v", err)
	}
	b, _ := json.MarshalIndent(in, "", "  ")
	if err := writeFileAtomic(a.instFile(name), b, 0o644); err != nil {
		return fail("写安装记录失败：%v", err)
	}
	os.Remove(a.keptFile(name))
	ok = true
	a.info(proto.EvReady, name, "%s 已恢复并启动", strings.TrimSpace(k.Title+" "+k.Version))
	return a.result(proto.CodeOK, "", in)
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------- 升级

// validKeep 升级时带过去的路径：安装目录下的相对路径。
func validKeep(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}

// swupgrade 换成新版本的程序：停下 → 旧程序挪到 .ferry-old → 放上新的 → 配置、数据（Keep）挪过去 → 启动、等就绪。
// 起不来时换回旧的程序再启动。数据目录不动（数据库新版本启动时自己升级数据）。
func (a *Agent) swupgrade() int {
	pl := a.req.Install
	if pl == nil || pl.Package == nil {
		return a.result(proto.CodeBadInput, "缺少新版本的安装包", nil)
	}
	in, err := a.loadInstance(pl.Name)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	pk := pl.Package
	if !reSafe.MatchString(pk.File) || !zipx.IsSHA256(pk.SHA256) || (pl.Version != "" && !reSwVersion.MatchString(pl.Version)) {
		return a.result(proto.CodeBadInput, "安装包信息无效", nil)
	}
	into := pk.Into
	if into == "" && (pk.Format == "jar" || pk.Format == "binary") {
		into = pk.File
	}
	if !validKeep(into) {
		return a.result(proto.CodeBadInput, "这个软件的程序直接放在安装目录里，和配置、数据混在一起：不能自动升级", nil)
	}
	for _, k := range pl.Keep {
		if !validKeep(k) {
			return a.result(proto.CodeBadInput, fmt.Sprintf("要带过去的路径无效：%q", k), nil)
		}
	}
	stage := ""
	if !pk.Cached {
		stage = filepath.Clean(a.req.Stage)
		if filepath.Dir(stage) != a.incomingDir() {
			return a.result(proto.CodeBadInput, "上传目录不在 "+a.incomingDir()+" 下", nil)
		}
		defer os.RemoveAll(stage)
	}
	unlock, err := a.Sys.Lock(a.swLock(), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "另一个安装 / 卸载正在进行："+err.Error(), nil)
	}
	defer unlock()
	name := in.Name
	fail := func(format string, args ...any) int {
		msg := fmt.Sprintf(format, args...)
		a.fail(proto.EvFail, name, "%s", msg)
		return a.result(proto.CodeFailed, msg, nil)
	}
	src := filepath.Join(stage, pk.File)
	if pk.Cached {
		src = filepath.Join(a.swCacheDir(), pk.SHA256)
	}
	if got, size, err := zipx.FileSHA256(src); err != nil || got != pk.SHA256 || size != pk.Size {
		os.Remove(filepath.Join(a.swCacheDir(), pk.SHA256))
		return a.result(proto.CodeVerify, "安装包校验失败（传输中损坏），请重试", nil)
	}
	uid, gid, err := a.Sys.LookupUser(in.User)
	if err != nil {
		return fail("运行用户 %s 不在了：%v", in.User, err)
	}
	cur := filepath.Join(in.Dir, filepath.FromSlash(into))
	if _, err := os.Stat(cur); err != nil {
		return fail("找不到现在的程序 %s：不能自动升级", cur)
	}
	oldRoot := swOldDir(in.Dir)
	os.RemoveAll(oldRoot)
	if err := os.MkdirAll(oldRoot, 0o750); err != nil {
		return fail("%v", err)
	}
	old := filepath.Join(oldRoot, filepath.Base(cur))
	a.info(proto.EvSoftware, name, "升级 %s：%s → %s", in.Title, orText(in.Version, "（未知版本）"), pl.Version)

	a.info(proto.EvStop, name, "停止 %s", in.Unit)
	a.Sys.Systemctl("stop", in.Unit)
	if p := a.Sys.UnitProps(in.Unit); isRunning(p) || p.PID != 0 {
		return fail("%s 没有停下来（%s/%s）：没有升级", in.Unit, p.Active, p.Sub)
	}
	if err := os.Rename(cur, old); err != nil {
		a.Sys.Systemctl("start", in.Unit)
		return fail("挪开旧程序失败：%v", err)
	}
	var moved [][2]string // 带过去的：新位置 → 旧位置（换回时挪回去）
	restore := func(why string) int {
		for i := len(moved) - 1; i >= 0; i-- {
			os.MkdirAll(filepath.Dir(moved[i][1]), 0o755)
			os.Rename(moved[i][0], moved[i][1])
		}
		os.RemoveAll(cur)
		if err := os.Rename(old, cur); err != nil {
			return fail("%s；换回旧程序也失败了（%v）：旧程序在 %s", why, err, old)
		}
		os.RemoveAll(oldRoot)
		a.Sys.Systemctl("start", in.Unit)
		spec := readySpec(name, in.Logs, in.ReadyPort, 300)
		if err := a.waitReady(spec, in.Unit); err != nil {
			return fail("%s；已换回 %s，但它也没能启动：%v", why, in.Version, err)
		}
		return fail("%s；已换回 %s 并启动", why, orText(in.Version, "原来的版本"))
	}
	if err := a.unpack(name, src, in.Dir, &proto.PlanPackage{File: pk.File, Format: pk.Format, Strip: pk.Strip, Into: into, Check: pk.Check}); err != nil {
		return restore(fmt.Sprintf("放上新版本失败：%v", err))
	}
	for _, k := range pl.Keep {
		rel, ok := strings.CutPrefix(k, into+"/")
		if !ok {
			continue // 不在程序目录里的配置、数据没有被挪开，不用带
		}
		from := filepath.Join(old, filepath.FromSlash(rel))
		to := filepath.Join(cur, filepath.FromSlash(rel))
		if _, err := os.Lstat(from); err != nil {
			continue
		}
		os.RemoveAll(to)
		os.MkdirAll(filepath.Dir(to), 0o755)
		if err := os.Rename(from, to); err != nil {
			return restore(fmt.Sprintf("把 %s 带到新版本失败：%v", k, err))
		}
		moved = append(moved, [2]string{to, from})
	}
	if err := a.chownTree(cur, uid, gid); err != nil {
		return restore(fmt.Sprintf("把新程序交给 %s 失败：%v", in.User, err))
	}
	a.info(proto.EvStart, name, "启动 %s", in.Unit)
	a.Sys.Systemctl("start", in.Unit)
	spec := readySpec(name, in.Logs, in.ReadyPort, 300)
	if err := a.waitReady(spec, in.Unit); err != nil {
		a.diag(spec, in.Unit)
		a.Sys.Systemctl("stop", in.Unit)
		return restore(fmt.Sprintf("%s 起不来：%v", pl.Version, err))
	}
	os.RemoveAll(oldRoot)
	os.RemoveAll(a.swCacheDir())
	in.Version = pl.Version
	b, _ := json.MarshalIndent(in, "", "  ")
	if err := writeFileAtomic(a.instFile(name), b, 0o644); err != nil {
		return fail("写安装记录失败：%v", err)
	}
	a.info(proto.EvReady, name, "%s 已升级到 %s", in.Title, pl.Version)
	return a.result(proto.CodeOK, "", in)
}

// ---------------------------------------------------------------- 告警

// swMonitor Ferry 装的软件：开机自启的没在运行、在反复重启（在 Ferry 里停掉的会取消开机自启，不算）。
func (a *Agent) swMonitor() []finding {
	var out []finding
	for _, in := range a.instances() {
		enabled, _ := a.Sys.Shell("systemctl is-enabled "+shellQuote(in.Unit)+" 2>/dev/null", 10*time.Second)
		if strings.TrimSpace(enabled) != "enabled" {
			continue
		}
		what := strings.TrimSpace(in.Title + "（" + in.Name + "）")
		p := a.Sys.UnitProps(in.Unit)
		switch {
		case p.Sub == "auto-restart":
			out = append(out, finding{proto.MonitorProblem{Key: "sw:" + in.Name, Level: "bad", Text: fmt.Sprintf("软件 %s启动失败，正在反复重启（%s）", what, in.Unit)}, ""})
		case p.Active == "failed" || p.Active == "inactive":
			out = append(out, finding{proto.MonitorProblem{Key: "sw:" + in.Name, Level: "bad", Text: fmt.Sprintf("软件 %s停了（%s，%s）", what, in.Unit, p.Active)}, ""})
		}
	}
	return out
}
