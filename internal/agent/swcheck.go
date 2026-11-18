package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// 安装软件前的检查（swcheck）：引擎在下载、上传之前调用，问题在一开始就发现，不用白传几百 MB 再撤销。
// 运行用户进不了上级目录（如 700 的 /home/alice）时，引擎问过用户后在安装计划里打开 FixParents，
// swinstall 创建运行用户后给这些目录加上进入的权限再继续。

// swCacheDir 上次安装失败时留下的安装包（只留一个，文件名是 sha256）：改好后重新安装不用再上传。
func (a *Agent) swCacheDir() string { return filepath.Join(a.swDir(), ".cache") }

func (a *Agent) swcheck() int {
	c := a.req.SwCheck
	if c == nil {
		return a.result(proto.CodeBadInput, "缺少检查内容", nil)
	}
	var out proto.SwCheck
	if c.Dir != "" {
		if err := validInstallDir(c.Dir); err != nil {
			return a.result(proto.CodeBadInput, err.Error(), nil)
		}
		if !reUserName.MatchString(c.User) || c.User == "root" {
			return a.result(proto.CodeBadInput, fmt.Sprintf("运行用户无效（不能是 root）：%q", c.User), nil)
		}
		if ents, err := os.ReadDir(c.Dir); err == nil && len(ents) > 0 {
			out.DirBusy = true
		}
		out.Blocked = a.blockedDirs(c.User, c.Dir, false)
		out.FreeMB, out.Inodes, out.ReadOnly = diskInfo(nearestDir(c.Dir))
		out.StageFreeMB, _, _ = diskInfo(nearestDir(a.incomingDir()))
		if c.ExecuteInDir && noExecMount(nearestDir(c.Dir)) {
			return a.result(proto.CodeBadInput, "安装目录所在的挂载点设置了 noexec："+c.Dir+"。请选择允许执行程序的目录", nil)
		}
	}
	if c.JavaDir != "" {
		if err := validInstallDir(c.JavaDir); err != nil {
			return a.result(proto.CodeBadInput, "JDK "+err.Error(), nil)
		}
		out.JavaHome = a.findJDK(c.JavaDir, c.Java)
		javaPath := c.JavaDir
		if out.JavaHome != "" {
			javaPath = out.JavaHome
		}
		if noExecMount(nearestDir(javaPath)) {
			return a.result(proto.CodeBadInput, "JDK 目录所在的挂载点设置了 noexec："+c.JavaDir, nil)
		}
		if c.User != "" {
			_, err := os.Stat(javaPath)
			for _, b := range a.blockedDirs(c.User, javaPath, err == nil) {
				if !blockedHas(out.Blocked, b.Path) {
					out.Blocked = append(out.Blocked, b)
				}
			}
		}
	}
	if c.PackageSHA != "" && zipx.IsSHA256(c.PackageSHA) {
		if _, err := os.Stat(filepath.Join(a.swCacheDir(), c.PackageSHA)); err == nil {
			out.Cached = true
		}
	}
	if c.NeedXZ {
		if _, err := a.Sys.Shell("command -v xz", 10*time.Second); err != nil {
			return a.result(proto.CodeBadInput, "服务器缺少 xz，不能解压这个安装包。请先安装 xz 或 xz-utils", nil)
		}
	}
	if c.System != nil {
		mgr := a.pkgManager()
		if err := checkSystemBundle(mgr, c.System.Bundle); err != nil {
			return a.result(proto.CodeBadInput, err.Error(), nil)
		}
		pkgs := systemPackages(c.System, mgr)
		if len(pkgs) == 0 && c.System.Require != "" {
			return a.result(proto.CodeBadInput, "当前服务器的软件包管理器不支持这套安装方案", nil)
		}
		_, want, alternatives := a.installedPackages(mgr, pkgs)
		out.RequiredPackages = append(want, alternatives...)
		out.MissingPackages = append(out.MissingPackages, out.RequiredPackages...)
	}
	if c.SELinux != nil && a.selinuxEnforcing() {
		if _, err := a.Sys.Shell("command -v semanage", 10*time.Second); err != nil {
			out.MissingPackages = append(out.MissingPackages, "semanage")
		}
	}
	return a.result(proto.CodeOK, "", out)
}

