package agent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type backendHTTPSState struct {
	Domains  []string `json:"domains"`
	Port     int      `json:"port"`
	Unit     string   `json:"unit"`
	Instance string   `json:"instance,omitempty"`
	Custom   bool     `json:"custom,omitempty"`
}

type backendHTTPSRelease struct {
	Upstream string       `json:"upstream"`
	Health   proto.Health `json:"health"`
}

func (a *Agent) backendHTTPSRelease(app, id string) (backendHTTPSRelease, bool) {
	var release backendHTTPSRelease
	data, err := os.ReadFile(filepath.Join(a.verMetaDir(app, id), "https-release.json"))
	return release, err == nil && json.Unmarshal(data, &release) == nil
}

func validateBackendHTTPS(s *proto.AppSpec) error {
	h := s.HTTPS
	if s.Type == proto.TypeStatic || s.Job != nil || h.Port < 1 || h.Port > 65535 || len(h.TLS.Domains) == 0 ||
		!strings.HasPrefix(h.Config, ManagedMarker) || !strings.Contains(h.Config, proto.BackendHTTPSUpstream) || (h.Cert == "") != (h.Key == "") {
		return fmt.Errorf("%s：后端 HTTPS 配置无效", s.Name)
	}
	if h.Instance != "" {
		if h.Instance != proto.SystemNginxInstance && !reInstName.MatchString(h.Instance) {
			return fmt.Errorf("%s：nginx 实例名无效", s.Name)
		}
	} else if !strings.HasSuffix(h.Unit, ".service") || !reSafe.MatchString(strings.ReplaceAll(h.Unit, "@", "_")) ||
		!strings.HasPrefix(h.UnitText, ManagedMarker) || !reNginxLink.MatchString(s.RuntimeNginx) ||
		!path.IsAbs(h.Binary) || !path.IsAbs(h.Prefix) {
		return fmt.Errorf("%s：本机 HTTPS nginx 的配置无效", s.Name)
	}
	if h.Instance == "" && (h.Unit == s.Unit || s.ZD != nil && h.Unit != s.ZD.ProxyUnit) {
		return fmt.Errorf("%s：HTTPS 代理单元无效", s.Name)
	}
	return nil
}

func (a *Agent) backendHTTPSFile(app string) string {
	return filepath.Join(a.appMetaDir(app), "https.json")
}

func (a *Agent) readBackendHTTPS(app string) (state backendHTTPSState) {
	b, _ := os.ReadFile(a.backendHTTPSFile(app))
	json.Unmarshal(b, &state)
	return state
}

func (a *Agent) httpsProblem(app, msg string) {
	a.setHTTPSProblem(app, "https-error", msg)
}

func (a *Agent) setHTTPSProblem(app, name, msg string) {
	p := filepath.Join(a.appMetaDir(app), name)
	if msg == "" {
		os.Remove(p)
	} else {
		writeFileAtomic(p, []byte(msg), 0o644)
	}
}

func (a *Agent) backendHTTPSStatus(s *proto.AppSpec) *proto.HTTPSStatus {
	state := a.readBackendHTTPS(s.Name)
	if s.HTTPS == nil && state.Unit == "" {
		return nil
	}
	out := &proto.HTTPSStatus{Domains: state.Domains, Port: state.Port, Custom: state.Custom}
	if state.Unit == "" {
		out.Domains, out.Port, out.Custom = s.HTTPS.TLS.Domains, s.HTTPS.Port, s.HTTPS.Cert != ""
		out.Problem = "HTTPS 尚未配置成功"
	} else {
		out.Active = isRunning(a.Sys.UnitProps(state.Unit))
		dir := a.tlsDir(s.Name)
		if state.Custom {
			dir = filepath.Join(a.appMetaDir(s.Name), "tls-custom")
		}
		if cert, err := certInfo(dir); err == nil {
			out.Expires, out.Temp = &cert.NotAfter, cert.Subject.CommonName == tempCertName
			if !a.Sys.Now().Before(cert.NotAfter) {
				out.Problem = "证书已到期"
			}
		} else {
			out.Problem = "无法读取 HTTPS 证书"
		}
		if !out.Active {
			out.Problem = "HTTPS 的 nginx 未运行"
		}
	}
	for _, name := range []string{"https-error", "https-renew-error"} {
		if problem, err := os.ReadFile(filepath.Join(a.appMetaDir(s.Name), name)); err == nil {
			out.Problem = string(problem)
			break
		}
	}
	return out
}

