package engine

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

// BackupOptions run / list / restore / delete / download 时：哪一项、哪个文件。
type BackupOptions struct {
	Name  string
	File  string
	Force bool // restore：恢复前备份现在的数据失败了也照样恢复
}

// Backup 数据备份：status 查看、on 打开（之后服务器上按时备份）、off 关闭、run 现在备份、list 列出备份文件、
// restore 恢复（先自动备份一份现在的）、delete 删掉一份。
func (e *Engine) Backup(target, action string, o BackupOptions) (proto.BackupStatus, error) {
	var out proto.BackupStatus
	t, err := e.File.Target(target)
	if err != nil {
		return out, err
	}
	items, err := e.backupItems(t)
	if err != nil {
		return out, err
	}
	spec := proto.BackupSpec{Action: action, Project: e.File.Project, Target: t.Name, Host: t.Host, Items: items, Name: o.Name, File: o.File, Force: o.Force}
	err = e.withSession(target, []string{}, func(s *session, _ []proto.AppSpec) error {
		if (action == "restore" || action == "delete") && !e.Yes {
			prompt := fmt.Sprintf("删掉 %s 上的备份 %s？", t.Name, o.File)
			if action == "restore" {
				prompt = fmt.Sprintf("用 %s 恢复 %s@%s？现在的数据会被换成备份里的（恢复前会先自动备份一份现在的）", o.File, o.Name, t.Name)
			}
			prod := []string{}
			if t.Production {
				prod = append(prod, t.Name)
			}
			if !e.confirmProduction(prompt, prod) {
				return ErrCanceled
			}
		}
		return s.callData("backup", &proto.Request{Backup: &spec}, &out)
	})
	return out, err
}

// backupItems 配置里在这个目标上的备份，变量已展开。
func (e *Engine) backupItems(t *config.Target) ([]proto.BackupItem, error) {
	out := []proto.BackupItem{}
	sc := &config.Scope{
		Builtins: map[string]string{"project": e.File.Project, "target": t.Name, "host": t.Host, "root": t.Root},
		Layers:   []map[string]string{t.Vars, e.File.Vars},
	}
	for _, b := range e.File.Backups {
		if !b.AppliesTo(t.Name) {
			continue
		}
		it := proto.BackupItem{Name: b.Name, Kind: b.Kind(), OnCalendar: b.OnCalendar(), Keep: b.KeepN(), BeforeDeploy: b.BeforeDeploy}
		var errs []error
		ex := func(s string) string {
			v, err := sc.Expand(s)
			if err != nil {
				errs = append(errs, err)
			}
			return v
		}
		for _, db := range []*config.DBBackup{b.MySQL, b.Postgres} {
			if db != nil {
				it.Database, it.Host, it.Port, it.User, it.Container, it.Bin = ex(db.Database), ex(db.Host), db.Port, ex(db.User), ex(db.Container), ex(db.Bin)
			}
		}
		for _, p := range b.Paths {
			it.Paths = append(it.Paths, path.Clean(ex(p)))
		}
		for _, p := range b.EnvFile {
			it.EnvFiles = append(it.EnvFiles, ex(p))
		}
		it.Command, it.Restore = ex(b.Command), ex(b.Restore)
		if len(errs) > 0 {
			return nil, fmt.Errorf("backups.%s：%v", b.Name, errs[0])
		}
		for _, p := range it.Paths {
			if !path.IsAbs(p) || p == "/" {
				return nil, fmt.Errorf("backups.%s：paths 要写服务器上的绝对路径，不能是 /（当前 %q）", b.Name, p)
			}
		}
		out = append(out, it)
	}
	return out, nil
}

// backupBefore 部署前：服务器上开着数据备份的话同步设置，执行 beforeDeploy 的备份；失败了不部署。
func (e *Engine) backupBefore(s *session) error {
	items, err := e.backupItems(s.t)
	if err != nil {
		return err
	}
	spec := proto.BackupSpec{Action: "before", Project: e.File.Project, Target: s.t.Name, Host: s.t.Host, Items: items}
	return s.callData("backup", &proto.Request{Backup: &spec}, nil)
}

var reBackupFile = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-\d{8}-\d{6}(-[a-z-]+)?\.(sql\.gz|tar\.gz|gz)$`)

// BackupDownload 把服务器上的一份备份下载到本机目录 dir，返回本机的文件。
func (e *Engine) BackupDownload(target, name, file, dir string) (string, error) {
	if !reBackupFile.MatchString(file) || len(file) <= len(name) || file[:len(name)+1] != name+"-" {
		return "", fmt.Errorf("不是 %s 的备份文件：%s", name, file)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("本机没有文件夹 %s", dir)
	}
	local := filepath.Join(dir, file)
	if _, err := os.Stat(local); err == nil {
		return "", fmt.Errorf("%s 已经有了", local)
	}
	err := e.withSession(target, []string{}, func(s *session, _ []proto.AppSpec) error {
		var st proto.BackupStatus
		spec := proto.BackupSpec{Action: "list", Project: e.File.Project, Target: s.t.Name, Name: name}
		if err := s.callData("backup", &proto.Request{Backup: &spec}, &st); err != nil {
			return err
		}
		var f *proto.BackupFile
		for i := range st.Files {
			if st.Files[i].Name == file {
				f = &st.Files[i]
			}
		}
		if f == nil {
			return fmt.Errorf("服务器上没有这份备份：%s", file)
		}
		part := local + ".part"
		w, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		pw := &progressWriter{w: w, total: f.Size, report: func(done, total int64) { e.Rep.Progress(file, done, total) }}
		err = s.c.Stream(s.sudo+"cat "+shQuote(f.Path), pw, false, nil)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err == nil && pw.done != f.Size {
			err = fmt.Errorf("没下载完（%s / %s）", humanBytes(pw.done), humanBytes(f.Size))
		}
		if err != nil {
			os.Remove(part)
			return err
		}
		return os.Rename(part, local)
	})
	if err != nil {
		return "", err
	}
	return local, nil
}

type progressWriter struct {
	w           io.Writer
	done, total int64
	last        int64
	report      func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.done-p.last >= 1<<20 || p.done == p.total {
		p.last = p.done
		p.report(p.done, p.total)
	}
	return n, err
}
