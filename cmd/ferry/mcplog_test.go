package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/mcpserver"
)

func TestMCPLog(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	defer appdir.Reset()

	var b strings.Builder
	for i := range 25 {
		at := time.Now().Add(time.Duration(i) * time.Minute)
		if i < 5 {
			at = time.Now().AddDate(0, 0, -40)
		}
		e, _ := json.Marshal(map[string]any{"time": at, "tool": fmt.Sprintf("t%d", i), "ok": true})
		b.Write(append(e, '\n'))
	}
	os.WriteFile(mcpserver.LogFile(), []byte(b.String()), 0o600)

	// 分页：新的在前
	out, code := runCLI(t, nil, "mcp-log", "--offset", "20", "--limit", "10", "--json")
	if code != 0 || !strings.Contains(out, `"total":25`) || !strings.Contains(out, `"tool":"t4"`) || strings.Contains(out, `"tool":"t5"`) {
		t.Fatalf("分页：%d %s", code, out)
	}
	// 筛选：工具、成功与否、关键字、时间（本地时间写法）
	out, code = runCLI(t, nil, "mcp-log", "--tool", "t7,t8", "--result", "ok", "--q", "T8", "--since", time.Now().AddDate(0, 0, -1).Format("2006-01-02 15:04"), "--changes", "--json")
	if code != 0 || !strings.Contains(out, `"total":0`) {
		t.Fatalf("筛选（只看改动时没有）：%d %s", code, out)
	}
	out, code = runCLI(t, nil, "mcp-log", "--tool", "t7,t8", "--q", "T8", "--json")
	if code != 0 || !strings.Contains(out, `"total":1`) || !strings.Contains(out, `"tool":"t8"`) || !strings.Contains(out, `"tools":["t0"`) {
		t.Fatalf("筛选：%d %s", code, out)
	}
	if out, code = runCLI(t, nil, "mcp-log", "--since", "昨天", "--json"); code == 0 {
		t.Fatalf("时间写错应报错：%s", out)
	}
	// 按表单上的天数清理
	out, code = runCLI(t, nil, "mcp-log", "clean", "--days", "30", "--json")
	if code != 0 || !strings.Contains(out, `"removed":5`) || !strings.Contains(out, `"count":20`) {
		t.Fatalf("清理：%d %s", code, out)
	}
	out, code = runCLI(t, nil, "mcp-log", "clean", "--all", "--json")
	if code != 0 || !strings.Contains(out, `"count":0`) {
		t.Fatalf("全部删除：%d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(appdir.Dir(), "mcp", "calls.jsonl")); err == nil {
		t.Fatal("全部删除后记录文件还在")
	}
}
