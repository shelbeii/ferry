package agent

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/rundist"
)

func (a *Agent) removeJDK() int {
	r := a.req.Runtime
	if r == nil || !reRuntimeID.MatchString(r.ID) || !path.IsAbs(r.Dir) || path.Clean(r.Dir) != r.Dir {
		return a.result(proto.CodeBadInput, "请指定运行环境清单中的 Java 绝对路径", nil)
	}
	dir := a.runtimesDir()
	home := filepath.Join(r.Dir, r.ID)
	if err := checkJDKParents(dir); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	if !realDir(dir) {
		return a.result(proto.CodeBadInput, "服务器上没有这个 Ferry 管理的 JDK", nil)
	}
	unlock, err := a.Sys.Lock(filepath.Join(dir, ".lock"), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "无法锁定运行环境："+err.Error(), nil)
	}
	defer unlock()

	item, ok := customJDKAt(home)
	if !ok {
		return a.result(proto.CodeBadInput, "服务器上没有这个 Ferry 管理的 JDK："+home, nil)
	}
	item.Custom = r.Dir != dir
	registration := ""
	if item.Custom {
		if err := validInstallDir(home); err != nil {
			return a.result(proto.CodeBadInput, err.Error(), nil)
		}
		locations := a.jdkLocationsDir()
		if err := checkJDKParents(locations); err != nil {
			return a.result(proto.CodeBadInput, err.Error(), nil)
		}
		registration = filepath.Join(locations, jdkLocationKey(home))
		if !sameFile(registration, []byte(home)) {
			return a.result(proto.CodeBadInput, "此位置不在 Ferry 的运行环境清单中："+home, nil)
		}
	}
	for _, other := range a.customJDKs() {
		if strings.HasPrefix(other.Path, home+"/") && other.Path != item.Path {
			return a.result(proto.CodeBadInput, "请先卸载目录内的其他运行环境："+other.Path, nil)
		}
	}
	for _, other := range a.managedRuntimes() {
		if strings.HasPrefix(other.Home, home+"/") {
			return a.result(proto.CodeBadInput, "请先卸载目录内的其他运行环境："+other.Path, nil)
		}
	}
	if err := checkRuntimeRemovalMounts(home); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	links, err := a.jdkRemovalLinks(item)
	if err != nil {
		return a.result(proto.CodeFailed, "读取 JDK 链接失败："+err.Error(), nil)
	}
	if err := a.checkRuntimeInUse(home, links, "JDK"); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	item.Shared = slices.Contains(links, filepath.Join(dir, "java-"+strconv.Itoa(item.Java)))
	if err := detachRuntime(home, registration, links); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	a.info(proto.EvRuntime, "", "已卸载 JDK %s：%s", item.Version, home)
	return a.result(proto.CodeOK, "", item)
}

func (a *Agent) jdkRemovalLinks(item proto.InstalledJDK) ([]string, error) {
	if item.Custom {
		return nil, nil
	}
	dir := a.runtimesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	shared := "java-" + strconv.Itoa(item.Java)
	var links []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink == 0 || (name != shared && !reJavaPin.MatchString(name)) {
			continue
		}
		link := filepath.Join(dir, name)
		target, err := os.Readlink(link)
		if err != nil {
			return nil, err
		}
		if target == item.ID {
			links = append(links, link)
		}
	}
	return links, nil
}

// 先移出清单，链接或登记清理失败时恢复安装。删除文件阶段只操作私有的暂存目录。
func detachRuntime(home, registration string, links []string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(home), ".ferry-remove-runtime-")
	if err != nil {
		return fmt.Errorf("创建卸载暂存目录失败：%w", err)
	}
	defer os.Remove(tmp)
	id := filepath.Base(home)
	parked := filepath.Join(tmp, id)
	if err := os.Rename(home, parked); err != nil {
		return fmt.Errorf("移动运行环境安装目录失败：%w", err)
	}
	restore := func(err error, removed []string) error {
		err = errors.Join(err, os.Rename(parked, home))
		for _, link := range removed {
			err = errors.Join(err, os.Symlink(id, link))
		}
		return err
	}
	for i, link := range links {
		if err := os.Remove(link); err != nil {
			return restore(fmt.Errorf("删除共享链接失败：%w", err), links[:i])
		}
	}
	if registration != "" {
		if err := os.Remove(registration); err != nil {
			return restore(fmt.Errorf("删除运行环境安装登记失败：%w", err), links)
		}
	}
	if err := os.RemoveAll(parked); err != nil {
		return fmt.Errorf("运行环境已从清单移除，清理暂存目录 %s 失败：%w", tmp, err)
	}
	return nil
}

func checkRuntimeRemovalMounts(home string) error {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("无法检查运行环境安装目录的挂载点：%w", err)
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for line := range strings.SplitSeq(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 {
			mount := unescape.Replace(fields[4])
			if mount == home || strings.HasPrefix(mount, home+"/") {
				return fmt.Errorf("运行环境安装目录内有挂载点，请先卸载挂载：%s", mount)
			}
		}
	}
	return nil
}

var absoluteUnitPath = regexp.MustCompile(`/[a-zA-Z0-9._+/-]+`)
var unitEnvFileSeparator = regexp.MustCompile(`\n|[ \t]+\(ignore_errors=(?:yes|no)\)`)

