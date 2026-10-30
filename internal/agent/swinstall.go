package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// Ferry 安装的软件（swinstall / swcontrol / swremove / swlogs）。
// 引擎按模板渲染好安装计划（proto.SoftwarePlan），agent 照着做，不需要认识具体软件：
//
//	<ferryDir>/software/<名称>.json      记录：模板、版本、安装目录、单元、端口、日志
//	<ferryDir>/software/.lock            安装 / 卸载互斥
//	<安装目录>/                           程序、配置、数据、日志（归运行用户）
//	<安装目录>/ferry.env                 只有 root 能读的环境变量（密码）
//	/etc/systemd/system/ferry-<名称>.service
//
// 安装顺序：软件源装包 → 运行用户、安装目录 → 解开安装包 → 写文件、改配置 → prepare 步骤
// → 交给运行用户 → init 步骤 → 系统服务启动、等端口 → 就绪后的文件与步骤 → 写记录。中途失败全部撤销。

var (
	reInstName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	reUserName  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reSwVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,40}$`)
	rePkgName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	reUnitName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]*\.service$`)
	reAccount   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{2,31}$`)
)

func (a *Agent) swDir() string               { return filepath.Join(a.ferryDir(), "software") }
func (a *Agent) swLock() string              { return filepath.Join(a.swDir(), ".lock") }
func (a *Agent) instFile(name string) string { return filepath.Join(a.swDir(), name+".json") }

// ferryUnitFile 单元文件是 Ferry 生成的（卸载时只删这种）。
func (a *Agent) ferryUnitFile(unit string) bool {
	b, err := os.ReadFile(a.unitPath(unit))
	return err == nil && strings.HasPrefix(string(b), ManagedMarker)
}

func swUnit(name string) string   { return "ferry-" + name + ".service" }
func swEnvFile(dir string) string { return filepath.Join(dir, "ferry.env") }

// instances Ferry 在这台服务器上装的软件。
func (a *Agent) instances() []proto.SoftwareInstance {
	ents, _ := os.ReadDir(a.swDir())
	var out []proto.SoftwareInstance
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !reInstName.MatchString(name) {
			continue
		}
		if in, err := a.loadInstance(name); err == nil {
			out = append(out, *in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *Agent) loadInstance(name string) (*proto.SoftwareInstance, error) {
	if a.instanceLookup != nil {
		return a.instanceLookup(name)
	}
	if !reInstName.MatchString(name) {
		return nil, fmt.Errorf("名称无效：%q", name)
	}
	b, err := os.ReadFile(a.instFile(name))
	if err != nil {
		return nil, fmt.Errorf("服务器上没有 Ferry 安装的 %s", name)
	}
	var in proto.SoftwareInstance
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("%s 的记录已损坏：%v", name, err)
	}
	if in.Name != name || in.Unit != swUnit(name) || validInstallDir(in.Dir) != nil {
		return nil, fmt.Errorf("%s 的记录无效", name)
	}
	return &in, nil
}

// 安装目录不能是这些目录本身，也不能放在 systemDirs 下面
var (
	topDirs    = []string{"/opt", "/data", "/home", "/srv", "/var", "/var/lib", "/usr/local", "/mnt", "/www"}
	systemDirs = []string{"/proc", "/sys", "/dev", "/etc", "/boot", "/bin", "/sbin", "/lib", "/lib64", "/usr", "/run", "/tmp", "/var/tmp", "/root"}
)

// validInstallDir 安装目录：绝对路径、每一级都是安全的名字、至少两级、不在系统目录下。
func validInstallDir(dir string) error {
	if !strings.HasPrefix(dir, "/") || path.Clean(dir) != dir {
		return fmt.Errorf("安装目录要写绝对路径：%q", dir)
	}
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	if len(parts) < 2 {
		return fmt.Errorf("安装目录 %s 太浅：请写到具体的目录，如 /opt/ferry-software/nacos-3.2.4", dir)
	}
	for _, p := range parts {
		if !reSafe.MatchString(p) {
			return fmt.Errorf("安装目录里只能用字母、数字、点、下划线、横线：%q", dir)
		}
	}
	for _, t := range topDirs {
		if dir == t {
			return fmt.Errorf("安装目录不能是 %s 本身：请写到它下面的目录", dir)
		}
	}
	for _, s := range systemDirs {
		if dir == s || strings.HasPrefix(dir, s+"/") && !strings.HasPrefix(dir, "/usr/local/") {
			return fmt.Errorf("安装目录不能放在系统目录 %s 下", s)
		}
	}
	return nil
}

// inDir 路径在安装目录下（写文件、改配置只能在这里）。
func inDir(dir, p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == p && strings.HasPrefix(p, dir+"/")
}

func validatePlan(pl *proto.SoftwarePlan) error {
	switch {
	case pl == nil:
		return errors.New("缺少安装计划")
	case !reInstName.MatchString(pl.Name):
		return fmt.Errorf("名称只能用小写字母、数字、横线，以字母开头：%q", pl.Name)
	case pl.Version != "" && !reSwVersion.MatchString(pl.Version):
		return fmt.Errorf("版本无效：%q", pl.Version)
	case !reUserName.MatchString(pl.User) || pl.User == "root":
		return fmt.Errorf("运行用户无效（不能是 root）：%q", pl.User)
	case pl.Account != "" && !reAccount.MatchString(pl.Account):
		return fmt.Errorf("账号无效：%q", pl.Account)
	case pl.Exec == "" || strings.ContainsAny(pl.Exec, "\n\r"):
		return errors.New("启动命令无效")
	case pl.ReadyPort < 1 || pl.ReadyPort > 65535:
		return fmt.Errorf("端口无效：%d", pl.ReadyPort)
	case pl.Package == nil && pl.System == nil:
		return errors.New("没有安装包，也没有要从软件源装的包")
	}
	if err := validInstallDir(pl.Dir); err != nil {
		return err
	}
	for _, r := range pl.Reach {
		if err := validInstallDir(r); err != nil {
			return err
		}
	}
	for _, list := range [][]int{pl.Ports, pl.UDPPorts} {
		seen := map[int]bool{}
		for _, p := range list {
			if p < 1 || p > 65535 {
				return fmt.Errorf("端口无效：%d", p)
			}
			if seen[p] {
				return fmt.Errorf("端口 %d 重复", p)
			}
			seen[p] = true
		}
	}
	if pk := pl.Package; pk != nil {
		if !reSafe.MatchString(pk.File) || !zipx.IsSHA256(pk.SHA256) {
			return errors.New("安装包信息无效")
		}
		switch pk.Format {
		case "tar.gz", "tar.xz", "zip", "jar", "binary":
		default:
			return fmt.Errorf("安装包格式 %q 不认识", pk.Format)
		}
		if pk.Into != "" && !inDir(pl.Dir, pl.Dir+"/"+pk.Into) {
			return fmt.Errorf("安装包要放的位置无效：%q", pk.Into)
		}
	}
	if s := pl.System; s != nil {
		for _, n := range append(append([]string{}, s.DNF...), s.APT...) {
			for _, c := range strings.Split(n, "|") {
				if !rePkgName.MatchString(c) {
					return fmt.Errorf("软件包名无效：%q", n)
				}
			}
		}
		for _, u := range s.Disable {
			if !reUnitName.MatchString(u) {
				return fmt.Errorf("服务名无效：%q", u)
			}
		}
	}
	for _, f := range pl.Files {
		if !inDir(pl.Dir, f.Path) {
			return fmt.Errorf("文件只能写在安装目录下：%s", f.Path)
		}
	}
	for _, p := range pl.Props {
		if !inDir(pl.Dir, p.File) {
			return fmt.Errorf("只能改安装目录下的配置：%s", p.File)
		}
	}
	for _, c := range pl.Configs {
		if !inDir(pl.Dir, c.Path) {
			return fmt.Errorf("软件配置必须在安装目录内：%s", c.Path)
		}
		if c.Apply != "" && c.Apply != "restart" && c.Apply != "reload" {
			return fmt.Errorf("无效的配置生效方式：%s", c.Apply)
		}
	}
	if h := pl.Hosting; h != nil {
		if h.System {
			return errors.New("安装计划不能声明已有系统 nginx")
		}
		if pl.Template != "nginx" || !inDir(pl.Dir, h.Config) || !inDir(pl.Dir, h.Sites) || strings.TrimSuffix(h.Prefix, "/") != pl.Dir || !filepath.IsAbs(h.Binary) {
			return errors.New("nginx 托管路径无效")
		}
	}
	for k := range pl.Env {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(k) {
			return fmt.Errorf("环境变量名无效：%q", k)
		}
	}
	for _, v := range pl.Env {
		if strings.ContainsAny(v, "\n\r") {
			return errors.New("环境变量的值不能换行")
		}
	}
	return nil
}

// listeningPorts 服务器上正在监听的 TCP 端口；listeningUDP 绑定着的 UDP 端口（测试时替换）。
var (
	listeningPorts = func() map[int]bool { return listenPortsIn("/proc/net") }
	listeningUDP   = func() map[int]bool { return udpPortsIn("/proc/net") }
)

func listenPortsIn(dir string) map[int]bool { return procPorts(dir, []string{"tcp", "tcp6"}, "0A") }

// udpPortsIn 绑定着、没有连接对方的 UDP 端口（状态 07；连着对方的是程序自己往外发用的临时端口）。
func udpPortsIn(dir string) map[int]bool { return procPorts(dir, []string{"udp", "udp6"}, "07") }

// swPortUsers Ferry 装的软件占的端口（键为 tcp/端口、udp/端口）：停着的也算，启动时要用。skip 为不算的实例。
func (a *Agent) swPortUsers(skip string) map[string]string {
	out := map[string]string{}
	for _, in := range a.instances() {
		if in.Name == skip {
			continue
		}
		who := fmt.Sprintf("Ferry 装的 %s（%s）", strings.TrimSpace(in.Title), in.Name)
		tcp := in.AllPorts
		if len(tcp) == 0 { // 老记录只有端口参数
			for _, p := range in.Ports {
				tcp = append(tcp, p)
			}
		}
		for _, p := range tcp {
			out["tcp/"+strconv.Itoa(p)] = who
		}
		for _, p := range in.UDPPorts {
			out["udp/"+strconv.Itoa(p)] = who
		}
	}
	return out
}

func procPorts(dir string, files []string, state string) map[int]bool {
	out := map[int]bool{}
	for _, name := range files {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 4 || f[3] != state {
				continue
			}
			if state == "07" && !strings.HasSuffix(f[2], ":0000") {
				continue
			}
			if _, ph, ok := strings.Cut(f[1], ":"); ok {
				if p, err := strconv.ParseInt(ph, 16, 32); err == nil {
					out[int(p)] = true
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- 安装

func (a *Agent) swinstall() int {
	pl := a.req.Install
	if err := validatePlan(pl); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	stage := ""
	if pl.Package != nil && !pl.Package.Cached {
		stage = filepath.Clean(a.req.Stage)
		if filepath.Dir(stage) != a.incomingDir() {
			return a.result(proto.CodeBadInput, "上传目录不在 "+a.incomingDir()+" 下", nil)
		}
		defer os.RemoveAll(stage)
	}
	if err := os.MkdirAll(a.swDir(), 0o755); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	unlock, err := a.Sys.Lock(a.swLock(), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "另一个安装 / 卸载正在进行："+err.Error(), nil)
	}
	defer unlock()

	name, unit := pl.Name, swUnit(pl.Name)
	fail := func(format string, args ...any) int {
		msg := fmt.Sprintf(format, args...)
		a.fail(proto.EvFail, name, "%s", msg)
		return a.result(proto.CodeFailed, msg, nil)
	}
	// 名称、单元、目录、端口都不能已被占用
	if _, err := os.Stat(a.instFile(name)); err == nil {
		return fail("已经装过名为 %s 的软件：换一个名称，或先卸载它", name)
	}
	if _, err := os.Stat(a.unitPath(unit)); err == nil {
		return fail("服务器上已有系统服务 %s：换一个名称", unit)
	}
	created := false
	if ents, err := os.ReadDir(pl.Dir); err == nil {
		if len(ents) > 0 {
			return fail("安装目录 %s 已存在且不是空的：换一个目录", pl.Dir)
		}
	} else if !os.IsNotExist(err) {
		return fail("安装目录 %s 无法使用：%v", pl.Dir, err)
	} else {
		created = true
	}
	inUse, udpInUse, users := listeningPorts(), listeningUDP(), a.swPortUsers(name)
	for _, p := range pl.Ports {
		if who := users["tcp/"+strconv.Itoa(p)]; who != "" {
			return fail("端口 %d 已被 %s占用（它停着时也算）：换一个端口", p, who)
		}
		if inUse[p] {
			return fail("端口 %d 已被占用：换一个端口", p)
		}
	}
	for _, p := range pl.UDPPorts {
		if who := users["udp/"+strconv.Itoa(p)]; who != "" {
			return fail("UDP 端口 %d 已被 %s占用（它停着时也算）：换一个端口", p, who)
		}
		if udpInUse[p] {
			return fail("UDP 端口 %d 已被占用：换一个端口", p)
		}
	}
	src := ""
	installed := false
	if pk := pl.Package; pk != nil {
		src = filepath.Join(stage, pk.File)
		if pk.Cached {
			src = filepath.Join(a.swCacheDir(), pk.SHA256)
		}
		if got, size, err := zipx.FileSHA256(src); err != nil || got != pk.SHA256 || size != pk.Size {
			os.Remove(filepath.Join(a.swCacheDir(), pk.SHA256))
			return a.result(proto.CodeVerify, "安装包校验失败（传输中损坏），请重试", nil)
		}
		// 装好了清掉留着的安装包；失败时留着，改好后重新安装不用再上传
		defer func() {
			if installed {
				os.RemoveAll(a.swCacheDir())
			} else {
				a.keepPackage(name, src, pk.SHA256)
			}
		}()
	}
	a.info(proto.EvSoftware, name, "安装 %s 到 %s", strings.TrimSpace(pl.Title+" "+pl.Version), pl.Dir)

	var packages []string
	if s := pl.System; s != nil {
		var err error
		if packages, err = a.systemInstall(name, s, proxyEnvPrefix(pl), pl.Package != nil); err != nil {
			return fail("%v", err)
		}
	}
	uid, gid, err := a.ensureUser(pl.User, pl.Dir)
	if err != nil {
		return fail("%v", err)
	}
	// 上级目录 755（运行用户要能进去）；安装目录本身 750，交给运行用户
	if err := os.MkdirAll(filepath.Dir(pl.Dir), 0o755); err != nil {
		return fail("创建安装目录失败：%v", err)
	}
	if err := os.Mkdir(pl.Dir, 0o750); err != nil && !os.IsExist(err) {
		return fail("创建安装目录失败：%v", err)
	}
	// 运行用户要能进安装目录（和装在别处的 JDK）：进不了的上级目录，用户同意过就加上权限
	if err := a.swReach(pl); err != nil {
		if created {
			os.Remove(pl.Dir)
		}
		return fail("%v", err)
	}
	ok := false
	defer func() {
		if ok {
			return
		}
		// 失败：拆掉装了一半的，方便改了再装（从软件源装的包不删）
		a.Sys.Systemctl("disable", "--now", unit)
		if a.ferryUnitFile(unit) {
			os.Remove(a.unitPath(unit))
			a.Sys.Systemctl("daemon-reload")
		}
		if created {
			os.RemoveAll(pl.Dir)
		} else {
			clearDir(pl.Dir)
		}
		a.info(proto.EvSoftware, name, "已撤销这次安装")
	}()

	if pk := pl.Package; pk != nil {
		if err := a.unpack(name, src, pl.Dir, pk); err != nil {
			return fail("%v", err)
		}
	}
	if err := a.writeFiles(pl, "install"); err != nil {
		return fail("%v", err)
	}
	for _, p := range pl.Props {
		if err := setProperties(p.File, p.Sep, p.Set); err != nil {
			return fail("修改配置 %s 失败：%v", p.File, err)
		}
	}
	if err := a.runSteps(pl, "prepare", nil, nil); err != nil {
		return fail("%v", err)
	}
	if err := a.chownTree(pl.Dir, uid, gid); err != nil {
		return fail("把安装目录交给 %s 失败：%v", pl.User, err)
	}
	// 上级目录不让其他用户进（如 700 的 /data）时，运行用户进不了自己的目录
	if ok, err := a.Sys.AccessAs(pl.User, pl.Dir, 'x'); err == nil && !ok {
		return fail("运行用户 %s 进不了 %s：它的上级目录不允许其他用户进入，换一个安装目录，或给上级目录加上 o+x 权限", pl.User, pl.Dir)
	}
	if err := a.runSteps(pl, "init", nil, nil); err != nil {
		return fail("%v", err)
	}
	if len(pl.Env) > 0 {
		keys := make([]string, 0, len(pl.Env))
		for k := range pl.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s\n", k, pl.Env[k])
		}
		if err := writeFileAtomic(swEnvFile(pl.Dir), []byte(b.String()), 0o600); err != nil {
			return fail("%v", err)
		}
		a.Sys.Lchown(swEnvFile(pl.Dir), 0, 0)
	}
	if err := writeFileAtomic(a.unitPath(unit), []byte(swUnitText(pl)), 0o644); err != nil {
		return fail("写系统服务失败：%v", err)
	}
	if err := a.Sys.Systemctl("daemon-reload"); err != nil {
		return fail("%v", err)
	}
	a.info(proto.EvStart, name, "启动 %s", unit)
	if err := a.Sys.Systemctl("enable", "--now", unit); err != nil {
		return fail("%v", err)
	}
	spec := readySpec(name, pl.Logs, pl.ReadyPort, pl.ReadyTimeout)
	if err := a.waitReady(spec, unit); err != nil {
		a.diag(spec, unit)
		return a.result(proto.CodeFailed, "启动失败："+err.Error(), nil)
	}
	notes := append([]string{}, pl.Notes...)
	var unset []string
	if err := a.writeFiles(pl, "ready"); err != nil {
		return fail("%v", err)
	}
	if err := a.runSteps(pl, "ready", &notes, &unset); err != nil {
		return fail("%v", err)
	}
	if len(packages) > 0 {
		notes = append(notes, "从软件源装了 "+strings.Join(packages, " ")+"（卸载时不会删除这些包）")
	}
	inst := proto.SoftwareInstance{Name: name, Kind: pl.Template, Title: pl.Title, Version: pl.Version, Dir: pl.Dir, Unit: unit, User: pl.User,
		Ports: pl.PortMap, AllPorts: pl.Ports, UDPPorts: pl.UDPPorts, ReadyPort: pl.ReadyPort, Logs: pl.Logs, Packages: packages, InstalledAt: a.Sys.Now(), InstalledBy: a.req.Actor, Notes: notes, Unset: unset, Account: pl.Account, Configs: pl.Configs, Hosting: pl.Hosting}
	b, _ := json.MarshalIndent(inst, "", "  ")
	if err := writeFileAtomic(a.instFile(name), b, 0o644); err != nil {
		return fail("写安装记录失败：%v", err)
	}
	ok, installed = true, true
	a.info(proto.EvReady, name, "%s 已安装并启动", strings.TrimSpace(pl.Title+" "+pl.Version))
	return a.result(proto.CodeOK, "", inst)
}

func readySpec(name string, logs []string, port, timeout int) *proto.AppSpec {
	if timeout <= 0 {
		timeout = 180
	}
	s := &proto.AppSpec{Name: name, Health: proto.Health{TCP: "127.0.0.1:" + strconv.Itoa(port), TimeoutSec: timeout, StableSec: 3}}
	if len(logs) > 0 {
		s.DiagLogs = logs[:1]
	}
	return s
}

// clearDir 清空目录（目录是原来就有的空目录时，失败后恢复原样）。
func clearDir(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// systemInstall 用服务器的软件源装包；装包时被自动启动的系统自带服务停掉并禁用（原来就在运行的不动）。
// env 不为空时下载经这台电脑（proxyEnvPrefix）。deps 为 true 时只是安装包需要的系统库：没有包管理器时跳过（后面的检查会说缺什么）。
// 包名可以写几个候选（a|b）：用已经装着的，或软件源里有的第一个（不同发行版、版本包名不同，如 libaio1t64|libaio1）。
func (a *Agent) systemInstall(name string, s *proto.PlanSystem, env string, deps bool) ([]string, error) {
	var mgr string
	var pkgs []string
	for _, m := range []string{"dnf", "yum", "apt-get"} {
		if _, err := a.Sys.Shell("command -v "+m, 10*time.Second); err == nil {
			mgr = m
			break
		}
	}
	switch mgr {
	case "dnf", "yum":
		pkgs = s.DNF
	case "apt-get":
		pkgs = s.APT
	}
	if mgr == "" || len(pkgs) == 0 {
		if deps {
			a.warn(proto.EvWarn, name, "服务器上没有 dnf / yum / apt：需要的系统库没有自动安装")
			return nil, nil
		}
		return nil, errors.New("这种装法用不了这台服务器的软件包管理器（需要 dnf / yum / apt）：换一种装法")
	}
	// 已经装着的包不交给包管理器：apt install 会顺手把它升级，升级脚本还会重启正在用它的系统服务
	var have, want, alts []string
	for _, p := range pkgs {
		cands := strings.Split(p, "|")
		got := ""
		for _, c := range cands {
			if a.pkgInstalled(mgr, c) {
				got = c
				break
			}
		}
		switch {
		case got != "":
			have = append(have, got)
		case len(cands) == 1:
			want = append(want, p)
		default:
			alts = append(alts, p)
		}
	}
	if len(alts) > 0 {
		want = append(want, a.pickPackages(mgr, env, alts)...)
	}
	if len(have) > 0 {
		a.info(proto.EvSoftware, name, "服务器上已经装着 %s：直接用，不升级", strings.Join(have, " "))
	}
	if len(want) == 0 {
		return nil, a.checkRequire(s)
	}
	pkgs = want
	// 装包前就有的系统服务（原来就装着、或用户自己建的）不动；只停掉这次装包新建出来的
	before := map[string]bool{}
	for _, u := range s.Disable {
		p := a.Sys.UnitProps(u)
		before[u] = isRunning(p) || p.Load == "loaded"
	}
	via := "服务器自己的网络"
	if env != "" {
		via = "下载经这台电脑"
	}
	a.info(proto.EvSoftware, name, "从软件源安装 %s（%s，%s，可能要几分钟）", strings.Join(pkgs, " "), mgr, via)
	cmd := env + mgr + " install -y " + strings.Join(pkgs, " ")
	if mgr == "apt-get" {
		cmd = env + "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-upgrade " + strings.Join(pkgs, " ")
	}
	out, err := a.Sys.Shell(cmd+" 2>&1", 20*time.Minute)
	if err != nil && mgr == "apt-get" {
		// 软件包索引太旧时先更新再试
		a.Sys.Shell(env+"DEBIAN_FRONTEND=noninteractive apt-get update 2>&1", 10*time.Minute)
		out, err = a.Sys.Shell(cmd+" 2>&1", 20*time.Minute)
	}
	if err != nil {
		lines := splitLines(out)
		if len(lines) > 8 {
			lines = lines[len(lines)-8:]
		}
		hint := "服务器连不上软件源时，安装时把下载方式选成「经这台电脑」"
		if env != "" {
			hint = "下载经这台电脑：这台电脑也要能访问这些软件源"
		}
		return nil, fmt.Errorf("从软件源安装 %s 失败（%s）：%v %s", strings.Join(pkgs, " "), hint, err, strings.Join(lines, " / "))
	}
	for _, u := range s.Disable {
		if before[u] {
			if isRunning(a.Sys.UnitProps(u)) {
				a.warn(proto.EvWarn, name, "%s 原来就有，没有动它", u)
			}
			continue
		}
		a.Sys.Systemctl("disable", "--now", u)
	}
	return pkgs, a.checkRequire(s)
}

// checkRequire 软件源装好后必须有的程序。
func (a *Agent) checkRequire(s *proto.PlanSystem) error {
	if s.Require != "" {
		if _, err := os.Stat(s.Require); err != nil {
			return fmt.Errorf("软件源装好了，但没有 %s：这个软件源里的包可能不一样", s.Require)
		}
	}
	return nil
}

// pickPackages 候选包名里软件源中有的第一个（apt 的索引里一个都没有时先更新索引）；都查不到时用第一个，装的时候报错。
func (a *Agent) pickPackages(mgr, env string, alts []string) []string {
	exists := func(p string) bool {
		cmd := env + mgr + " -q list " + shellQuote(p) + " >/dev/null 2>&1"
		if mgr == "apt-get" {
			cmd = "apt-cache show " + shellQuote(p) + " 2>/dev/null | grep -q '^Package:'"
		}
		_, err := a.Sys.Shell(cmd, 5*time.Minute)
		return err == nil
	}
	var out []string
	for _, alt := range alts {
		cands := strings.Split(alt, "|")
		got := ""
		for round := 0; round < 2 && got == ""; round++ {
			for _, c := range cands {
				if exists(c) {
					got = c
					break
				}
			}
			if got == "" && round == 0 && mgr == "apt-get" {
				a.Sys.Shell(env+"DEBIAN_FRONTEND=noninteractive apt-get update 2>&1", 10*time.Minute)
				continue
			}
			break
		}
		if got == "" {
			got = cands[0]
		}
		out = append(out, got)
	}
	return out
}

// pkgInstalled 包已经装在服务器上。
func (a *Agent) pkgInstalled(mgr, pkg string) bool {
	cmd := "rpm -q --quiet " + shellQuote(pkg)
	if mgr == "apt-get" {
		cmd = "dpkg-query -W -f='${Status}' " + shellQuote(pkg) + " 2>/dev/null | grep -q 'install ok installed'"
	}
	_, err := a.Sys.Shell(cmd, 30*time.Second)
	return err == nil
}

// ensureUser 运行用户：没有就创建（系统用户，不能登录）。
func (a *Agent) ensureUser(name, home string) (int, int, error) {
	if uid, gid, err := a.Sys.LookupUser(name); err == nil {
		return uid, gid, nil
	}
	a.info(proto.EvSoftware, "", "创建运行用户 %s", name)
	nologin := "/sbin/nologin"
	if _, err := os.Stat(nologin); err != nil {
		nologin = "/usr/sbin/nologin"
	}
	if out, err := a.Sys.Shell(fmt.Sprintf("useradd -r -M -s %s -d %s %s", nologin, shellQuote(home), name), time.Minute); err != nil {
		return 0, 0, fmt.Errorf("创建运行用户 %s 失败：%v %s", name, err, strings.TrimSpace(out))
	}
	return a.Sys.LookupUser(name)
}

// createRunUser 部署时创建应用的运行用户：系统用户、不能登录，有家目录（有的程序会往家目录写缓存）。
func (a *Agent) createRunUser(name string) error {
	nologin := "/sbin/nologin"
	if _, err := os.Stat(nologin); err != nil {
		nologin = "/usr/sbin/nologin"
	}
	if out, err := a.Sys.Shell(fmt.Sprintf("useradd -r -m -s %s %s", nologin, shellQuote(name)), time.Minute); err != nil {
		return fmt.Errorf("创建运行用户 %s 失败：%v %s", name, err, strings.TrimSpace(out))
	}
	if _, _, err := a.Sys.LookupUser(name); err != nil {
		return fmt.Errorf("创建运行用户 %s 后仍查不到它：%v", name, err)
	}
	return nil
}

// unpack 把安装包放进安装目录。
func (a *Agent) unpack(name, src, dir string, pk *proto.PlanPackage) error {
	dst := dir
	if pk.Into != "" {
		dst = filepath.Join(dir, filepath.FromSlash(pk.Into))
	}
	switch pk.Format {
	case "jar", "binary":
		if pk.Into == "" {
			dst = filepath.Join(dir, pk.File)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := moveFile(src, dst); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if pk.Format == "binary" {
			mode = 0o755
		}
		return os.Chmod(dst, mode)
	}
	a.info(proto.EvSoftware, name, "解压安装包")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	var err error
	switch pk.Format {
	case "tar.gz":
		err = extractTarGzStrip(src, dst, pk.Strip)
	case "zip":
		err = extractZip(src, dst, pk.Strip)
	case "tar.xz":
		if _, e := a.Sys.Shell("command -v xz", 10*time.Second); e != nil {
			return errors.New("服务器上没有 xz，解不开 .tar.xz：先安装 xz（yum install -y xz 或 apt install -y xz-utils）")
		}
		var out string
		out, err = a.Sys.Shell(fmt.Sprintf("tar -xJf %s -C %s --strip-components=%d --no-same-owner", shellQuote(src), shellQuote(dst), pk.Strip), 10*time.Minute)
		if err != nil {
			err = fmt.Errorf("%v %s", err, strings.TrimSpace(out))
		}
	}
	if err != nil {
		return fmt.Errorf("解压失败：%v", err)
	}
	if pk.Check != "" {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(pk.Check))); err != nil {
			return fmt.Errorf("安装包里没有 %s：不是这个软件的安装包？", pk.Check)
		}
	}
	return nil
}

// archivePath 去掉前 strip 层目录后的相对路径；越界的返回错误，去完为空的返回空。
func archivePath(name string, strip int) (string, error) {
	n := path.Clean(strings.TrimPrefix(name, "./"))
	parts := strings.Split(n, "/")
	if len(parts) <= strip {
		return "", nil
	}
	rest := strings.Join(parts[strip:], "/")
	if path.IsAbs(n) || rest == ".." || strings.HasPrefix(rest, "../") {
		return "", fmt.Errorf("压缩包里有越界路径：%s", name)
	}
	return rest, nil
}

// extractTarGzStrip 解开 tar.gz（去掉前 strip 层目录），保留可执行位与目录内的符号链接；越界的一律拒绝。
func extractTarGzStrip(src, dst string, strip int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rest, err := archivePath(h.Name, strip)
		if err != nil {
			return err
		}
		if rest == "" || rest == "." {
			continue
		}
		p := filepath.Join(dst, filepath.FromSlash(rest))
		if !within(dst, p) {
			return fmt.Errorf("压缩包里有越界路径：%s", h.Name)
		}
		mode := os.FileMode(h.Mode).Perm()&0o755 | 0o600
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeEntry(p, tr, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if path.IsAbs(h.Linkname) || !within(dst, filepath.Join(filepath.Dir(p), filepath.FromSlash(h.Linkname))) {
				return fmt.Errorf("压缩包里有指向外部的链接：%s → %s", h.Name, h.Linkname)
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeLink:
			lr, err := archivePath(h.Linkname, strip)
			if err != nil || lr == "" {
				return fmt.Errorf("压缩包里有指向外部的链接：%s → %s", h.Name, h.Linkname)
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Link(filepath.Join(dst, filepath.FromSlash(lr)), p); err != nil {
				return err
			}
		}
	}
}

// extractZip 解开 zip（去掉前 strip 层目录）。
func extractZip(src, dst string, strip int) error {
	z, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer z.Close()
	for _, f := range z.File {
		rest, err := archivePath(f.Name, strip)
		if err != nil {
			return err
		}
		if rest == "" || rest == "." {
			continue
		}
		p := filepath.Join(dst, filepath.FromSlash(rest))
		if !within(dst, p) {
			return fmt.Errorf("压缩包里有越界路径：%s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue // zip 里的符号链接不展开
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode().Perm()&0o755 | 0o600
		err = writeEntry(p, rc, mode)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeEntry(p string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(p, mode)
}

// writeFiles 写 at 阶段的文件（就绪后写的要保留运行用户为属主）。
func (a *Agent) writeFiles(pl *proto.SoftwarePlan, at string) error {
	for _, f := range pl.Files {
		fa := f.At
		if fa == "" {
			fa = "install"
		}
		if fa != at {
			continue
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o640
		}
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(f.Path, []byte(f.Content), mode&0o777); err != nil {
			return fmt.Errorf("写 %s 失败：%v", f.Path, err)
		}
		if at == "ready" {
			if uid, gid, err := a.Sys.LookupUser(pl.User); err == nil {
				a.Sys.Lchown(f.Path, uid, gid)
			}
		}
	}
	return nil
}

// runSteps 执行 at 阶段的步骤；soft 的失败只提示（加到 notes），它要设置的参数记进 unset。
func (a *Agent) runSteps(pl *proto.SoftwarePlan, at string, notes, unset *[]string) error {
	for _, s := range pl.Steps {
		sa := s.At
		if sa == "" {
			sa = "prepare"
		}
		if sa != at {
			continue
		}
		var err error
		if s.HTTP != nil {
			err = a.stepHTTP(s.HTTP)
		} else {
			timeout := time.Duration(s.Timeout) * time.Second
			if timeout <= 0 {
				timeout = 10 * time.Minute
			}
			var out string
			out, err = a.Sys.Shell(proxyEnvPrefix(pl)+"cd "+shellQuote(pl.Dir)+" && { "+s.Run+"\n} 2>&1", timeout)
			if err != nil {
				lines := splitLines(strings.TrimSpace(out))
				if len(lines) > 10 {
					lines = lines[len(lines)-10:]
				}
				if len(lines) > 0 {
					err = fmt.Errorf("%v：%s", err, strings.Join(lines, " / "))
				}
			}
		}
		if err == nil {
			continue
		}
		if s.Soft != "" {
			a.warn(proto.EvWarn, pl.Name, "%s（%v）", s.Soft, err)
			if notes != nil {
				*notes = append(*notes, s.Soft)
			}
			if unset != nil && s.Sets != "" {
				*unset = append(*unset, s.Sets)
			}
			continue
		}
		if s.Error != "" {
			return fmt.Errorf("%s（%v）", s.Error, err)
		}
		return fmt.Errorf("执行失败：%s（%v）", firstLine(s.Run), err)
	}
	return nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(l) > 120 {
		l = l[:120] + "…"
	}
	return l
}

// stepHTTP 就绪后调用接口（只能调本机）。
func (a *Agent) stepHTTP(h *proto.PlanHTTP) error {
	u, err := url.Parse(h.Post)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return fmt.Errorf("只能调用本机的接口：%s", h.Post)
	}
	form := url.Values{}
	for k, v := range h.Form {
		form.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := a.Sys.Now().Add(time.Duration(h.Retry) * time.Second)
	for {
		resp, err := client.PostForm(h.Post, form)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return nil
			}
			err = fmt.Errorf("%s 返回 %d", h.Post, resp.StatusCode)
		}
		if !a.Sys.Now().Before(deadline) {
			return err
		}
		a.Sys.Sleep(2 * time.Second)
	}
}

// swUnitText 软件的 systemd 单元。
func swUnitText(pl *proto.SoftwarePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s（软件 %s）\n", ManagedMarker, pl.Name)
	fmt.Fprintf(&b, "[Unit]\nDescription=%s %s (%s)\nAfter=network.target\n\n[Service]\nType=simple\n", pl.Title, pl.Version, pl.Name)
	fmt.Fprintf(&b, "User=%s\n", pl.User)
	if pl.Workdir != "" {
		fmt.Fprintf(&b, "WorkingDirectory=%s\n", pl.Workdir)
	}
	if len(pl.Env) > 0 {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", swEnvFile(pl.Dir))
	}
	// systemd 会把 % 当说明符：写成 %%
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.ReplaceAll(pl.Exec, "%", "%%"))
	if pl.Hosting != nil {
		fmt.Fprintf(&b, "ExecReload=/bin/kill -HUP $MAINPID\n")
	}
	lowPort := pl.Hosting != nil // 共享 nginx 后续添加的站点可能使用 80 / 443。
	for _, port := range pl.Ports {
		if port < 1024 {
			lowPort = true
		}
	}
	if lowPort {
		b.WriteString("AmbientCapabilities=CAP_NET_BIND_SERVICE\n")
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\nLimitNOFILE=65535\nSuccessExitStatus=143\nTimeoutStopSec=60\n\n[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}

// setProperties 改配置文件里的项（sep 为 = 或空格）：有这一行就替换，没有就加在最后。
func setProperties(file, sep string, kv map[string]string) error {
	if sep == "" {
		sep = "="
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	done := map[string]bool{}
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		var k string
		if sep == "=" {
			k, _, _ = strings.Cut(t, "=")
		} else {
			k, _, _ = strings.Cut(t, " ")
		}
		k = strings.TrimSpace(k)
		if v, want := kv[k]; want {
			lines[i] = k + sep + v
			done[k] = true
		}
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !done[k] {
			lines = append(lines, k+sep+kv[k])
		}
	}
	fi, _ := os.Stat(file)
	mode := os.FileMode(0o640)
	if fi != nil {
		mode = fi.Mode().Perm()
	}
	return os.WriteFile(file, []byte(strings.Join(lines, "\n")), mode)
}

// ---------------------------------------------------------------- 启停、卸载、日志

func (a *Agent) swcontrol() int {
	in, err := a.loadInstance(a.req.Instance)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	action := a.req.Action
	if action != "start" && action != "stop" && action != "restart" {
		return a.result(proto.CodeBadInput, "未知操作："+action, nil)
	}
	label := map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}[action]
	a.info(proto.EvSoftware, in.Name, "%s %s", label, in.Unit)
	if err := a.Sys.Systemctl(action, in.Unit); err != nil {
		a.fail(proto.EvFail, in.Name, "%v", err)
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	if action != "stop" && in.ReadyPort > 0 {
		spec := readySpec(in.Name, in.Logs, in.ReadyPort, 240)
		if err := a.waitReady(spec, in.Unit); err != nil {
			a.diag(spec, in.Unit)
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
	}
	return a.result(proto.CodeOK, "", nil)
}

func (a *Agent) swremove() int {
	in, err := a.loadInstance(a.req.Instance)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	if in.Hosting != nil {
		hostUnlock, err := a.Sys.Lock(filepath.Join(a.swDir(), "nginx-"+in.Name+".lock"), 30*time.Second)
		if err != nil {
			return a.result(proto.CodeLocked, err.Error(), nil)
		}
		defer hostUnlock()
		sites, _ := filepath.Glob(filepath.Join(in.Hosting.Sites, "*.conf"))
		if len(sites) > 0 {
			return a.result(proto.CodeBadInput, "此 nginx 仍有项目站点，请先停用或迁移站点后再卸载", nil)
		}
	}
	unlock, err := a.Sys.Lock(a.swLock(), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "另一个安装 / 卸载正在进行："+err.Error(), nil)
	}
	defer unlock()
	a.info(proto.EvSoftware, in.Name, "停止并删除系统服务 %s", in.Unit)
	stopErr := a.Sys.Systemctl("disable", "--now", in.Unit)
	// 确认停下来了才删：还在运行时删目录，数据会在使用中被删掉（单元文件已经不在、停止报错但进程已退出的可以继续）
	if p := a.Sys.UnitProps(in.Unit); isRunning(p) || p.Active == "deactivating" || p.PID != 0 {
		why := ""
		if stopErr != nil {
			why = "：" + stopErr.Error()
		}
		msg := fmt.Sprintf("%s 没有停下来（%s/%s）%s。没有删除任何东西，查看日志处理后再卸载", in.Unit, p.Active, p.Sub, why)
		a.fail(proto.EvFail, in.Name, "%s", msg)
		return a.result(proto.CodeFailed, msg, nil)
	}
	if a.ferryUnitFile(in.Unit) {
		if err := os.Remove(a.unitPath(in.Unit)); err != nil {
			msg := fmt.Sprintf("删除系统服务 %s 失败：%v。安装记录保留，可以再卸载一次", in.Unit, err)
			a.fail(proto.EvFail, in.Name, "%s", msg)
			return a.result(proto.CodeFailed, msg, nil)
		}
	}
	a.Sys.Systemctl("daemon-reload")
	if a.req.Purge {
		a.info(proto.EvSoftware, in.Name, "删除安装目录 %s（含数据）", in.Dir)
		if err := os.RemoveAll(in.Dir); err != nil {
			msg := fmt.Sprintf("删除 %s 失败：%v。服务已停止并删除，安装记录保留，处理后可以再卸载一次", in.Dir, err)
			a.fail(proto.EvFail, in.Name, "%s", msg)
			return a.result(proto.CodeFailed, msg, nil)
		}
	} else {
		a.info(proto.EvSoftware, in.Name, "安装目录 %s（含数据）保留，不需要了可以手动删除", in.Dir)
	}
	if len(in.Packages) > 0 {
		a.info(proto.EvSoftware, in.Name, "从软件源装的 %s 没有删除（可能还有别的程序在用），不需要的话手动卸载", strings.Join(in.Packages, " "))
	}
	if err := os.Remove(a.instFile(in.Name)); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	a.info(proto.EvSoftware, in.Name, "已卸载")
	return a.result(proto.CodeOK, "", nil)
}

func (a *Agent) swlogs() int {
	in, err := a.loadInstance(a.req.Instance)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	n := a.req.Lines
	if n <= 0 || n > 5000 {
		n = 200
	}
	if a.req.LogMain {
		if len(in.Logs) == 0 {
			return a.result(proto.CodeFailed, "这个软件没有单独的日志文件：看系统服务输出", nil)
		}
		f := in.Logs[0]
		lines, err := tailFile(f, n+1)
		if err != nil {
			return a.result(proto.CodeFailed, "读 "+f+" 失败："+err.Error(), nil)
		}
		trunc := len(lines) > n
		if trunc {
			lines = lines[1:]
		}
		return a.result(proto.CodeOK, f, proto.LogText{Lines: lines, Truncated: trunc})
	}
	lines := a.Sys.Journal(in.Unit, n, time.Time{})
	return a.result(proto.CodeOK, "journalctl -u "+in.Unit, proto.LogText{Lines: lines, Truncated: len(lines) >= n})
}
