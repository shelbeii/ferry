package agent

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type nginxSiteBinding struct{ Instance, File string }

func (a *Agent) siteBindingFile(app string) string {
	return filepath.Join(a.appMetaDir(app), "nginx-site.json")
}
func (a *Agent) readSiteBinding(app string) nginxSiteBinding {
	var b nginxSiteBinding
	raw, _ := os.ReadFile(a.siteBindingFile(app))
	json.Unmarshal(raw, &b)
	return b
}
func (a *Agent) hostingInstance(s *proto.AppSpec) (*proto.SoftwareInstance, error) {
	in, err := a.loadHostingInstance(s.NginxInstance)
	if err != nil {
		if s.NginxInstance == proto.SystemNginxInstance {
			return nil, fmt.Errorf("无法接入系统 nginx：%w", err)
		}
		return nil, fmt.Errorf("请先在软件中安装 nginx 实例 %s：%v", s.NginxInstance, err)
	}
	if in.Kind != "nginx" || in.Hosting == nil {
		return nil, fmt.Errorf("%s 不是支持托管站点的独立 nginx 实例", s.NginxInstance)
	}
	h := in.Hosting
	if !h.System && (!hostingPathWithin(in.Dir, h.Config) || !hostingPathWithin(in.Dir, h.Sites) || strings.TrimSuffix(h.Prefix, "/") != in.Dir || !filepath.IsAbs(h.Binary)) {
		return nil, errors.New("nginx 实例的托管路径记录无效")
	}
	if h.System && s.NginxInstance != proto.SystemNginxInstance {
		return nil, errors.New("软件安装记录不能冒充系统 nginx 实例")
	}
	if !isRunning(a.Sys.UnitProps(in.Unit)) {
		return nil, fmt.Errorf("nginx 实例 %s 未运行，请先在软件中启动它", in.Name)
	}
	return in, nil
}
func hostingPathWithin(dir, p string) bool {
	if inDir(dir, p) {
		return true
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsAbs(p) && filepath.Clean(p) == p && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (a *Agent) nginxSitePath(s *proto.AppSpec, in *proto.SoftwareInstance) string {
	sum := sha256.Sum256([]byte(a.req.Root + "\x00" + a.req.Project + "\x00" + s.Name))
	return filepath.Join(in.Hosting.Sites, fmt.Sprintf("ferry-%x.conf", sum[:12]))
}

func (a *Agent) publishNginxSite(s *proto.AppSpec, id string) error {
	in, err := a.hostingInstance(s)
	if err != nil {
		return err
	}
	unlock, err := a.Sys.Lock(filepath.Join(a.swDir(), "nginx-"+in.Name+".lock"), 30*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	file := a.nginxSitePath(s, in)
	binding := a.readSiteBinding(s.Name)
	if binding.Instance != "" && binding.Instance != in.Name && exists(binding.File) {
		return errors.New("此站点还在另一个 nginx 实例中启用，请先在原配置下停止站点，再切换实例")
	}
	before, readErr := os.ReadFile(file)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if readErr == nil && !strings.HasPrefix(string(before), ManagedMarker) {
		return errors.New("站点配置已存在且不是 Ferry 生成，拒绝覆盖")
	}
	if err := os.MkdirAll(in.Hosting.Sites, 0o755); err != nil {
		return err
	}
	if id == "" {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		_, port := splitSiteListen(s.SitePort)
		if listeningPorts()[port] {
			own := false
			for _, l := range a.Sys.Listening(a.Sys.UnitProps(in.Unit).PID) {
				if strings.HasSuffix(l, ":"+strconv.Itoa(port)) {
					own = true
				}
			}
			if !own {
				if msg := a.portBusy(port, in.Unit); msg != "" {
					return errors.New(msg)
				}
			}
		}
		conf, err := os.ReadFile(a.verFile(s.Name, id, "nginx.conf"))
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(conf), ManagedMarker) {
			return errors.New("站点版本的 nginx 配置没有管理标记")
		}
		logs := filepath.Join(a.sharedDir(s.Name), "logs")
		if err := os.MkdirAll(logs, 0o755); err != nil {
			return err
		}
		uid, gid, err := a.Sys.LookupUser(in.User)
		if err != nil {
			return err
		}
		if err := a.Sys.Lchown(logs, uid, gid); err != nil {
			return err
		}
		if s.TLS != nil {
			copy := *s
			copy.Unit = in.Unit
			copy.User = in.User
			if err := a.tlsPrepare(&copy); err != nil {
				return err
			}
		}
		if err := writeFileAtomic(file, conf, 0o644); err != nil {
			return err
		}
	}
	revert := func() {
		if readErr == nil {
			writeFileAtomic(file, before, 0o644)
		} else {
			os.Remove(file)
		}
	}
	check := nginxCheckCommand(in.Hosting)
	if !in.Hosting.System {
		check = "runuser -u " + shellQuote(in.User) + " -- " + check
	}
	if out, err := a.Sys.Shell(check+" 2>&1", 15*time.Second); err != nil || strings.Contains(out, "conflicting server name") {
		revert()
		return fmt.Errorf("nginx 配置校验失败，原站点配置已保留：%v %s", err, strings.TrimSpace(out))
	}
	if err := a.Sys.Systemctl("reload", in.Unit); err != nil {
		revert()
		if rollback := a.Sys.Systemctl("reload", in.Unit); rollback != nil {
			return fmt.Errorf("重新加载 nginx 失败：%v；已还原配置文件，但重新加载原配置也失败：%v", err, rollback)
		}
		return fmt.Errorf("重新加载 nginx 失败，已重新加载原配置：%v", err)
	}
	if id != "" {
		b, _ := json.Marshal(nginxSiteBinding{Instance: in.Name, File: file})
		if err := writeFileAtomic(a.siteBindingFile(s.Name), b, 0o644); err != nil {
			return err
		}
		os.Remove(filepath.Join(a.appMetaDir(s.Name), "site-disabled"))
		if s.TLS != nil {
			copy := *s
			copy.Unit = in.Unit
			copy.User = in.User
			a.tlsAfterStart(&copy)
		}
	}
	a.info(proto.EvReload, s.Name, "已更新 nginx 实例 %s 的站点配置", in.Name)
	return nil
}

// standaloneUnits 这个应用以前自带 nginx 时的单元（Ferry 生成的，同一项目、同一应用）：改用 nginx 实例后不再需要。
func (a *Agent) standaloneUnits(s *proto.AppSpec) []string {
	var out []string
	for unit, v := range a.managedUnits() {
		if v[0] == a.req.Project && v[1] == s.Name && (v[2] == "" || a.req.Target == "" || v[2] == a.req.Target) {
			out = append(out, unit)
		}
	}
	sort.Strings(out)
	return out
}

// retireUnits 改用 nginx 实例成功后：停用并删掉自带 nginx 的单元。
func (a *Agent) retireUnits(s *proto.AppSpec, units []string) {
	for _, u := range units {
		if err := a.Sys.Systemctl("disable", u); err != nil {
			a.warn(proto.EvWarn, s.Name, "取消 %s 的开机自启失败：%v", u, err)
		}
		if err := os.Remove(a.unitPath(u)); err != nil && !os.IsNotExist(err) {
			a.warn(proto.EvWarn, s.Name, "删除 %s 失败：%v", a.unitPath(u), err)
			continue
		}
		a.info(proto.EvInfo, s.Name, "已删除原来自带 nginx 的服务 %s（改用 nginx 实例 %s）", u, s.NginxInstance)
	}
	if len(units) > 0 {
		if err := a.Sys.Systemctl("daemon-reload"); err != nil {
			a.warn(proto.EvWarn, s.Name, "%v", err)
		}
	}
}

func splitSiteListen(s string) (string, int) {
	host := ""
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		host = s[:i]
		s = s[i+1:]
	}
	p, _ := strconv.Atoi(s)
	return host, p
}
func (a *Agent) controlNginxSite(s *proto.AppSpec, action string) error {
	cur := a.current(s.Name)
	if cur == "" {
		return errors.New("站点还没有部署")
	}
	if action == "stop" {
		if err := a.publishNginxSite(s, ""); err != nil {
			return err
		}
		return writeFileAtomic(filepath.Join(a.appMetaDir(s.Name), "site-disabled"), []byte("disabled\n"), 0o644)
	}
	if err := a.publishNginxSite(s, cur); err != nil {
		return err
	}
	return a.waitProbe(s)
}
func (a *Agent) nginxSiteStatus(s *proto.AppSpec, st *proto.AppStatus) {
	in, err := a.loadHostingInstance(s.NginxInstance)
	if err != nil || in.Hosting == nil {
		st.Active = "inactive"
		st.Problem = "所选 nginx 实例不存在"
		if err != nil {
			st.Problem = "nginx 实例检测失败：" + err.Error()
		}
		return
	}
	p := a.Sys.UnitProps(in.Unit)
	st.Unit = in.Unit
	st.PID = p.PID
	st.Since = p.Since
	st.Active = p.Active
	st.Sub = p.Sub
	st.Port = healthPort(s.Health)
	if !exists(a.nginxSitePath(s, in)) {
		st.Active = "inactive"
		st.PID = 0
		st.Problem = "站点已停用或尚未发布"
		return
	}
	st.Listen = []string{s.SitePort}
	st.Sites = []proto.Site{{Listen: []string{s.SitePort}, Names: s.SiteNames, File: a.nginxSitePath(s, in)}}
	if isRunning(p) {
		ok := a.Sys.Probe(s.Health) == nil
		st.Healthy = &ok
	}
}
