package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/zalando/go-keyring"
)

// Change 只在进程内使用的凭据变更；值和旧值都不能写入事务日志或命令输出。
type Change struct {
	Account string
	Value   string
	Delete  bool
}

type vaultStore interface {
	Get(string) (string, bool, error)
	Set(string, string) error
	Delete(string) error
}

type systemVault struct{}

func (systemVault) Get(account string) (string, bool, error) { return get(account) }
func (systemVault) Set(account, value string) error          { return keyring.Set(service(), account, value) }
func (systemVault) Delete(account string) error {
	err := keyring.Delete(service(), account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// WithChanges 在内存中备份旧值，全部凭据和索引写完后提交设置。
// commit 必须是失败时不替换原文件的原子写入，不能在成功写入后再返回其他错误。
// 普通失败逆序恢复；凭据库本身无法恢复时明确报告，而不宣称保存成功。
func WithChanges(changes []Change, commit func() error) error {
	vaultMu.Lock()
	defer vaultMu.Unlock()
	return withChanges(changes, commit, systemVault{})
}

func withChanges(changes []Change, commit func() error, store vaultStore) error {
	if len(changes) == 0 {
		return commit()
	}
	seen := map[string]bool{}
	values := []string{}
	for _, c := range changes {
		if c.Account == "" || seen[c.Account] || (!c.Delete && c.Value == "") || (c.Delete && c.Value != "") {
			return errors.New("凭据变更无效，本次未保存")
		}
		seen[c.Account] = true
		values = append(values, c.Value)
	}
	indexMu.Lock()
	defer indexMu.Unlock()
	indexPath := indexFile()
	indexBefore, err := os.ReadFile(indexPath)
	indexExists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return errors.New("读取凭据索引失败，本次未保存")
	}
	var entries []Entry
	if indexExists && json.Unmarshal(indexBefore, &entries) != nil {
		return errors.New("凭据索引格式错误，本次未保存")
	}
	indexMode := os.FileMode(0o600)
	if st, err := os.Stat(indexPath); err == nil {
		indexMode = st.Mode().Perm()
	}
	type previous struct {
		value  string
		exists bool
	}
	before := make([]previous, len(changes))
	for i, c := range changes {
		v, exists, err := store.Get(c.Account)
		if err != nil {
			return errors.New("读取现有系统凭据失败，本次未保存；请检查凭据库是否已解锁")
		}
		before[i] = previous{v, exists}
		values = append(values, v)
	}
	// 先替换长值，防止某个短值是另一个值的子串而留下敏感片段。
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	mask := func(err error) string {
		text := err.Error()
		for _, value := range values {
			if value != "" {
				text = strings.ReplaceAll(text, value, "[已隐藏]")
			}
		}
		return text
	}
	indexChanged := false
	rollback := func(at int, cause error) error {
		var failed []string
		for i := at; i >= 0; i-- {
			var err error
			if before[i].exists {
				err = store.Set(changes[i].Account, before[i].value)
			} else {
				err = store.Delete(changes[i].Account)
			}
			if err != nil {
				failed = append(failed, "系统凭据："+mask(err))
			}
		}
		if indexChanged {
			var err error
			if indexExists {
				err = appdir.WriteFileAtomic(indexPath, indexBefore, indexMode)
			} else {
				err = os.Remove(indexPath)
				if os.IsNotExist(err) {
					err = nil
				}
			}
			if err != nil {
				failed = append(failed, "凭据索引："+mask(err))
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("设置未保存，自动恢复未完全成功：%s；%s。请检查系统凭据库和数据目录后重新保存", mask(cause), strings.Join(failed, "；"))
		}
		return fmt.Errorf("保存失败，已恢复原有设置和凭据：%s", mask(cause))
	}
	for i, c := range changes {
		var err error
		if c.Delete {
			err = store.Delete(c.Account)
		} else {
			err = store.Set(c.Account, c.Value)
		}
		// 写入返回错误也可能已经修改了凭据，因此当前项同样需要恢复。
		if err != nil {
			return rollback(i, err)
		}
	}
	next := make([]Entry, 0, len(entries)+len(changes))
	labels := map[string]string{}
	for _, e := range entries {
		labels[e.Account] = e.Label
		if !seen[e.Account] {
			next = append(next, e)
		}
	}
	for _, c := range changes {
		if c.Delete {
			continue
		}
		kind, label := describe(c.Account)
		if labels[c.Account] != "" {
			label = labels[c.Account]
		}
		next = append(next, Entry{Account: c.Account, Kind: kind, Label: label, SavedAt: time.Now()})
	}
	if err := writeIndex(next); err != nil {
		return rollback(len(changes)-1, err)
	}
	indexChanged = true
	if err := commit(); err != nil {
		return rollback(len(changes)-1, err)
	}
	return nil
}
