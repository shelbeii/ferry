package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/settings"
	"github.com/zalando/go-keyring"
)

func TestSettingsTransactionProtocol(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	t.Setenv("FERRY_KEYRING_SERVICE", "ferry-settings-protocol-test")
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	runSettings := func(raw string, args ...string) (string, int) {
		t.Helper()
		var out, stderr bytes.Buffer
		getenv := func(k string) string {
			if k == "FERRY_SETTINGS_VALUE" {
				return `{"ui":{"theme":"dark"}}`
			}
			if k == "FERRY_SETTINGS_SECRETS" {
				return raw
			}
			return os.Getenv(k)
		}
		code := run(args, &cliIO{ctx: context.Background(), stdout: &out, stderr: &stderr, getenv: getenv})
		return out.String() + stderr.String(), code
	}
	const value = "ONLY-TEST-KEY-PRIVATE-42"
	changes, _ := json.Marshal([]settings.SecretChange{{Kind: "ai", ID: "m1", Value: value}})
	out, code := runSettings(string(changes), "settings", "save", "--with-secrets", "--json")
	var result struct {
		OK   bool `json:"ok"`
		Data struct {
			Settings settings.Settings `json:"settings"`
			Version  int               `json:"secretsTransaction"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(out), &result) != nil || code != 0 || !result.OK || result.Data.Settings.UI.Theme != "dark" || result.Data.Version != 1 {
		t.Fatalf("批量保存协议失败，退出码 %d", code)
	}
	if strings.Contains(out, value) {
		t.Fatal("返回值不应包含凭据")
	}
	got, ok, err := secret.Get(secret.AIModelKeyAccount("m1"))
	if err != nil || !ok || got != value {
		t.Fatal("凭据未保存")
	}
	err = filepath.WalkDir(appdir.Dir(), func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(b, []byte(value)) {
				t.Fatal("凭据不能写入设置、索引或日志")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", `[{"kind":"ai","value":"` + value + `"`, `[{"kind":"ai","id":"m1","value":"` + value + `"},{"kind":"dns","id":"aliyun","value":"incomplete"}]`} {
		out, code := runSettings(raw, "settings", "save", "--with-secrets", "--json")
		if code == 0 || strings.Contains(out, value) {
			t.Fatal("无效批量请求应失败且不能输出敏感值")
		}
	}
}
