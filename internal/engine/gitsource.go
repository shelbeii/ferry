package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/build"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/gitinfo"
)

// deployGit 写进部署记录的提交信息。默认读配置文件所在目录的仓库；那里不是 git 仓库时（配置文件放在代码仓库外面，
// 如桌面上），依次看要部署的应用的构建目录、产物所在目录，用找到的第一个仓库。
// fallback 为真表示不是从配置文件所在目录读到的。
func deployGit(f *config.File, plans []*targetPlan) (info gitinfo.Info, fallback bool) {
	if g := gitinfo.Read(f.Dir); g.Commit != "" {
		return g, false
	}
	var dirs []string
	for _, p := range plans {
		if steps, err := build.Plan(f, p.t, p.apps); err == nil {
			for _, s := range steps {
				dirs = append(dirs, s.Workdir)
			}
		}
		for _, it := range p.items {
			dirs = append(dirs, existingDir(it.local))
		}
	}
	tried := map[string]bool{}
	for _, d := range dirs {
		if d == "" || tried[d] {
			continue
		}
		tried[d] = true
		if g := gitinfo.Read(d); g.Commit != "" {
			return g, true
		}
	}
	return gitinfo.Info{}, false
}

// existingDir 路径本身（是目录时）或离它最近的已存在的上级目录（产物可能还没构建出来）。
func existingDir(p string) string {
	for p != "" {
		if st, err := os.Stat(p); err == nil {
			if st.IsDir() {
				return p
			}
			return filepath.Dir(p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
	return ""
}

// DeployPreview 部署前给人看的信息：代码的提交、要执行的构建命令（按顺序，相同的只算一次）。不连服务器、不构建。
func (e *Engine) DeployPreview(target string, names []string) (gitinfo.Info, []*build.Step, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return gitinfo.Info{}, nil, err
	}
	apps, err := e.File.Select(t, names)
	if err != nil {
		return gitinfo.Info{}, nil, err
	}
	p := &targetPlan{t: t, apps: apps}
	for _, a := range apps {
		if local, err := LocalArtifact(e.File, t, a); err == nil {
			p.items = append(p.items, &item{app: a, local: local})
		}
	}
	git, _ := deployGit(e.File, []*targetPlan{p})
	steps, err := build.Plan(e.File, t, apps)
	return git, steps, err
}

// BuildLogFile 一次构建的日志（本机）。
type BuildLogFile struct {
	Name string    `json:"name"`
	Path string    `json:"path"`
	Time time.Time `json:"time"`
	Size int64     `json:"size"`
}

// BuildLogs 本项目最近的构建日志，新的在前，最多 n 个。
func (e *Engine) BuildLogs(n int) []BuildLogFile {
	dir := filepath.Join(e.ProjectDir(), "logs")
	ents, _ := os.ReadDir(dir)
	var out []BuildLogFile
	for _, ent := range ents {
		name := ent.Name()
		if ent.IsDir() || !strings.HasPrefix(name, "build-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		if fi, err := ent.Info(); err == nil {
			out = append(out, BuildLogFile{Name: name, Path: filepath.Join(dir, name), Time: fi.ModTime(), Size: fi.Size()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name }) // 文件名里有时间
	if len(out) > n {
		out = out[:n]
	}
	return out
}
