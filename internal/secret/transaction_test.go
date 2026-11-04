package secret

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/zalando/go-keyring"
)

type memoryVault struct {
	values   map[string]string
	getError string
	writes   []string
	fail     func(op, account, value string) error
}

func (m *memoryVault) Get(account string) (string, bool, error) {
	if account == m.getError {
		return "", false, errors.New("locked")
	}
	v, ok := m.values[account]
	return v, ok, nil
}
func (m *memoryVault) Set(account, value string) error {
	m.writes = append(m.writes, "set:"+account)
	m.values[account] = value
	if m.fail != nil {
		return m.fail("set", account, value)
	}
	return nil
}
func (m *memoryVault) Delete(account string) error {
	m.writes = append(m.writes, "delete:"+account)
	delete(m.values, account)
	if m.fail != nil {
		return m.fail("delete", account, "")
	}
	return nil
}
func transactionHome(t *testing.T) {
	t.Helper()
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
}
func transactionIndex(t *testing.T) []byte {
	t.Helper()
	if err := writeIndex([]Entry{{Account: "ai:api-key:old", Kind: KindAI, Label: "原标签", SavedAt: time.Unix(100, 0)}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(indexFile())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCredentialTransactionSuccessAndMetadata(t *testing.T) {
	transactionHome(t)
	transactionIndex(t)
	store := &memoryVault{values: map[string]string{"ai:api-key:old": "old-private", "notify:sign:gone": "remove-private"}}
	commits := 0
	err := withChanges([]Change{{Account: "ai:api-key:old", Value: "new-private"}, {Account: "notify:url:new", Value: "https://example.test/private"}, {Account: "notify:sign:gone", Delete: true}}, func() error { commits++; return nil }, store)
	if err != nil || commits != 1 || store.values["ai:api-key:old"] != "new-private" || store.values["notify:url:new"] == "" {
		t.Fatalf("批量提交失败：%v", err)
	}
	if _, ok := store.values["notify:sign:gone"]; ok {
		t.Fatal("删除未生效")
	}
	entries, err := List()
	if err != nil || len(entries) != 2 {
		t.Fatal("索引没有同步更新")
	}
	for _, e := range entries {
		if e.Account == "ai:api-key:old" && e.Label != "原标签" {
			t.Fatal("丢失自定义标签")
		}
	}
	b, _ := os.ReadFile(indexFile())
	for _, v := range store.values {
		if bytes.Contains(b, []byte(v)) {
			t.Fatal("索引不应记录凭据值")
		}
	}
}

func TestCredentialTransactionWriteFailureRestoresAttemptedItem(t *testing.T) {
	transactionHome(t)
	originalIndex := transactionIndex(t)
	original := map[string]string{"ai:api-key:old": "old-private", "notify:sign:old": "old-sign-private"}
	store := &memoryVault{values: map[string]string{"ai:api-key:old": "old-private", "notify:sign:old": "old-sign-private"}}
	failed := false
	store.fail = func(op, account, value string) error {
		if !failed && account == "notify:sign:old" {
			failed = true
			return errors.New("cannot write " + value)
		}
		return nil
	}
	committed := false
	err := withChanges([]Change{{Account: "ai:api-key:old", Value: "new-private"}, {Account: "notify:url:added", Value: "added-private"}, {Account: "notify:sign:old", Value: "new-sign-private"}}, func() error { committed = true; return nil }, store)
	if err == nil || !strings.Contains(err.Error(), "已恢复") || committed || !reflect.DeepEqual(store.values, original) {
		t.Fatalf("写入中途失败应恢复当前失败项、新增和替换项：%v", err)
	}
	if strings.Contains(err.Error(), "new-sign-private") {
		t.Fatal("错误信息泄露敏感值")
	}
	index, _ := os.ReadFile(indexFile())
	if !bytes.Equal(index, originalIndex) {
		t.Fatal("原索引改变")
	}
	want := []string{"set:ai:api-key:old", "set:notify:url:added", "set:notify:sign:old", "set:notify:sign:old", "delete:notify:url:added", "set:ai:api-key:old"}
	if !reflect.DeepEqual(store.writes, want) {
		t.Fatal("未按逆序恢复所有尝试过的项")
	}
}

func TestCredentialTransactionCommitFailureRestoresDeleteAndIndex(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{true: "已有索引", false: "首次保存"}[exists], func(t *testing.T) {
			transactionHome(t)
			var originalIndex []byte
			if exists {
				originalIndex = transactionIndex(t)
			}
			store := &memoryVault{values: map[string]string{"ai:api-key:old": "old-private"}}
			err := withChanges([]Change{{Account: "ai:api-key:old", Delete: true}, {Account: "dns:cloudflare:token", Value: "new-private"}}, func() error { return errors.New("settings read-only") }, store)
			if err == nil || !strings.Contains(err.Error(), "已恢复") || !reflect.DeepEqual(store.values, map[string]string{"ai:api-key:old": "old-private"}) {
				t.Fatalf("设置写入失败没有恢复凭据：%v", err)
			}
			index, statErr := os.ReadFile(indexFile())
			if exists && (!bytes.Equal(index, originalIndex) || statErr != nil) {
				t.Fatal("索引未恢复原字节")
			}
			if !exists && !os.IsNotExist(statErr) {
				t.Fatal("首次保存失败后不应留下索引")
			}
		})
	}
}

func TestCredentialTransactionIndexFailureDoesNotCommit(t *testing.T) {
	transactionHome(t)
	originalIndex := transactionIndex(t)
	if err := os.Mkdir(indexFile()+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	store := &memoryVault{values: map[string]string{"ai:api-key:old": "old-private"}}
	committed := false
	err := withChanges([]Change{{Account: "ai:api-key:old", Value: "new-private"}}, func() error { committed = true; return nil }, store)
	if err == nil || committed || store.values["ai:api-key:old"] != "old-private" {
		t.Fatal("索引写入失败也应恢复凭据")
	}
	index, _ := os.ReadFile(indexFile())
	if !bytes.Equal(index, originalIndex) {
		t.Fatal("索引改变")
	}
}

func TestCredentialTransactionSnapshotFailureMakesNoChanges(t *testing.T) {
	transactionHome(t)
	store := &memoryVault{values: map[string]string{"one": "old-private"}, getError: "two"}
	err := withChanges([]Change{{Account: "one", Value: "new-private"}, {Account: "two", Value: "other-private"}}, func() error { t.Fatal("不应提交"); return nil }, store)
	if err == nil || len(store.writes) != 0 {
		t.Fatal("备份失败前不能开始写入")
	}
}

func TestCredentialTransactionRollbackFailureIsExplicitAndRedacted(t *testing.T) {
	transactionHome(t)
	store := &memoryVault{values: map[string]string{"one": "original-private", "two": "original-two-private"}}
	store.fail = func(op, account, value string) error {
		if account == "two" {
			return errors.New("store rejected " + value)
		}
		return nil
	}
	err := withChanges([]Change{{Account: "one", Value: "new-one-private"}, {Account: "two", Value: "new-two-private"}}, func() error { return nil }, store)
	if err == nil || !strings.Contains(err.Error(), "自动恢复未完全成功") || store.values["one"] != "original-private" {
		t.Fatal("恢复失败不能误报成功，也不能跳过其他项的恢复")
	}
	for _, v := range []string{"original-private", "original-two-private", "new-one-private", "new-two-private"} {
		if strings.Contains(err.Error(), v) {
			t.Fatal("错误中出现敏感值")
		}
	}
}

func TestCredentialTransactionSerializesOrdinaryWrites(t *testing.T) {
	transactionHome(t)
	t.Setenv("FERRY_KEYRING_SERVICE", "ferry-settings-transaction-test")
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	if err := Set("ai:api-key:old", "original-private"); err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	transactionDone := make(chan error, 1)
	go func() {
		transactionDone <- WithChanges([]Change{{Account: "ai:api-key:old", Value: "transaction-private"}}, func() error { close(entered); <-finish; return errors.New("settings failed") })
	}()
	<-entered
	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- Set("ai:api-key:old", "newest-private") }()
	select {
	case <-ordinaryDone:
		close(finish)
		t.Fatal("普通写入不应插入批量提交中间")
	case <-time.After(30 * time.Millisecond):
	}
	close(finish)
	if err := <-transactionDone; err == nil {
		t.Fatal("应报告提交失败")
	}
	if err := <-ordinaryDone; err != nil {
		t.Fatal(err)
	}
	got, ok, err := Get("ai:api-key:old")
	if err != nil || !ok || got != "newest-private" {
		t.Fatal("回滚覆盖了后来的写入")
	}
}
