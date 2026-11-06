package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

func gitRepo(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("没有可用的 git：%v %s", err, out)
		}
	}
}

func TestAppGits(t *testing.T) {
	root := t.TempDir()
	code := filepath.Join(root, "code")
	gitRepo(t, code)
	web := filepath.Join(root, "web", "dist") // 前端：不在 git 里
	os.MkdirAll(web, 0o755)
	load := func(conf string) *config.File {
		t.Helper()
		os.MkdirAll(filepath.Dir(conf), 0o755)
		os.WriteFile(conf, []byte(`
version: 1
project: shop
vars: {repo: `+filepath.ToSlash(code)+`, web: `+filepath.ToSlash(web)+`}
targets:
  t1: {host: 10.0.0.1, root: /opt/shop, runAs: shop}
builds:
  mvn: {workdir: "${repo}", command: mvn package}
apps:
  - {name: api, type: binary, build: mvn, artifact: {path: "${repo}/target/api"}}
  - {name: web, type: static, artifact: {path: "${web}"}, static: {link: /usr/share/nginx/html/web}}
`), 0o644)
		f, err := config.Load(conf)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	plan := func(f *config.File) []*targetPlan {
		tg, _ := f.Target("t1")
		return []*targetPlan{{t: tg, apps: []*config.App{f.App("api"), f.App("web")}, items: []*item{
			{app: f.App("api"), local: filepath.Join(code, "target", "api")},
			{app: f.App("web"), local: web},
		}}}
	}

	// 配置文件放在后端仓库里：前端不在 git 里，不能记成后端的提交
	f := load(filepath.Join(code, config.DefaultFileName))
	g := appGits(f, plan(f))
	if api := g["api"]; api.Commit == "" || api.Branch != "main" || !samePath(api.Top, code) {
		t.Fatalf("api：%+v", api)
	}
	if g["web"].Commit != "" {
		t.Fatalf("web 不在 git 里：%+v", g["web"])
	}
	if _, ok := g.Common(); ok {
		t.Fatal("两个应用不是同一个提交")
	}
	if s := g.Short(); !strings.HasPrefix(s, "api ") || !strings.HasSuffix(s, "web 不在 git 里") {
		t.Fatalf("Short：%s", s)
	}
	if d := g.Describe(); len(d) != 2 || !strings.HasPrefix(d[0], "代码（api）：main 分支") || d[1] != "代码（web）：不在 git 仓库里，看不到提交" {
		t.Fatalf("Describe：%v", d)
	}

	// 配置文件在仓库外面（如桌面）：后端照样从构建目录找到仓库
	f2 := load(filepath.Join(root, "desktop", config.DefaultFileName))
	if g2 := appGits(f2, plan(f2)); g2["api"].Commit == "" || g2["web"].Commit != "" {
		t.Fatalf("%+v", g2)
	}

	// 只有后端：一个提交，写法和以前一样
	only := DeployGit{"api": g["api"]}
	if c, ok := only.Common(); !ok || only.Short() != c.Short() || !strings.HasPrefix(only.Describe()[0], "代码：main 分支，提交 ") {
		t.Fatalf("%v %s %v", ok, only.Short(), only.Describe())
	}
	// 只有前端：看不到提交
	if d := (DeployGit{"web": {}}).Describe(); len(d) != 1 || d[0] != "代码：不在 git 仓库里，看不到提交" || (DeployGit{"web": {}}).Short() != "-" {
		t.Fatalf("%v", d)
	}
}

// samePath 比较时解开符号链接（macOS 的临时目录 /var 是 /private/var 的链接，git 给的是解开后的）。
func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && filepath.Clean(ra) == filepath.Clean(rb)
}
