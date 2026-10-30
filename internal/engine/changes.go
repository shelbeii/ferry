package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/artifact"
	"github.com/captain/ferry/internal/config"

	"github.com/captain/ferry/internal/gitinfo"
	"github.com/captain/ferry/internal/proto"
)

// AppChange 一个应用这次部署会带上线的改动：线上运行的版本、本地的提交、中间的提交。
type AppChange struct {
	App    string             `json:"app"`
	Online *proto.ReleaseMeta `json:"online,omitempty"` // 线上运行的版本；没部署过为空
	Local  gitinfo.Info       `json:"local"`
	gitinfo.Range
	Note string `json:"note,omitempty"` // 给人看的说明（第一次部署、看不到提交、线上比本地新……）
	Warn bool   `json:"warn,omitempty"` // Note 是要留意的（线上有本地没有的提交）
	// UnitDiff 生成的 systemd 单元和线上的比，变了的行（「+ 行」新增、「- 行」去掉）；附带文件新增、改了、去掉的
	UnitDiff     []string `json:"unitDiff,omitempty"`
	FilesAdded   []string `json:"filesAdded,omitempty"`
	FilesChanged []string `json:"filesChanged,omitempty"`
	FilesRemoved []string `json:"filesRemoved,omitempty"`
}

// localApp 一个应用这次要部署的：生成的单元文件、附带文件的 sha256。
type localApp struct {
	unit  string
	files map[string]string
	zd    *proto.ZeroDowntime
}

// localApps 各应用这次要部署的单元文件、附带文件（和线上的比）。
func localApps(f *config.File, t *config.Target, apps []*config.App) map[string]localApp {
	out := map[string]localApp{}
	for _, a := range apps {
		s, err := Spec(f, t, a)
		if err != nil {
			continue
		}
		la := localApp{unit: s.UnitText, zd: s.ZD}
		if files, err := LocalFiles(f, t, a); err == nil && len(files) > 0 {
			la.files = artifact.FileSums(files)
		}
		out[a.Name] = la
	}
	return out
}

// unitDiff 两份单元文件变了的行（注释、空行不算）。不停机发布的应用两个槽位的路径、端口不同：先都换成槽位 a 的再比。
func unitDiff(old, now string, zd *proto.ZeroDowntime) []string {
	norm := func(s string) string {
		if zd == nil {
			return s
		}
		s = strings.ReplaceAll(s, "/slots/b", "/slots/a")
		re := regexp.MustCompile(`\b` + strconv.Itoa(zd.Slots[1].Port) + `\b`)
		s = re.ReplaceAllString(s, strconv.Itoa(zd.Slots[0].Port))
		return strings.ReplaceAll(s, "（槽位 b）", "（槽位 a）")
	}
	// 有内容的行（按原来的顺序）和它们的集合
	lines := func(s string) ([]string, map[string]bool) {
		var ls []string
		set := map[string]bool{}
		for _, l := range strings.Split(norm(s), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") && !set[l] {
				ls = append(ls, l)
				set[l] = true
			}
		}
		return ls, set
	}
	ol, oldSet := lines(old)
	nl, ns := lines(now)
	var out []string
	for _, l := range ol {
		if !ns[l] {
			out = append(out, "- "+l)
		}
	}
	for _, l := range nl {
		if !oldSet[l] {
			out = append(out, "+ "+l)
		}
	}
	return out
}

