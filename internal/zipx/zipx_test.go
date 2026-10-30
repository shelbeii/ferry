package zipx

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func innerJar(t *testing.T, name string, size int) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, _ := w.Create(name + "/A.class")
	data := make([]byte, size)
	rand.Read(data)
	f.Write(data)
	w.Close()
	return b.Bytes()
}

func fatJar(t *testing.T, path string, libs map[string][]byte, appClass []byte) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(out)
	m, _ := w.Create("META-INF/MANIFEST.MF")
	m.Write([]byte("Main-Class: org.springframework.boot.loader.launch.JarLauncher\n"))
	c, _ := w.Create("BOOT-INF/classes/App.class")
	c.Write(appClass)
	for _, name := range []string{"a.jar", "b.jar", "c.jar"} {
		data, ok := libs[name]
		if !ok {
			continue
		}
		h := &zip.FileHeader{Name: "BOOT-INF/lib/" + name, Method: zip.Store}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(data)
	}
	// 压缩存放的 jar 不能复用
	h := &zip.FileHeader{Name: "BOOT-INF/lib/deflated.jar", Method: zip.Deflate}
	f, _ := w.CreateHeader(h)
	f.Write(innerJar(t, "d", 100))
	w.Close()
	out.Close()
}

func TestSplitAssembleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	libs := map[string][]byte{"a.jar": innerJar(t, "a", 5000), "b.jar": innerJar(t, "b", 3000), "c.jar": innerJar(t, "c", 10)}
	jar := filepath.Join(dir, "app.jar")
	fatJar(t, jar, libs, []byte("v1"))

	cache := filepath.Join(dir, "cache")
	os.MkdirAll(cache, 0o755)
	// 服务器上已有旧版本：从中抽取依赖
	old := filepath.Join(dir, "old.jar")
	fatJar(t, old, map[string][]byte{"a.jar": libs["a.jar"]}, []byte("v0"))
	n, err := ExtractLibs(old, cache)
	if err != nil || n != 1 {
		t.Fatalf("ExtractLibs：%d %v", n, err)
	}
	if n2, _ := ExtractLibs(old, cache); n2 != 0 {
		t.Fatal("重复抽取应跳过")
	}

	have := map[string]bool{}
	ents, _ := os.ReadDir(cache)
	for _, e := range ents {
		have[e.Name()[:64]] = true
	}
	up := filepath.Join(dir, "up")
	os.MkdirAll(up, 0o755)
	st, err := Split(jar, filepath.Join(up, "plan"), filepath.Join(up, "pack"), up, have)
	if err != nil {
		t.Fatal(err)
	}
	if st.Libs != 3 || st.NewLibs != 2 || st.ReusedBytes != int64(len(libs["a.jar"])) {
		t.Fatalf("统计：%+v", st)
	}
	// 服务器收下新依赖
	ups, _ := os.ReadDir(up)
	for _, e := range ups {
		if filepath.Ext(e.Name()) == ".jar" {
			os.Rename(filepath.Join(up, e.Name()), filepath.Join(cache, e.Name()))
		}
	}
	got := filepath.Join(dir, "assembled.jar")
	err = Assemble(filepath.Join(up, "plan"), filepath.Join(up, "pack"), got, func(s string) string { return filepath.Join(cache, s+".jar") })
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := FileSHA256(jar)
	b, _, _ := FileSHA256(got)
	if a != b {
		t.Fatal("拼装结果与原文件不一致")
	}
	zr, err := zip.OpenReader(got)
	if err != nil {
		t.Fatal(err)
	}
	zr.Close()
}

func TestAssembleMissingLib(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "app.jar")
	fatJar(t, jar, map[string][]byte{"a.jar": innerJar(t, "a", 100)}, []byte("x"))
	st, err := Split(jar, filepath.Join(dir, "plan"), filepath.Join(dir, "pack"), dir, map[string]bool{})
	if err != nil || st.NewLibs != 1 {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.jar")
	err = Assemble(filepath.Join(dir, "plan"), filepath.Join(dir, "pack"), out, func(s string) string { return filepath.Join(dir, "nope", s) })
	if err == nil {
		t.Fatal("缺依赖应失败")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("失败时应删除半成品")
	}
}

// 用商城项目里真实构建出的 fat jar 做一次往返（没有就跳过）。
func TestRealJar(t *testing.T) {
	jar := os.Getenv("FERRY_TEST_JAR")
	if jar == "" {
		t.Skip("设置 FERRY_TEST_JAR 指向一个 Spring Boot fat jar")
	}
	dir := t.TempDir()
	st, err := Split(jar, filepath.Join(dir, "plan"), filepath.Join(dir, "pack"), dir, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", st)
	out := filepath.Join(dir, "out.jar")
	if err := Assemble(filepath.Join(dir, "plan"), filepath.Join(dir, "pack"), out, func(s string) string { return filepath.Join(dir, s+".jar") }); err != nil {
		t.Fatal(err)
	}
	a, _, _ := FileSHA256(jar)
	b, _, _ := FileSHA256(out)
	if a != b {
		t.Fatal("不一致")
	}
}
