package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/swpkg"
	"github.com/captain/ferry/internal/swtpl"
)

// Ferry 装的软件装好以后：恢复卸载时保留了数据的、升级到新版本、一键设置数据备份。

// softwareInfo 服务器上 Ferry 装的软件的记录；kept 为卸载时保留了数据的（还没恢复）。
func softwareInfo(s *session, name string) (in proto.SoftwareInstance, kept bool, err error) {
	r, err := s.call("swinfo", &proto.Request{Instance: name})
	if err != nil {
		return in, false, err
	}
	if r.RC != proto.CodeOK {
		return in, false, &AgentError{Cmd: "swinfo", RC: r.RC, Msg: r.Msg}
	}
	err = json.Unmarshal(r.Data, &in)
	return in, r.Msg == "kept", err
}

// SoftwareRestore 恢复卸载时保留了数据的软件（照原样写回系统服务、启动）。
func (e *Engine) SoftwareRestore(target, name string) (*proto.SoftwareInstance, error) {
	var in proto.SoftwareInstance
	err := e.withTarget(target, func(s *session) error {
		e.Rep.Step("在 %s 上恢复 %s", s.t.Name, name)
		return s.callData("swrestore", &proto.Request{Instance: name}, &in)
	})
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// UpgradeOptions 升级的选项。
type UpgradeOptions struct {
	Version     string // 不填 = 最新
	PackageFile string // 用本机库里的这个安装包
	Major       bool   // 跨大版本升级（用户已知道可能不兼容）
}

// MajorUpgradeError 跨大版本升级：可能不兼容，要用户确认（--major）。
type MajorUpgradeError struct {
	Target, Name, From, To string
}

func (e *MajorUpgradeError) Error() string {
	return fmt.Sprintf("%s 从 %s 升到 %s 是跨大版本升级：配置、数据可能不兼容，先备份、看过新版本的升级说明再升级（加 --major）", e.Name, e.From, e.To)
}

// JSONDetail --json 输出的附加信息。
func (e *MajorUpgradeError) JSONDetail() map[string]any {
	return map[string]any{"kind": "major-upgrade", "target": e.Target, "name": e.Name, "from": e.From, "to": e.To}
}

// majorOf 版本号的第一段（3.2.4 → 3）。
func majorOf(v string) string {
	m, _, _ := strings.Cut(strings.TrimPrefix(strings.ToLower(v), "v"), ".")
	return m
}

// SoftwareUpgrade 把 Ferry 用安装包装的软件升级到新版本：换掉程序，配置、数据带过去；新版本起不来时换回旧的。
// 从软件源装的随系统更新（dnf / apt），不在这里升级。
func (e *Engine) SoftwareUpgrade(target, name string, o UpgradeOptions) (*proto.SoftwareInstance, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	s, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	in, kept, err := softwareInfo(s, name)
	if err != nil {
		return nil, err
	}
	if kept {
		return nil, fmt.Errorf("%s 已经卸载了（保留着数据）：先恢复再升级", name)
	}
	tpl, err := swtpl.Find(e.Settings.TemplateDir(), in.Kind)
	if err != nil {
		return nil, fmt.Errorf("%s 是用模板 %s 装的，这个模板已经不在了：不能自动升级", name, in.Kind)
	}
	if in.Variant < 0 || in.Variant >= len(tpl.Variants) {
		return nil, fmt.Errorf("模板 %s 改过了（没有第 %d 种装法）：不能自动升级", tpl.ID, in.Variant+1)
	}
	v := tpl.Variants[in.Variant]
	p := v.Package
	if p == nil {
		return nil, fmt.Errorf("%s 是从服务器的软件源装的：用 dnf / apt 更新系统软件包后，在这里重启它就是新版本", name)
	}

	// 版本、安装包：和安装时一样，本机库里没有就下载
	lib := e.Settings.SoftwareDir()
	version := o.Version
	var pkg *swpkg.Meta
	switch {
	case o.PackageFile != "":
		if pkg = swpkg.FindFile(lib, o.PackageFile); pkg == nil {
			return nil, fmt.Errorf("本机库里没有 %s：先导入", o.PackageFile)
		}
		if version == "" {
			version = pkg.Version
		}
	case p.URL == "":
		if pkg = swpkg.Find(lib, tpl.ID, version); pkg == nil {
			return nil, fmt.Errorf("%s 没有下载地址：先在「导入安装包」里导入新版本，再选它", tpl.Name)
		}
		version = pkg.Version
	case version == "":
		if version, err = swpkg.Latest(e.ctx(), e.SoftwareSources(), p); err != nil {
			return nil, fmt.Errorf("查不到 %s 的最新版本（%v）：写上要升到的版本", tpl.Name, err)
		}
	}
	if version == "" {
		return nil, errors.New("不知道要升到哪个版本：写上版本")
	}
	if version == in.Version {
		return nil, fmt.Errorf("%s 已经是 %s 了", name, version)
	}
	if in.Version != "" && majorOf(in.Version) != majorOf(version) && !o.Major {
		return nil, &MajorUpgradeError{Target: t.Name, Name: name, From: in.Version, To: version}
	}
	var chk proto.SwCheck
	if err := s.callData("swcheck", &proto.Request{SwCheck: &proto.SwCheckSpec{Dir: in.Dir, User: in.User}}, &chk); err != nil {
		return nil, err
	}
	if pkg == nil {
		if pkg = swpkg.Find(lib, tpl.ID, version); pkg == nil {
			e.Rep.Step("下载 %s %s（%s）", tpl.Name, version, orUnknown(p.Size))
			pkg, err = swpkg.Download(e.ctx(), e.SoftwareSources(), tpl.ID, p, version, lib, func(done, total int64) { e.Rep.Progress(tpl.Name, done, total) })
			if err != nil {
				return nil, fmt.Errorf("下载 %s %s 失败：%w（可以在设置 → 运行环境里设置 GitHub 加速，或在浏览器里下载后导入）", tpl.Name, version, err)
			}
		} else {
			e.Rep.Info("使用本机已有的 %s（%s）", pkg.File, humanBytes(pkg.Size))
		}
	}
	if err := diskRoom(t.Name, in.Dir, chk, pkg.Size); err != nil {
		return nil, err
	}
	format := swtpl.FormatOf(p.Format, pkg.File)
	if format == "binary" {
		if err := checkProgramArch(filepath.Join(lib, pkg.File), s.arch); err != nil {
			return nil, err
		}
	}
	vars := swtpl.Vars{"name": name, "dir": in.Dir, "user": in.User, "host": t.Host, "version": version}
	into, err1 := vars.Expand(p.Into)
	check, err2 := vars.Expand(p.Check)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("模板 %s：%v", tpl.ID, err)
	}
	pp := &proto.PlanPackage{File: pkg.File, SHA256: pkg.SHA256, Size: pkg.Size, Format: format, Strip: p.Strip, Into: into, Check: check}
	if format == "jar" {
		pp.Check = ""
	}
	// 带到新版本的：模板写的数据目录，加上软件的配置文件
	var keep []string
	if v.Upgrade != nil {
		keep = append(keep, v.Upgrade.Keep...)
	}
	for _, c := range in.Configs {
		if rel, ok := strings.CutPrefix(c.Path, in.Dir+"/"); ok {
			keep = append(keep, rel)
		}
	}

	var c proto.SwCheck
	if err := s.callData("swcheck", &proto.Request{SwCheck: &proto.SwCheckSpec{PackageSHA: pkg.SHA256}}, &c); err == nil && c.Cached {
		pp.Cached = true
	}
	stage := ""
	if !pp.Cached {
		var prep proto.Prepared
		if err := s.callData("prepare", &proto.Request{}, &prep); err != nil {
			return nil, err
		}
		stage = prep.Stage
		e.Rep.Step("上传 %s 到 %s", pkg.File, t.Name)
		item := remote.UploadItem{Local: filepath.Join(lib, pkg.File), Remote: remote.Join(stage, pkg.File), Mode: 0o640, Size: pkg.Size}
		if err := e.uploadItems(s, []remote.UploadItem{item}, pkg.Size, pkg.Size); err != nil {
			return nil, fmt.Errorf("上传安装包失败：%w", err)
		}
	}
	e.Rep.Step("在 %s 上把 %s 升级到 %s", t.Name, name, version)
	var out proto.SoftwareInstance
	plan := &proto.SoftwarePlan{Name: name, Version: version, Package: pp, Keep: keep}
	if err := s.callData("swupgrade", &proto.Request{Stage: stage, Instance: name, Install: plan}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SoftwareBackup 给 Ferry 装的数据库设置数据备份：在服务器上写好备份用的密码文件，在配置的 backups 里加上一项。
// 返回备份项的名字；之后在「数据备份」里打开、现在备份。database 为空时备份全部库。
func (e *Engine) SoftwareBackup(target, name, database string) (string, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return "", err
	}
	if e.File.Path == "" || e.File.Kind != "" {
		return "", errors.New("这台服务器不在项目里：数据备份写在项目的 ferry.yaml 里，先把它加进一个项目")
	}
	s, err := e.connect(t)
	if err != nil {
		return "", err
	}
	defer s.close()
	in, kept, err := softwareInfo(s, name)
	if err != nil {
		return "", err
	}
	if kept {
		return "", fmt.Errorf("%s 已经卸载了（保留着数据）：先恢复再设置备份", name)
	}
	kind, pwVar, bin, user := "", "", "", in.Account
	switch in.Kind {
	case "mysql":
		kind, pwVar, bin = "mysql", "MYSQL_PWD", in.Dir+"/mysql/bin"
	case "mariadb":
		kind, pwVar = "mysql", "MYSQL_PWD"
	case "postgresql":
		kind, pwVar, bin = "postgres", "PGPASSWORD", in.Dir+"/bin"
	default:
		return "", fmt.Errorf("%s 不是 Ferry 装的 MySQL、MariaDB、PostgreSQL：在「数据备份」里按说明自己写备份项", name)
	}
	if user == "" {
		user = map[string]string{"mysql": "root", "postgres": "postgres"}[kind]
	}
	port := in.Ports["port"]
	if port == 0 {
		port = in.ReadyPort
	}
	if database == "" {
		database = "*"
	}
	pw, ok, err := secret.Get(secret.SoftwareAccount(name, t.Host, t.Port))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("本机没有保存 %s 的密码（可能是在别的电脑上装的）：在服务器上写好 %s/ferry-backup.env（%s=密码，只有 root 能读），再在 ferry.yaml 的 backups 里自己加上", name, in.Dir, pwVar)
	}
	envFile := in.Dir + "/ferry-backup.env"
	e.Rep.Step("在 %s 上写备份用的密码文件 %s（只有 root 能读）", t.Name, envFile)
	cmd := s.sudo + "sh -c " + shQuote("umask 077; cat > "+shQuote(envFile)+" && chown root:root "+shQuote(envFile))
	res, err := s.c.RunContext(e.ctx(), cmd, []byte(pwVar+"="+pw+"\n"), nil)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("写 %s 失败：%s", envFile, strings.TrimSpace(res.Stderr))
	}
	item := name
	for _, b := range e.File.Backups {
		if b.Name == item {
			item = name + "-" + t.Name
		}
	}
	b := config.SoftwareBackup{Name: item, Kind: kind, Database: database, Port: port, User: user, Bin: bin, EnvFile: envFile, Target: t.Name,
		Comment: fmt.Sprintf("Ferry 装的 %s（%s）", strings.TrimSpace(in.Title), name)}
	if err := config.AddBackup(e.File.Path, b); err != nil {
		return "", err
	}
	e.Rep.Info("已在 %s 的 backups 里加上 %s：在「数据备份」里打开后每天 3 点备份", filepath.Base(e.File.Path), item)
	return item, nil
}
