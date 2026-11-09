package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 真的 nginx / OpenResty（容器里的也行）：认出站点、把文件换进它的网站目录、访问到新的内容。
// FERRY_LIVE_SITE=容器里的网站目录（如 /www/sites/web/index）时才跑；要能看到 nginx 的进程（容器里跑时 --pid=host）、能用 docker。
func TestLiveExistingSite(t *testing.T) {
	root := os.Getenv("FERRY_LIVE_SITE")
	if root == "" {
		t.Skip("没有设置 FERRY_LIVE_SITE")
	}
	sys := realSystem{}
	sites, notes := DiscoverNginxSites(sys.Shell)
	for _, s := range sites {
		t.Logf("站点：%+v", s)
	}
	t.Logf("notes：%v", notes)
	var site *struct {
		dir  string
		port int
	}
	for _, s := range sites {
		if s.Root == root && s.Disabled == "" && len(s.Ports) > 0 {
			site = &struct {
				dir  string
				port int
			}{s.Dir, s.Ports[0]}
		}
	}
	if site == nil {
		t.Fatalf("没有认出 %s", root)
	}
	src := t.TempDir()
	mark := "ferry-live-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	os.MkdirAll(filepath.Join(src, "assets"), 0o755)
	os.WriteFile(filepath.Join(src, "index.html"), []byte("<h1>"+mark+"</h1>"), 0o600)
	os.WriteFile(filepath.Join(src, "assets", "app.js"), []byte("console.log(1)"), 0o600)
	n, err := syncSiteFiles(src, site.dir, nil)
	if err != nil || n != 2 {
		t.Fatalf("放了 %d 个：%v", n, err)
	}
	out, err := sys.Shell("wget -qO- http://127.0.0.1:"+strconv.Itoa(site.port)+"/ || curl -s http://127.0.0.1:"+strconv.Itoa(site.port)+"/", 10*time.Second)
	if !strings.Contains(out, mark) {
		t.Fatalf("访问不到新的内容：%q %v", out, err)
	}
	out, _ = sys.Shell("wget -qO- http://127.0.0.1:"+strconv.Itoa(site.port)+"/assets/app.js", 10*time.Second)
	if !strings.Contains(out, "console.log") {
		t.Fatalf("assets：%q", out)
	}
	if _, err := os.Stat(filepath.Join(site.dir, "old.css")); err == nil {
		t.Fatal("原来的 old.css 应该删掉了")
	}
}
