package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// 运行用户已存在：以它的身份测试，进不去的那一级就是问题所在（下面的不再列）
func TestBlockedDirsExistingUser(t *testing.T) {
	f := newFake(t)
	home := filepath.ToSlash(filepath.Join(t.TempDir(), "home", "alice"))
	os.MkdirAll(home+"/soft", 0o755)
	f.denied = map[string]bool{home: true}
	f.owners = map[string]Ownership{home: {UID: 1005, GID: 1005, User: "alice", Group: "alice", Perm: os.ModeDir | 0o700}}
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	got := a.blockedDirs("svc", home+"/soft/nacos", false)
	if len(got) != 1 || got[0].Path != home || got[0].Owner != "alice" || got[0].Mode != "drwx------" {
		t.Fatalf("应只报 %s：%+v", home, got)
	}
	if got := a.blockedDirs("svc", home+"/soft/nacos", false); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	delete(f.denied, home)
	if got := a.blockedDirs("svc", home+"/soft/nacos", false); len(got) != 0 {
		t.Fatalf("能进去时不应报：%+v", got)
	}
}

// 运行用户还没创建：按「其他用户」的权限位判断
func TestBlockedDirsNewUser(t *testing.T) {
	f := newFake(t)
	base := filepath.ToSlash(t.TempDir())
	home := base + "/home/alice"
	os.MkdirAll(home, 0o755)
	f.owners = map[string]Ownership{
		base + "/home": {User: "root", Perm: os.ModeDir | 0o755},
		home:           {UID: 1005, User: "alice", Perm: os.ModeDir | 0o750},
	}
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	got := a.blockedDirs("shop", home+"/ferry-software/nacos", false)
	if len(got) != 1 || got[0].Path != home {
		t.Fatalf("应报 %s（750，其他用户进不去）：%+v", home, got)
	}
}

func TestSwReach(t *testing.T) {
	base := filepath.ToSlash(t.TempDir())
	home := base + "/home/alice"
	os.MkdirAll(home+"/soft", 0o700)
	setup := func(acl bool) (*fakeSys, *Agent, *proto.SoftwarePlan) {
		f := newFake(t)
		f.acl = acl
		f.denied = map[string]bool{home: true}
		f.owners = map[string]Ownership{home: {UID: 1005, GID: 1005, User: "alice", Group: "alice", Perm: os.ModeDir | 0o700}}
		return f, &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}, &proto.SoftwarePlan{Name: "nacos", User: "svc", Dir: home + "/soft/nacos"}
	}

	// 没同意：报错，说清楚是哪一级、怎么办
	f, a, pl := setup(false)
	err := a.swReach(pl)
	if err == nil || !strings.Contains(err.Error(), home+"（属于 alice，drwx------）") || !strings.Contains(err.Error(), "同意") {
		t.Fatalf("%v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("没同意时不应改动：%v", f.calls)
	}

	// 同意了，不支持 ACL：chmod o+x，保留原来的权限位
	f, a, pl = setup(false)
	pl.FixParents = true
	if err := a.swReach(pl); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, "\n") != "chmod 701 "+home {
		t.Fatalf("%v", f.calls)
	}

	// 支持 ACL：只给运行用户开
	f, a, pl = setup(true)
	pl.FixParents = true
	if err := a.swReach(pl); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, "\n") != "setfacl u:svc:x "+home {
		t.Fatalf("%v", f.calls)
	}

}

func TestSystemDir(t *testing.T) {
	for p, want := range map[string]bool{"/": true, "/etc": true, "/etc/x": true, "/root": true, "/usr/lib": true,
		"/usr/local/app": false, "/home/alice": false, "/data": false, "/opt/x": false} {
		if systemDir(p) != want {
			t.Errorf("systemDir(%s) 应为 %v", p, want)
		}
	}
}

// 安装失败时留下安装包（只留最新一个），swcheck 报告有；再失败时换成新的
func TestKeepPackage(t *testing.T) {
	f := newFake(t)
	ferry := filepath.Join(f.root, "ferry")
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{FerryDir: ferry}}
	stage := t.TempDir()
	put := func(body string) (string, string) {
		p := filepath.Join(stage, "pkg-"+body)
		os.WriteFile(p, []byte(body), 0o640)
		h := sha256.Sum256([]byte(body))
		return p, hex.EncodeToString(h[:])
	}
	src1, sha1 := put("one")
	a.keepPackage("nacos", src1, sha1)
	check := func(sha string) bool {
		r := exec1(t, f, "swcheck", &proto.Request{FerryDir: ferry, SwCheck: &proto.SwCheckSpec{PackageSHA: sha}})
		return strings.Contains(string(r.result.Data), `"cached":true`)
	}
	if !check(sha1) {
		t.Fatal("应报告留着的安装包")
	}
	src2, sha2 := put("two")
	a.keepPackage("nacos", src2, sha2)
	if check(sha1) || !check(sha2) {
		t.Fatal("只留最新的一个")
	}
	// 已经在缓存里（用留着的包装又失败了）：不动
	a.keepPackage("nacos", filepath.Join(a.swCacheDir(), sha2), sha2)
	if !check(sha2) {
		t.Fatal("留着的包不应被删掉")
	}
}

// 指定目录的 JDK：解到那里，认得出是 Ferry 装的哪个大版本
func TestUnpackJDKAt(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	stage := t.TempDir()
	data := jdkTar(t, nil)
	os.WriteFile(filepath.Join(stage, "jdk.tar.gz"), data, 0o644)
	h := sha256.Sum256(data)
	r := &proto.RuntimeSpec{ID: "jdk-17.0.13_11-x64", Java: 17, File: "jdk.tar.gz", SHA256: hex.EncodeToString(h[:]), Size: int64(len(data))}
	final := filepath.Join(t.TempDir(), "opt", "jdk17")
	os.MkdirAll(filepath.Dir(final), 0o755)
	if code, msg := a.unpackJDK(r, stage, final); code != proto.CodeOK {
		t.Fatal(msg)
	}
	if !a.ferryJDKAt(final, 17) || a.ferryJDKAt(final, 21) {
		t.Fatal("应认出是 JDK 17")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(final), ".tmp-jdk17")); err == nil {
		t.Fatal("临时目录应已改名")
	}
}

// 放 JDK 的目录：找出下面 Ferry 装的这个大版本的 JDK（有几个时用最新的）；目录本身就是 JDK 时用它
func TestFindJDK(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{}}
	base := filepath.ToSlash(t.TempDir())
	put := func(dir string, java int) {
		os.MkdirAll(dir+"/bin", 0o755)
		os.WriteFile(dir+"/bin/java", []byte("#!/bin/sh\n"), 0o755)
		os.WriteFile(dir+"/.ferry-runtime.json", []byte(`{"id":"x","java":`+strconv.Itoa(java)+`}`), 0o644)
	}
	put(base+"/jdk-17.0.9_9-x64", 17)
	put(base+"/jdk-17.0.13_11-x64", 17)
	put(base+"/jdk-21.0.5_11-x64", 21)
	os.MkdirAll(base+"/other", 0o755)
	if got := a.findJDK(base, 17); got != base+"/jdk-17.0.13_11-x64" {
		t.Fatalf("应选最新的 17：%s", got)
	}
	if got := a.findJDK(base, 25); got != "" {
		t.Fatalf("没有 25：%s", got)
	}
	flat := base + "/flat"
	put(flat, 17)
	if got := a.findJDK(flat, 17); got != flat {
		t.Fatalf("目录本身就是 JDK：%s", got)
	}
}
