package agent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/filetext"
	"github.com/captain/ferry/internal/proto"
	"gopkg.in/yaml.v3"
)

func (a *Agent) softwareConfig() int {
	q := a.req.SoftwareConfig
	if q == nil {
		return a.result(proto.CodeBadInput, "缺少软件配置请求", nil)
	}
	it, in, err := a.configSoftware(q)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	if q.Operation == "associate" {
		if err := a.associateConfig(it, q.FilePath); err != nil {
			return a.result(proto.CodeBadInput, err.Error(), nil)
		}
		return a.result(proto.CodeOK, "配置文件已关联", a.discoverConfigs(it, in))
	}
	report := a.discoverConfigs(it, in)
	if q.Operation == "list" {
		return a.result(proto.CodeOK, "", report)
	}
	var selected *proto.SoftwareConfigFile
	for i := range report.Files {
		if report.Files[i].ID == q.ID {
			selected = &report.Files[i]
			break
		}
	}
	if selected == nil {
		return a.result(proto.CodeBadInput, "配置不存在或与此软件不关联，请刷新配置列表", nil)
	}
	c := *selected
	if q.Operation == "read" {
		doc, _, _, err := a.readConfig(c)
		if err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		return a.result(proto.CodeOK, "", doc)
	}
	if c.ReadOnly != "" {
		return a.result(proto.CodeBadInput, c.ReadOnly, nil)
	}
	unlock, err := a.Sys.Lock(filepath.Join(a.swDir(), "config-"+c.ID+".lock"), 30*time.Second)
	if err != nil {
		return a.result(proto.CodeLocked, err.Error(), nil)
	}
	defer unlock()
	switch q.Operation {
	case "save", "restore":
		if q.Operation == "save" && q.Content == nil {
			return a.result(proto.CodeBadInput, "没有提供文件内容", nil)
		}
		doc, err := a.saveConfig(c, q)
		if err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		return a.result(proto.CodeOK, "配置已保存；需要生效时请执行相应操作", doc)
	case "apply":
		if err := a.applySoftwareConfig(it, in, c); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		return a.result(proto.CodeOK, "配置已生效", nil)
	default:
		return a.result(proto.CodeBadInput, "未知配置操作", nil)
	}
}

func (a *Agent) configSoftware(q *proto.SoftwareConfigRequest) (proto.Software, *proto.SoftwareInstance, error) {
	if q.Instance != "" {
		in, err := a.loadInstance(q.Instance)
		if err != nil {
			return proto.Software{}, nil, err
		}
		return proto.Software{Kind: in.Kind, Name: in.Title, Instance: in.Name, Dir: in.Dir, Unit: in.Unit, Via: "systemd", Manager: "Ferry", PID: a.Sys.UnitProps(in.Unit).PID, Hosting: in.Hosting}, in, nil
	}
	if q.Unit == "" && q.Container == "" && q.PID <= 0 {
		return proto.Software{}, nil, errors.New("没有选择软件实例")
	}
	sc := &swScan{proc: "/proc", unitDir: a.Sys.UnitDir(), run: a.Sys.Shell, unitProps: a.Sys.UnitProps, readlink: os.Readlink, listening: func(int) []string { return nil }, instances: a.instances()}
	for _, it := range sc.scan().Items {
		if q.Unit != "" && it.Unit == q.Unit || q.Container != "" && it.Container == q.Container || q.PID > 0 && it.PID == q.PID {
			if it.Instance != "" {
				in, err := a.loadInstance(it.Instance)
				return it, in, err
			}
			return it, nil, nil
		}
	}
	return proto.Software{}, nil, errors.New("没有找到此软件，可能已卸载或进程已退出，请刷新软件列表")
}

