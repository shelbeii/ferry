package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func canSymlink(t *testing.T) {
	d := t.TempDir()
	if err := os.Symlink(d, filepath.Join(d, "x")); err != nil {
		t.Skip("这台电脑不能建符号链接")
	}
}

// python：依赖装进 deps 下的 venv，requirements.txt 没变就复用，没有版本在用的去掉
func TestPythonDeps(t *testing.T) {
	canSymlink(t)
	f := newFake(t)
	api := proto.AppSpec{Name: "api", Type: proto.TypePython, Unit: "shop-api.service", User: "svc",
		UnitText:     ManagedMarker + "\n[Service]\nExecStart=/opt/shop/apps/api/running/.venv/bin/uvicorn main:app\n",
		Health:       proto.Health{TCP: "127.0.0.1:api", TimeoutSec: 20},
		KeepReleases: 1, Deps: &proto.Deps{Kind: proto.TypePython, Registry: "https://pypi.example/simple"}}
	deploy := func(id, req string) {
		t.Helper()
		pkg := tgz(t, map[string]string{"main.py": "app = 1\n", "requirements.txt": req})
		r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{api}, id, map[string][]byte{"api": pkg}))
		if r.code != 0 {
			t.Fatalf("部署 %s：\n%s", id, r.dump())
		}
	}
	deploy(id1, "fastapi==0.110\n")
	if len(f.hooks) != 1 || !strings.Contains(f.hooks[0].Cmd, "-m venv") || !strings.Contains(f.hooks[0].Cmd, "--index-url 'https://pypi.example/simple'") {
		t.Fatalf("要建 venv 装依赖：%+v", f.hooks)
	}
	venv := filepath.Join(f.root, "apps", "api", "running", ".venv") // 运行中的版本在 running/
	first, err := os.Readlink(venv)
	if err != nil || !strings.HasPrefix(first, filepath.Join(f.root, "apps", "api", "deps", "python-")) {
		t.Fatalf(".venv 要链接到 deps：%q %v", first, err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, "apps", "api", "running", "main.py")); string(b) != "app = 1\n" {
		t.Fatal("源码要解压到版本目录")
	}
	// requirements.txt 没变：不再装
	deploy(id2, "fastapi==0.110\n")
	if len(f.hooks) != 1 {
		t.Fatalf("依赖没变不要再装：%+v", f.hooks)
	}
	// 变了：装一份新的；只留 1 个版本时旧的 venv 没人用了，去掉
	deploy(id3, "fastapi==0.111\n")
	if len(f.hooks) != 2 {
		t.Fatalf("依赖变了要装：%+v", f.hooks)
	}
	ents, _ := os.ReadDir(filepath.Join(f.root, "apps", "api", "deps"))
	if len(ents) > 2 {
		t.Fatalf("没人用的 venv 要去掉：%v", ents)
	}
}

func TestExecCommand(t *testing.T) {
	if c := execCommand("[Service]\nExecStart=/usr/bin/env node /opt/a/dist/main.js\n"); c != "node" {
		t.Fatal(c)
	}
	if c := execCommand("[Service]\nExecStart=/opt/a/running/App\n"); c != "" {
		t.Fatal(c)
	}
}
