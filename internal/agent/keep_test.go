package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func readOr(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<没有>"
	}
	return string(b)
}

func releaseMeta(t *testing.T, f *fakeSys, app, id string) proto.ReleaseMeta {
	t.Helper()
	var m proto.ReleaseMeta
	b, _ := os.ReadFile(filepath.Join(f.root, ".ferry", "apps", app, "versions", id, "release.json"))
	json.Unmarshal(b, &m)
	return m
}

// 静态站点保留服务器上的配置文件：第一次用包里的；之后用服务器上改过的，增量上传也一样；回滚回到那个版本的。
func TestKeepStaticCopy(t *testing.T) {
	f := newFake(t)
	web := proto.AppSpec{Name: "web", Type: proto.TypeStatic, KeepReleases: 5, Keep: []proto.KeepItem{{Path: "config.js"}, {Path: "conf/*.json"}}}
	run := filepath.Join(f.root, "apps", "web", "running")
	read := func(rel string) string { return readOr(filepath.Join(run, filepath.FromSlash(rel))) }

	v1 := map[string]string{"index.html": "1", "config.js": "pkg1", "conf/a.json": "A1"}
	req, _ := f.stageSite(t, web, id1, v1, nil)
	if r := exec1(t, f, "deploy", req); r.code != 0 {
		t.Fatalf("第一次部署：\n%s", r.dump())
	}
	if read("config.js") != "pkg1" || read("conf/a.json") != "A1" {
		t.Fatalf("第一次部署应用包里的：%q %q", read("config.js"), read("conf/a.json"))
	}
	// 服务器上改了配置、加了一个
	os.WriteFile(filepath.Join(run, "config.js"), []byte("server"), 0o644)
	os.WriteFile(filepath.Join(run, "conf", "b.json"), []byte("B-server"), 0o644)

	// 增量：保留的总是带上包里的（engine 这样传）
	v2 := map[string]string{"index.html": "2", "config.js": "pkg2", "conf/a.json": "A1"}
	req, _ = f.stageSite(t, web, id2, v2, []string{"index.html", "config.js", "conf/a.json"})
	r := exec1(t, f, "deploy", req)
	if r.code != 0 {
		t.Fatalf("第二次部署：\n%s", r.dump())
	}
	if read("index.html") != "2" || read("config.js") != "server" || read("conf/b.json") != "B-server" || read("conf/a.json") != "A1" {
		t.Fatalf("应保留服务器上的：%q %q %q", read("index.html"), read("config.js"), read("conf/b.json"))
	}
	if !r.has(proto.EvKeep, "config.js：用服务器上的（和包里的不同）") || !r.has(proto.EvKeep, "conf/a.json：用服务器上的（和包里的一样）") {
		t.Fatalf("应说明保留了什么：\n%s", r.dump())
	}
	if m := releaseMeta(t, f, "web", id2); strings.Join(m.Kept, ",") != "config.js,conf/a.json,conf/b.json" {
		t.Fatalf("版本记录应记下保留的：%v", m.Kept)
	}
	b, _ := os.ReadFile(filepath.Join(f.root, ".ferry", "apps", "web", "versions", id2, "site.json"))
	if strings.Contains(string(b), "config.js") || strings.Contains(string(b), "conf/") || !strings.Contains(string(b), "index.html") {
		t.Fatalf("站点清单里不应有保留的：%s", b)
	}

	// 下一次增量：保留的不在清单里，照样能拼出来（不会因为服务器上的被改过而校验失败）
	os.WriteFile(filepath.Join(run, "config.js"), []byte("server2"), 0o644)
	req, _ = f.stageSite(t, web, id3, v2, []string{"config.js", "conf/a.json"})
	if r := exec1(t, f, "deploy", req); r.code != 0 || read("config.js") != "server2" || read("index.html") != "2" {
		t.Fatalf("第三次部署：%q\n%s", read("config.js"), r.dump())
	}

	// 回滚：回到那个版本自己的
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{web}, To: id1}); r.code != 0 {
		t.Fatalf("回滚：\n%s", r.dump())
	}
	if read("index.html") != "1" || read("config.js") != "server" {
		t.Fatalf("回滚后应是 %s 的：%q %q", id1, read("index.html"), read("config.js"))
	}
}