var configCandidates = map[string][]string{
	"redis":      {"redis.conf", "/etc/redis.conf", "/etc/redis/redis.conf", "/etc/redis/*.conf"},
	"valkey":     {"valkey.conf", "/etc/valkey/valkey.conf"},
	"nacos":      {"nacos/conf/application.properties", "nacos/conf/nacos-logback.xml", "conf/application.properties", "conf/cluster.conf"},
	"mysql":      {"my.cnf", "/etc/my.cnf", "/etc/my.cnf.d/*.cnf", "/etc/mysql/my.cnf", "/etc/mysql/conf.d/*.cnf", "/etc/mysql/mysql.conf.d/*.cnf"},
	"mariadb":    {"my.cnf", "/etc/my.cnf", "/etc/my.cnf.d/*.cnf", "/etc/mysql/mariadb.conf.d/*.cnf"},
	"postgresql": {"data/postgresql.conf", "data/pg_hba.conf", "/etc/postgresql/*/*/postgresql.conf", "/etc/postgresql/*/*/pg_hba.conf"},
	"nginx":      {"nginx.conf", "/etc/nginx/nginx.conf", "/etc/nginx/conf.d/*.conf", "/etc/nginx/sites-enabled/*"},
	"openresty":  {"conf/nginx.conf", "/usr/local/openresty/nginx/conf/nginx.conf"},
	"mongodb":    {"/etc/mongod.conf"}, "docker": {"/etc/docker/daemon.json"},
	"httpd":   {"/etc/httpd/conf/httpd.conf", "/etc/httpd/conf.d/*.conf", "/etc/apache2/apache2.conf", "/etc/apache2/sites-enabled/*.conf"},
	"haproxy": {"/etc/haproxy/haproxy.cfg"}, "prometheus": {"/etc/prometheus/prometheus.yml"},
	"grafana": {"/etc/grafana/grafana.ini"}, "rabbitmq": {"/etc/rabbitmq/rabbitmq.conf"},
	"elasticsearch": {"config/elasticsearch.yml", "/etc/elasticsearch/elasticsearch.yml"},
	"kafka":         {"config/server.properties"}, "zookeeper": {"conf/zoo.cfg"},
	"frps": {"frps.toml", "/etc/frp/frps.toml"}, "frpc": {"frpc.toml", "/etc/frp/frpc.toml"},
	"tomcat": {"conf/server.xml", "conf/context.xml"}, "1panel": {"/opt/1panel/conf/app.yaml"},
}

var configArgPath = regexp.MustCompile(`(?:^|[\s=])(/[A-Za-z0-9_./@+:-]+\.(?:conf|cnf|properties|ya?ml|json|toml|ini|cfg|xml|env))\b`)

func configID(it proto.Software, p string) string {
	identity := it.Instance + "\x00" + it.Unit + "\x00" + it.Container
	if it.Instance == "" && it.Unit == "" && it.Container == "" {
		identity += "\x00" + it.Kind + "\x00" + it.Path + "\x00" + it.Dir
	}
	sum := sha256.Sum256([]byte(identity + "\x00" + p))
	return hex.EncodeToString(sum[:12])
}

