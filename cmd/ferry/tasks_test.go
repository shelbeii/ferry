package main

import (
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
)

func TestTaskHistory(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	defer appdir.Reset()

	save := func(v string) {
		t.Helper()
		if out, code := runCLI(t, map[string]string{"FERRY_TASK_VALUE": v}, "task-history", "save", "--json"); code != 0 {
			t.Fatalf("保存：%s", out)
		}
	}
	save(`{"id":"task0001-a","title":"部署 prod","status":"running","startedAt":1000,"lines":[{"kind":"step","msg":"构建"}]}`)
	save(`{"id":"task0001-b","title":"重启 prod","status":"ok","startedAt":2000,"endedAt":3000}`)

	// 桌面版启动时读列表：上次没跑完的改成中断
	out, code := runCLI(t, nil, "task-history", "list", "--interrupt", "--json")
	if code != 0 || !strings.Contains(out, `"count":2`) || !strings.Contains(out, `"status":"interrupted"`) {
		t.Fatalf("列表：%d %s", code, out)
	}
	out, code = runCLI(t, nil, "task-history", "show", "task0001-a", "--json")
	if code != 0 || !strings.Contains(out, `"msg":"构建"`) || !strings.Contains(out, `"status":"interrupted"`) {
		t.Fatalf("打开：%d %s", code, out)
	}
	// 清除已完成
	out, code = runCLI(t, nil, "task-history", "delete", "task0001-a", "task0001-b", "--json")
	if code != 0 || !strings.Contains(out, `"removed":2`) || !strings.Contains(out, `"count":0`) {
		t.Fatalf("删除：%d %s", code, out)
	}
	// 按设置清理：最多保留 5 个
	for _, id := range []string{"task0002-1", "task0002-2", "task0002-3", "task0002-4", "task0002-5", "task0002-6", "task0002-7"} {
		save(`{"id":"` + id + `","title":"x","status":"ok","startedAt":1}`)
	}
	out, code = runCLI(t, nil, "task-history", "clean", "--keep", "5", "--json")
	if code != 0 || !strings.Contains(out, `"removed":2`) || !strings.Contains(out, `"count":5`) {
		t.Fatalf("清理：%d %s", code, out)
	}
	out, code = runCLI(t, nil, "task-history", "clean", "--all", "--json")
	if code != 0 || !strings.Contains(out, `"count":0`) {
		t.Fatalf("全部删除：%d %s", code, out)
	}
}
