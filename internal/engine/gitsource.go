package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/build"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/gitinfo"
)

// DeployGit 这次部署的各应用的代码提交（应用名 → 提交信息）。应用的代码在哪个仓库，看它的构建目录、再看产物所在的目录；
// 都不在 git 仓库里的应用没有提交信息（Commit 为空）。配置文件所在的仓库不算：配置文件放在后端仓库里、
// 前端不在 git 里时，前端不能记成后端的提交。
type DeployGit map[string]gitinfo.Info

// appGits 读出要部署的各应用的提交信息（同一个目录只读一次）。
func appGits(f *config.File, plans []*targetPlan) DeployGit {
	cache := map[string]gitinfo.Info{}
	read := func(dir string) gitinfo.Info {
		if g, ok := cache[dir]; ok {
			return g
		}
		g := gitinfo.Read(dir)
		cache[dir] = g
		return g
	}
	out := DeployGit{}
	for _, p := range plans {
		for _, it := range p.items {
			if _, ok := out[it.app.Name]; ok {
				continue
			}
			var dirs []string
			if steps, err := build.Plan(f, p.t, []*config.App{it.app}); err == nil {
				for _, s := range steps {
					dirs = append(dirs, s.Workdir)
				}
			}
			dirs = append(dirs, existingDir(it.local))
			var g gitinfo.Info
			for _, d := range dirs {
				if d == "" {
					continue
				}
				if x := read(d); x.Commit != "" {
					g = x
					break
				}
			}
			out[it.app.Name] = g
		}
	}
	return out
}

// repoGit 一个仓库和这次部署里用它的应用。
type repoGit struct {
	gitinfo.Info
	Apps []string
}

// Repos 用到的仓库（按仓库目录、提交分开，按应用名排序）；没在仓库里的应用在 none 里。
func (g DeployGit) Repos() (repos []repoGit, none []string) {
	names := make([]string, 0, len(g))
	for n := range g {
		names = append(names, n)
	}
	sort.Strings(names)
	at := map[string]int{}
	for _, n := range names {
		x := g[n]
		if x.Commit == "" {
			none = append(none, n)
			continue
		}
		k := x.Top + "|" + x.Commit
		if i, ok := at[k]; ok {
			repos[i].Apps = append(repos[i].Apps, n)
			continue
		}
		at[k] = len(repos)
		repos = append(repos, repoGit{Info: x, Apps: []string{n}})
	}
	return
}

// Common 所有应用都是同一个仓库的同一个提交时就是它（有应用不在仓库里时不算）。
func (g DeployGit) Common() (gitinfo.Info, bool) {
	repos, none := g.Repos()
	if len(repos) == 1 && len(none) == 0 {
		return repos[0].Info, true
	}
	return gitinfo.Info{}, false
}

// Short 一句话：一个仓库时是提交号（带 * 表示有没提交的修改）；不一样时按应用写，如「api 74684793、web 不在 git 里」。
func (g DeployGit) Short() string {
	if c, ok := g.Common(); ok {
		return c.Short()
	}
	repos, none := g.Repos()
	if len(repos) == 0 {
		return "-"
	}
	var parts []string
	for _, r := range repos {
		parts = append(parts, strings.Join(r.Apps, "、")+" "+r.Short())
	}
	if len(none) > 0 {
		parts = append(parts, strings.Join(none, "、")+" 不在 git 里")
	}
	return strings.Join(parts, "，")
}

// Describe 确认框里给人看的几行：代码在哪个分支、哪个提交，有没有没提交的修改。
func (g DeployGit) Describe() []string {
	repos, none := g.Repos()
	one := len(repos) == 0 || (len(repos) == 1 && len(none) == 0) // 不用按应用分开写
	var out []string
	for _, r := range repos {
		line := "代码"
		if !one {
			line += "（" + strings.Join(r.Apps, "、") + "）"
		}
		branch := r.Branch
		if branch == "" {
			branch = "-"
		}
		line += fmt.Sprintf("：%s 分支，提交 %s", branch, r.Short())
		if r.Dirty {
			line += fmt.Sprintf("，有 %d 个文件改了没提交", len(r.Files))
		}
		out = append(out, line)
	}
	if len(none) > 0 {
		if one {
			out = append(out, "代码：不在 git 仓库里，看不到提交")
		} else {
			out = append(out, "代码（"+strings.Join(none, "、")+"）：不在 git 仓库里，看不到提交")
		}
	}
	return out
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
func (e *Engine) DeployPreview(target string, names []string) (DeployGit, []*build.Step, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, nil, err
	}
	apps, err := e.File.Select(t, names)
	if err != nil {
		return nil, nil, err
	}
	p := &targetPlan{t: t, apps: apps}
	for _, a := range apps {
		if local, err := LocalArtifact(e.File, t, a); err == nil {
			p.items = append(p.items, &item{app: a, local: local})
		}
	}
	steps, err := build.Plan(e.File, t, apps)
	return appGits(e.File, []*targetPlan{p}), steps, err
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