func (a *Agent) discoverConfigs(it proto.Software, in *proto.SoftwareInstance) *proto.SoftwareConfigs {
	r := &proto.SoftwareConfigs{Software: it, Files: []proto.SoftwareConfigFile{}}
	isNginx := it.Kind == "nginx" || it.Kind == "openresty"
	seen := map[string]bool{}
	add := func(p, label, source string, inplace bool) {
		if len(r.Files) >= 200 || !filepath.IsAbs(p) || seen[p] {
			return
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return
		}
		seen[p] = true
		c := proto.SoftwareConfigFile{ID: configID(it, p), Path: p, Label: label, Source: source, Apply: "restart", InPlace: inplace}
		if c.Label == "" {
			c.Label = filepath.Base(p)
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".json":
			c.Format = "json"
		case ".yaml", ".yml":
			c.Format = "yaml"
		case ".properties":
			c.Format = "properties"
		}
		if isNginx {
			c.Format = "nginx"
			c.Apply = "reload"
		}
		if it.Via == "process" {
			c.Apply = ""
		}
		if !isNginx && it.Manager == "Ferry" && it.Instance == "" {
			c.ReadOnly = "此配置由 Ferry 部署生成，请在应用的 ferry.yaml 中修改；下次部署会覆盖手工改动"
		}
		if file, err := os.Open(p); err == nil {
			prefix := make([]byte, 256)
			n, _ := file.Read(prefix)
			file.Close()
			if isNginx && source != "service" && (strings.HasPrefix(string(prefix[:n]), ManagedMarker) || strings.HasPrefix(string(prefix[:n]), "# 由 ferry 生成")) {
				c.ReadOnly = "此配置由 Ferry 项目管理，请在对应项目的 ferry.yaml 中修改"
			}
		}
		c.Backup = a.hasConfigBackup(c.ID)
		r.Files = append(r.Files, c)
	}
	addGlob := func(pattern, label, source string) {
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)
		for _, p := range matches {
			add(p, label, source, false)
		}
	}
	if it.Container != "" {
		a.containerConfigs(it, r, add)
		a.linkedConfigs(it, add)
		return r
	}
	if in != nil {
		for _, c := range in.Configs {
			if isNginx {
				continue // nginx 按实际启动配置和 -T 的 include 结果发现。
			}
			addGlob(c.Path, c.Label, "template")
			for i := range r.Files {
				if r.Files[i].Path == c.Path {
					if c.Format != "" {
						r.Files[i].Format = c.Format
					}
					if c.Apply != "" {
						r.Files[i].Apply = c.Apply
					}
				}
			}
		}
		add(swEnvFile(in.Dir), "环境变量（可能包含密码）", "environment", false)
		add(filepath.Join(a.Sys.UnitDir(), in.Unit), "启动参数与系统服务", "service", false)
		if in.Hosting != nil && !isNginx {
			addGlob(filepath.Join(in.Hosting.Sites, "*.conf"), "", "site")
		}
	}
	if isNginx {
		a.discoverNginxConfigs(it, in, r, add)
	} else {
		for _, pattern := range configCandidates[it.Kind] {
			if !filepath.IsAbs(pattern) {
				if it.Dir == "" {
					continue
				}
				pattern = filepath.Join(it.Dir, pattern)
			}
			// Ferry 实例优先自己的目录，避免误列另一套系统实例的配置。
			if in != nil && !inDir(in.Dir, pattern) {
				continue
			}
			addGlob(pattern, "", "known-location")
		}
		var args []string
		for _, p := range a.Sys.ProcArgs(it.PID) {
			args = append(args, p...)
		}
		if it.Unit != "" {
			out, _ := a.Sys.Shell("systemctl show -p ExecStart -p EnvironmentFiles --no-pager "+shellQuote(it.Unit), 5*time.Second)
			args = append(args, out)
			for _, line := range strings.Split(out, "\n") {
				if env, ok := strings.CutPrefix(line, "EnvironmentFiles="); ok {
					for _, p := range strings.Fields(env) {
						if filepath.IsAbs(p) {
							add(p, "环境变量", "environment", false)
						}
					}
				}
			}
		}
		for _, m := range configArgPath.FindAllStringSubmatch(strings.Join(args, " "), -1) {
			add(m[1], "", "startup", false)
		}
	}
	for i := range r.Files {
		c := &r.Files[i]
		if c.Source == "site" {
			c.ReadOnly = "项目站点配置由对应项目的 ferry.yaml 管理，请在应用中修改"
		}
		if c.Source == "service" {
			c.Format = "systemd"
			c.Apply = "restart"
		}
	}
	a.linkedConfigs(it, add)
	if len(r.Files) == 0 {
		r.Notes = append(r.Notes, "未找到可确认的文本配置文件；软件可能使用启动参数、数据库或管理面板保存设置")
	}
	if in != nil {
		r.Notes = append(r.Notes, "修改端口或密码后，请核对健康检查及本机保存的凭据；原始文本修改不会自动改写它们")
		if in.Hosting != nil {
			r.Notes = append(r.Notes, "此 nginx 实例可由多个项目共用，修改主配置或重启实例会影响其全部站点")
		}
	}
	return r
}

func (a *Agent) containerConfigs(it proto.Software, r *proto.SoftwareConfigs, add func(string, string, string, bool)) {
	out, err := a.Sys.Shell("docker inspect "+shellQuote(it.Container), 10*time.Second)
	if err != nil {
		r.Notes = append(r.Notes, "无法读取容器挂载信息："+err.Error())
		return
	}
	var containers []struct {
		Mounts []struct {
			Type, Source, Destination string
			RW                        bool
		}
	}
	if json.Unmarshal([]byte(out), &containers) != nil || len(containers) != 1 {
		return
	}
	for _, m := range containers[0].Mounts {
		if m.Type != "bind" && m.Type != "volume" {
			continue
		}
		st, err := os.Stat(m.Source)
		if err != nil {
			continue
		}
		if st.Mode().IsRegular() {
			if configArgPath.MatchString(" "+m.Destination) || strings.Contains(strings.ToLower(m.Destination), "conf") {
				add(m.Source, m.Destination, "container-file", true)
			}
			continue
		}
		if !st.IsDir() {
			continue
		}
		for _, candidate := range configCandidates[it.Kind] {
			if !strings.HasPrefix(candidate, m.Destination+"/") {
				continue
			}
			pattern := filepath.Join(m.Source, strings.TrimPrefix(candidate, m.Destination+"/"))
			matches, _ := filepath.Glob(pattern)
			for _, p := range matches {
				add(p, m.Destination+"/"+strings.TrimPrefix(p, m.Source+string(filepath.Separator)), "container-volume", false)
			}
		}
	}
	if len(r.Files) == 0 {
		r.Notes = append(r.Notes, "未发现持久化挂载的配置文件；容器内部临时文件暂不提供写入，请先将配置挂载到宿主机")
	}
}