func (a *Agent) checkBackendHTTPS(s *proto.AppSpec) error {
	h := s.HTTPS
	unit := h.Unit
	if h.Instance != "" {
		in, err := a.hostingInstance(&proto.AppSpec{NginxInstance: h.Instance})
		if err != nil {
			return err
		}
		unit = in.Unit
	}
	if h.HTTP {
		if msg := a.portBusy(80, unit); msg != "" {
			return fmt.Errorf("%s。可用同一个 nginx 实例，或改用 DNS 验证", msg)
		}
	}
	if msg := a.portBusy(h.Port, unit); msg != "" {
		return errors.New(msg)
	}
	return nil
}

// applyBackendHTTPS 在应用通过健康检查后更新独立 HTTPS 入口。
func (a *Agent) applyBackendHTTPS(s *proto.AppSpec, port int) error {
	if s.HTTPS == nil {
		if err := a.removeBackendHTTPS(s); err != nil {
			a.warn(proto.EvCert, s.Name, "移除 HTTPS 失败：%v", err)
			return err
		}
		return nil
	}
	if err := a.configureBackendHTTPS(s, port); err != nil {
		a.httpsProblem(s.Name, err.Error())
		a.warn(proto.EvCert, s.Name, "HTTPS 没配好：%v", err)
		return err
	}
	a.finishBackendHTTPS(s)
	return nil
}

func (a *Agent) rollbackBackendHTTPS(s *proto.AppSpec, id string) {
	if s.HTTPS == nil || id == LegacyID {
		return
	}
	spec, h := *s, *s.HTTPS
	if release, ok := a.backendHTTPSRelease(s.Name, id); ok {
		h.Upstream = release.Upstream
	}
	spec.HTTPS = &h
	a.applyBackendHTTPS(&spec, 0)
}

func (a *Agent) switchZDProxy(s *proto.AppSpec, port int) error {
	if s.HTTPS == nil || s.HTTPS.Instance != "" {
		return a.zdProxy(s, port)
	}
	if err := a.configureBackendHTTPS(s, port); err != nil {
		a.httpsProblem(s.Name, err.Error())
		a.warn(proto.EvCert, s.Name, "HTTPS 没配好：%v", err)
		if a.readBackendHTTPS(s.Name).Unit != "" {
			return err // 入口正在服务，必须保留它正在转发的槽位。
		}
		if strings.HasPrefix(s.ZD.ProxyConf, ManagedMarker) {
			return a.zdProxy(s, port) // 原有 HTTP 入口仍可继续使用。
		}
	}
	return nil
}

