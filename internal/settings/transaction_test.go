package settings

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/secret"
	"github.com/zalando/go-keyring"
)

func settingsTransactionHome(t *testing.T) {
	t.Helper()
	t.Setenv("FERRY_HOME", t.TempDir())
	t.Setenv("FERRY_KEYRING_SERVICE", "ferry-settings-save-test")
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
}

func TestSettingsCredentialChangeValidation(t *testing.T) {
	for _, changes := range [][]SecretChange{
		{{Kind: "ai", ID: "m1", Value: ""}},
		{{Kind: "other", ID: "m1", Value: "private"}},
		{{Kind: "ai", ID: "BAD ID", Value: "private"}},
		{{Kind: "ai", ID: "m1", Remove: true, Value: "private"}},
		{{Kind: "ai", ID: "m1", Value: "private", AccessID: "wrong"}},
		{{Kind: "notify-url", ID: "n1", Value: "invalid-url-private"}},
		{{Kind: "dns", ID: "aliyun", Value: "private"}},
		{{Kind: "dns", ID: "unsupported", Value: "private"}},
		{{Kind: "ai", ID: "m1", Value: "private"}, {Kind: "ai", ID: "m1", Remove: true}},
	} {
		if _, err := secretChanges(changes); err == nil {
			t.Fatal("无效变更应在任何写入前被拒绝")
		}
	}
	changes, err := secretChanges([]SecretChange{{Kind: "dns", ID: "aliyun", Value: " secret ", AccessID: " id "}, {Kind: "dns", ID: "cloudflare", Remove: true}})
	want := []secret.Change{{Account: secret.DNSAccount("aliyun", "id"), Value: "id"}, {Account: secret.DNSAccount("aliyun", "secret"), Value: "secret"}, {Account: secret.DNSAccount("cloudflare", "token"), Delete: true}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("DNS 配对变更：%v", err)
	}
}

func TestSettingsAndCredentialsCommitTogether(t *testing.T) {
	settingsTransactionHome(t)
	if err := secret.Set(secret.AIKeyAccount, "legacy-private"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(secret.NotifySignAccount("gone"), "old-sign-private"); err != nil {
		t.Fatal(err)
	}
	next := Defaults()
	next.UI.Theme = "dark"
	next.Connect.TimeoutSec = -10
	saved, err := SaveWithSecrets(next, []SecretChange{
		{Kind: "ai", ID: LegacyModelID, Value: "new-ai-private"},
		{Kind: "notify-url", ID: "n1", Value: "https://example.test/private"},
		{Kind: "notify-sign", ID: "gone", Remove: true},
		{Kind: "dns", ID: "dnspod", AccessID: "dns-id-private", Value: "dns-secret-private"},
	})
	if err != nil || saved.UI.Theme != "dark" || saved.Connect.TimeoutSec != 15 {
		t.Fatalf("保存没有返回规范化结果：%v", err)
	}
	loaded, err := Load()
	if err != nil || loaded.UI.Theme != "dark" {
		t.Fatal("设置没有写入")
	}
	for account, want := range map[string]string{secret.AIModelKeyAccount(LegacyModelID): "new-ai-private", secret.NotifyURLAccount("n1"): "https://example.test/private", secret.DNSAccount("dnspod", "id"): "dns-id-private", secret.DNSAccount("dnspod", "secret"): "dns-secret-private"} {
		got, ok, err := secret.Get(account)
		if err != nil || !ok || got != want {
			t.Fatal("凭据未提交")
		}
	}
	for _, account := range []string{secret.AIKeyAccount, secret.NotifySignAccount("gone")} {
		if _, ok, err := secret.Get(account); err != nil || ok {
			t.Fatal("旧凭据没有删除")
		}
	}
	b, _ := os.ReadFile(appdir.SettingsFile())
	if bytes.Contains(b, []byte("private")) {
		t.Fatal("设置文件不能包含批量凭据")
	}
}

func TestSettingsFileFailureRollsBackAllCredentials(t *testing.T) {
	settingsTransactionHome(t)
	if err := Save(Defaults()); err != nil {
		t.Fatal(err)
	}
	for account, value := range map[string]string{secret.AIKeyAccount: "legacy-private", secret.AIModelKeyAccount(LegacyModelID): "old-ai-private", secret.NotifyURLAccount("n1"): "https://example.test/old-private"} {
		if err := secret.Set(account, value); err != nil {
			t.Fatal(err)
		}
	}
	originalSettings, _ := os.ReadFile(appdir.SettingsFile())
	originalIndex, _ := os.ReadFile(appdir.Path("secrets-index.json"))
	if err := os.Mkdir(appdir.SettingsFile()+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	next := Defaults()
	next.UI.Theme = "dark"
	_, err := SaveWithSecrets(next, []SecretChange{{Kind: "ai", ID: LegacyModelID, Value: "new-ai-private"}, {Kind: "notify-url", ID: "n1", Remove: true}, {Kind: "dns", ID: "cloudflare", Value: "new-token-private"}})
	if err == nil {
		t.Fatal("应报告设置写入失败")
	}
	settingsAfter, _ := os.ReadFile(appdir.SettingsFile())
	indexAfter, _ := os.ReadFile(appdir.Path("secrets-index.json"))
	if !bytes.Equal(originalSettings, settingsAfter) || !bytes.Equal(originalIndex, indexAfter) {
		t.Fatal("设置和凭据索引应完整保留")
	}
	for account, want := range map[string]string{secret.AIKeyAccount: "legacy-private", secret.AIModelKeyAccount(LegacyModelID): "old-ai-private", secret.NotifyURLAccount("n1"): "https://example.test/old-private"} {
		got, ok, err := secret.Get(account)
		if err != nil || !ok || got != want {
			t.Fatal("删除或替换的旧凭据未恢复")
		}
	}
	if _, ok, err := secret.Get(secret.DNSAccount("cloudflare", "token")); err != nil || ok {
		t.Fatal("新增凭据没有撤回")
	}
}

func TestLateValidationErrorDoesNotApplyEarlierChanges(t *testing.T) {
	settingsTransactionHome(t)
	if err := Save(Defaults()); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(secret.AIModelKeyAccount("m1"), "original-private"); err != nil {
		t.Fatal(err)
	}
	next := Defaults()
	next.UI.Theme = "dark"
	_, err := SaveWithSecrets(next, []SecretChange{{Kind: "ai", ID: "m1", Value: "new-private"}, {Kind: "dns", ID: "aliyun", Value: "incomplete-private"}})
	if err == nil {
		t.Fatal("缺少 DNS ID 应拒绝整批提交")
	}
	got, _, _ := secret.Get(secret.AIModelKeyAccount("m1"))
	loaded, _ := Load()
	if got != "original-private" || loaded.UI.Theme != "system" {
		t.Fatal("校验失败前不应改变任何数据")
	}
}

func TestSettingsWithoutSecretChangesDoesNotRequireVault(t *testing.T) {
	settingsTransactionHome(t)
	keyring.MockInitWithError(errors.New("vault locked"))
	next := Defaults()
	next.UI.Theme = "dark"
	if _, err := SaveWithSecrets(next, nil); err != nil {
		t.Fatal("没有凭据变更时不应该读取凭据库")
	}
}
