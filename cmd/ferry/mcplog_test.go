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

func TestMCPLogChannelAndDownloadsAndExecCLI(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	defer appdir.Reset()

	// 1. mcp-log --channel 筛选
	now := time.Now()
	lines := []map[string]any{
		{"time": now, "tool": "restart（结果）", "op": "op1", "args": map[string]any{"channel": "mcp-app"}, "decision": "同意", "ok": true},
		{"time": now, "tool": "deploy（结果）", "op": "op2", "args": map[string]any{"channel": "desktop"}, "decision": "同意", "ok": true},
	}
	var sb strings.Builder
	for _, ln := range lines {
		b, _ := json.Marshal(ln)
		sb.Write(append(b, '\n'))
	}
	os.WriteFile(mcpserver.LogFile(), []byte(sb.String()), 0o600)
	out, code := runCLI(t, nil, "mcp-log", "--channel", "mcp-app", "--json")
	if code != 0 || !strings.Contains(out, `"total":1`) || !strings.Contains(out, `"op":"op1"`) || strings.Contains(out, `"op":"op2"`) {
		t.Fatalf("mcp-log --channel 筛选异常：%d %s", code, out)
	}

	// 2. mcp-downloads status / clean
	dlOld := filepath.Join(mcpserver.DownloadsDir(), "20250101-000001")
	dlNew := filepath.Join(mcpserver.DownloadsDir(), "20260101-000002")
	os.MkdirAll(dlOld, 0o700)
	os.MkdirAll(dlNew, 0o700)
	os.WriteFile(filepath.Join(dlOld, "a.log"), []byte("123456"), 0o600)
	os.WriteFile(filepath.Join(dlNew, "b.log"), []byte("1234567890"), 0o600)
	oldTime := time.Now().AddDate(0, 0, -45)
	os.Chtimes(dlOld, oldTime, oldTime)

	out, code = runCLI(t, nil, "mcp-downloads", "status", "--json")
	if code != 0 || !strings.Contains(out, `"count":2`) || !strings.Contains(out, `"bytes":16`) {
		t.Fatalf("mcp-downloads status 异常：%d %s", code, out)
	}
	out, code = runCLI(t, nil, "mcp-downloads", "clean", "--days", "30", "--json")
	if code != 0 || !strings.Contains(out, `"removed":1`) || !strings.Contains(out, `"count":1`) {
		t.Fatalf("mcp-downloads clean --days 异常：%d %s", code, out)
	}
	out, code = runCLI(t, nil, "mcp-downloads", "clean", "--all", "--json")
	if code != 0 || !strings.Contains(out, `"removed":1`) || !strings.Contains(out, `"count":0`) {
		t.Fatalf("mcp-downloads clean --all 异常：%d %s", code, out)
	}

	// 3. mcp-exec 统一执行器：create 与 config-edit
	execDir := t.TempDir()
	cfgPath := filepath.Join(execDir, "ferry.yaml")
	createReq, _ := json.Marshal(map[string]any{
		"action":  "create",
		"project": "execproj",
		"config":  cfgPath,
		"spec": map[string]any{
			"projectDir": filepath.ToSlash(execDir),
			"project":    "execproj",
			"targets":    []map[string]any{{"name": "test", "host": "127.0.0.1", "port": 22, "user": "root", "root": "/opt/execproj", "runAs": "demo"}},
			"apps":       []map[string]any{{"name": "web", "type": "static", "artifact": "dist"}},
		},
	})
	out, code = runCLI(t, map[string]string{"FERRY_MCP_REQ": string(createReq)}, "mcp-exec", "-c", cfgPath, "--json")
	if code != 0 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("mcp-exec create 失败：%d %s", code, out)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("mcp-exec create 未写入文件：%v", err)
	}

	// 未开启 allowChanges 时 mcp-exec config-edit 必须被拒
	editReq, _ := json.Marshal(map[string]any{
		"action": "config-edit",
		"config": cfgPath,
		"input":  map[string]any{"edits": []map[string]any{{"op": "set", "path": "targets.test.host", "value": "10.0.0.99"}}},
	})
	if out, code = runCLI(t, map[string]string{"FERRY_MCP_REQ": string(editReq)}, "mcp-exec", "-c", cfgPath, "--json"); code == 0 {
		t.Fatalf("未开启 allowChanges 时 mcp-exec config-edit 应失败：%s", out)
	}
	// 开启后应正常写入
	os.WriteFile(appdir.SettingsFile(), []byte(`{"mcp":{"allowChanges":true}}`), 0o600)
	if out, code = runCLI(t, map[string]string{"FERRY_MCP_REQ": string(editReq)}, "mcp-exec", "-c", cfgPath, "--json"); code != 0 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("mcp-exec config-edit 失败：%d %s", code, out)
	}
	b, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(b), "10.0.0.99") {
		t.Fatalf("mcp-exec config-edit 未落盘：%s", b)
	}
}
