package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/gitinfo"
	"github.com/captain/ferry/internal/proto"
)

func TestChangesFrom(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=张三", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=张三", "GIT_COMMITTER_EMAIL=a@b")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v：%v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	var hashes []string
	for _, m := range []string{"第一版", "修好登录", "加上导出"} {
		os.WriteFile(filepath.Join(dir, "f.txt"), []byte(m), 0o644)
		run("add", ".")
		run("commit", "-q", "-m", m)
		hashes = append(hashes, run("rev-parse", "HEAD"))
	}
	git := gitinfo.Read(dir)
	rel := func(app, commit string) proto.AppReleases {
		return proto.AppReleases{App: app, Releases: []proto.ReleaseInfo{{ReleaseMeta: proto.ReleaseMeta{ID: "20261025-100000-aaaa", Commit: commit}, Current: true}}}
	}
	cs := changesFrom(dir, git, []proto.AppReleases{
		rel("api", hashes[0]),
		rel("web", hashes[2]),
		rel("job", strings.Repeat("ab", 20)),
		{App: "new"},
	}, nil)
	api := cs[0]
	if !api.Known || api.Ahead != 2 || len(api.Commits) != 2 || api.Commits[0].Subject != "加上导出" || api.Commits[0].Author != "张三" || api.Warn {
		t.Fatalf("api：%+v", api)
	}
	if cs[1].Note != "和线上是同一个提交" || cs[1].Ahead != 0 {
		t.Fatalf("web：%+v", cs[1])
	}
	if !cs[2].Warn || !strings.Contains(cs[2].Note, "不在本地仓库里") {
		t.Fatalf("job：%+v", cs[2])
	}
	if cs[3].Note != "第一次部署" {
		t.Fatalf("new：%+v", cs[3])
	}

	// 本地退回到第二个提交：线上（第三个）有本地没有的提交，部署会把它去掉
	run("checkout", "-q", hashes[1])
	cs = changesFrom(dir, gitinfo.Read(dir), []proto.AppReleases{rel("api", hashes[2])}, nil)
	if c := cs[0]; !c.Warn || c.Behind != 1 || !strings.Contains(c.Note, "会把它们去掉") {
		t.Fatalf("%+v", c)
	}
}
