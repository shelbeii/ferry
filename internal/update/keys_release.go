//go:build !testkeys

package update

import "crypto/ed25519"

// extraKeys 正式版没有：不能用环境变量加钥绕过验签。
func extraKeys() []ed25519.PublicKey { return nil }