func (a *Agent) configureBackendHTTPS(s *proto.AppSpec, port int) error {
	h := s.HTTPS
	old := a.readBackendHTTPS(s.Name)
	if old.Unit != "" && (old.Instance != h.Instance || (h.Instance == "" && old.Unit != h.Unit)) {
		return errors.New("HTTPS 已绑定其他 nginx 入口，请先去掉 https 配置并部署一次，再更换入口")
	}
	if err := a.checkBackendHTTPS(s); err != nil {
		return err
	}
	if port == 0 {
		_, upstreamPort, _ := net.SplitHostPort(h.Upstream)
		n, _ := strconv.Atoi(upstreamPort)
		if pid := a.Sys.UnitProps(s.Unit).PID; n > 0 && pid > 0 {
			for _, listen := range a.Sys.Listening(pid) {
				addr, bound := splitSiteListen(listen)
				if bound == n && (addr == "*" || addr == "0.0.0.0" || addr == "[::]" || addr == "::") {
					a.warn(proto.EvCert, s.Name, "后端端口 %d 也监听非本机地址；只经 HTTPS 对外时请让后端监听 127.0.0.1", n)
					break
				}
			}
		}
	}
	if err := os.MkdirAll(a.appMetaDir(s.Name), 0o755); err != nil {
		return err
	}
	if h.Cert == "" && h.TLS.DNS == "" {
		webroot := filepath.Join(a.appMetaDir(s.Name), "acme-web")
		if filepath.Clean(h.TLS.Webroot) != webroot {
			return errors.New("HTTPS 验证目录与应用不匹配")
		}
		if err := os.MkdirAll(webroot, 0o755); err != nil {
			return err
		}
	}
	conf := filepath.Join(a.appMetaDir(s.Name), "https.conf")
	unit, user := h.Unit, s.User
	var hosted *proto.SoftwareInstance
	if h.Instance != "" {
		in, err := a.hostingInstance(&proto.AppSpec{NginxInstance: h.Instance})
		if err != nil {
			return err
		}
		unlock, err := a.lockNginxInstances(in.Name)
		if err != nil {
			return err
		}
		defer unlock()
		hosted, unit, user = in, in.Unit, in.User
		if in.Hosting.System {
			user = "root" // 系统 nginx 的 master 默认以 root 读取证书。
		}
		conf = a.nginxSitePath(s, in)
		if err := os.MkdirAll(in.Hosting.Sites, 0o755); err != nil {
			return err
		}
	} else if s.ZD != nil {
		conf = filepath.Join(a.appMetaDir(s.Name), "proxy.conf")
	}
	if hosted == nil && filepath.Clean(h.Prefix) != filepath.Join(a.appMetaDir(s.Name), "https-nginx") {
		return errors.New("HTTPS nginx 的运行目录与应用不匹配")
	}
	certSpec := proto.AppSpec{Name: s.Name, User: user, Unit: unit, TLS: &h.TLS}
	restoreCert := func() {}
	if h.Cert == "" {
		if err := a.tlsPrepare(&certSpec); err != nil {
			return err
		}
	} else {
		restore, err := a.loadCustomCertificate(s.Name, user, h)
		if err != nil {
			return err
		}
		restoreCert = restore
	}
	upstream := strings.ReplaceAll(h.Upstream, proto.ProxyPort, strconv.Itoa(port))
	text := []byte(strings.ReplaceAll(h.Config, proto.BackendHTTPSUpstream, upstream))
	if err := a.installBackendProxy(s, conf, text, hosted); err != nil {
		restoreCert()
		return err
	}
	state, _ := json.Marshal(backendHTTPSState{Domains: h.TLS.Domains, Port: h.Port, Unit: unit, Instance: h.Instance, Custom: h.Cert != ""})
	if err := writeFileAtomic(a.backendHTTPSFile(s.Name), state, 0o644); err != nil {
		return err
	}
	a.httpsProblem(s.Name, "")
	return nil
}

