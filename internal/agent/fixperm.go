package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
)

// 部署时自动修正权限（Request.FixPerms）："plan" 只列出将要做的修改，"apply" 修改后重新检查。
// 只做最小的改动：要写入的交给运行用户（chown，不递归）；要读 / 进入 / 执行的只补缺的那一位（ACL 或 chmod）。
// 系统目录、一级目录（/home、/data 这类挂载点）、别人的家目录不动；部署根目录的上级只补权限、不改属主。

const (
	FixPlan  = "plan"
	FixApply = "apply"
)

// protectedPath 不自动修改的路径。
func protectedPath(p string) bool {
	p = path.Clean(p)
	if p == "/" || strings.Count(p, "/") == 1 {
		return true // 一级目录
	}
	for _, d := range []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot", "/proc", "/sys", "/dev", "/run", "/var/lib", "/var/run", "/root"} {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// otherHome 是别的用户的家目录本身（/home/alice）。
func otherHome(p, user string) bool {
	p = path.Clean(p)
	return strings.HasPrefix(p, "/home/") && strings.Count(p, "/") == 2 && path.Base(p) != user
}

// isRootOrAncestor p 是某个部署根目录或其上级（不能交给运行用户，否则服务能替换 root 执行的东西）。
func (a *Agent) isRootOrAncestor(p string) bool {
	roots := []string{a.req.Root}
	for _, r := range a.appRoots {
		roots = append(roots, r)
	}
	for _, r := range roots {
		if r == p || strings.HasPrefix(r, p+"/") || p == "/" {
			return true
		}
	}
	return false
}

// fixAccess 运行用户 user 对 p 缺 flag 权限时的修正。返回给人看的说明与是否能修正；apply 时真的修改。
func (a *Agent) fixAccess(user, p string, flag byte, isDir, apply bool) (string, bool) {
	if protectedPath(p) || otherHome(p, user) {
		return "", false
	}
	o, err := a.Sys.Ownership(p)
	uid, gids, uerr := a.Sys.UserGroups(user)
	if err != nil || uerr != nil {
		return "", false
	}
	perm := uint32(o.Perm.Perm())
	bit := map[byte]uint32{'r': 4, 'w': 2, 'x': 1}[flag]
	inGroup := false
	for _, g := range gids {
		if g == o.GID {
			inGroup = true
		}
	}
	var cls uint32
	switch {
	case uid == o.UID:
		cls = perm >> 6 & 7
	case inGroup:
		cls = perm >> 3 & 7
	default:
		cls = perm & 7
	}
	if cls&bit != 0 {
		return "", false // 权限位允许却访问不了：ACL / 安全策略，不自动改
	}
	if flag == 'w' {
		// 要写入（如临时目录）：交给运行用户
		if a.isRootOrAncestor(p) {
			return "", false
		}
		_, gid, err := a.Sys.LookupUser(user)
		if err != nil {
			return "", false
		}
		desc := fmt.Sprintf("把 %s 交给 %s（chown %s: %s）", p, user, user, p)
		if apply {
			if err := a.Sys.Lchown(p, uid, gid); err != nil {
				return desc + "失败：" + err.Error(), false
			}
		}
		return desc, true
	}
	word := permWord(flag, isDir)
	switch {
	case uid == o.UID:
		desc := fmt.Sprintf("给属主 %s 加上%s %s 的权限（chmod u+%c %s）", user, word, p, flag, p)
		if apply {
			if err := a.Sys.Chmod(p, modeBits(o.Perm)|os.FileMode(bit<<6)); err != nil {
				return desc + "失败：" + err.Error(), false
			}
		}
		return desc, true
	case inGroup:
		desc := fmt.Sprintf("让组 %s 可以%s %s（chmod g+%c %s）", o.Group, word, p, flag, p)
		if apply {
			if err := a.Sys.Chmod(p, modeBits(o.Perm)|os.FileMode(bit<<3)); err != nil {
				return desc + "失败：" + err.Error(), false
			}
		}
		return desc, true
	}
	// 其他用户：优先只给运行用户开（ACL），文件系统不支持时再对所有用户开放这一位
	aclDesc := fmt.Sprintf("只给 %s 加上%s %s 的权限（setfacl -m u:%s:%c %s）", user, word, p, user, flag, p)
	if !apply {
		if _, err := exec.LookPath("setfacl"); err == nil {
			return aclDesc, true
		}
	} else if supported, err := a.Sys.SetFACL(user, p, flag); supported {
		if err != nil {
			return aclDesc + "失败：" + err.Error(), false
		}
		return aclDesc, true
	}
	desc := fmt.Sprintf("让所有用户可以%s %s（chmod o+%c %s）", word, p, flag, p)
	if apply {
		if err := a.Sys.Chmod(p, modeBits(o.Perm)|os.FileMode(bit)); err != nil {
			return desc + "失败：" + err.Error(), false
		}
	}
	return desc, true
}

// modeBits 权限位连同 setuid / setgid / sticky（chmod 时不能丢掉）。
func modeBits(m os.FileMode) os.FileMode {
	return m & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

// fixRootDir 部署根目录（及其下 ferry 管理的目录）属主不是 root 或他人可写：改回 root 所有、去掉组与其他用户的写权限。
// 上级目录不自动改（可能是挂载点或别人的目录）。
func (a *Agent) fixRootDir(root, d string, apply bool) (string, bool) {
	if d != root && !strings.HasPrefix(d, root+"/") || protectedPath(d) {
		return "", false
	}
	o, err := a.Sys.Ownership(d)
	if err != nil {
		return "", false
	}
	var steps []string
	if o.UID != 0 {
		steps = append(steps, "chown root:root "+d)
	}
	if o.Perm.Perm()&0o022 != 0 {
		steps = append(steps, "chmod go-w "+d)
	}
	if len(steps) == 0 {
		return "", false
	}
	desc := fmt.Sprintf("把部署目录 %s 改回 root 所有、其他用户不可写（%s）", d, strings.Join(steps, "；"))
	if apply {
		if o.UID != 0 {
			if err := a.Sys.Lchown(d, 0, 0); err != nil {
				return desc + "失败：" + err.Error(), false
			}
		}
		if err := a.Sys.Chmod(d, modeBits(o.Perm)&^0o022); err != nil {
			return desc + "失败：" + err.Error(), false
		}
	}
	return desc, true
}
