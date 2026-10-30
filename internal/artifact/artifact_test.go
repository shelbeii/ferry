package artifact

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/config"
)

func TestStaticPack(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("x"), 0o644)
	a := &config.App{Name: "web", Type: "static"}
	a.Artifact.Verify.Static = &config.StaticVerify{Index: "index.html"}
	info, err := Check(a, dir, time.Time{}, "")
	if err != nil || info.Files != 2 {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := Check(a, dir, time.Now().Add(time.Hour), ""); err == nil {
		t.Fatal("旧产物应报错")
	}
	out := filepath.Join(t.TempDir(), "web.tar.gz")
	if err := PackDir(dir, out); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "assets/,assets/app.js,index.html" {
		t.Fatal(names)
	}
	a.Artifact.Verify.Static.Index = "missing.html"
	if _, err := Check(a, dir, time.Time{}, ""); err == nil {
		t.Fatal("缺 index 应报错")
	}
}

func TestNotELF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	if _, err := Check(&config.App{Type: "binary"}, p, time.Time{}, "amd64"); err == nil {
		t.Fatal("非 ELF 应报错")
	}
}

func TestRealSpringBootJar(t *testing.T) {
	jar := os.Getenv("FERRY_TEST_JAR")
	if jar == "" {
		t.Skip("设置 FERRY_TEST_JAR")
	}
	a := &config.App{Type: "java"}
	a.Artifact.Verify.SpringBoot = &config.SpringBootVerify{RequireLibs: []string{"ShopMall-common-core-*.jar"}, NonEmptyLibs: true}
	info, err := Check(a, jar, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(info.Notes)
	a.Artifact.Verify.SpringBoot.RequireLibs = []string{"not-there-*.jar"}
	if _, err := Check(a, jar, time.Time{}, ""); err == nil {
		t.Fatal("缺依赖应报错")
	}
}
