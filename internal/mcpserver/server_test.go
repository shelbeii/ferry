package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const shopYAML = `version: 1
project: shop
targets:
  prod:
    host: 127.0.0.1
    port: 1
    root: /opt/shop
    runAs: shop
    production: true
  test:
    host: 127.0.0.1
    port: 1
    root: /opt/shop
    runAs: shop
apps:
  - name: api
    type: java
    artifact: {path: target/api.jar}
`

func writeProject(t *testing.T, dir, yaml string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "ferry.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// connect 起一个 MCP 服务，用内存里的传输连上它。
func connect(t *testing.T, dir string) *mcp.ClientSession {
	t.Helper()
	srv, closeSrv := New(Options{Version: "test", Dir: dir})
	t.Cleanup(closeSrv)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s：%v", name, err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), r.IsError
}

func TestMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	root := t.TempDir()
	shop := writeProject(t, filepath.Join(root, "shop"), shopYAML)
	blog := writeProject(t, filepath.Join(root, "blog"), strings.ReplaceAll(shopYAML, "project: shop", "project: blog"))
	broken := writeProject(t, filepath.Join(root, "broken"), "version: 1\nproject: [\n")
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}, {Path: filepath.Dir(blog), Name: "博客"}, {Path: broken, Name: "坏的"}}
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}

	cs := connect(t, filepath.Join(root, "shop", "src"))
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	changes := map[string]bool{"deploy": true, "app_control": true, "rollback": true, "software_control": true, "port_control": true, "create_project": true, "ssh_disconnect": true, "firewall_enable": true, "backup_run": true,
		"run_change_command": true, "write_file": true, "software_install": true, "save_software_template": true, "ask_secret": true,
		"software_upgrade": true, "software_restore": true, "software_backup": true}
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.Annotations == nil || tl.Annotations.ReadOnlyHint == changes[tl.Name] {
			t.Errorf("%s 的只读标记不对", tl.Name)
		}
	}
	if got := strings.Join(names, ","); got != "app_control,app_logs,app_releases,app_status,ask_secret,backup_run,backup_status,build_log,create_project,deploy,deploy_changes,deploy_history,detect_project,firewall_enable,list_projects,monitor_status,port_check,port_control,preflight,rollback,run_change_command,run_command,save_software_template,server_check,server_ports,server_software,software_backup,software_control,software_install,software_logs,software_restore,software_templates,software_upgrade,ssh_disconnect,ssh_sessions,task_status,template_guide,write_file" {
		t.Fatalf("工具：%s", got)
	}

	text, isErr := callText(t, cs, "list_projects", nil)
	var list []projectInfo
	if isErr || json.Unmarshal([]byte(text), &list) != nil || len(list) != 3 {
		t.Fatalf("list_projects：%v %s", isErr, text)
	}
	if list[0].Name != "商城" || !list[0].Here || len(list[0].Targets) != 2 || !list[0].Targets[0].Production || list[0].Apps[0].Name != "api" {
		t.Errorf("商城：%+v", list[0])
	}
	if list[1].Config != blog || list[1].Here { // 列表里写的是目录：找到其中的 ferry.yaml
		t.Errorf("博客：%+v", list[1])
	}
	if list[2].Error == "" {
		t.Errorf("坏的配置应该带上错误：%+v", list[2])
	}

	// 不填 project：用当前目录的；不填 target：有两个目标时要求指定
	if text, isErr := callText(t, cs, "app_status", nil); !isErr || !strings.Contains(text, "target") || !strings.Contains(text, "prod") {
		t.Errorf("没指定目标：%v %s", isErr, text)
	}
	if text, isErr := callText(t, cs, "app_status", map[string]any{"project": "没有的", "target": "prod"}); !isErr || !strings.Contains(text, "博客") {
		t.Errorf("没有的项目：%v %s", isErr, text)
	}
	if text, isErr := callText(t, cs, "app_logs", map[string]any{"project": "博客", "target": "test"}); !isErr || !strings.Contains(text, "app") {
		t.Errorf("没指定应用：%v %s", isErr, text)
	}
	if text, isErr := callText(t, cs, "app_status", map[string]any{"project": "坏的", "target": "x"}); !isErr || !strings.Contains(text, "坏的") {
		t.Errorf("坏的配置：%v %s", isErr, text)
	}
	// 按名称找到项目、连接服务器（端口 1 连不上）：返回错误，不是协议错误
	if text, isErr := callText(t, cs, "app_status", map[string]any{"project": "blog", "target": "test"}); !isErr || text == "" {
		t.Errorf("连不上：%v %s", isErr, text)
	}

	// 每次调用都有记录，带上编程软件的名称（参数不合格式、没进到工具里的不算）
	b, err := os.ReadFile(LogFile())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], `"client":"test-client 1.0"`) || !strings.Contains(lines[0], `"tool":"list_projects"`) ||
		!strings.Contains(lines[4], `"ok":false`) {
		t.Errorf("调用记录：\n%s", b)
	}
}

func TestResolveSingleProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	shop := writeProject(t, filepath.Join(t.TempDir(), "shop"), shopYAML)
	st := settings.Defaults()
	st.Desktop.Projects = []settings.Project{{Path: shop, Name: "商城"}}
	settings.Save(st)

	s := &server{o: Options{Dir: t.TempDir()}}
	f, err := s.resolve("") // 只有一个项目：不填也行
	if err != nil || f.Project != "shop" {
		t.Fatalf("%v %v", f, err)
	}
	if f, err := s.resolve(filepath.Dir(shop)); err != nil || f.Project != "shop" { // 目录
		t.Fatalf("%v %v", f, err)
	}
	if f, err := s.resolve("SHOP"); err != nil || f.Project != "shop" { // 配置里的项目名，不分大小写
		t.Fatalf("%v %v", f, err)
	}
}

func TestRedactJSON(t *testing.T) {
	v := map[string]any{"lines": []string{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456", "password=Secr3tPw"}, "n": 2}
	out := redactJSON(v)
	var back map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("不是合法的 JSON：%v\n%s", err, out)
	}
	if strings.Contains(out, "Secr3tPw") || strings.Contains(out, "abcdefghij") || back["n"] != float64(2) {
		t.Fatalf("%s", out)
	}
}
