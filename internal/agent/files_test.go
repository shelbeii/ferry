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

// withFiles 往上传目录放附带文件的 tar.gz，写进第一个应用的发布信息。
func withFiles(t *testing.T, req *proto.Request, files map[string]string) *proto.Request {
	t.Helper()
	data := tgz(t, files)
	name := req.Releases[0].App + ".files.tar.gz"
	if err := os.WriteFile(filepath.Join(req.Stage, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	r := &req.Releases[0]
	r.Files, r.FilesSHA256, r.FilesSize = name, hex.EncodeToString(sum[:]), int64(len(data))
	for n := range files {
		r.Meta.Files = append(r.Meta.Files, n)
	}
	return req
}

func TestDeployExtraFilesFollowRelease(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	run := filepath.Join(f.root, "apps", "System", "running")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(run, filepath.FromSlash(rel)))
		if err != nil {
			return "<没有>"
		}
		return string(b)
	}

	req := withFiles(t, f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")}),
		map[string]string{"application.yml": "port: 1", "config/certs/a.pem": "k1"})
	if r := exec1(t, f, "deploy", req); r.code != 0 {
		t.Fatalf("首次部署：\n%s", r.dump())
	}
	if read("System.jar") != "v1" || read("application.yml") != "port: 1" || read("config/certs/a.pem") != "k1" {
		t.Fatalf("附带文件应和程序放在一起：%q %q", read("application.yml"), read("config/certs/a.pem"))
	}
	var meta proto.ReleaseMeta
	b, _ := os.ReadFile(filepath.Join(f.root, ".ferry", "apps", "System", "versions", id1, "release.json"))
	json.Unmarshal(b, &meta)
	if len(meta.Files) != 2 {
		t.Fatalf("release.json 应记下附带文件：%s", b)
	}

	req = withFiles(t, f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}),
		map[string]string{"application.yml": "port: 2"})
	if r := exec1(t, f, "deploy", req); r.code != 0 {
		t.Fatalf("第二次部署：\n%s", r.dump())
	}
	if read("application.yml") != "port: 2" || read("config/certs/a.pem") != "<没有>" {
		t.Fatalf("新版本只带新的附带文件：%q %q", read("application.yml"), read("config/certs/a.pem"))
	}

	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{sys}, To: id1}); r.code != 0 {
		t.Fatalf("回滚：\n%s", r.dump())
	}
	if read("System.jar") != "v1" || read("application.yml") != "port: 1" || read("config/certs/a.pem") != "k1" {
		t.Fatalf("回滚后附带文件应回到旧版本：%q %q", read("application.yml"), read("config/certs/a.pem"))
	}
}

func TestDeployExtraFilesRejected(t *testing.T) {
	for _, c := range []struct{ name, want string }{
		{"logs/app.log", "运行日志的目录"},
		{"System.jar", "和程序文件同名"},
		{".ferry/x", "Ferry 自己用的名字"},
	} {
		f := newFake(t)
		sys := javaApp("System")
		req := withFiles(t, f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")}), map[string]string{c.name: "x"})
		r := exec1(t, f, "deploy", req)
		if r.code == 0 || !strings.Contains(r.dump(), c.want) {
			t.Fatalf("%s 应被拒绝（%s）：\n%s", c.name, c.want, r.dump())
		}
		if f.current("System") != "" {
			t.Fatalf("%s：不应有运行中的版本", c.name)
		}
		if ents, _ := os.ReadDir(filepath.Join(f.root, "apps", "System", "versions")); len(ents) != 0 {
			t.Fatalf("%s：失败后不应留下版本目录：%v", c.name, ents)
		}
	}
}

func TestDeployExtraFilesVerify(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	req := withFiles(t, f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")}), map[string]string{"a.yml": "x"})
	req.Releases[0].FilesSHA256 = strings.Repeat("0", 64)
	r := exec1(t, f, "deploy", req)
	if r.code != proto.CodeVerify || len(f.calls) != 0 || !strings.Contains(r.result.Msg, "附带文件") {
		t.Fatalf("附带文件校验失败不应改动：%v\n%s", f.calls, r.dump())
	}
}