// swresolve 模拟包事务。包管理器可以刷新索引，但不会安装或改动已装的软件包。
func (a *Agent) swresolve() int {
	spec := a.req.SwResolve
	if spec == nil || len(spec.Packages) == 0 {
		return a.result(proto.CodeBadInput, "没有要检查的软件包", nil)
	}
	if err := validatePackageNames(spec.Packages); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	mgr := a.pkgManager()
	if mgr == "" {
		return a.result(proto.CodeBadInput, "服务器没有可用的 dnf、yum 或 apt-get", nil)
	}
	pl := &proto.SoftwarePlan{Proxy: spec.Proxy, NoProxy: spec.NoProxy}
	env := proxyEnvPrefix(pl)
	if spec.Proxy != "" && env == "" {
		return a.result(proto.CodeBadInput, "软件源代理地址无效", nil)
	}
	deadline := time.Now().Add(time.Minute)
	run := func(_ string, cmd string, timeout time.Duration) (string, error) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", fmt.Errorf("软件包预检超时（1 分钟）")
		}
		return a.Sys.Shell(cmd, min(remaining, timeout))
	}
	_, want, alternatives := a.installedPackages(mgr, spec.Packages)
	want = append(want, pickPackages(run, mgr, env, alternatives)...)
	var changes []string
	if len(want) > 0 {
		var err error
		changes, err = pkgChanges(run, mgr, env, want, nil)
		if err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
	}
	return a.result(proto.CodeOK, "", proto.SwResolution{Packages: want, Changes: changes})
}

// nearestDir p 或它最近的已经存在的上级目录。
func nearestDir(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func blockedHas(list []proto.BlockedDir, p string) bool {
	for _, b := range list {
		if b.Path == p {
			return true
		}
	}
	return false
}

// blockedDirs 运行用户进不了的 p 的上级目录（从上往下；self 时连 p 本身）。不存在的目录不算（安装时按 755 创建）。
// 用户已存在时以它的身份实际测试；还没创建时按权限位判断（新建的系统用户对这些目录来说是「其他用户」）。
func (a *Agent) blockedDirs(user, p string, self bool) []proto.BlockedDir {
	_, _, uerr := a.Sys.LookupUser(user)
	var out []proto.BlockedDir
	dirs := []string{}
	for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
		dirs = append([]string{d}, dirs...)
	}
	if self {
		dirs = append(dirs, p)
	}
	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			break // 下面的都还不存在
		}
		blocked := false
		if uerr == nil {
			ok, err := a.Sys.AccessAs(user, d, 'x')
			blocked = err == nil && !ok
		} else if o, err := a.Sys.Ownership(d); err == nil {
			blocked = o.Perm.Perm()&0o001 == 0
		}
		if blocked {
			b := proto.BlockedDir{Path: d}
			if o, err := a.Sys.Ownership(d); err == nil {
				b.Owner, b.Mode = o.User, o.Perm.String()
			}
			out = append(out, b)
			if uerr == nil {
				break // 进不去这一级，下面的实际测试都会失败，说明不了什么
			}
		}
	}
	return out
}

// systemDir 不自动改权限的系统目录。
func systemDir(p string) bool {
	if p == "/" {
		return true
	}
	for _, s := range systemDirs {
		if p == s || strings.HasPrefix(p, s+"/") && !strings.HasPrefix(p, "/usr/local/") {
			return true
		}
	}
	return false
}

// grantTraverse 让 user 能进入目录 p：支持 ACL 时只给它开，否则给其他用户加上 x（进得去，但列不出里面有哪些文件）。
func (a *Agent) grantTraverse(user, p string) (string, error) {
	if systemDir(p) {
		return "", fmt.Errorf("%s 是系统目录，不自动修改它的权限", p)
	}
	acl := fmt.Sprintf("只给 %s 加上进入 %s 的权限（setfacl -m u:%s:x %s）", user, p, user, p)
	if supported, err := a.Sys.SetFACL(user, p, 'x'); supported {
		if err != nil {
			return "", fmt.Errorf("%s失败：%v", acl, err)
		}
		return acl, nil
	}
	o, err := a.Sys.Ownership(p)
	if err != nil {
		return "", fmt.Errorf("读取 %s 的权限失败：%v", p, err)
	}
	desc := fmt.Sprintf("给 %s 加上 o+x（chmod o+x %s：其他用户可以进入，但看不到里面有哪些文件）", p, p)
	if err := a.Sys.Chmod(p, modeBits(o.Perm)|0o001); err != nil {
		return "", fmt.Errorf("%s失败：%v", desc, err)
	}
	return desc, nil
}

