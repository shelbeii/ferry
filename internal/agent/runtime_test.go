package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func jdkTar(t *testing.T, extra func(tw *tar.Writer)) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	add := func(name, body string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: "jdk-17.0.13+11/" + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	add("release", `JAVA_VERSION="17.0.13"`+"\n", 0o644)
	add("bin/java", "#!/bin/sh\n", 0o755)
	add("lib/modules", "x", 0o666) // 他人可写的要去掉
	if extra != nil {
		extra(tw)
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func installRuntime(t *testing.T, f *fakeSys, data []byte, sum string) run {
	t.Helper()
	r := exec1(t, f, "prepare", &proto.Request{})
	var p proto.Prepared
	json.Unmarshal(r.result.Data, &p)
	os.WriteFile(filepath.Join(p.Stage, "jdk.tar.gz"), data, 0o644)
	if sum == "" {
		h := sha256.Sum256(data)
		sum = hex.EncodeToString(h[:])
	}
	return exec1(t, f, "runtime", &proto.Request{Stage: p.Stage, FerryDir: filepath.Join(f.root, "ferry"),
		Runtime: &proto.RuntimeSpec{ID: "jdk-17.0.13_11-x64", Java: 17, File: "jdk.tar.gz", SHA256: sum, Size: int64(len(data))}})
}

func TestRuntimeInstall(t *testing.T) {
	f := newFake(t)
	data := jdkTar(t, nil)
	r := installRuntime(t, f, data, "")
	if r.code != 0 {
		t.Fatalf("安装：\n%s", r.dump())
	}
	dir := filepath.Join(f.root, "ferry", "runtimes")
	if _, err := os.Stat(filepath.Join(dir, "jdk-17.0.13_11-x64", "bin", "java")); err != nil {
		t.Fatal("bin/java 应已解开（去掉最外层目录）")
	}
	if f.links[filepath.Join(dir, "java-17")] != "jdk-17.0.13_11-x64" {
		t.Fatalf("java-17 链接：%v", f.links)
	}
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root, FerryDir: filepath.Join(f.root, "ferry")}}
	if rt := a.installedRuntimes(); rt[17].ID != "jdk-17.0.13_11-x64" {
		t.Fatalf("installedRuntimes：%+v", rt)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, ".ferry", "incoming")); len(entries) != 0 {
		t.Fatal("上传目录应已删除")
	}
	// 已经装过：再装一次不出错
	if r := installRuntime(t, f, data, ""); r.code != 0 {
		t.Fatalf("重复安装：\n%s", r.dump())
	}
	// 校验不过
	if r := installRuntime(t, newFake(t), data, strings.Repeat("0", 64)); r.code != proto.CodeVerify {
		t.Fatalf("sha256 不符应拒绝：%d %s", r.code, r.result.Msg)
	}
}

func TestRuntimeRejectsEscapingLinks(t *testing.T) {
	f := newFake(t)
	data := jdkTar(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "jdk-17.0.13+11/lib/evil", Linkname: "../../../../etc/shadow", Typeflag: tar.TypeSymlink})
	})
	r := installRuntime(t, f, data, "")
	if r.code == 0 || !strings.Contains(r.result.Msg, "指向外部的链接") {
		t.Fatalf("越界链接应拒绝：%d %s", r.code, r.result.Msg)
	}
	if _, err := os.Stat(filepath.Join(f.root, "ferry", "runtimes", "jdk-17.0.13_11-x64")); err == nil {
		t.Fatal("失败时不应留下目录")
	}
}

// 锁定版本：只建这个版本自己的链接，不动 java-N；装过的不用再上传
func TestRuntimePin(t *testing.T) {
	f := newFake(t)
	data := jdkTar(t, nil)
	if r := installRuntime(t, f, data, ""); r.code != 0 { // 别的项目不锁版本装的：java-17 → jdk-17.0.13_11-x64
		t.Fatalf("安装：\n%s", r.dump())
	}
	ferry := filepath.Join(f.root, "ferry")
	dir := filepath.Join(ferry, "runtimes")
	pin := func(spec *proto.RuntimeSpec, stage string) run {
		return exec1(t, f, "runtime", &proto.Request{Stage: stage, FerryDir: ferry, Runtime: spec})
	}

	// 装过这个版本：只建链接
	r := pin(&proto.RuntimeSpec{ID: "jdk-17.0.13_11-x64", Java: 17, Version: "17.0.13+11", Pin: "java-17.0.13_11"}, "")
	if r.code != 0 || f.links[filepath.Join(dir, "java-17.0.13_11")] != "jdk-17.0.13_11-x64" {
		t.Fatalf("只建链接：%v\n%s", f.links, r.dump())
	}

	// 没装过的版本：上传、解开、建链接；java-17 还指向原来的
	p := exec1(t, f, "prepare", &proto.Request{})
	var prep proto.Prepared
	json.Unmarshal(p.result.Data, &prep)
	os.WriteFile(filepath.Join(prep.Stage, "jdk.tar.gz"), data, 0o644)
	h := sha256.Sum256(data)
	r = pin(&proto.RuntimeSpec{ID: "jdk-17.0.14_7-x64", Java: 17, Version: "17.0.14+7", Pin: "java-17.0.14_7",
		File: "jdk.tar.gz", SHA256: hex.EncodeToString(h[:]), Size: int64(len(data))}, prep.Stage)
	if r.code != 0 || f.links[filepath.Join(dir, "java-17.0.14_7")] != "jdk-17.0.14_7-x64" {
		t.Fatalf("上传并锁定：%v\n%s", f.links, r.dump())
	}
	if f.links[filepath.Join(dir, "java-17")] != "jdk-17.0.13_11-x64" {
		t.Fatalf("锁定版本不能动 java-17：%v", f.links)
	}

	a := &Agent{Sys: f, req: &proto.Request{Root: f.root, FerryDir: ferry}}
	jdks, pins := a.installedJDKs([]string{"java-17.0.13_11", "java-17.0.14_7", "java-17", "java-17.0.9_9"})
	if len(jdks) != 2 || pins["java-17.0.13_11"] != "jdk-17.0.13_11-x64" || pins["java-17.0.14_7"] != "jdk-17.0.14_7-x64" || pins["java-17"] != "" {
		t.Fatalf("installedJDKs：%+v %v", jdks, pins)
	}
	for _, j := range jdks {
		if j.ID == "jdk-17.0.14_7-x64" && j.Version != "17.0.14+7" {
			t.Fatalf("应记下完整版本：%+v", j)
		}
	}

	// 服务器上没有、又没上传：报错
	r = pin(&proto.RuntimeSpec{ID: "jdk-17.0.9_9-x64", Java: 17, Version: "17.0.9+9", Pin: "java-17.0.9_9"}, "")
	if r.code == 0 || !strings.Contains(r.result.Msg, "需要上传") {
		t.Fatalf("没有这个 JDK 应报错：\n%s", r.dump())
	}
	// 链接名不对：拒绝
	if r := pin(&proto.RuntimeSpec{ID: "jdk-17.0.13_11-x64", Java: 17, Pin: "../x"}, ""); r.code != proto.CodeBadInput {
		t.Fatalf("链接名无效应拒绝：%d", r.code)
	}
}
