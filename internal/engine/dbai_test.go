package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
)

// Ferry 的 AI 助手只拿到经当前服务器连、允许 AI 查询的连接；没有时不给数据库工具，也不在提示里说。
func TestAIDBToolsOnTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	defer appdir.Reset()
	cfg := filepath.Join(home, "shop", "ferry.yaml")
	other := filepath.Join(home, "blog", "ferry.yaml")
	conns := `{"connections":[
 {"id":"a","name":"商城库","kind":"mysql","host":"127.0.0.1","port":3306,"user":"root","database":"shop","ai":true,"aiWrite":true,"via":{"config":"` + filepath.ToSlash(cfg) + `","target":"prod"}},
 {"id":"b","name":"没允许","kind":"mysql","host":"127.0.0.1","port":3306,"user":"root","via":{"config":"` + filepath.ToSlash(cfg) + `","target":"prod"}},
 {"id":"c","name":"别的服务器","kind":"mysql","host":"127.0.0.1","port":3306,"user":"root","ai":true,"via":{"config":"` + filepath.ToSlash(cfg) + `","target":"test"}},
 {"id":"d","name":"别的项目","kind":"postgres","host":"127.0.0.1","port":5432,"user":"postgres","ai":true,"via":{"config":"` + filepath.ToSlash(other) + `","target":"prod"}},
 {"id":"e","name":"直接连","kind":"mysql","host":"10.0.0.5","port":3306,"user":"root","ai":true}
]}`
	os.MkdirAll(filepath.Join(home, "db"), 0o700)
	if err := os.WriteFile(filepath.Join(home, "db", "connections.json"), []byte(conns), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{File: &config.File{Path: cfg, Project: "shop"}}
	prod := &config.Target{Name: "prod", User: "root"}
	tools := e.aiDBTools(prod, false)
	if len(tools) != 3 {
		t.Fatalf("应该有 3 个数据库工具：%d", len(tools))
	}
	out, err := tools[0].Run(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "商城库") || strings.Contains(out, "没允许") || strings.Contains(out, "别的服务器") || strings.Contains(out, "别的项目") || strings.Contains(out, "直接连") {
		t.Fatalf("连接不对：%s", out)
	}
	if _, err := tools[2].Run(t.Context(), []byte(`{"connection":"别的项目","sql":"SELECT 1"}`)); err == nil || !strings.Contains(err.Error(), "商城库") {
		t.Fatalf("别的服务器上的连接不能用：%v", err)
	}
	if _, err := tools[2].Run(t.Context(), []byte(`{"sql":"DELETE FROM x"}`)); err == nil || !strings.Contains(err.Error(), "只能执行查询") {
		t.Fatalf("应该先拒绝改动的语句（不用连数据库）：%v", err)
	}
	if !e.hasAIDB(prod) { // 提示里据此说能查数据库
		t.Fatal("提示里应该说能查数据库")
	}
	if e.aiDBTools(&config.Target{Name: "dev", User: "root"}, true) != nil {
		t.Fatal("没有连接的服务器不该有数据库工具")
	}
	if e.hasAIDB(&config.Target{Name: "dev", User: "root"}) {
		t.Fatal("没有连接时提示里不该说")
	}

	// 允许改动时多一个 db_execute：每次都要确认，危险的标出来；不合规的不打扰用户，执行时报错
	ct := e.aiDBTools(prod, true)
	if len(ct) != 4 || ct[3].Name != "db_execute" || ct[3].Approve == nil {
		t.Fatalf("改动的工具：%d", len(ct))
	}
	ap := ct[3].Approve(t.Context(), []byte(`{"sql":"UPDATE users SET vip = 1"}`))
	if ap == nil || ap.Once == "" || !strings.Contains(ap.Danger, "没写 WHERE") || !strings.Contains(ap.Detail, "UPDATE users SET vip = 1;") ||
		!strings.Contains(ap.Detail, "库：shop") || !strings.Contains(ap.Title, "1 条语句") {
		t.Fatalf("确认：%+v", ap)
	}
	if ct[3].Approve(t.Context(), []byte(`{"sql":"GRANT ALL ON *.* TO x"}`)) != nil {
		t.Fatal("不合规的不用问用户")
	}
	if _, err := ct[3].Run(t.Context(), []byte(`{"sql":"GRANT ALL ON *.* TO x"}`)); err == nil || !strings.Contains(err.Error(), "GRANT") {
		t.Fatalf("不合规的执行时报错：%v", err)
	}

	// 编程软件（MCP）看到全部允许的，按 ID 或名称找
	list, hidden, err := AIDBConns()
	if err != nil || len(list) != 4 || hidden != 1 {
		t.Fatalf("允许的连接：%v %d %d", err, len(list), hidden)
	}
	if _, err := AIDBFind("没允许"); err == nil || !strings.Contains(err.Error(), "允许 AI 查询") {
		t.Fatalf("没允许的：%v", err)
	}
	if c, err := AIDBFind("e"); err != nil || c.Name != "直接连" {
		t.Fatalf("按 ID 找：%v", err)
	}
}
