package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
)

func runCLI(t *testing.T, env map[string]string, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	getenv := func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return os.Getenv(k)
	}
	code := run(args, &cliIO{ctx: context.Background(), stdout: &out, stderr: &errb, getenv: getenv})
	return out.String() + errb.String(), code
}

func TestConnections(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	defer appdir.Reset()

	out, code := runCLI(t, nil, "connection", "list", "--json")
	if code != 0 || !strings.Contains(out, `"connections":[]`) {
		t.Fatalf("空列表：%d %s", code, out)
	}
	out, code = runCLI(t, map[string]string{"FERRY_CONNECTION_VALUE": `{"title":"测试机","host":"10.0.0.5","port":2222,"user":"deploy","auth":"password"}`},
		"connection", "save", "--json")
	if code != 0 || !strings.Contains(out, `"name":"deploy-10.0.0.5"`) {
		t.Fatalf("保存：%d %s", code, out)
	}
	// 同一台再存一个：名字加序号
	out, _ = runCLI(t, map[string]string{"FERRY_CONNECTION_VALUE": `{"host":"10.0.0.5","port":2222,"user":"deploy"}`}, "connection", "save", "--json")
	if !strings.Contains(out, `"name":"deploy-10.0.0.5-2"`) {
		t.Fatalf("重名：%s", out)
	}
	// 修改第一个
	out, code = runCLI(t, map[string]string{"FERRY_CONNECTION_VALUE": `{"name":"deploy-10.0.0.5","title":"改名","host":"10.0.0.5","port":22,"user":"deploy","auth":"password"}`},
		"connection", "save", "--json")
	if code != 0 {
		t.Fatalf("修改：%s", out)
	}
	f, err := config.Load(connectionsPath())
	if err != nil {
		t.Fatal(err)
	}
	tg := f.Targets["deploy-10.0.0.5"]
	if tg == nil || tg.Title != "改名" || tg.Port != 22 || !tg.Plain || tg.Auth != "password" {
		t.Fatalf("文件内容：%+v", tg)
	}
	data, _ := os.ReadFile(connectionsPath())
	if strings.Contains(strings.ToLower(string(data)), "password:") {
		t.Fatalf("文件里不能有密码：%s", data)
	}
	// 只能开终端、管理文件
	out, code = runCLI(t, nil, "deploy", "deploy-10.0.0.5", "-c", connectionsPath(), "--json")
	if code == 0 || !strings.Contains(out, "只能打开终端") {
		t.Fatalf("应拒绝部署：%d %s", code, out)
	}
	// AI 助手可以用（这里没配接口地址，停在模型配置上）
	out, _ = runCLI(t, map[string]string{"FERRY_AI_PROVIDER": "openai", "FERRY_AI_KEY": "x"}, "ai", "deploy-10.0.0.5", "看看", "-c", connectionsPath(), "--json")
	if strings.Contains(out, "只能打开终端") {
		t.Fatalf("连接上应能用 AI 助手：%s", out)
	}
	out, code = runCLI(t, map[string]string{"FERRY_CONNECTION_VALUE": `{"host":""}`}, "connection", "save", "--json")
	if code == 0 || !strings.Contains(out, "请填写主机") {
		t.Fatalf("应要求主机：%s", out)
	}
	out, code = runCLI(t, nil, "connection", "remove", "deploy-10.0.0.5", "--json")
	if code != 0 {
		t.Fatalf("删除：%s", out)
	}
	out, _ = runCLI(t, nil, "connection", "list", "--json")
	if strings.Contains(out, `"name":"deploy-10.0.0.5"`) || !strings.Contains(out, `"name":"deploy-10.0.0.5-2"`) {
		t.Fatalf("删除后：%s", out)
	}
}
