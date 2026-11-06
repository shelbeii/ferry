package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/config"
)

// verify.minSize：目录产物按要上传的文件加起来算（不传的不算）。
func TestDirMinSize(t *testing.T) {
	web := t.TempDir()
	os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>0123456789"), 0o644) // 16 字节
	a := &config.App{Name: "web", Type: "static"}
	a.Artifact.Verify.MinSize = 10
	if _, err := Check(a, web, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	a.Artifact.Verify.MinSize = 100
	if _, err := Check(a, web, time.Time{}, ""); err == nil || !strings.Contains(err.Error(), "minSize 100") {
		t.Fatalf("前端目录太小要拦下：%v", err)
	}

	py := t.TempDir()
	os.WriteFile(filepath.Join(py, "main.py"), []byte("print(1)"), 0o644)
	os.MkdirAll(filepath.Join(py, ".venv"), 0o755)
	os.WriteFile(filepath.Join(py, ".venv", "big"), make([]byte, 4096), 0o644) // 默认不上传
	p := &config.App{Name: "api", Type: "python"}
	p.Artifact.Verify.MinSize = 1000
	if _, err := Check(p, py, time.Time{}, ""); err == nil || !strings.Contains(err.Error(), "一共只有 8 字节") {
		t.Fatalf(".venv 不上传，不算进去：%v", err)
	}
	p.Artifact.Verify.MinSize = 0
	if _, err := Check(p, py, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
}
