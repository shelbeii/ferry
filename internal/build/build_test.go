package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

func TestPlanGroupsAndEach(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ferry.yaml")
	os.WriteFile(p, []byte(`
version: 1
project: demo
builds:
  maven: {command: "mvn -pl ${each:module} -am package -P${profile}", lock: maven}
targets:
  prod: {host: h, runAs: u, vars: {profile: prod}}
apps:
  - {name: A, type: java, build: maven, vars: {module: m/a}, artifact: {path: a.jar}}
  - {name: B, type: java, build: maven, vars: {module: m/b}, artifact: {path: b.jar}}
  - {name: C, type: java, build: {command: "echo ${app} ${module}", workdir: sub}, vars: {module: c}, artifact: {path: c.jar}}
  - {name: D, type: java, artifact: {path: d.jar}}
`), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	f, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("prod")
	apps, _ := f.Select(tg, nil)
	steps, err := Plan(f, tg, apps)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("应合并为 2 步：%d", len(steps))
	}
	if steps[0].Command != "mvn -pl m/a,m/b -am package -Pprod" || steps[0].Lock != "maven" || len(steps[0].Apps) != 2 {
		t.Fatalf("%+v", steps[0])
	}
	if steps[1].Command != "echo C c" || steps[1].Workdir != filepath.Join(dir, "sub") || !strings.HasPrefix(steps[1].Lock, "dir-") {
		t.Fatalf("%+v", steps[1])
	}
}

func TestRunCapturesFailure(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{LogDir: filepath.Join(dir, "logs"), LockDir: filepath.Join(dir, "locks")}
	if _, err := r.Run(&Step{Name: "ok", Command: "echo hello", Workdir: dir, Lock: "x"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Run(&Step{Name: "bad", Command: "echo boom && exit 3", Workdir: dir, Lock: "x"})
	be, ok := err.(*Error)
	if !ok || len(be.Tail) == 0 || !strings.Contains(be.Tail[len(be.Tail)-1], "boom") {
		t.Fatalf("%v", err)
	}
}

func TestPlanRejectsBadWorkdir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ferry.yaml")
	write := func(workdir string) error {
		os.WriteFile(p, []byte(`
version: 1
project: demo
targets: {prod: {host: h, runAs: u}}
apps:
  - {name: A, type: java, build: {command: mvn package, workdir: "`+workdir+`"}, artifact: {path: a.jar}}
`), 0o644)
		f, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		tg, _ := f.Target("prod")
		apps, _ := f.Select(tg, nil)
		_, err = Plan(f, tg, apps)
		return err
	}
	if err := write("no-such-dir"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("不存在的目录应报错：%v", err)
	}
	root := filepath.VolumeName(dir) + string(filepath.Separator)
	if err := write(filepath.ToSlash(root)); err == nil || !strings.Contains(err.Error(), "磁盘根目录") {
		t.Fatalf("磁盘根目录应报错：%v", err)
	}
}
