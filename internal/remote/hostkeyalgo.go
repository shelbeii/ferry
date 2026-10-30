package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// DefaultHostKeyAlgorithms 没有记录时的主机公钥算法顺序，与 OpenSSH 默认一致（ED25519 优先）。
// trust / hostkey 取指纹与实际连接用同一顺序，核对过的指纹就是连接时用的那把公钥。
// （x/crypto 默认 ECDSA、RSA 在前，服务器同时有多种主机密钥时会和 OpenSSH 记录的类型对不上。）
var DefaultHostKeyAlgorithms = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA,
}

var (
	probeOnce sync.Once
	probeKey  ssh.PublicKey
)

// probe 一把随机公钥：拿去查 known_hosts，返回的 KeyError.Want 就是该主机已记录的全部公钥。
func probe() ssh.PublicKey {
	probeOnce.Do(func() {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		probeKey, _ = ssh.NewPublicKey(pub)
	})
	return probeKey
}

// keyType 算法对应的公钥类型（rsa-sha2-256/512 都是 ssh-rsa 公钥）。
func keyType(algo string) string {
	switch algo {
	case ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512:
		return ssh.KeyAlgoRSA
	}
	return algo
}

// knownAlgorithms known_hosts 里已记录的该主机公钥类型对应的算法（按默认顺序）；没有记录返回 nil。
// 像 OpenSSH 一样只用已记录的类型协商，避免服务器换用另一种主机密钥时被误判为「公钥不一致」。
func knownAlgorithms(cb ssh.HostKeyCallback, host string, port int) []string {
	if cb == nil {
		return nil
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	remote := &net.TCPAddr{IP: net.ParseIP(host), Port: port}
	err := cb(addr, remote, probe())
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) || len(ke.Want) == 0 {
		return nil
	}
	types := map[string]bool{}
	for _, k := range ke.Want {
		types[k.Key.Type()] = true
	}
	var out []string
	for _, a := range DefaultHostKeyAlgorithms {
		if types[keyType(a)] {
			out = append(out, a)
		}
	}
	return out
}