// 发布使用 Ferry 运行环境的服务定义与卸载互斥。
func (a *Agent) lockUnitRuntimes(text []byte) (func(), error) {
	dir := a.runtimesDir()
	nop := func() {}
	if !realDir(dir) {
		return nop, nil
	}
	needs, _ := unitAccessNeeds(string(text))
	var programs []string
	for _, need := range needs {
		if need.soft {
			continue
		}
		p := need.path
		if py := venvRuntimePath(p); py != "" {
			p = py
		}
		if home, ok := strings.CutSuffix(p, "/bin/java"); ok && (filepath.Dir(home) == dir || reRuntimeID.MatchString(filepath.Base(home))) {
			programs = append(programs, p)
			continue
		}
		for _, kind := range []string{"node", "python", "dotnet"} {
			if home, ok := strings.CutSuffix(p, "/"+rundist.Executable(kind)); ok &&
				(filepath.Dir(home) == dir || strings.HasPrefix(filepath.Base(home), kind+"-")) {
				programs = append(programs, p)
				break
			}
		}
	}
	if len(programs) == 0 {
		return nop, nil
	}
	unlock, err := a.Sys.Lock(filepath.Join(dir, ".lock"), 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("无法锁定运行环境：%w", err)
	}
	for _, program := range programs {
		if _, err := os.Stat(program); err != nil {
			unlock()
			return nil, fmt.Errorf("运行环境路径不可用 %s，请安装对应版本后重试：%w", program, err)
		}
	}
	return unlock, nil
}

func venvRuntimePath(program string) string {
	base, _, ok := strings.Cut(program, "/.venv/bin/")
	if !ok {
		return ""
	}
	file := filepath.Join(base, ".venv", "pyvenv.cfg")
	if st, err := os.Stat(file); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	data, err := infoReadFile(file, 8<<10)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, _ := strings.Cut(line, "=")
		if strings.TrimSpace(key) == "home" && path.IsAbs(strings.TrimSpace(value)) {
			return path.Join(strings.TrimSpace(value), "python3")
		}
	}
	return ""
}

// runtimeEnvUsesPath 检查 systemd 已加载的 EnvironmentFile 列表，兼容旧版的单数属性。
// 环境文件可能包含凭据，检测结果只返回是否引用运行环境。
func runtimeEnvUsesPath(files string, usesRuntime func(string) bool) bool {
	for _, pattern := range unitEnvFileSeparator.Split(files, -1) {
		pattern = strings.Trim(strings.TrimSpace(pattern), `"`)
		if !filepath.IsAbs(pattern) {
			continue
		}
		matches, _ := filepath.Glob(pattern)
		for _, file := range matches {
			if st, err := os.Stat(file); err != nil || !st.Mode().IsRegular() {
				continue
			}
			data, err := infoReadFile(file, 1<<20)
			if err != nil {
				continue
			}
			for line := range strings.SplitSeq(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || line[0] == '#' || line[0] == ';' {
					continue
				}
				if _, value, ok := strings.Cut(line, "="); ok && usesRuntime(value) {
					return true
				}
			}
		}
	}
	return false
}

func (a *Agent) checkRuntimeInUse(home string, links []string, label string) error {
	references := func(p string) bool {
		if p == home || strings.HasPrefix(p, home+"/") {
			return true
		}
		for _, link := range links {
			if p == link || strings.HasPrefix(p, link+"/") {
				return true
			}
		}
		resolved, err := filepath.EvalSymlinks(p)
		return err == nil && (resolved == home || strings.HasPrefix(resolved, home+"/"))
	}
	usesRuntime := func(text string) bool {
		for _, p := range absoluteUnitPath.FindAllString(text, -1) {
			if references(p) || (label == "Python" && references(venvRuntimePath(p))) {
				return true
			}
		}
		return false
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("无法检查服务器进程：%w", err)
	}
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil || !p.IsDir() {
			continue
		}
		proc := filepath.Join("/proc", p.Name())
		exe, _ := os.Readlink(filepath.Join(proc, "exe"))
		if exe != "" && references(strings.TrimSuffix(exe, " (deleted)")) {
			return fmt.Errorf("%s 正被运行进程 PID %s 使用，请先停止该进程", label, p.Name())
		}
	}

	list, err := a.Sys.Shell("LC_ALL=C systemctl list-unit-files --type=service --no-legend --no-pager && LC_ALL=C systemctl list-units --type=service --all --plain --no-legend --no-pager", 20*time.Second)
	if err != nil || len(list) >= shellOutputMax {
		return errors.New("无法读取服务器的 systemd 服务清单，卸载已停止")
	}
	units := map[string]bool{}
	for line := range strings.SplitSeq(list, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}
		unit := fields[0]
		if template, ok := strings.CutSuffix(unit, "@.service"); ok {
			unit = template + "@ferry-runtime-check.service"
		}
		units[unit] = true
	}
	var args []string
	for unit := range units {
		args = append(args, shellQuote(unit))
	}
	slices.Sort(args)
	for batch := range slices.Chunk(args, 32) {
		out, err := a.Sys.Shell("LC_ALL=C systemctl show --all --no-pager -p Id -p Environment -p EnvironmentFiles -p ExecStart -p ExecStartPre -p ExecStartPost -p ExecReload -p ExecStop -p ExecStopPost -- "+strings.Join(batch, " "), 20*time.Second)
		if err != nil || len(out) >= shellOutputMax {
			return errors.New("无法核对 systemd 服务的运行环境，卸载已停止")
		}
		for block := range strings.SplitSeq(out, "\n\n") {
			props := appInfoUnitProperties(block)
			if usesRuntime(block) || runtimeEnvUsesPath(props["EnvironmentFiles"], usesRuntime) {
				return fmt.Errorf("%s 被系统服务 %s 引用，请切换该服务的运行环境后重试", label, props["Id"])
			}
		}
	}
	return nil
}
