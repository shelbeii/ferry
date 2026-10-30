package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows 上对话记录的目录只有当前用户和 SYSTEM 能访问，而且自己照样能写
func TestMakePrivateWindows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ai")
	c := &Conversation{ID: NewID(), Messages: []Message{{Role: "user", Text: "hi"}}}
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	if !strings.Contains(s, "D:P") || strings.Contains(s, ";;;BU)") || strings.Contains(s, ";;;WD)") || strings.Contains(s, ";;;AU)") {
		t.Fatalf("权限：%s", s)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("自己应能写：%v", err)
	}
}
