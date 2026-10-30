package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// stageSite 走一遍 prepare（要文件清单），放好站点的文件清单和包：send 为空时全量，否则只放 send 里的文件（增量）。
func (f *fakeSys) stageSite(t *testing.T, web proto.AppSpec, id string, site map[string]string, send []string) (*proto.Request, proto.Prepared) {
	t.Helper()
	r := exec1(t, f, "prepare", &proto.Request{Apps: []proto.AppSpec{web}, Sites: true})
	if r.code != 0 {
		t.Fatalf("prepare：\n%s", r.dump())
	}
	var p proto.Prepared
	json.Unmarshal(r.result.Data, &p)
	var files []proto.SiteFile
	for path, c := range site {
		sum := sha256.Sum256([]byte(c))
		files = append(files, proto.SiteFile{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(c))})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	mb, _ := json.Marshal(files)
	os.WriteFile(filepath.Join(p.Stage, "web.site.json"), mb, 0o644)
	msum := sha256.Sum256(mb)
	rel := proto.ReleaseSpec{App: "web", ID: id, Archive: true, Site: "web.site.json", SiteSHA256: hex.EncodeToString(msum[:])}
	pack := site
	rel.File = "web.tar.gz"
	if send != nil {
		pack = map[string]string{}
		for _, n := range send {
			pack[n] = site[n]
		}
		rel.File, rel.SiteDelta = "web.delta.tar.gz", true
	}
	data := tgz(t, pack)
	os.WriteFile(filepath.Join(p.Stage, rel.File), data, 0o644)
	sum := sha256.Sum256(data)
	rel.SHA256, rel.Size = hex.EncodeToString(sum[:]), int64(len(data))
	f.now = f.now.Add(1e9)
	return &proto.Request{Apps: []proto.AppSpec{web}, Stage: p.Stage, Releases: []proto.ReleaseSpec{rel}}, p
}

func TestStaticSiteDelta(t *testing.T) {
	f := newFake(t)
	web := proto.AppSpec{Name: "web", Type: proto.TypeStatic, KeepReleases: 3}
	run := filepath.Join(f.root, "apps", "web", "running")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(run, filepath.FromSlash(rel)))
		if err != nil {
			return "<没有>"
		}
		return string(b)
	}

	v1 := map[string]string{"index.html": "1", "assets/a.js": "A", "assets/b.css": "B"}
	req, p := f.stageSite(t, web, id1, v1, nil)
	if p.Sites != nil {
		t.Fatalf("还没有版本时不应有清单：%v", p.Sites)
	}
	if r := exec1(t, f, "deploy", req); r.code != 0 {
		t.Fatalf("全量部署：\n%s", r.dump())
	}
	if _, err := os.Stat(filepath.Join(f.root, ".ferry", "apps", "web", "versions", id1, "site.json")); err != nil {
		t.Fatal("文件清单应存进版本记录")
	}

	// 改一个、加一个：只传这两个，其余从当前版本复制
	v2 := map[string]string{"index.html": "2", "assets/a.js": "A", "assets/b.css": "B", "new.txt": "N"}
	req, p = f.stageSite(t, web, id2, v2, []string{"index.html", "new.txt"})
	if len(p.Sites["web"]) != 3 {
		t.Fatalf("prepare 应给出当前版本的清单：%v", p.Sites)
	}
	r := exec1(t, f, "deploy", req)
	if r.code != 0 || !r.has(proto.EvAssemble, "2 个文件沿用当前版本，2 个新传") {
		t.Fatalf("增量部署：\n%s", r.dump())
	}
	if read("index.html") != "2" || read("assets/a.js") != "A" || read("assets/b.css") != "B" || read("new.txt") != "N" {
		t.Fatal("拼出的站点不对")
	}

	// 删掉的文件不会留下来
	v3 := map[string]string{"index.html": "2", "assets/a.js": "A", "new.txt": "N"}
	req, _ = f.stageSite(t, web, id3, v3, []string{})
	if r := exec1(t, f, "deploy", req); r.code != 0 || read("assets/b.css") != "<没有>" {
		t.Fatalf("删掉的文件不应留下：\n%s", r.dump())
	}

	// 服务器上的文件被改过：对不上，不动应用（本机会改为全量重试）
	os.WriteFile(filepath.Join(run, "assets", "a.js"), []byte("hacked"), 0o644)
	req, _ = f.stageSite(t, web, id4, v3, []string{"index.html"})
	r = exec1(t, f, "deploy", req)
	if r.code != proto.CodeVerify || !strings.Contains(r.result.Msg, "assets/a.js 和本机的不一样") || f.current("web") != id3 {
		t.Fatalf("文件被改过应校验失败、不动应用：\n%s", r.dump())
	}
}

func TestStaticSiteDeltaNoCurrent(t *testing.T) {
	f := newFake(t)
	web := proto.AppSpec{Name: "web", Type: proto.TypeStatic, KeepReleases: 3}
	req, _ := f.stageSite(t, web, id1, map[string]string{"index.html": "1"}, []string{})
	if r := exec1(t, f, "deploy", req); r.code != proto.CodeVerify || f.current("web") != "" {
		t.Fatalf("没有当前版本时增量应校验失败：\n%s", r.dump())
	}
}
