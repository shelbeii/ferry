package agent

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 数据备份（ferry backup）：配置里的 backups 在服务器上打开后，各项有自己的定时器
// ferry-backup-<目录的短哈希>-<名字>.timer，按时运行 <ferryDir>/ferry-backup backup-run <部署目录> <名字>。
// 打开、关闭随时可以换；关着的时候不定时备份、部署前也不备份，手动备份、恢复照样能用。
//
//	<root>/backups/<名字>/<名字>-20261025-030000.sql.gz   备份文件（只有 root 能读），保留最近 keep 份
//	<root>/.ferry/backup/config.json                       开没开、各项的设置（打开、部署时同步）
//	<root>/.ferry/backup/state.json                        各项最近几次备份的结果

const backupUnitPrefix = "ferry-backup-"

type backupConfig struct {
	On      bool               `json:"on"`
	Project string             `json:"project"`
	Target  string             `json:"target"`
	Host    string             `json:"host,omitempty"`
	Items   []proto.BackupItem `json:"items"`
}

type backupState struct {
	Runs map[string][]proto.BackupRun `json:"runs"` // 名字 → 最近的结果（新的在后面）
}

func backupMeta(root string) string           { return filepath.Join(root, ".ferry", "backup") }
func backupDir(root string) string            { return filepath.Join(root, "backups") }
func backupItemDir(root, name string) string  { return filepath.Join(backupDir(root), name) }
func backupUnitBase(root, name string) string { return backupUnitPrefix + rootHash(root) + "-" + name }
func backupLock(root string) string           { return filepath.Join(backupMeta(root), "lock") }
func backupConfigPath(root string) string     { return filepath.Join(backupMeta(root), "config.json") }
func backupStatePath(root string) string      { return filepath.Join(backupMeta(root), "state.json") }
func rootHash(root string) string             { s := sha256.Sum256([]byte(root)); return hex.EncodeToString(s[:4]) }
func backupExt(kind string) string {
	switch kind {
	case "mysql", "postgres":
		return ".sql.gz"
	case "paths":
		return ".tar.gz"
	}
	return ".gz"
}

func findItem(items []proto.BackupItem, name string) int {
	return slices.IndexFunc(items, func(it proto.BackupItem) bool { return it.Name == name })
}

func loadBackup(root string) (backupConfig, backupState) {
	var c backupConfig
	var st backupState
	readJSON(backupConfigPath(root), &c)
	readJSON(backupStatePath(root), &st)
	if st.Runs == nil {
		st.Runs = map[string][]proto.BackupRun{}
	}
	return c, st
}

// lockBackup 同一个部署目录同时只做一个备份、恢复（最多等 2 小时）。
func lockBackup(sys System, root string) (func(), error) {
	if err := os.MkdirAll(backupMeta(root), 0o700); err != nil {
		return nil, err
	}
	return sys.Lock(backupLock(root), 2*time.Hour)
}

func saveJSON0600(p string, v any) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return writeFileAtomic(p, b, 0o600)
}

