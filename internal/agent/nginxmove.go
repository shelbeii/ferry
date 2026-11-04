package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type nginxRuntimeRecord struct {
	Instance string       `json:"instance"`
	Port     string       `json:"port"`
	Names    []string     `json:"names,omitempty"`
	Health   proto.Health `json:"health"`
}

func (a *Agent) saveNginxRuntime(s *proto.AppSpec, id string) error {
	file := filepath.Join(a.verMetaDir(s.Name, id), "nginx-runtime.json")
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return err
	}
	data, _ := json.Marshal(nginxRuntimeRecord{Instance: s.NginxInstance, Port: s.SitePort, Names: s.SiteNames, Health: s.Health})
	return writeFileAtomic(file, data, 0644)
}
func (a *Agent) loadNginxRuntime(s *proto.AppSpec, id string) bool {
	raw, err := os.ReadFile(filepath.Join(a.verMetaDir(s.Name, id), "nginx-runtime.json"))
	if err != nil {
		return false
	}
	var r nginxRuntimeRecord
	if json.Unmarshal(raw, &r) != nil || r.Instance == "" {
		return false
	}
	s.NginxInstance, s.SitePort, s.SiteNames, s.Health = r.Instance, r.Port, r.Names, r.Health
	s.Unit = ""
	return true
}

// 查询运行状态时以当前版本和已发布绑定为准；保存的新选择可能还没部署，或部署已经回滚。
func (a *Agent) deployedStaticSpec(s proto.AppSpec) proto.AppSpec {
	if s.Type != proto.TypeStatic {
		return s
	}
	current := a.current(s.Name)
	if current == "" {
		return s
	}
	if unit, err := infoReadFile(a.verFile(s.Name, current, "unit.service"), 256*1024); err == nil && len(unit) > 0 {
		s.NginxInstance = ""
		if units := a.standaloneUnits(&s); len(units) == 1 {
			s.Unit = units[0]
		}
		if conf, err := infoReadFile(a.verFile(s.Name, current, "nginx.conf"), 256*1024); err == nil {
			for _, server := range parseNginx(string(conf)) {
				if len(server.listen) > 0 {
					s.SitePort = strings.TrimPrefix(server.listen[0], "*:")
					_, port := splitSiteListen(s.SitePort)
					if configured := healthPort(s.Health); configured != "" && configured != fmt.Sprint(port) {
						// 旧版本没有保存完整健康检查，不把新环境的检查地址当成旧站点的。
						s.Health = proto.Health{}
					}
					break
				}
			}
		}
	} else if binding := a.readSiteBinding(s.Name); binding.Instance != "" && (s.NginxInstance != "" || exists(a.verFile(s.Name, current, "nginx.conf")) || exists(binding.File)) {
		s.NginxInstance, s.Unit = binding.Instance, ""
		a.loadNginxRuntime(&s, current)
	}
	return s
}

// 多实例事务按名字排序加锁，publish 中复用持有的锁，避免相反方向迁移互相等待。
func (a *Agent) lockNginxInstances(names ...string) (func(), error) {
	if a.nginxLocks == nil {
		a.nginxLocks = map[string]bool{}
	}
	sort.Strings(names)
	var releases []func()
	unlock := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, name := range names {
		if a.nginxLocks[name] {
			continue
		}
		release, err := a.Sys.Lock(filepath.Join(a.swDir(), "nginx-"+name+".lock"), 30*time.Second)
		if err != nil {
			unlock()
			return nil, err
		}
		a.nginxLocks[name] = true
		releases = append(releases, func() { delete(a.nginxLocks, name); release() })
	}
	return unlock, nil
}

func (a *Agent) reloadHosting(in *proto.SoftwareInstance) error {
	if !isRunning(a.Sys.UnitProps(in.Unit)) {
		return nil
	}
	if err := a.checkHostingNginx(in); err != nil {
		return fmt.Errorf("%s 配置检查失败：%w", in.Name, err)
	}
	return a.Sys.Systemctl("reload", in.Unit)
}

