package secret

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/appdir"
)

// Entry 已保存凭据的索引项（只有账户名，没有值）。
// 系统凭据库没有统一的跨平台「列出全部」接口，所以 ferry 自己记一份索引供设置页显示与删除。
type Entry struct {
	Account string    `json:"account"`
	Kind    string    `json:"kind"`  // password | passphrase
	Label   string    `json:"label"` // 给人看的说明：user@host:port 或私钥路径
	SavedAt time.Time `json:"savedAt"`
}

var indexMu sync.Mutex

func indexFile() string { return appdir.Path("secrets-index.json") }

// List 已保存的凭据（按保存时间倒序）。
func List() ([]Entry, error) {
	indexMu.Lock()
	defer indexMu.Unlock()
	return readIndex()
}

func readIndex() ([]Entry, error) {
	b, err := os.ReadFile(indexFile())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var es []Entry
	if err := json.Unmarshal(b, &es); err != nil {
		return nil, nil // 索引损坏不影响使用，重新开始记录
	}
	return es, nil
}

func writeIndex(es []Entry) error {
	sort.Slice(es, func(i, j int) bool { return es[i].SavedAt.After(es[j].SavedAt) })
	b, _ := json.MarshalIndent(es, "", "  ")
	return appdir.WriteFileAtomic(indexFile(), append(b, '\n'), 0o600)
}

func indexAdd(account string) {
	indexMu.Lock()
	defer indexMu.Unlock()
	es, _ := readIndex()
	kind, label := describe(account)
	out := es[:0]
	for _, e := range es {
		if e.Account != account {
			out = append(out, e)
		}
	}
	out = append(out, Entry{Account: account, Kind: kind, Label: label, SavedAt: time.Now()})
	writeIndex(out)
}

func indexRemove(account string) {
	indexMu.Lock()
	defer indexMu.Unlock()
	es, _ := readIndex()
	out := es[:0]
	for _, e := range es {
		if e.Account != account {
			out = append(out, e)
		}
	}
	writeIndex(out)
}

func describe(account string) (kind, label string) {
	for _, k := range []string{KindPassword, KindPassphrase, KindSoftware, KindAI, KindNotify, KindDNS} {
		if len(account) > len(k)+1 && account[:len(k)+1] == k+":" {
			label = account[len(k)+1:]
			if k == KindAI && strings.HasPrefix(label, "api-key:") {
				label = "API Key（" + strings.TrimPrefix(label, "api-key:") + "）"
			}
			if k == KindNotify {
				if id, ok := strings.CutPrefix(label, "url:"); ok {
					label = "部署通知的地址（" + id + "）"
				} else if id, ok := strings.CutPrefix(label, "sign:"); ok {
					label = "部署通知的加签密钥（" + id + "）"
				}
			}
			return k, label
		}
	}
	return "", account
}
