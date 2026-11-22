package engine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
)

// agentTrust 这台电脑上验证过的 agent 文件。再连同一台服务器时只核对属主、权限和大小，
// 不再对整个 agent（十几 MB）算 sha256：空闲断连后重连每次都要算一遍，慢的盘上要几十到上百毫秒。
// 文件内容变了（升级 ferry、协议变了）sha 也跟着变，缓存自然失效。
type agentTrust struct {
	SHA  string    `json:"sha"`
	Arch string    `json:"arch"`
	Size int64     `json:"size"`
	At   time.Time `json:"at"`
}

// agentTrustPath 按服务器、登录方式和组件目录分开记：换了任何一个都重新完整核对一次。
func agentTrustPath(t *config.Target, ferryDir string) string {
	address := strings.Join([]string{t.User, t.Host, fmt.Sprint(t.Port), fmt.Sprint(t.Sudo), t.SudoAuth, ferryDir}, "|")
	key := sha256.Sum256([]byte(address))
	return filepath.Join(appdir.Dir(), "agent-trust", fmt.Sprintf("%x.json", key))
}

// readAgentTrust 读上次验证过的记录；没有、坏了都当作没有。
func readAgentTrust(path string) agentTrust {
	var at agentTrust
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &at) != nil {
		return agentTrust{}
	}
	return at
}

// saveAgentTrust 记下这次完整核对通过的 agent；写不进去不影响连接。
func saveAgentTrust(path string, at agentTrust) {
	at.At = time.Now()
	b, err := json.Marshal(at)
	if err != nil {
		return
	}
	_ = appdir.WriteFileAtomic(path, b, 0o600)
}
