package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestKnownAlgorithms(t *testing.T) {
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	ed, _ := ssh.NewPublicKey(edPub)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rs, _ := ssh.NewPublicKey(&rk.PublicKey)

	dir := t.TempDir()
	p := filepath.Join(dir, "known_hosts")
	os.WriteFile(p, []byte(
		knownhosts.Line([]string{knownhosts.Normalize("10.0.0.1:22")}, ed)+"\n"+
			knownhosts.Line([]string{knownhosts.Normalize("10.0.0.2:2222")}, rs)+"\n"), 0o600)
	cb, err := knownhosts.New(p)
	if err != nil {
		t.Fatal(err)
	}
	// 只记录了 ED25519：只用 ED25519 协商（服务器同时有 RSA 也不会被误判为公钥变化）
	if got := knownAlgorithms(cb, "10.0.0.1", 22); !reflect.DeepEqual(got, []string{ssh.KeyAlgoED25519}) {
		t.Fatalf("ed25519：%v", got)
	}
	if got := knownAlgorithms(cb, "10.0.0.2", 2222); !reflect.DeepEqual(got, []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}) {
		t.Fatalf("rsa：%v", got)
	}
	if got := knownAlgorithms(cb, "10.0.0.3", 22); got != nil {
		t.Fatalf("没有记录应返回 nil：%v", got)
	}
	if DefaultHostKeyAlgorithms[0] != ssh.KeyAlgoED25519 {
		t.Fatal("默认应 ED25519 优先（与 OpenSSH 一致）")
	}
}
