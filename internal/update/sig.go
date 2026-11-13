package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// 更新包签名：发布时用 Ed25519 私钥对 SHA256SUMS.txt 的全部字节签名，写成 SHA256SUMS.txt.sig（私钥只在 GitHub Actions 的
// secret FERRY_SIGN_KEY 里）。Ferry 更新时用编进程序的公钥（trustedKeys）验签，通过了才按里面的 SHA-256 校验安装包——
// 这样 Release 或下载的「GitHub 加速」把安装包和校验和一起换掉也装不上。签名文件：
//
//	ferry-sig v1
//	key: <公钥指纹：公钥 sha256 的前 16 个十六进制>
//	sig: <base64(Ed25519(SHA256SUMS.txt))>
//
// 换钥：先发一个内置新旧两把公钥、用旧钥签名的版本，之后的版本用新钥签名（所以 trustedKeys 一开始就留了一把离线保存的备用钥）。

const sigHeader = "ferry-sig v1"

// SumsSigFile 签名文件名。
const SumsSigFile = "SHA256SUMS.txt.sig"

// KeyID 公钥指纹。
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// SignSums 用私钥（32 字节的种子）给 SHA256SUMS.txt 的内容签名，返回签名文件的内容。
func SignSums(seed, sums []byte) ([]byte, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("私钥应为 %d 字节（base64 的 Ed25519 种子），现在是 %d 字节", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	sig := ed25519.Sign(priv, sums)
	return []byte(fmt.Sprintf("%s\nkey: %s\nsig: %s\n", sigHeader, KeyID(priv.Public().(ed25519.PublicKey)), base64.StdEncoding.EncodeToString(sig))), nil
}

// ErrBadSig 签名不对（被改过、或者用了不认识的钥）。
var ErrBadSig = errors.New("更新包的签名不对，可能被篡改，没有安装")

// VerifySums 用 keys 里指纹对得上的那把验签。
func VerifySums(sums, sigFile []byte, keys []ed25519.PublicKey) error {
	lines := strings.Split(strings.ReplaceAll(string(sigFile), "\r\n", "\n"), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != sigHeader {
		return fmt.Errorf("%w（签名文件的格式看不懂）", ErrBadSig)
	}
	var id, sig64 string
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "key":
			id = strings.TrimSpace(v)
		case "sig":
			sig64 = strings.TrimSpace(v)
		}
	}
	sig, err := base64.StdEncoding.DecodeString(sig64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w（签名看不懂）", ErrBadSig)
	}
	for _, k := range keys {
		if KeyID(k) != id {
			continue
		}
		if !ed25519.Verify(k, sums, sig) {
			return fmt.Errorf("%w（校验和文件和签名对不上）", ErrBadSig)
		}
		return nil
	}
	return fmt.Errorf("%w（签名用的钥 %s 这个版本的 Ferry 不认识）", ErrBadSig, id)
}

// mustKey 解码编进程序的公钥（base64）。
func mustKey(b64 string) ed25519.PublicKey {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b) != ed25519.PublicKeySize {
		panic("update: 公钥写错了：" + b64)
	}
	return ed25519.PublicKey(b)
}
