package proto

import (
	"os"
	"strings"
	"testing"
)

// 桌面版的类型名（desktop/src/ferry.ts 的 AUDIT_GROUPS）和这里的一样，每种都有
func TestAuditKindsMatchDesktop(t *testing.T) {
	b, err := os.ReadFile("../../desktop/src/ferry.ts")
	if err != nil {
		t.Skip(err)
	}
	src := string(b)
	i := strings.Index(src, "AUDIT_GROUPS")
	if i < 0 {
		t.Fatal("ferry.ts 里没有 AUDIT_GROUPS")
	}
	src = src[i:]
	for k, v := range AuditKinds {
		key := k
		if strings.Contains(k, "-") {
			key = "'" + k + "'"
		}
		if !strings.Contains(src, key+": '"+v+"'") {
			t.Errorf("ferry.ts 的 AUDIT_GROUPS 里没有 %s: '%s'", key, v)
		}
	}
}