// swReach 运行用户要能进安装目录和 Reach 里的目录：进不了的上级目录在 FixParents 时加上权限，否则报错。
func (a *Agent) swReach(pl *proto.SoftwarePlan) error {
	for i, p := range append([]string{pl.Dir}, pl.Reach...) {
		granted := map[string]bool{}
		for {
			blocked := a.blockedDirs(pl.User, p, i > 0)
			if len(blocked) == 0 {
				break
			}
			b := blocked[0]
			if !pl.FixParents {
				return fmt.Errorf("运行用户 %s 进不了 %s：%s。同意给它加上进入的权限后重新安装，或换一个安装目录", pl.User, p, blockedWords(blocked))
			}
			if granted[b.Path] {
				return fmt.Errorf("给 %s 加上权限后，运行用户 %s 还是进不去：可能被 ACL 或安全策略（SELinux、麒麟安全）拦截了，换一个安装目录", b.Path, pl.User)
			}
			desc, err := a.grantTraverse(pl.User, b.Path)
			if err != nil {
				return err
			}
			granted[b.Path] = true
			a.info(proto.EvSoftware, pl.Name, "%s", desc)
		}
	}
	return nil
}

// blockedWords 「上级目录 /home/alice（属于 alice，drwx------）不允许其他用户进入」
func blockedWords(list []proto.BlockedDir) string {
	parts := make([]string, 0, len(list))
	for _, b := range list {
		s := b.Path
		if b.Owner != "" {
			s += fmt.Sprintf("（属于 %s，%s）", b.Owner, b.Mode)
		}
		parts = append(parts, s)
	}
	return "上级目录 " + strings.Join(parts, "、") + " 不允许其他用户进入"
}

// keepPackage 安装失败时把校验过的安装包留在服务器上（只留这一个），下次装同一个包时不用再上传。
func (a *Agent) keepPackage(name, src, sha string) {
	dir := a.swCacheDir()
	dst := filepath.Join(dir, sha)
	if src == dst {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	if os.Rename(src, dst) == nil {
		a.info(proto.EvSoftware, name, "安装包留在服务器上：改好后重新安装同一个版本不用再上传")
	}
}

// findJDK dir 下 Ferry 装的 java 大版本的 JDK（以版本命名的文件夹，有几个时用版本最新的）；dir 本身就是时用它。没有为空。
func (a *Agent) findJDK(dir string, java int) string {
	if a.ferryJDKAt(dir, java) {
		return dir
	}
	ents, _ := os.ReadDir(dir)
	found := ""
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() && reRuntimeID.MatchString(e.Name()) && a.ferryJDKAt(p, java) && (found == "" || versionLess(filepath.Base(found), e.Name())) {
			found = p
		}
	}
	return filepath.ToSlash(found)
}

// versionLess 按数字逐段比较版本（jdk-17.0.9_9-x64 < jdk-17.0.13_11-x64）。
func versionLess(a, b string) bool {
	num := func(s string) []int {
		var out []int
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
			n, _ := strconv.Atoi(p)
			out = append(out, n)
		}
		return out
	}
	x, y := num(a), num(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// ferryJDKAt dir 里是 Ferry 装的 java 大版本的 JDK。
func (a *Agent) ferryJDKAt(dir string, java int) bool {
	b, err := os.ReadFile(filepath.Join(dir, ".ferry-runtime.json"))
	if err != nil {
		return false
	}
	var m proto.Runtime
	if json.Unmarshal(b, &m) != nil || m.Java != java {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, "bin", "java"))
	return err == nil
}