var reBackupFile = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-\d{8}-\d{6}(-[a-z-]+)?\.(sql\.gz|tar\.gz|gz)$`)

// backup 命令。
func (a *Agent) backup() int {
	b := a.req.Backup
	if b == nil || b.Project == "" || b.Target == "" || a.req.Root == "" {
		return a.result(proto.CodeBadInput, "缺少备份的设置", nil)
	}
	root := a.req.Root
	for _, it := range b.Items {
		if !reBackupFile.MatchString(it.Name + "-00000000-000000.gz") {
			return a.result(proto.CodeBadInput, "备份的名字不对："+it.Name, nil)
		}
	}
	cfg, st := loadBackup(root)
	switch b.Action {
	case "", "status":
	case "list":
		return a.result(proto.CodeOK, "", proto.BackupStatus{On: cfg.On, Dir: backupDir(root), Items: []proto.BackupItemStatus{}, Files: listBackupFiles(root, b.Name)})
	case "on":
		if len(b.Items) == 0 {
			return a.result(proto.CodeBadInput, "配置里没有 backups：先写上要备份什么", nil)
		}
		cfg = backupConfig{On: true, Project: b.Project, Target: b.Target, Host: b.Host, Items: b.Items}
		if err := a.applyBackup(root, cfg); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		a.info(proto.EvInfo, "", "数据备份已打开：%s", backupPlan(b.Items))
	case "off":
		cfg.On = false
		if cfg.Project == "" {
			cfg.Project, cfg.Target = b.Project, b.Target
		}
		if err := a.applyBackup(root, cfg); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		a.info(proto.EvInfo, "", "数据备份已关闭（已有的备份文件留着，在 %s）", backupDir(root))
	case "before":
		if !cfg.On {
			return a.result(proto.CodeOK, "", proto.BackupStatus{Dir: backupDir(root), Items: []proto.BackupItemStatus{}})
		}
		if !sameItems(cfg.Items, b.Items) {
			cfg.Items, cfg.Host = b.Items, b.Host
			if err := a.applyBackup(root, cfg); err != nil {
				return a.result(proto.CodeFailed, err.Error(), nil)
			}
			a.info(proto.EvInfo, "", "备份的设置有变化，已同步到服务器")
		}
		unlock, err := lockBackup(a.Sys, root)
		if err != nil {
			return a.result(proto.CodeFailed, "等别的备份结束超时："+err.Error(), nil)
		}
		defer unlock()
		for _, it := range cfg.Items {
			if !it.BeforeDeploy {
				continue
			}
			a.info(proto.EvInfo, "", "部署前备份 %s…", it.Name)
			r := a.runBackup(root, it, "部署前", "", "")
			a.recordRun(root, it.Name, r)
			if !r.OK {
				return a.result(proto.CodeFailed, fmt.Sprintf("部署前备份 %s 失败：%s（没有部署。不想在部署前备份的话去掉 beforeDeploy，或关闭数据备份）", it.Name, r.Error), nil)
			}
			a.info(proto.EvInfo, "", "已备份 %s：%s（%s，%.1f 秒）", it.Name, r.File, sizeText(r.Size), r.Sec)
		}
		_, st = loadBackup(root)
	case "run":
		items := b.Items
		if b.Name != "" {
			i := findItem(items, b.Name)
			if i < 0 {
				return a.result(proto.CodeBadInput, "配置里没有备份 "+b.Name, nil)
			}
			items = items[i : i+1]
		}
		if len(items) == 0 {
			return a.result(proto.CodeBadInput, "配置里没有 backups：先写上要备份什么", nil)
		}
		unlock, err := lockBackup(a.Sys, root)
		if err != nil {
			return a.result(proto.CodeFailed, "等别的备份结束超时："+err.Error(), nil)
		}
		defer unlock()
		var failed []string
		for _, it := range items {
			a.info(proto.EvInfo, "", "备份 %s…", it.Name)
			r := a.runBackup(root, it, "手动", "", "")
			a.recordRun(root, it.Name, r)
			if r.OK {
				a.info(proto.EvInfo, "", "已备份 %s：%s（%s，%.1f 秒）", it.Name, r.File, sizeText(r.Size), r.Sec)
			} else {
				a.warn(proto.EvInfo, "", "备份 %s 失败：%s", it.Name, r.Error)
				failed = append(failed, it.Name)
			}
		}
		_, st = loadBackup(root)
		if len(failed) > 0 {
			return a.result(proto.CodeFailed, "备份失败："+strings.Join(failed, "、"), a.backupStatus(root, cfg, st, b.Items))
		}
	case "restore":
		return a.backupRestore(root, cfg, b)
	case "delete":
		p, err := backupFilePath(root, b.Name, b.File)
		if err != nil {
			return a.result(proto.CodeBadInput, err.Error(), nil)
		}
		if err := os.Remove(p); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		a.info(proto.EvInfo, "", "已删除 %s", p)
		return a.result(proto.CodeOK, "", proto.BackupStatus{On: cfg.On, Dir: backupDir(root), Items: []proto.BackupItemStatus{}, Files: listBackupFiles(root, b.Name)})
	default:
		return a.result(proto.CodeBadInput, "未知的操作："+b.Action, nil)
	}
	cfg, _ = loadBackup(root)
	return a.result(proto.CodeOK, "", a.backupStatus(root, cfg, st, b.Items))
}

func sameItems(x, y []proto.BackupItem) bool {
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(bx) == string(by)
}

func backupPlan(items []proto.BackupItem) string {
	var p []string
	for _, it := range items {
		s := it.Name + "（" + calendarText(it.OnCalendar)
		if it.BeforeDeploy {
			s += "、部署前"
		}
		p = append(p, s+fmt.Sprintf("，留 %d 份）", it.Keep))
	}
	return strings.Join(p, "、")
}

func calendarText(c string) string {
	switch c {
	case "*-*-* 03:00:00":
		return "每天 3 点"
	case "hourly":
		return "每小时"
	case "Sun *-*-* 03:00:00":
		return "每周日 3 点"
	}
	return c
}

// applyBackup 保存设置，装上开着的各项的定时器、去掉其余的（这个部署目录的）。
func (a *Agent) applyBackup(root string, cfg backupConfig) error {
	if err := saveJSON0600(backupConfigPath(root), cfg); err != nil {
		return err
	}
	want := map[string]bool{}
	if cfg.On {
		bin, err := a.selfCopy("ferry-backup")
		if err != nil {
			return err
		}
		for _, it := range cfg.Items {
			u := backupUnitBase(root, it.Name)
			want[u] = true
			head := fmt.Sprintf("%s — 由 ferry 生成：数据备份\n# backup root=%s name=%s project=%s target=%s\n", ManagedMarker, root, it.Name, cfg.Project, cfg.Target)
			svc := head + "[Unit]\nDescription=Ferry 数据备份 " + it.Name + "（" + cfg.Project + "）\nAfter=network-online.target\n\n" +
				"[Service]\nType=oneshot\nNice=10\nIOSchedulingClass=idle\nExecStart=" + bin + " backup-run " + systemdQuote(root) + " " + it.Name + "\n"
			timer := head + "[Unit]\nDescription=Ferry 数据备份 " + it.Name + "（定时）\n\n" +
				"[Timer]\nOnCalendar=" + it.OnCalendar + "\nPersistent=true\nRandomizedDelaySec=120\n\n[Install]\nWantedBy=timers.target\n"
			if err := a.installUnit("", u+".service", []byte(svc)); err != nil {
				return err
			}
			changed := !sameFile(a.unitPath(u+".timer"), []byte(timer))
			if err := a.installUnit("", u+".timer", []byte(timer)); err != nil {
				return err
			}
			if changed {
				a.Sys.Systemctl("restart", u+".timer")
			}
			if err := a.Sys.Systemctl("enable", "--now", u+".timer"); err != nil {
				return fmt.Errorf("打开定时器 %s 失败：%v", u, err)
			}
		}
	}
	prefix := backupUnitPrefix + rootHash(root) + "-"
	ents, _ := os.ReadDir(a.Sys.UnitDir())
	removed := false
	for _, e := range ents {
		n := e.Name()
		base := strings.TrimSuffix(strings.TrimSuffix(n, ".timer"), ".service")
		if !strings.HasPrefix(n, prefix) || want[base] {
			continue
		}
		if strings.HasSuffix(n, ".timer") {
			a.Sys.Systemctl("disable", "--now", n)
		}
		os.Remove(filepath.Join(a.Sys.UnitDir(), n))
		removed = true
	}
	if removed {
		a.Sys.Systemctl("daemon-reload")
	}
	return nil
}

// systemdQuote 单元文件 ExecStart 里的一个参数（路径里有空格时）。
func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (a *Agent) backupStatus(root string, cfg backupConfig, st backupState, req []proto.BackupItem) proto.BackupStatus {
	out := proto.BackupStatus{On: cfg.On, Dir: backupDir(root), Items: []proto.BackupItemStatus{}}
	items := req
	if cfg.On {
		items = cfg.Items
	}
	files := listBackupFiles(root, "")
	add := func(it proto.BackupItem) proto.BackupItemStatus {
		s := proto.BackupItemStatus{BackupItem: it}
		runs := st.Runs[it.Name]
		if len(runs) > 0 {
			s.Last = &runs[len(runs)-1]
		}
		for i := len(runs) - 1; i >= 0; i-- {
			if runs[i].OK {
				s.LastOK = &runs[i]
				break
			}
		}
		for _, f := range files {
			if f.Item == it.Name {
				s.Count++
				s.Bytes += f.Size
			}
		}
		if cfg.On {
			if v, err := a.Sys.Shell("systemctl show -p NextElapseUSecRealtime --value "+backupUnitBase(root, it.Name)+".timer 2>/dev/null", 10*time.Second); err == nil {
				s.Next = strings.TrimSpace(v)
			}
			if i := findItem(req, it.Name); i < 0 {
				s.Removed = len(req) > 0
			} else {
				s.Changed = !sameItems([]proto.BackupItem{req[i]}, []proto.BackupItem{it})
			}
		}
		return s
	}
	for _, it := range items {
		out.Items = append(out.Items, add(it))
	}
	if cfg.On {
		for _, it := range req {
			if findItem(items, it.Name) < 0 {
				s := add(it)
				s.Changed = true
				out.Items = append(out.Items, s)
			}
		}
	}
	return out
}

// listBackupFiles 备份文件，新的在前。name 空 = 全部。
func listBackupFiles(root, name string) []proto.BackupFile {
	out := []proto.BackupFile{}
	dirs, _ := os.ReadDir(backupDir(root))
	for _, d := range dirs {
		if !d.IsDir() || name != "" && d.Name() != name {
			continue
		}
		ents, _ := os.ReadDir(backupItemDir(root, d.Name()))
		for _, e := range ents {
			if !reBackupFile.MatchString(e.Name()) || !strings.HasPrefix(e.Name(), d.Name()+"-") {
				continue
			}
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			out = append(out, proto.BackupFile{Item: d.Name(), Name: e.Name(), Path: filepath.ToSlash(filepath.Join(backupItemDir(root, d.Name()), e.Name())), Size: info.Size(), Time: info.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

func backupFilePath(root, name, file string) (string, error) {
	if !reBackupFile.MatchString(file) || !strings.HasPrefix(file, name+"-") || name == "" {
		return "", fmt.Errorf("不是 %s 的备份文件：%s", name, file)
	}
	p := filepath.Join(backupItemDir(root, name), file)
	if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("没有这份备份：%s", p)
	}
	return p, nil
}

func (a *Agent) recordRun(root, name string, r proto.BackupRun) {
	_, st := loadBackup(root)
	runs := append(st.Runs[name], r)
	if len(runs) > 20 {
		runs = runs[len(runs)-20:]
	}
	st.Runs[name] = runs
	saveJSON0600(backupStatePath(root), st)
}

// ---------------------------------------------------------------- 备份

// countWriter 数一共写了多少（压缩前的大小：备份是不是空的）。
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// runBackup 备份一次：写到临时文件，成功后改名，再删掉多出来的旧备份（protect 这份不删：正要拿它恢复）。
func (a *Agent) runBackup(root string, it proto.BackupItem, reason, suffix, protect string) proto.BackupRun {
	start := a.Sys.Now()
	r := proto.BackupRun{Time: start, Reason: reason}
	fail := func(err error) proto.BackupRun {
		r.Error = err.Error()
		r.Sec = a.Sys.Now().Sub(start).Seconds()
		return r
	}
	dir := backupItemDir(root, it.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	os.Chmod(backupDir(root), 0o700)
	name := it.Name + "-" + start.Format("20060102-150405") + suffix + backupExt(it.Kind)
	part := filepath.Join(dir, "."+name+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	defer os.Remove(part)
	gz := gzip.NewWriter(f)
	cw := &countWriter{w: gz}
	switch it.Kind {
	case "paths":
		err = writePathsTar(cw, it.Paths)
	default:
		var env []string
		if env, err = backupEnv(it); err == nil {
			err = a.Sys.Pipe(dumpCommand(it, env, false), env, nil, cw, 6*time.Hour)
		}
	}
	if err == nil && cw.n == 0 {
		err = errors.New("备份出来的内容是空的")
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(part, final); err != nil {
		return fail(err)
	}
	if info, err := os.Stat(final); err == nil {
		r.Size = info.Size()
	}
	r.OK, r.File = true, name
	r.Sec = a.Sys.Now().Sub(start).Seconds()
	pruneBackups(root, it.Name, max(it.Keep, 1), name, protect)
	return r
}

// pruneBackups 只留最近 keep 份（keepAlso、protect 不删）。
func pruneBackups(root, name string, keep int, keepAlso, protect string) {
	files := listBackupFiles(root, name)
	n := 0
	for _, f := range files {
		if f.Name == keepAlso || f.Name == protect {
			n++
			continue
		}
		if n < keep {
			n++
			continue
		}
		os.Remove(filepath.FromSlash(f.Path))
	}
}

// backupEnv envFile 里的变量（数据库密码等），NAME=VALUE。
func backupEnv(it proto.BackupItem) ([]string, error) {
	var out []string
	for _, p := range it.EnvFiles {
		vars, err := readEnvFile(p)
		if err != nil {
			return nil, fmt.Errorf("读不了 envFile %s：%v", p, err)
		}
		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, k+"="+vars[k])
		}
	}
	return out, nil
}

// dumpCommand 备份（restore 为 false）或恢复的命令。数据库在容器里时在容器里执行，
// 密码没写在 envFile 里就用容器自己的 MYSQL_ROOT_PASSWORD / POSTGRES_PASSWORD（1Panel、官方镜像都有）。
func dumpCommand(it proto.BackupItem, env []string, restore bool) string {
	q := shellQuote
	var inner, pwVar, pwFallback string
	switch it.Kind {
	case "command":
		if restore {
			return it.Restore
		}
		return it.Command
	case "mysql":
		user := or(it.User, "root")
		flags := " -u " + q(user)
		if it.Container == "" || it.Host != "" {
			flags += " -h " + q(or(it.Host, "127.0.0.1")) + fmt.Sprintf(" -P %d", orInt(it.Port, 3306))
		}
		if restore {
			inner = `"$(command -v mysql || command -v mariadb || echo mysql)"` + flags + " " + q(it.Database)
		} else {
			inner = `"$(command -v mysqldump || command -v mariadb-dump || echo mysqldump)" --single-transaction --quick --routines --triggers --events --hex-blob` + flags + " " + q(it.Database)
		}
		pwVar, pwFallback = "MYSQL_PWD", "$MYSQL_ROOT_PASSWORD"
		if user != "root" {
			pwFallback = "$MYSQL_PASSWORD"
		}
	case "postgres":
		user := q(or(it.User, "postgres"))
		if it.User == "" && it.Container != "" {
			user = `"${POSTGRES_USER:-postgres}"`
		}
		flags := " -U " + user
		if it.Container == "" || it.Host != "" {
			flags += " -h " + q(or(it.Host, "127.0.0.1")) + fmt.Sprintf(" -p %d", orInt(it.Port, 5432))
		}
		if restore {
			inner = "psql -q -v ON_ERROR_STOP=1" + flags + " -d " + q(it.Database)
		} else {
			inner = "pg_dump --clean --if-exists" + flags + " " + q(it.Database)
		}
		pwVar, pwFallback = "PGPASSWORD", "$POSTGRES_PASSWORD"
	}
	if it.Container == "" {
		return inner
	}
	// 容器里：envFile 里的变量用 -e 传进去
	var es string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		es += " -e " + q(k)
	}
	script := fmt.Sprintf(`p="${%s:-%s}"; [ -n "$p" ] && export %s="$p"; exec %s`, pwVar, pwFallback, pwVar, inner)
	return "docker exec -i" + es + " " + q(it.Container) + " sh -c " + q(script)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orInt(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

// writePathsTar 把这些目录、文件打成 tar（第一项 .ferry-paths 记着有哪些路径，恢复时用）。保留属主、权限、链接。
func writePathsTar(w io.Writer, paths []string) error {
	tw := tar.NewWriter(w)
	manifest := strings.Join(paths, "\n") + "\n"
	if err := tw.WriteHeader(&tar.Header{Name: ".ferry-paths", Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg, ModTime: time.Now()}); err != nil {
		return err
	}
	io.WriteString(tw, manifest)
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			return fmt.Errorf("没有 %s", p)
		}
		err := filepath.Walk(p, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			mode := info.Mode()
			if !mode.IsRegular() && !mode.IsDir() && mode&os.ModeSymlink == 0 {
				return nil // 套接字、管道……
			}
			link := ""
			if mode&os.ModeSymlink != 0 {
				if link, err = os.Readlink(path); err != nil {
					return err
				}
			}
			h, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			h.Name = strings.TrimPrefix(filepath.ToSlash(path), "/")
			if mode.IsDir() {
				h.Name += "/"
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if !mode.IsRegular() {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			n, err := io.Copy(tw, io.LimitReader(f, h.Size))
			if err != nil {
				return err
			}
			// 备份时文件变短了：补齐（tar 里每项的大小是写好的）
			for ; n < h.Size; n++ {
				if _, err := tw.Write([]byte{0}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// ---------------------------------------------------------------- 恢复

func (a *Agent) backupRestore(root string, cfg backupConfig, b *proto.BackupSpec) int {
	i := findItem(b.Items, b.Name)
	var it proto.BackupItem
	switch {
	case i >= 0:
		it = b.Items[i]
	case findItem(cfg.Items, b.Name) >= 0:
		it = cfg.Items[findItem(cfg.Items, b.Name)]
	default:
		return a.result(proto.CodeBadInput, "配置里没有备份 "+b.Name, nil)
	}
	if it.Kind == "command" && it.Restore == "" {
		return a.result(proto.CodeBadInput, "备份 "+it.Name+" 没写 restore（怎么恢复）：自己写的备份命令要写上恢复的命令", nil)
	}
	p, err := backupFilePath(root, it.Name, b.File)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	if !strings.HasSuffix(b.File, backupExt(it.Kind)) {
		return a.result(proto.CodeBadInput, fmt.Sprintf("%s 不是 %s 的备份（种类不一样）", b.File, it.Kind), nil)
	}
	unlock, err := lockBackup(a.Sys, root)
	if err != nil {
		return a.result(proto.CodeFailed, "等别的备份结束超时："+err.Error(), nil)
	}
	defer unlock()
	// 先备份一份现在的：恢复错了还能回去
	a.info(proto.EvInfo, "", "恢复前先备份现在的 %s…", it.Name)
	r := a.runBackup(root, it, "恢复前", "-pre-restore", b.File)
	a.recordRun(root, it.Name, r)
	if r.OK {
		a.info(proto.EvInfo, "", "已备份现在的 %s：%s", it.Name, r.File)
	} else if !b.Force {
		return a.result(proto.CodeFailed, "恢复前备份现在的数据失败："+r.Error+"（没有恢复。确定不要这份的话加 --force 再恢复）", nil)
	} else {
		a.warn(proto.EvInfo, "", "恢复前备份现在的数据失败（%s），照样恢复", r.Error)
	}
	a.info(proto.EvInfo, "", "用 %s 恢复 %s…", b.File, it.Name)
	start := a.Sys.Now()
	f, err := os.Open(p)
	if err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return a.result(proto.CodeFailed, "备份文件坏了："+err.Error(), nil)
	}
	switch it.Kind {
	case "paths":
		err = restorePathsTar(gz, a.Sys)
	default:
		var env []string
		if env, err = backupEnv(it); err == nil {
			err = a.Sys.Pipe(dumpCommand(it, env, true), env, gz, io.Discard, 6*time.Hour)
		}
	}
	if err != nil {
		msg := "恢复失败：" + err.Error()
		if r.OK {
			msg += "（恢复前的数据在 " + r.File + "）"
		}
		return a.result(proto.CodeFailed, msg, nil)
	}
	a.info(proto.EvInfo, "", "已恢复 %s（%.1f 秒）", it.Name, a.Sys.Now().Sub(start).Seconds())
	_, st := loadBackup(root)
	return a.result(proto.CodeOK, "", a.backupStatus(root, cfg, st, b.Items))
}

// restorePathsTar 把 tar 里的各路径先解到旁边的临时目录，全部解好后再换掉现在的。
func restorePathsTar(r io.Reader, sys System) error {
	tr := tar.NewReader(r)
	h, err := tr.Next()
	if err != nil || h.Name != ".ferry-paths" {
		return errors.New("不是 Ferry 的目录备份")
	}
	b, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
	var paths []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, filepath.Clean(l))
		}
	}
	stage := map[string]string{}
	for _, p := range paths {
		if !filepath.IsAbs(p) && !strings.HasPrefix(filepath.ToSlash(p), "/") {
			return fmt.Errorf("备份里的路径不对：%s", p)
		}
		tmp := filepath.Join(filepath.Dir(p), ".ferry-restore-"+filepath.Base(p))
		os.RemoveAll(tmp)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		stage[p] = tmp
	}
	cleanup := func() {
		for _, t := range stage {
			os.RemoveAll(t)
		}
	}
	type dirMode struct {
		path string
		mode os.FileMode
	}
	var dirs []dirMode
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return fmt.Errorf("备份文件坏了：%v", err)
		}
		name := filepath.Clean("/" + strings.TrimSuffix(h.Name, "/"))
		var base string
		for _, p := range paths {
			pp := filepath.ToSlash(p)
			if n := filepath.ToSlash(name); n == pp || strings.HasPrefix(n, strings.TrimSuffix(pp, "/")+"/") {
				base = p
				break
			}
		}
		if base == "" {
			continue
		}
		dest := stage[base] + filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(name), filepath.ToSlash(base)))
		mode := os.FileMode(h.Mode).Perm() | os.FileMode(h.Mode)&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o700); err != nil {
				cleanup()
				return err
			}
			dirs = append(dirs, dirMode{dest, os.FileMode(h.Mode).Perm()})
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(dest), 0o700)
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err == nil {
				_, err = io.Copy(f, tr)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
			}
			if err != nil {
				cleanup()
				return err
			}
			os.Chmod(dest, mode)
			os.Chtimes(dest, h.ModTime, h.ModTime)
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(dest), 0o700)
			if err := os.Symlink(h.Linkname, dest); err != nil {
				cleanup()
				return err
			}
		default:
			continue
		}
		sys.Lchown(dest, h.Uid, h.Gid)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chmod(dirs[i].path, dirs[i].mode)
	}
	// 换上：现在的先挪开，新的放过去，再删掉挪开的
	for _, p := range paths {
		tmp := stage[p]
		if _, err := os.Lstat(tmp); err != nil {
			continue
		}
		old := filepath.Join(filepath.Dir(p), ".ferry-old-"+filepath.Base(p))
		os.RemoveAll(old)
		if _, err := os.Lstat(p); err == nil {
			if err := os.Rename(p, old); err != nil {
				cleanup()
				return err
			}
		}
		if err := os.Rename(tmp, p); err != nil {
			os.Rename(old, p)
			cleanup()
			return err
		}
		os.RemoveAll(old)
	}
	return nil
}

// ---------------------------------------------------------------- 定时器

// BackupRun ferry-backup backup-run <部署目录> <名字>：定时器按时运行。
func BackupRun(sys System, out io.Writer, root, name string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	a := &Agent{Sys: sys, Out: out, Plain: true, req: &proto.Request{FerryDir: filepath.Dir(exe), Root: root}, appRoots: map[string]string{}}
	cfg, _ := loadBackup(root)
	i := findItem(cfg.Items, name)
	if !cfg.On || i < 0 {
		fmt.Fprintf(out, "%s 的数据备份 %s 没有开着：不备份\n", root, name)
		return 0
	}
	unlock, err := lockBackup(sys, root)
	if err != nil {
		fmt.Fprintln(out, "等别的备份结束超时：", err)
		return 1
	}
	defer unlock()
	r := a.runBackup(root, cfg.Items[i], "定时", "", "")
	a.recordRun(root, name, r)
	if !r.OK {
		fmt.Fprintf(out, "备份 %s 失败：%s\n", name, r.Error)
		return 1
	}
	fmt.Fprintf(out, "已备份 %s：%s（%s，%.1f 秒）\n", name, r.File, sizeText(r.Size), r.Sec)
	return 0
}

// backupProblems 监控用：开着的备份里最近一次失败了的。
func backupProblems(root, project, target string) []proto.MonitorProblem {
	cfg, st := loadBackup(root)
	if !cfg.On || cfg.Project != project || cfg.Target != target {
		return nil
	}
	var out []proto.MonitorProblem
	for _, it := range cfg.Items {
		runs := st.Runs[it.Name]
		if len(runs) == 0 || runs[len(runs)-1].OK {
			continue
		}
		r := runs[len(runs)-1]
		out = append(out, proto.MonitorProblem{Key: "backup:" + it.Name, Level: "bad", Text: fmt.Sprintf("数据备份 %s 失败了（%s，%s）：%s", it.Name, r.Reason, r.Time.Local().Format("01-02 15:04"), firstLine(r.Error))})
	}
	return out
}