func (a *Agent) backupConfigPath(id string) string {
	return filepath.Join(a.swDir(), "config-backups", id+".json")
}
func (a *Agent) hasConfigBackup(id string) bool {
	_, err := os.Stat(a.backupConfigPath(id))
	return err == nil
}

type configBackup struct {
	Path     string
	Content  []byte
	Mode     os.FileMode
	UID, GID int
	At       time.Time
}

func localConfigRevision(target string, raw []byte, st os.FileInfo) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%x\x00%d\x00%d", target, sha256.Sum256(raw), st.ModTime().UnixNano(), st.Mode()))))
}

func (a *Agent) readConfig(c proto.SoftwareConfigFile) (*filetext.Document, []byte, string, error) {
	p, err := filepath.EvalSymlinks(c.Path)
	if err != nil {
		return nil, nil, "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, "", err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, "", errors.New("只能编辑普通文本文件")
	}
	b, err := io.ReadAll(io.LimitReader(f, filetext.Limit+1))
	if err != nil {
		return nil, nil, "", err
	}
	doc, err := filetext.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, nil, "", err
	}
	doc.Path, doc.Size, doc.ReadOnly, doc.Backup = c.Path, st.Size(), c.ReadOnly, a.hasConfigBackup(c.ID)
	if doc.Revision != "" {
		doc.Revision = localConfigRevision(p, b, st)
	}
	return doc, b, p, nil
}

func (a *Agent) saveConfig(c proto.SoftwareConfigFile, q *proto.SoftwareConfigRequest) (*filetext.Document, error) {
	doc, raw, target, err := a.readConfig(c)
	if err != nil {
		return nil, err
	}
	if q.Revision == "" || doc.Revision != q.Revision {
		return nil, errors.New("配置已被其他操作修改，请复制草稿后重新读取")
	}
	if doc.Truncated || doc.Unsupported != "" {
		return nil, errors.New("只能保存完整加载的文本文件，最大 1 MiB")
	}
	var content []byte
	if q.Operation == "restore" {
		b, err := os.ReadFile(a.backupConfigPath(c.ID))
		if err != nil {
			return nil, err
		}
		var backup configBackup
		if json.Unmarshal(b, &backup) != nil || backup.Path != target {
			return nil, errors.New("备份目标与当前文件不一致")
		}
		content = backup.Content
	} else {
		content, err = filetext.Encode(*q.Content, doc)
		if err != nil {
			return nil, err
		}
	}
	decoded, err := filetext.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	if c.Format == "json" && !json.Valid([]byte(decoded.Content)) {
		return nil, errors.New("JSON 语法错误，未保存")
	}
	if c.Format == "yaml" {
		var v any
		if err := yaml.Unmarshal([]byte(decoded.Content), &v); err != nil {
			return nil, fmt.Errorf("YAML 语法错误，未保存：%v", err)
		}
	}
	owner, err := a.Sys.Ownership(target)
	if err != nil {
		return nil, fmt.Errorf("无法读取原文件权限：%v", err)
	}
	backup := configBackup{Path: target, Content: raw, Mode: owner.Perm, UID: owner.UID, GID: owner.GID, At: a.Sys.Now()}
	if err := os.MkdirAll(filepath.Dir(a.backupConfigPath(c.ID)), 0o700); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(backup)
	if err := writeFileAtomic(a.backupConfigPath(c.ID), b, 0o600); err != nil {
		return nil, err
	}
	var token [10]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	tmp := filepath.Join(filepath.Dir(target), ".ferry-config-"+hex.EncodeToString(token[:]))
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	if f, err := os.OpenFile(tmp, os.O_WRONLY, 0); err != nil {
		return nil, err
	} else {
		err = f.Sync()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := a.Sys.Lchown(tmp, owner.UID, owner.GID); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, owner.Perm); err != nil {
		return nil, err
	}
	latest, _, resolved, err := a.readConfig(c)
	if err != nil {
		return nil, err
	}
	if latest.Revision != q.Revision || resolved != target {
		return nil, errors.New("保存期间配置发生变化，原文件未覆盖")
	}
	if c.InPlace {
		// 单文件 bind mount 保留 inode，失败时恢复完整备份。
		err = writeConfigInPlace(target, content)
		if err != nil {
			if restore := writeConfigInPlace(target, raw); restore != nil {
				return nil, fmt.Errorf("保存失败：%v；恢复备份失败：%v", err, restore)
			}
			return nil, err
		}
	} else if err := os.Rename(tmp, target); err != nil {
		return nil, err
	}
	result, _, _, err := a.readConfig(c)
	if err != nil {
		return nil, fmt.Errorf("文件已写入，但重新读取失败，请刷新确认：%v", err)
	}
	return result, nil
}

