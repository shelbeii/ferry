//go:build testkeys

package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
)

// extraKeys 只在测试构建里（go build -tags testkeys）：环境变量 FERRY_UPDATE_TESTKEY（base64 公钥）加一把，给假的 GitHub 用。
func extraKeys() []ed25519.PublicKey {
	b, err := base64.StdEncoding.DecodeString(os.Getenv("FERRY_UPDATE_TESTKEY"))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil
	}
	return []ed25519.PublicKey{b}
}
