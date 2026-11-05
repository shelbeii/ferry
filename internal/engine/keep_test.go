package engine

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/artifact"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

const keepYAML = `
version: 1
project: shop
targets:
  t1: {host: 10.0.0.1, root: /data/shop, runAs: web}
apps:
  - name: web
    type: static
    artifact:
      path: dist
      keep: [config.js, {path: uploads, shared: true}]
  - name: api
    type: java
    artifact: {path: api.jar, files: [conf/app.yml]}
`

func TestKeepSpec(t *testing.T) {
	f, err := loadYAML(t, keepYAML)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("t1")
	s, err := Spec(f, tg, f.App("web"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Keep) != 2 || s.Keep[0].Path != "config.js" || !s.Keep[1].Shared {
		t.Fatalf("%+v", s.Keep)
	}
	api := f.App("api")
	for _, c := range []struct {
		keep []*config.KeepItem
		want string
	}{
		{[]*config.KeepItem{{Path: "api.jar"}}, "程序文件"},
		{[]*config.KeepItem{{Path: "app.yml"}}, "附带文件"}, // files 的 conf/app.yml 放到 app.yml
		{[]*config.KeepItem{{Path: "a"}, {Path: "a/b"}}, "重复"},
	} {
		if _, err := KeepSpec(api, "api.jar", c.keep); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v：应报「%s」，实际 %v", c.keep[0].Path, c.want, err)
		}
	}
	if k, err := KeepSpec(api, "api.jar", []*config.KeepItem{{Path: "./prod.yml"}, {Path: "prod.yml"}}); err != nil || len(k) != 1 || k[0].Path != "prod.yml" {
		t.Fatalf("应整理路径、去掉重复的：%v %v", k, err)
	}
}

func tarNames(t *testing.T, p string) []string {
	t.Helper()
	fh, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Typeflag == tar.TypeReg {
			out = append(out, h.Name)
		}
	}
	sort.Strings(out)
	return out
}

// 增量上传：保留的总是带上包里的（服务器上的可能被改过，不能从当前版本复制）
func TestSiteDeltaSendsKept(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, c string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
	}
	write("index.html", strings.Repeat("i", 1000))
	write("assets/a.js", strings.Repeat("a", 5000))
	write("config.js", "c")
	write("uploads/.keep", "")
	site, err := artifact.SiteManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Rep: &recRep{}}
	it := &item{app: &config.App{Name: "web", Type: "static"}, local: dir, site: site,
		spec: proto.AppSpec{Keep: []proto.KeepItem{{Path: "config.js"}, {Path: "uploads", Shared: true}}}}
	var rel proto.ReleaseSpec
	tmp := t.TempDir()
	_, pack, err := e.siteDelta(it, &rel, site, true, tmp) // 服务器上的和本机的完全一样
	if err != nil || pack == nil {
		t.Fatalf("应增量：%v %v", pack, err)
	}
	if got := strings.Join(tarNames(t, pack.local), ","); got != "config.js,uploads/.keep" {
		t.Fatalf("增量包里应只有保留的：%s", got)
	}
	if sha, _, _ := zipx.FileSHA256(pack.local); sha != rel.SHA256 || !rel.SiteDelta {
		t.Fatalf("%+v", rel)
	}
}

func TestKeepNotes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.js"), []byte("pkg"), 0o644)
	os.Mkdir(filepath.Join(dir, "uploads"), 0o755)
	f, err := loadYAML(t, keepYAML)
	if err != nil {
		t.Fatal(err)
	}
	f.Dir = dir
	f.App("web").Artifact.Path = dir
	tg, _ := f.Target("t1")
	ks := proto.KeepScan{App: "web", Release: "r1", States: []proto.KeepState{
		{KeepItem: proto.KeepItem{Path: "config.js"}, Server: "file", SHA256: strings.Repeat("0", 64)},
		{KeepItem: proto.KeepItem{Path: "uploads", Shared: true}, Server: "dir", Size: 3},
		{KeepItem: proto.KeepItem{Path: "missing.js"}, Server: "none"},
	}}
	rs := keepReports(f, tg, f.App("web"), ks)
	if len(rs) != 3 || !rs[0].Differs || rs[0].Package != "file" || !strings.Contains(rs[0].Note, "和包里的不同") {
		t.Fatalf("%+v", rs[0])
	}
	if !strings.Contains(rs[1].Note, "第一次共用：复制服务器上的") || rs[2].Note != "服务器上和包里都没有" {
		t.Fatalf("%q %q", rs[1].Note, rs[2].Note)
	}
	ks.Release = ""
	ks.States = ks.States[2:]
	if rs := keepReports(f, tg, f.App("web"), ks); rs[0].Note != "还没部署过：用包里的" {
		t.Fatalf("%q", rs[0].Note)
	}
}