func (a *Agent) switchNginxInstance(s *proto.AppSpec, id, prev string, binding nginxSiteBinding) (string, string) {
	old, err := a.loadHostingInstance(binding.Instance)
	if err != nil || old == nil || old.Hosting == nil {
		return "FAILED", fmt.Sprintf("无法读取原 Nginx 实例 %s：%v", binding.Instance, err)
	}
	next, err := a.hostingInstance(s)
	if err != nil {
		return "FAILED", err.Error()
	}
	previous := *s
	previous.NginxInstance = binding.Instance
	if binding.File != a.nginxSitePath(&previous, old) {
		return "FAILED", "原站点绑定的位置不匹配，未修改任何站点"
	}
	file := a.nginxSitePath(s, next)
	if file == binding.File {
		return "FAILED", "两个实例引用了相同的站点目录，无法确认是独立运行环境"
	}
	unlock, err := a.lockNginxInstances(old.Name, next.Name)
	if err != nil {
		return "FAILED", err.Error()
	}
	defer unlock()
	before, oldErr := os.ReadFile(binding.File)
	if oldErr != nil && !os.IsNotExist(oldErr) {
		return "FAILED", oldErr.Error()
	}
	if oldErr == nil && !strings.HasPrefix(string(before), ManagedMarker) {
		return "FAILED", "原站点不是 Ferry 管理的配置，拒绝迁移"
	}
	targetBefore, targetErr := os.ReadFile(file)
	if targetErr != nil && !os.IsNotExist(targetErr) {
		return "FAILED", targetErr.Error()
	}
	if targetErr == nil && !strings.HasPrefix(string(targetBefore), ManagedMarker) {
		return "FAILED", "目标站点不是 Ferry 管理的配置，拒绝覆盖"
	}
	bindingBefore, err := os.ReadFile(a.siteBindingFile(s.Name))
	if err != nil {
		return "FAILED", err.Error()
	}
	disabled := exists(filepath.Join(a.appMetaDir(s.Name), "site-disabled"))
	if !a.loadNginxRuntime(&previous, prev) {
		for _, server := range parseNginx(string(before)) {
			if len(server.listen) > 0 {
				previous.SitePort = strings.TrimPrefix(server.listen[0], "*:")
			}
			previous.SiteNames = server.names
			break
		}
		if err := a.saveNginxRuntime(&previous, prev); err != nil {
			return "FAILED", err.Error()
		}
	}
	// 恢复时尝试所有步骤，任何一个失败都明确报告，不掩盖配置或重载错误。
	restore := func() error {
		var issues []error
		restoreFile := func(p string, bytes []byte, readErr error) {
			var err error
			if readErr == nil {
				err = writeFileAtomic(p, bytes, 0644)
			} else {
				err = os.Remove(p)
				if os.IsNotExist(err) {
					err = nil
				}
			}
			if err != nil {
				issues = append(issues, err)
			}
		}
		restoreFile(file, targetBefore, targetErr)
		if err := a.reloadHosting(next); err != nil {
			issues = append(issues, err)
		}
		if err := a.activate(s, prev); err != nil {
			issues = append(issues, err)
		}
		restoreFile(binding.File, before, oldErr)
		if err := writeFileAtomic(a.siteBindingFile(s.Name), bindingBefore, 0644); err != nil {
			issues = append(issues, err)
		}
		if disabled {
			if err := writeFileAtomic(filepath.Join(a.appMetaDir(s.Name), "site-disabled"), []byte("disabled\n"), 0644); err != nil {
				issues = append(issues, err)
			}
		}
		if err := a.prepareNginxSiteLogs(&previous, old.User); err != nil {
			issues = append(issues, err)
		}
		if err := a.reloadHosting(old); err != nil {
			issues = append(issues, err)
		}
		return errors.Join(issues...)
	}
	a.info(proto.EvInfo, s.Name, "迁移本站点：Nginx %s → %s（不停止共享实例）", old.Name, next.Name)
	if oldErr == nil {
		err = os.Remove(binding.File)
	}
	if err == nil {
		err = a.reloadHosting(old)
	}
	if err == nil {
		err = a.activate(s, id)
	}
	if err == nil {
		err = a.publishNginxSite(s, id)
	}
	if err == nil {
		err = a.waitProbe(s)
	}
	if err == nil && s.Hooks != nil {
		err = a.runHooks(s, hookPhase{name: "afterSwitch", label: "切换后的命令", dir: a.runningDir(s.Name)}, s.Hooks.AfterSwitch, id, prev)
	}
	if err == nil {
		a.info(proto.EvSwitch, s.Name, "已切换到 Nginx %s，running → %s", next.Name, id)
		return "OK", ""
	}
	a.fail(proto.EvFail, s.Name, "切换 Nginx 失败：%v", err)
	a.markFailed(s.Name, id)
	if a.req.NoRollback {
		return "FAILED", err.Error()
	}
	if rollbackErr := restore(); rollbackErr != nil {
		return "ROLLBACK_FAILED", fmt.Sprintf("%v；恢复原 Nginx 失败：%v", err, rollbackErr)
	}
	a.info(proto.EvRollback, s.Name, "已恢复 Nginx %s 和版本 %s", old.Name, prev)
	return "ROLLED_BACK", err.Error()
}
