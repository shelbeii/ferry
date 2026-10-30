package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func servedApp() proto.AppSpec {
	return proto.AppSpec{Name: "admin", Type: proto.TypeStatic, Unit: "shop-admin.service", User: "svc",
		UnitText:   ManagedMarker + "\n[Service]\nExecStart=/srv/app/runtimes/nginx/nginx -c current/nginx.conf\n",
		NginxConf:  "daemon off; # v1\n",
		SharedDirs: []string{"run", "tmp"}, RuntimeNginx: "nginx",
		Health: proto.Health{TCP: "127.0.0.1:admin", TimeoutSec: 20}, KeepReleases: 3}
}

func TestServedStaticDeployAndRollback(t *testing.T) {
	f := newFake(t)
	web := servedApp()
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{web}, id1, map[string][]byte{"admin": []byte("<h1>1</h1>")}))
	if r.code != 0 || f.current("admin") != id1 || f.units[web.Unit].active != "active" {
		t.Fatalf("\n%s", r.dump())
	}
	app := filepath.Join(f.root, "apps", "admin")
	for name, want := range map[string]string{"running/www/index.html": "<h1>1</h1>", "../../.ferry/apps/admin/versions/" + id1 + "/nginx.conf": web.NginxConf,
		"../../.ferry/apps/admin/versions/" + id1 + "/unit.service": web.UnitText, "../../.ferry/apps/admin/nginx.conf": web.NginxConf} {
		if b, _ := os.ReadFile(filepath.Join(app, filepath.FromSlash(name))); string(b) != want {
			t.Errorf("%s = %q", name, b)
		}
	}
	for _, d := range []string{"data/run", "data/tmp", "running/logs"} {
		if _, err := os.Stat(filepath.Join(app, filepath.FromSlash(d))); err != nil {
			t.Errorf("%s 未创建", d)
		}
	}
	// running/ 里只有站点文件和日志
	if ents, _ := os.ReadDir(filepath.Join(app, "running")); len(ents) != 2 {
		t.Errorf("running/ 里应只有 www 和 logs：%v", ents)
	}
	web2 := web
	web2.NginxConf = "daemon off; # v2\n"
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{web2}, id2, map[string][]byte{"admin": []byte("<h1>2</h1>")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	// 回滚走服务的流程（重启），不是只切链接
	f.calls = nil
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{web2}, To: id1}); r.code != 0 || f.current("admin") != id1 {
		t.Fatal(r.dump())
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "start shop-admin.service") {
		t.Fatalf("回滚应重启服务：%v", f.calls)
	}
	// 回滚后 nginx 用的是那个版本保存的配置，站点文件也是那个版本的
	if b, _ := os.ReadFile(filepath.Join(f.root, ".ferry", "apps", "admin", "nginx.conf")); string(b) != web.NginxConf {
		t.Errorf("回滚后的 nginx 配置 = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "running", "www", "index.html")); string(b) != "<h1>1</h1>" {
		t.Errorf("回滚后的站点文件 = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "versions", id2, "www", "index.html")); string(b) != "<h1>2</h1>" {
		t.Errorf("v2 应放回 versions/：%q", b)
	}
	// 状态：当作服务（有 PID、主端口），不去看系统 nginx
	f.listen = map[int][]string{f.units[web.Unit].pid: {"*:8081"}}
	st := exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{web}})
	var out []proto.AppStatus
	json.Unmarshal(st.result.Data, &out)
	if len(out) != 1 || out[0].Active != "active" || out[0].Serving != "" || len(out[0].Listen)+len(out[0].Extra) != 1 { // 假系统的健康检查端口是应用名
		t.Fatalf("%+v", out)
	}
}

func TestInstallNginx(t *testing.T) {
	f := newFake(t)
	r := exec1(t, f, "prepare", &proto.Request{})
	var p proto.Prepared
	json.Unmarshal(r.result.Data, &p)
	bin := append([]byte("\x7fELF"), make([]byte, 60)...)
	bin[18] = 0x3E // x86_64
	os.WriteFile(filepath.Join(p.Stage, "nginx-1.30.5-x86_64"), bin, 0o644)
	sum := sha256.Sum256(bin)
	spec := &proto.RuntimeSpec{Kind: "nginx", ID: "nginx-1.30.5-x86_64", Link: "nginx", Version: "1.30.5",
		File: "nginx-1.30.5-x86_64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(bin))}
	if r := exec1(t, f, "runtime", &proto.Request{Stage: p.Stage, Runtime: spec}); r.code != 0 {
		t.Fatal(r.dump())
	}
	if _, err := os.Stat(filepath.Join(f.root, "runtimes", "nginx-1.30.5-x86_64", "nginx")); err != nil {
		t.Fatal("nginx 没有放到部署根目录的 runtimes 下")
	}
	if f.links[filepath.Join(f.root, "runtimes", "nginx")] != "nginx-1.30.5-x86_64" {
		t.Fatalf("%v", f.links)
	}
	pf := exec1(t, f, "preflight", &proto.Request{})
	var got proto.Preflight
	json.Unmarshal(pf.result.Data, &got)
	if got.Nginx["nginx"].Version != "1.30.5" {
		t.Fatalf("%+v", got.Nginx)
	}
	// 坏链接名、校验和不对都拒绝
	bad := *spec
	bad.Link = "../x"
	if r := exec1(t, f, "runtime", &proto.Request{Stage: p.Stage, Runtime: &bad}); r.code != proto.CodeBadInput {
		t.Fatal(r.dump())
	}
}
