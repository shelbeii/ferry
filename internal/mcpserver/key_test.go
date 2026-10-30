package mcpserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"

	"golang.org/x/crypto/ssh"
)

// withTestKey 给连接一把临时生成的私钥（FERRY_SSH_KEY）：要测「连不上」的地方都能走到连接这一步，
// 不管这台机器的 ~/.ssh 里有没有私钥、有没有 ssh-agent（CI 的机器上都没有）。
func withTestKey(t *testing.T) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FERRY_SSH_KEY", string(pem.EncodeToMemory(block)))
}