// 共用的目录：第一次复制运行中的版本里的到 data/，以后各版本都链接到它；包里的不用。
func TestKeepStaticShared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("要符号链接")
	}
	f := newFake(t)
	web := proto.AppSpec{Name: "web", Type: proto.TypeStatic, KeepReleases: 5}
	run := filepath.Join(f.root, "apps", "web", "running")
	data := filepath.Join(f.root, "apps", "web", "data", "uploads")

	// 先不保留：服务器上运行中的版本里有人传了文件
	req, _ := f.stageSite(t, web, id1, map[string]string{"index.html": "1", "uploads/.keep": ""}, nil)
	if r := exec1(t, f, "deploy", req); r.code != 0 {
		t.Fatalf("第一次部署：\n%s", r.dump())
	}
	os.WriteFile(filepath.Join(run, "uploads", "u1.png"), []byte("U1"), 0o644)

	web.Keep = []proto.KeepItem{{Path: "uploads", Shared: true}}
	req, _ = f.stageSite(t, web, id2, map[string]string{"index.html": "2", "uploads/pkg.png": "P"}, nil)
	r := exec1(t, f, "deploy", req)
	if r.code != 0 || !r.has(proto.EvKeep, "复制运行中的版本里的 2 个文件") {
		t.Fatalf("第一次共用：\n%s", r.dump())
	}
	if l, err := os.Readlink(filepath.Join(run, "uploads")); err != nil || l != data {
		t.Fatalf("新版本里应是指向 data 的链接：%q %v", l, err)
	}
	if readOr(filepath.Join(run, "uploads", "u1.png")) != "U1" || readOr(filepath.Join(run, "uploads", "pkg.png")) != "<没有>" {
		t.Fatal("共用目录应是服务器上的，不用包里的")
	}
	if m := releaseMeta(t, f, "web", id2); strings.Join(m.Kept, ",") != "uploads（共用）" {
		t.Fatalf("版本记录：%v", m.Kept)
	}

	// 之后：直接链接，新传的文件都在
	os.WriteFile(filepath.Join(data, "u2.png"), []byte("U2"), 0o644)
	req, _ = f.stageSite(t, web, id3, map[string]string{"index.html": "3"}, nil)
	if r := exec1(t, f, "deploy", req); r.code != 0 || !r.has(proto.EvKeep, "uploads：用共用目录 data/uploads") {
		t.Fatalf("第三次部署：\n%s", r.dump())
	}
	if readOr(filepath.Join(run, "uploads", "u2.png")) != "U2" || readOr(filepath.Join(run, "index.html")) != "3" {
		t.Fatal("应链接到共用目录")
	}
	// 回滚到 id2：也是链接，看到的还是共用目录（回滚不动它）
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{web}, To: id2}); r.code != 0 {
		t.Fatalf("回滚：\n%s", r.dump())
	}
	if readOr(filepath.Join(run, "uploads", "u2.png")) != "U2" {
		t.Fatal("回滚后共用目录不变")
	}
	// 清理旧版本不会删掉共用目录里的
	if _, err := os.Stat(filepath.Join(data, "u1.png")); err != nil {
		t.Fatal("共用目录里的文件不应丢")
	}
}

// java：保留服务器上手工放的配置文件，回滚时跟着版本走。
func TestKeepJavaCopy(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	sys.Keep = []proto.KeepItem{{Path: "application-prod.yml"}}
	run := filepath.Join(f.root, "apps", "System", "running")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})); r.code != 0 {
		t.Fatalf("第一次部署：\n%s", r.dump())
	}
	os.WriteFile(filepath.Join(run, "application-prod.yml"), []byte("db: prod"), 0o640)
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}))
	if r.code != 0 || !r.has(proto.EvKeep, "application-prod.yml：用服务器上的（包里没有）") {
		t.Fatalf("第二次部署：\n%s", r.dump())
	}
	if readOr(filepath.Join(run, "System.jar")) != "v2" || readOr(filepath.Join(run, "application-prod.yml")) != "db: prod" {
		t.Fatal("新版本应带上服务器上的配置")
	}
}

