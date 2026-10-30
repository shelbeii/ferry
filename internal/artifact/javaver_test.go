package artifact

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/config"
)

// bootJar 最小的 Spring Boot 结构：MANIFEST 指向 Start-Class，主类的 class 文件版本为 major。
func bootJar(t *testing.T, major byte, startClass string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app.jar")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("META-INF/MANIFEST.MF")
	// 长类名按 MANIFEST 规范折行：下一行以空格开头
	mf := "Manifest-Version: 1.0\r\nMain-Class: org.springframework.boot.loader.launch.JarLauncher\r\nStart-Class: " + startClass + "\r\n"
	if len(mf) > 0 && strings.Contains(startClass, "VeryLong") {
		line := "Start-Class: " + startClass
		mf = "Manifest-Version: 1.0\r\nMain-Class: org.springframework.boot.loader.launch.JarLauncher\r\n" + line[:40] + "\r\n " + line[40:] + "\r\n"
	}
	w.Write([]byte(mf))
	w, _ = zw.Create("BOOT-INF/classes/" + strings.ReplaceAll(startClass, ".", "/") + ".class")
	w.Write([]byte{0xCA, 0xFE, 0xBA, 0xBE, 0, 0, 0, major, 0, 0})
	w, _ = zw.Create("BOOT-INF/lib/x.jar")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	return p
}

func TestJarJavaVersion(t *testing.T) {
	a := &config.App{Name: "a", Type: "java", Artifact: config.Artifact{Verify: config.Verify{SpringBoot: &config.SpringBootVerify{}}}}
	info, err := Check(a, bootJar(t, 61, "com.example.Demo"), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if info.ClassMajor != 61 || JavaOf(info.ClassMajor) != 17 || !strings.Contains(strings.Join(info.Notes, ","), "需要 Java 17+") {
		t.Fatalf("%+v", info)
	}
	info, err = Check(a, bootJar(t, 52, "com.example.some.VeryLongPackageName.DemoApplication"), time.Time{}, "")
	if err != nil || info.ClassMajor != 52 {
		t.Fatalf("折行的 MANIFEST：%+v %v", info, err)
	}
}
