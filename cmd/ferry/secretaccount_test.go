package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/secret"
	"github.com/zalando/go-keyring"
)

func TestSavedCredentialView(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	t.Setenv("FERRY_KEYRING_SERVICE", "ferry-credential-view-test")
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	// 全程使用内存凭据库，不读取或改动这台电脑上真实保存的凭据。
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	pool := engine.NewPool(0)
	defer pool.Close()
	runView := func(args ...string) (string, int) {
		t.Helper()
		var out, stderr bytes.Buffer
		code := run(args, &cliIO{ctx: context.Background(), pool: pool, stdout: &out, stderr: &stderr, getenv: os.Getenv})
		return out.String() + stderr.String(), code
	}
	const value = "test-only password: \"引号\" \\ & < >\n第二行"
	for _, account := range []string{"password:root@192.0.2.1:22", "passphrase:/keys/id_ed25519", "software:minio@192.0.2.1:22", "ai:api-key:example", "notify:url:example", "dns:cloudflare:token"} {
		if err := secret.Set(account, value); err != nil {
			t.Fatal(err)
		}
		out, code := runView("secret", "show", "--account", account)
		var result struct {
			OK   bool `json:"ok"`
			Data struct {
				Value string `json:"value"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil || code != 0 || !result.OK || result.Data.Value != value {
			t.Fatalf("读取测试凭据失败：%s（退出码 %d）", account, code)
		}
		// 列表接口和磁盘索引必须仍然只有账户信息。
		out, code = runView("secret", "list")
		if code != 0 || strings.Contains(out, "test-only") || strings.Contains(out, `"value"`) {
			t.Fatal("凭据内容不应出现在列表里")
		}
	}
	index, err := os.ReadFile(appdir.Path("secrets-index.json"))
	if err != nil || bytes.Contains(index, []byte("test-only")) {
		t.Fatal("索引只应保存账户，不能保存凭据内容")
	}
	for _, args := range [][]string{
		{"secret", "show"},
		{"secret", "show", "--account", "missing"},
		{"secret", "show", "unexpected", "--account", "ai:api-key:example"},
		{"secret", "--account", "ai:api-key:example"},
		{"secret", "set", "--account", "ai:api-key:example"},
		{"secret", "list", "--account", "ai:api-key:example"},
	} {
		out, code := runView(args...)
		if code == 0 || strings.Contains(out, "test-only") {
			t.Fatalf("无效请求不应读出凭据：%v", args)
		}
	}
	// 查看功能只通过桌面版 sidecar 响应，不把值输出到普通命令行。
	if out, code := runCLI(t, nil, "secret", "show", "--account", "ai:api-key:example", "--json"); code == 0 || strings.Contains(out, "test-only") {
		t.Fatal("普通命令行不应输出凭据")
	}
	keyring.MockInitWithError(errors.New("系统凭据库已锁定"))
	if out, code := runView("secret", "show", "--account", "ai:api-key:example"); code == 0 || !strings.Contains(out, "系统凭据库已锁定") {
		t.Fatal("读取失败应返回明确的错误")
	}
}
