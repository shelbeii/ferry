package engine

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestDeployGitFallsBackToBuildDir(t *testing.T) {
	root := t.TempDir()
	code := filepath.Join(root, "code")
	gitRepo(t, code)
	// 配置文件在代码仓库外面（如桌面），构建目录用变量指向仓库
	conf := filepath.Join(root, "desktop", config.DefaultFileName)
	os.MkdirAll(filepath.Dir(conf), 0o755)
	os.WriteFile(conf, []byte(`
version: 1
project: shop
vars: {repo: `+filepath.ToSlash(code)+`}
targets:
  t1: {host: 10.0.0.1, root: /opt/shop, runAs: shop}
builds:
  mvn: {workdir: "${repo}", command: mvn package}
apps:
  - {name: api, type: binary, build: mvn, artifact: {path: "${repo}/target/api"}}
`), 0o644)
	f, err := config.Load(conf)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("t1")
	p := &targetPlan{t: tg, apps: []*config.App{f.App("api")}, items: []*item{{app: f.App("api"), local: filepath.Join(code, "target", "api")}}}
	g, fallback := deployGit(f, []*targetPlan{p})
	if g.Commit == "" || !fallback || g.Branch != "main" || !samePath(g.Top, code) {
		t.Fatalf("%+v fallback=%v", g, fallback)
	}

	// 配置文件就在仓库里：直接用，不算回退
	inRepo := filepath.Join(code, config.DefaultFileName)
	b, _ := os.ReadFile(conf)
	os.WriteFile(inRepo, b, 0o644)
	f2, err := config.Load(inRepo)
	if err != nil {
		t.Fatal(err)
	}
	if g, fallback := deployGit(f2, []*targetPlan{p}); g.Commit == "" || fallback {
		t.Fatalf("%+v fallback=%v", g, fallback)
	}

	// 都不是仓库：空
	plain := filepath.Join(root, "plain", config.DefaultFileName)
	os.MkdirAll(filepath.Dir(plain), 0o755)
	os.WriteFile(plain, []byte(`
version: 1
project: shop
targets:
  t1: {host: 10.0.0.1, root: /opt/shop, runAs: shop}
apps:
  - {name: api, type: binary, artifact: {path: out/api}}
`), 0o644)
	f3, _ := config.Load(plain)
	tg3, _ := f3.Target("t1")
	p3 := &targetPlan{t: tg3, apps: []*config.App{f3.App("api")}, items: []*item{{app: f3.App("api"), local: filepath.Join(root, "plain", "out", "api")}}}
	if g, _ := deployGit(f3, []*targetPlan{p3}); g.Commit != "" {
		t.Fatalf("%+v", g)
	}
}

// samePath 比较时解开符号链接（macOS 的临时目录 /var 是 /private/var 的链接，git 给的是解开后的）。
func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && filepath.Clean(ra) == filepath.Clean(rb)
}
