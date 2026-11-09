package engine

import (
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// 审阅部署的「运行环境」：服务器上已有的直接用；本机、服务器上都没有的要下载。
func TestRuntimeDesc(t *testing.T) {
	e := &Engine{}
	e.Settings.Runtime.CacheDir = t.TempDir()
	web := proto.AppSpec{Type: proto.TypeStatic, Unit: "shop-web.service", SitePort: "8000", RuntimeNginx: "nginx-1.30"}
	cases := []struct {
		s    proto.AppSpec
		pc   *proto.PortCheck
		want string
	}{
		{web, &proto.PortCheck{}, "将下载 Nginx 1.30（本机和服务器上都还没有）并上传，单独运行在 8000 端口（服务 shop-web.service）"},
		{web, &proto.PortCheck{Nginx: map[string]proto.Runtime{"nginx-1.30": {Version: "1.30.5"}}}, "用服务器上已有的 Nginx 1.30.5，单独运行在 8000 端口"},
		{web, nil, "服务器上没有时先下载"},
		{proto.AppSpec{Type: proto.TypeJava, RuntimeJava: 17}, &proto.PortCheck{}, "将下载 JDK 17 的最新版"},
		{proto.AppSpec{Type: proto.TypeJava, RuntimeJava: 17}, &proto.PortCheck{Runtimes: map[int]proto.Runtime{17: {ID: "jdk-17.0.13_11-x64", Version: "17.0.13+11"}}}, "用服务器上已有的 JDK 17（17.0.13+11）"},
		{proto.AppSpec{Type: proto.TypeStatic, SiteDir: "/www/wwwroot/web"}, nil, "只换服务器上已有站点网站目录里的文件：/www/wwwroot/web"},
		{proto.AppSpec{Type: proto.TypeBinary}, nil, ""},
	}
	for _, c := range cases {
		got := e.runtimeDesc(c.s, c.pc, "amd64")
		if (c.want == "" && got != "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v：%q，要有 %q", c.s, got, c.want)
		}
	}
}