// diffFiles 附带文件：新增、改了、去掉的（old 是线上版本记下的，nil = 没有记录）。
func diffFiles(old, now map[string]string) (added, changed, removed []string) {
	for p, sum := range now {
		switch o, ok := old[p]; {
		case !ok:
			added = append(added, p)
		case o != sum:
			changed = append(changed, p)
		}
	}
	for p := range old {
		if _, ok := now[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return
}

const maxChangeCommits = 20

// changesFrom 按各应用线上的版本算出改动。dir 是本地仓库的目录，local 是这次要部署的单元文件、附带文件（可以为 nil）。
func changesFrom(dir string, git gitinfo.Info, rels []proto.AppReleases, local map[string]localApp) []AppChange {
	var out []AppChange
	for _, r := range rels {
		c := AppChange{App: r.App, Local: git}
		for _, x := range r.Releases {
			if x.Current {
				m := x.ReleaseMeta
				c.Online = &m
			}
		}
		switch {
		case c.Online == nil:
			c.Note = "第一次部署"
		case git.Commit == "":
			c.Note = "本地不是 git 仓库，看不到提交"
		case c.Online.Commit == "":
			c.Note = "线上的版本没有记下提交（不在 git 仓库里构建的）"
		case c.Online.Commit == git.Commit:
			c.Note = "和线上是同一个提交"
			c.Known = true
		default:
			c.Range = gitinfo.Between(dir, c.Online.Commit, git.Commit, maxChangeCommits)
			switch {
			case !c.Known:
				c.Note = fmt.Sprintf("线上的提交 %s 不在本地仓库里（别的电脑上提交的？先 git pull 再部署）", short(c.Online.Commit))
				c.Warn = true
			case c.Behind > 0:
				c.Note = fmt.Sprintf("线上有 %d 个提交本地没有（线上可能是别的分支，或别人刚部署了更新的版本）：这次部署会把它们去掉", c.Behind)
				c.Warn = true
			}
		}
		if git.Dirty && c.Online != nil {
			if c.Note != "" {
				c.Note += "；"
			}
			c.Note += "本地有没提交的修改，也会带上线"
		}
		if la, ok := local[r.App]; ok && c.Online != nil {
			if r.Unit != "" && la.unit != "" {
				c.UnitDiff = unitDiff(r.Unit, la.unit, la.zd)
			}
			if c.Online.FileSums != nil || len(la.files) > 0 {
				c.FilesAdded, c.FilesChanged, c.FilesRemoved = diffFiles(c.Online.FileSums, la.files)
			}
		}
		out = append(out, c)
	}
	return out
}

func short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

// ReportChanges 部署前列出要上线的改动：线上版本相同的应用写在一起。
func (e *Engine) ReportChanges(target string, cs []AppChange) {
	type group struct {
		apps []string
		c    AppChange
	}
	var groups []*group
	idx := map[string]*group{}
	for _, c := range cs {
		key := c.Note + "|"
		if c.Online != nil {
			key += c.Online.Commit + "|" + c.Online.ID
		}
		if g, ok := idx[key]; ok && (c.Online == nil || g.c.Online != nil) {
			g.apps = append(g.apps, c.App)
			continue
		}
		g := &group{apps: []string{c.App}, c: c}
		idx[key] = g
		groups = append(groups, g)
	}
	for _, g := range groups {
		c := g.c
		what := strings.Join(g.apps, "、")
		if c.Online == nil {
			e.Rep.Info("%s：%s（%s）", what, c.Note, target)
			continue
		}
		head := fmt.Sprintf("%s：线上 %s（%s %s 部署）→ 本次 %s", what, orDash(short(c.Online.Commit)),
			c.Online.DeployedAt.Local().Format("01-02 15:04"), c.Online.DeployedBy, c.Local.Short())
		if c.Ahead > 0 {
			head += fmt.Sprintf("，新增 %d 个提交", c.Ahead)
		}
		e.Rep.Info("%s", head)
		for i, x := range c.Commits {
			if i == 5 {
				e.Rep.Info("    …还有 %d 个", c.Ahead-5)
				break
			}
			e.Rep.Info("    %s %s（%s，%s）", x.Hash, x.Subject, x.Author, x.Date)
		}
		if c.Note != "" {
			if c.Warn {
				e.Rep.Warn("%s：%s", what, c.Note)
			} else {
				e.Rep.Info("    %s", c.Note)
			}
		}
	}
	// 配置、附带文件每个应用不一样，分开列
	for _, c := range cs {
		if len(c.UnitDiff) > 0 {
			e.Rep.Info("%s 的 systemd 单元有变化：", c.App)
			for i, l := range c.UnitDiff {
				if i == 12 {
					e.Rep.Info("    …还有 %d 行", len(c.UnitDiff)-12)
					break
				}
				e.Rep.Info("    %s", l)
			}
		}
		var fs []string
		if len(c.FilesAdded) > 0 {
			fs = append(fs, "新增 "+strings.Join(c.FilesAdded, "、"))
		}
		if len(c.FilesChanged) > 0 {
			fs = append(fs, "改了 "+strings.Join(c.FilesChanged, "、"))
		}
		if len(c.FilesRemoved) > 0 {
			fs = append(fs, "去掉 "+strings.Join(c.FilesRemoved, "、"))
		}
		if len(fs) > 0 {
			e.Rep.Info("%s 的附带文件：%s", c.App, strings.Join(fs, "；"))
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Changes 这次部署会带上线的改动（不改动服务器）：连上目标读线上的版本，和本地的提交比。
func (e *Engine) Changes(target string, names []string) ([]AppChange, error) {
	git, _, err := e.DeployPreview(target, names)
	if err != nil {
		return nil, err
	}
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	apps, err := e.File.Select(t, names)
	if err != nil {
		return nil, err
	}
	var rels []proto.AppReleases
	err = e.withSession(target, names, func(s *session, specs []proto.AppSpec) error {
		return s.callData("releases", &proto.Request{Apps: specs, Detail: true}, &rels)
	})
	if err != nil {
		return nil, err
	}
	dir := git.Top
	if dir == "" {
		dir = e.File.Dir
	}
	return changesFrom(dir, git, rels, localApps(e.File, t, apps)), nil
}