func (a *Agent) installBackendProxy(s *proto.AppSpec, conf string, text []byte, hosted *proto.SoftwareInstance) error {
	h := s.HTTPS
	before, err := os.ReadFile(conf)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if hosted != nil && err == nil && !strings.HasPrefix(string(before), ManagedMarker) {
		return errors.New("站点配置已存在且不是 Ferry 生成")
	}
	var unitBefore []byte
	if hosted == nil {
		unitBefore, err = os.ReadFile(a.unitPath(h.Unit))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if unitBefore != nil && !strings.HasPrefix(string(unitBefore), ManagedMarker) {
			return errors.New("HTTPS 单元已存在且不是 Ferry 生成")
		}
	}
	if err := writeFileAtomic(conf, text, 0o644); err != nil {
		return err
	}
	revert := func(err error) error {
		if before == nil {
			os.Remove(conf)
		} else {
			writeFileAtomic(conf, before, 0o644)
		}
		return err
	}
	if hosted != nil {
		if err := a.checkHostingNginx(hosted); err != nil {
			return revert(err)
		}
		if err := a.Sys.Systemctl("reload", hosted.Unit); err != nil {
			return revert(err)
		}
		return nil
	}
	uid, gid, err := a.Sys.LookupUser(s.User)
	if err != nil {
		return revert(err)
	}
	if err := os.MkdirAll(h.Prefix, 0o755); err != nil {
		return revert(err)
	}
	if err := os.Chmod(h.Prefix, 0o755); err != nil {
		return revert(err)
	}
	for _, sub := range []string{"run", "tmp", "tmp/client_body", "tmp/proxy", "tmp/fastcgi", "tmp/uwsgi", "tmp/scgi"} {
		d := filepath.Join(h.Prefix, sub)
		if err := os.MkdirAll(d, 0o750); err != nil {
			return revert(err)
		}
		if err := a.Sys.Lchown(d, uid, gid); err != nil {
			return revert(err)
		}
	}
	// nginx -t 会创建 pid；先用服务用户的权限建好，低端口的校验仍由 root 运行。
	pid := filepath.Join(h.Prefix, "run", "nginx.pid")
	file, err := os.OpenFile(pid, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err == nil {
		err = file.Close()
	}
	if err != nil && !os.IsExist(err) {
		return revert(err)
	}
	if fi, err := os.Lstat(pid); err != nil || !fi.Mode().IsRegular() {
		return revert(errors.New("nginx.pid 不是普通文件"))
	}
	if err := a.Sys.Lchown(pid, uid, gid); err != nil {
		return revert(err)
	}
	cmd := shellQuote(h.Binary) + " -t -q -e stderr -p " + shellQuote(h.Prefix+"/") + " -c " + shellQuote(conf)
	if out, err := a.Sys.Shell(cmd+" 2>&1", 15*time.Second); err != nil {
		return revert(fmt.Errorf("nginx 配置检查失败：%s %w", strings.TrimSpace(out), err))
	}
	restoreUnit := func(err error) error {
		if unitBefore == nil {
			os.Remove(a.unitPath(h.Unit))
		} else {
			writeFileAtomic(a.unitPath(h.Unit), unitBefore, 0o644)
		}
		a.Sys.Systemctl("daemon-reload")
		return revert(err)
	}
	if err := a.installUnit(s.Name, h.Unit, []byte(h.UnitText)); err != nil {
		return restoreUnit(err)
	}
	action := "start"
	if isRunning(a.Sys.UnitProps(h.Unit)) {
		action = "reload"
	}
	if err := a.Sys.Systemctl(action, h.Unit); err != nil {
		return restoreUnit(err)
	}
	if err := a.Sys.Systemctl("enable", h.Unit); err != nil {
		a.warn(proto.EvCert, s.Name, "启用 HTTPS 开机自启失败：%v", err)
	}
	return nil
}

func (a *Agent) loadCustomCertificate(app, user string, h *proto.BackendHTTPS) (func(), error) {
	cert, err := infoReadFile(h.Cert, 1024*1024)
	if err != nil {
		return nil, fmt.Errorf("读取证书：%w", err)
	}
	key, err := infoReadFile(h.Key, 1024*1024)
	if err != nil {
		return nil, fmt.Errorf("读取证书私钥：%w", err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("证书与私钥无效：%w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	now := a.Sys.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("自带证书不在有效期内")
	}
	for _, d := range h.TLS.Domains {
		if !slices.Contains(leaf.DNSNames, d) {
			if err := leaf.VerifyHostname(d); err != nil {
				return nil, err
			}
		}
	}
	_, gid, err := a.Sys.LookupUser(user)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(a.appMetaDir(app), "tls-custom")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	if err := a.Sys.Lchown(dir, 0, gid); err != nil {
		return nil, err
	}
	type certPart struct {
		file     string
		previous []byte
	}
	var saved []certPart
	restore := func() {
		for _, part := range saved {
			if part.previous == nil {
				os.Remove(part.file)
			} else if writeFileAtomic(part.file, part.previous, 0o640) == nil {
				a.Sys.Lchown(part.file, 0, gid)
			}
		}
	}
	for name, data := range map[string][]byte{certFile: cert, certKeyFile: key} {
		p := filepath.Join(dir, name)
		previous, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			restore()
			return nil, err
		}
		saved = append(saved, certPart{p, previous})
		if err := writeFileAtomic(p, data, 0o640); err != nil {
			restore()
			return nil, err
		}
		if err := a.Sys.Lchown(p, 0, gid); err != nil {
			restore()
			return nil, err
		}
	}
	return restore, nil
}

func (a *Agent) finishBackendHTTPS(s *proto.AppSpec) {
	state := a.readBackendHTTPS(s.Name)
	if state.Unit == "" || state.Custom {
		if state.Custom {
			os.Remove(a.acmeRecordPath(s.Name))
			a.httpsRenewalProblem(s.Name, "")
		}
		return
	}
	user := s.User
	if state.Instance != "" {
		if in, err := a.loadHostingInstance(state.Instance); err == nil {
			user = in.User
			if in.Hosting.System {
				user = "root"
			}
		}
	}
	a.configureRenewal(&proto.AppSpec{Name: s.Name, Unit: state.Unit, User: user, TLS: &s.HTTPS.TLS}, true)
}

func (a *Agent) removeBackendHTTPS(s *proto.AppSpec) error {
	old := a.readBackendHTTPS(s.Name)
	if old.Unit == "" {
		a.httpsProblem(s.Name, "")
		a.httpsRenewalProblem(s.Name, "")
		return nil
	}
	if old.Instance != "" {
		in, err := a.loadHostingInstance(old.Instance)
		if err != nil {
			return err
		}
		unlock, err := a.lockNginxInstances(old.Instance)
		if err != nil {
			return err
		}
		defer unlock()
		conf := a.nginxSitePath(s, in)
		before, err := os.ReadFile(conf)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if len(before) > 0 {
			if !strings.HasPrefix(string(before), ManagedMarker) {
				return errors.New("nginx 站点配置已由其他程序接管")
			}
			if err := os.Remove(conf); err != nil {
				return err
			}
			if err := a.reloadHosting(in); err != nil {
				writeFileAtomic(conf, before, 0o644)
				return err
			}
		}
	} else if s.ZD == nil || s.ZD.ProxyUnit != old.Unit {
		if exists(a.unitPath(old.Unit)) {
			if b, err := os.ReadFile(a.unitPath(old.Unit)); err != nil || !strings.HasPrefix(string(b), ManagedMarker) {
				return errors.New("HTTPS 单元已由其他程序接管")
			}
			if err := a.Sys.Systemctl("disable", "--now", old.Unit); err != nil {
				return err
			}
			if err := os.Remove(a.unitPath(old.Unit)); err != nil {
				return err
			}
			a.Sys.Systemctl("daemon-reload")
		}
	}
	for _, p := range []string{a.acmeRecordPath(s.Name), a.backendHTTPSFile(s.Name)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, dir := range []string{a.tlsDir(s.Name), filepath.Join(a.appMetaDir(s.Name), "tls-custom")} {
		os.Remove(filepath.Join(dir, certKeyFile))
		os.Remove(filepath.Join(dir, certFile))
		os.Remove(dir)
	}
	a.httpsProblem(s.Name, "")
	a.httpsRenewalProblem(s.Name, "")
	return nil
}

func (a *Agent) acmeRecordPath(app string) string {
	return filepath.Join(a.acmeDir(), "apps", app+".json")
}

func (a *Agent) renewBackendHTTPS() int {
	if len(a.req.Apps) != 1 {
		return a.result(proto.CodeBadInput, "请选择一个应用", nil)
	}
	app := a.req.Apps[0].Name
	err := a.renewSavedBackendHTTPS(app, true)
	if errors.Is(err, ErrLocked) {
		return a.result(proto.CodeLocked, err.Error(), nil)
	}
	if err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	return a.result(proto.CodeOK, "HTTPS 证书已续期", nil)
}

func (a *Agent) renewSavedBackendHTTPS(app string, force bool) error {
	unlock, err := a.lock(0)
	if err != nil {
		return err
	}
	defer unlock()
	state := a.readBackendHTTPS(app)
	if state.Unit == "" || state.Custom {
		return errors.New("此应用没有由 Ferry 自动续期的 HTTPS 证书")
	}
	if !isRunning(a.Sys.UnitProps(state.Unit)) {
		if force {
			return errors.New("HTTPS 的 nginx 未运行")
		}
		return nil
	}
	var rec acmeRecord
	data, err := os.ReadFile(a.acmeRecordPath(app))
	if err != nil {
		return fmt.Errorf("无法读取 HTTPS 续期配置，请重新部署：%w", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil || rec.App != app || rec.Unit != state.Unit {
		return errors.New("HTTPS 续期配置无效，请重新部署")
	}
	if err := a.ensureCert(rec, force); err != nil {
		a.httpsRenewalProblem(app, "续期失败："+err.Error())
		return err
	}
	a.httpsRenewalProblem(app, "")
	return nil
}

func (a *Agent) httpsRenewalProblem(app, msg string) {
	a.setHTTPSProblem(app, "https-renew-error", msg)
}
