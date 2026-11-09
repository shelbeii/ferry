package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// 前端站点只换服务器上已有站点网站目录里的文件：部署、失败切回、回到接管前、第一次部署失败换回原来的。
func TestSiteDirDeployRollbackLegacy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("网站目录在 Linux 服务器上")
	}
	f := newFake(t)
	dir := filepath.Join(t.TempDir(), "www", "sites", "web", "index")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("orig"), 0o644)
	os.WriteFile(filepath.Join(dir, "old.css"), []byte("css"), 0o644)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "<none>"
		}
		return string(b)
	}
	web := proto.AppSpec{Name: "web", Type: proto.TypeStatic, SiteDir: dir, KeepReleases: 3}
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{web}, id1, map[string][]byte{"web": []byte("<h1>1</h1>")}))
	if r.code != 0 || f.current("web") != id1 || read("index.html") != "<h1>1</h1>" || read("old.css") != "<none>" {
		t.Fatalf("\n%s", r.dump())
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, "apps", "web", siteBackupDir, "old.css")); string(b) != "css" {
		t.Fatal("接管前的文件要备份")
	}
	// 第二次失败（切换后的命令失败）：网站目录换回第一版
	web.Reload = "nginx -s reload"
	f.shellFail["nginx -s reload"] = true
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{web}, id2, map[string][]byte{"web": []byte("<h1>2</h1>")}))
	if r.code != proto.CodeFailed || f.current("web") != id1 || read("index.html") != "<h1>1</h1>" {
		t.Fatalf("失败要切回：%q\n%s", read("index.html"), r.dump())
	}
	web.Reload = ""
	// 版本列表里能「回到接管前」
	r = exec1(t, f, "releases", &proto.Request{Apps: []proto.AppSpec{web}})
	var rels []proto.AppReleases
	json.Unmarshal(r.result.Data, &rels)
	if r.code != 0 || len(rels) != 1 || !rels[0].Legacy {
		t.Fatalf("要能回到接管前：%+v\n%s", rels, r.dump())
	}
	r = exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{web}, To: LegacyID})
	if r.code != 0 || f.current("web") != "" || read("index.html") != "orig" || read("old.css") != "css" {
		t.Fatalf("回到接管前：%q\n%s", read("index.html"), r.dump())
	}

	// 第一次部署就失败：换回原来的文件
	dir2 := filepath.Join(t.TempDir(), "www", "sites", "admin", "index")
	os.MkdirAll(dir2, 0o755)
	os.WriteFile(filepath.Join(dir2, "index.html"), []byte("admin-orig"), 0o644)
	admin := proto.AppSpec{Name: "admin", Type: proto.TypeStatic, SiteDir: dir2, Reload: "nginx -s reload", KeepReleases: 3}
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{admin}, id1, map[string][]byte{"admin": []byte("new")}))
	if b, _ := os.ReadFile(filepath.Join(dir2, "index.html")); r.code != proto.CodeFailed || string(b) != "admin-orig" {
		t.Fatalf("第一次失败要换回原来的：%q\n%s", b, r.dump())
	}

	// 网站目录不能和 Ferry 的目录重叠
	bad := proto.AppSpec{Name: "bad", Type: proto.TypeStatic, SiteDir: filepath.Join(f.root, "apps", "x"), KeepReleases: 3}
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{bad}, id1, map[string][]byte{"bad": []byte("x")}))
	if r.code == 0 {
		t.Fatalf("应拒绝：\n%s", r.dump())
	}
}
