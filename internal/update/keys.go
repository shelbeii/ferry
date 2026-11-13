package update

import "crypto/ed25519"

// trustedKeys 验更新包签名用的公钥（见 sig.go）。第一把平时用：私钥在 GitHub 仓库的 Secrets（FERRY_SIGN_KEY）；
// 第二把是备用：私钥离线保存，第一把丢了、泄露了时换用。换钥、加钥只能靠发新版本。
var trustedKeys = []ed25519.PublicKey{
	mustKey("3EFqUXe4e+GKNhZ9o7nYTvlpBDUWmOmlJgHQQsHxO4o="), // e1cae50ac19a2b0c
	mustKey("cpl4QCdhZVgCBIFuGwrIAJhAYTJAUMjX0jB1oVMSUxw="), // 94944000abf4bb53（备用）
}

// VerifyTrusted 用编进程序的公钥验签。
func VerifyTrusted(sums, sig []byte) error { return VerifySums(sums, sig, keys()) }

func keys() []ed25519.PublicKey {
	return append(append([]ed25519.PublicKey{}, trustedKeys...), extraKeys()...)
}