func TestKeepRejected(t *testing.T) {
	for _, c := range []struct {
		k    proto.KeepItem
		want string
	}{
		{proto.KeepItem{Path: "logs"}, "运行日志"},
		{proto.KeepItem{Path: "System.jar"}, "程序文件"},
		{proto.KeepItem{Path: "../x"}, "无效"},
		{proto.KeepItem{Path: "/etc/x"}, "相对路径"},
		{proto.KeepItem{Path: "up*", Shared: true}, "通配符"},
		{proto.KeepItem{Path: ".ferry/x"}, "Ferry 自己的目录"},
	} {
		f := newFake(t)
		sys := javaApp("System")
		sys.Keep = []proto.KeepItem{c.k}
		r := exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{sys}})
		if r.code != proto.CodeBadInput || !strings.Contains(r.result.Msg, c.want) {
			t.Fatalf("%s 应被拒绝（%s）：\n%s", c.k.Path, c.want, r.dump())
		}
	}
}

func TestKeepScan(t *testing.T) {
	f := newFake(t)
	web := proto.AppSpec{Name: "web", Type: proto.TypeStatic, KeepReleases: 5, Keep: []proto.KeepItem{{Path: "config.js"}, {Path: "missing.txt"}, {Path: "assets"}}}
	req, _ := f.stageSite(t, web, id1, map[string]string{"index.html": "1", "config.js": "C", "assets/a.js": "A", "assets/b.js": "B"}, nil)
	if r := exec1(t, f, "deploy", req); r.code != 0 {
		t.Fatalf("部署：\n%s", r.dump())
	}
	r := exec1(t, f, "keepscan", &proto.Request{Apps: []proto.AppSpec{web}, KeepDir: "."})
	if r.code != 0 {
		t.Fatalf("keepscan：\n%s", r.dump())
	}
	var out []proto.KeepScan
	json.Unmarshal(r.result.Data, &out)
	if len(out) != 1 || out[0].Release != id1 || len(out[0].States) != 3 {
		t.Fatalf("%s", r.result.Data)
	}
	st := out[0].States
	if st[0].Server != "file" || st[0].SHA256 == "" || st[1].Server != "none" || st[2].Server != "dir" || st[2].Size != 2 {
		t.Fatalf("各项的情况不对：%+v", st)
	}
	var names []string
	for _, e := range out[0].Entries {
		names = append(names, e.Path)
	}
	if strings.Join(names, ",") != "assets,config.js,index.html" || !out[0].Entries[0].Dir {
		t.Fatalf("目录列表（目录在前）：%v", names)
	}
	r = exec1(t, f, "keepscan", &proto.Request{Apps: []proto.AppSpec{web}, KeepDir: "assets"})
	json.Unmarshal(r.result.Data, &out)
	if r.code != 0 || len(out[0].Entries) != 2 || out[0].Entries[0].Path != "assets/a.js" {
		t.Fatalf("子目录：\n%s", r.dump())
	}
	if r := exec1(t, f, "keepscan", &proto.Request{Apps: []proto.AppSpec{web}, KeepDir: "../.."}); r.code == 0 {
		t.Fatal("不能列出版本目录外面的")
	}
}

func TestKeepMatch(t *testing.T) {
	keep := []proto.KeepItem{{Path: "config.js"}, {Path: "conf/*.json"}, {Path: "uploads"}}
	for p, want := range map[string]bool{
		"config.js": true, "config.json": false, "conf/a.json": true, "conf/a.txt": false,
		"uploads": true, "uploads/x/y.png": true, "uploadsx": false, "a/config.js": false,
	} {
		if proto.KeepMatch(keep, p) != want {
			t.Errorf("%s：应为 %v", p, want)
		}
	}
}