func writeConfigInPlace(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteAt(b, 0); err != nil {
		return err
	}
	if err = f.Truncate(int64(len(b))); err != nil {
		return err
	}
	return f.Sync()
}

func (a *Agent) applySoftwareConfig(it proto.Software, in *proto.SoftwareInstance, c proto.SoftwareConfigFile) error {
	if c.Apply == "" {
		return errors.New("此进程没有可用的服务管理方式，请按原启动方式重新加载")
	}
	if it.Kind == "nginx" || it.Kind == "openresty" {
		cmd := ""
		if it.Container != "" {
			cmd = "docker exec " + shellQuote(it.Container) + " nginx -t"
		} else {
			inv, err := a.nginxConfigInvocation(it, in)
			if err != nil {
				return err
			}
			cmd = inv.command("-t")
		}
		out, err := a.Sys.Shell(cmd+" 2>&1", 15*time.Second)
		if err != nil {
			return fmt.Errorf("配置校验失败，尚未生效：%s %v", strings.TrimSpace(out), err)
		}
	}
	if it.Container != "" {
		cmd := "docker restart " + shellQuote(it.Container)
		if c.Apply == "reload" {
			cmd = "docker kill --signal=HUP " + shellQuote(it.Container)
		}
		out, err := a.Sys.Shell(cmd, 60*time.Second)
		if err != nil {
			return fmt.Errorf("%v：%s", err, strings.TrimSpace(out))
		}
		return nil
	}
	if !reUnitName.MatchString(it.Unit) {
		return errors.New("无法确认要操作的系统服务")
	}
	if c.Source == "service" {
		if err := a.Sys.Systemctl("daemon-reload"); err != nil {
			return err
		}
	}
	return a.Sys.Systemctl(c.Apply, it.Unit)
}

func nginxCheckCommand(h *proto.NginxHosting) string {
	return shellQuote(h.Binary) + " -t -p " + shellQuote(strings.TrimSuffix(h.Prefix, "/")+"/") + " -c " + shellQuote(h.Config)
}

func (a *Agent) configLinksFile(it proto.Software) string {
	return filepath.Join(a.swDir(), "config-links", configID(it, "links")+".json")
}
func (a *Agent) linkedConfigs(it proto.Software, add func(string, string, string, bool)) {
	b, _ := os.ReadFile(a.configLinksFile(it))
	var paths []string
	json.Unmarshal(b, &paths)
	for _, p := range paths {
		add(p, "", "manual", it.Container != "")
	}
}
func (a *Agent) associateConfig(it proto.Software, p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return errors.New("请填写配置文件的绝对路径")
	}
	allowed := it.Dir != "" && hostingPathWithin(it.Dir, p)
	if it.Kind != "" && (hostingPathWithin("/etc/"+it.Kind, p) || p == "/etc/"+it.Kind+".conf") {
		allowed = true
	}
	if it.Container != "" {
		out, err := a.Sys.Shell("docker inspect "+shellQuote(it.Container), 10*time.Second)
		if err != nil {
			return err
		}
		var rows []struct {
			Mounts []struct{ Source, Type string }
		}
		json.Unmarshal([]byte(out), &rows)
		for _, row := range rows {
			for _, m := range row.Mounts {
				if (m.Type == "bind" || m.Type == "volume") && (p == m.Source || hostingPathWithin(m.Source, p)) {
					allowed = true
				}
			}
		}
	}
	if !allowed {
		return errors.New("配置路径应位于此软件的安装目录、对应的 /etc 配置目录，或此容器的持久化挂载内")
	}
	doc, _, _, err := a.readConfig(proto.SoftwareConfigFile{ID: configID(it, p), Path: p})
	if err != nil {
		return err
	}
	if doc.Unsupported != "" {
		return errors.New(doc.Unsupported)
	}
	file := a.configLinksFile(it)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	unlock, err := a.Sys.Lock(file+".lock", 15*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	b, _ := os.ReadFile(file)
	var paths []string
	json.Unmarshal(b, &paths)
	for _, old := range paths {
		if p == old {
			return nil
		}
	}
	if len(paths) >= 100 {
		return errors.New("此软件关联的配置文件已达上限")
	}
	paths = append(paths, p)
	b, _ = json.Marshal(paths)
	return writeFileAtomic(file, b, 0o644)
}
