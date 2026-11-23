package engine

import (
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

// 部署可选项在确认框里的说明：全量上传、失败不自动回滚、不改权限、这次保留的文件。
func TestDeployOptionNotes(t *testing.T) {
	if got := DeployOptionNotes(DeployOptions{}); len(got) != 0 {
		t.Fatalf("没选项时不该有说明：%v", got)
	}
	notes := strings.Join(DeployOptionNotes(DeployOptions{
		Full: true, NoRollback: true, KeepPerms: true,
		Keep: map[string][]*config.KeepItem{"api": {{Path: "config.js"}, {Path: "uploads", Shared: true}}},
	}), "\n")
	for _, want := range []string{"全量上传（不用增量）", "不自动回滚", "不自动修正", `"config.js"`, `"shared":true`} {
		if !strings.Contains(notes, want) {
			t.Errorf("缺少 %q：%s", want, notes)
		}
	}
}
