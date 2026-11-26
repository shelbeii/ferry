package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/remote"
	"golang.org/x/crypto/ssh"
)

func TestFirewallUninstallAuthForTarget(t *testing.T) {
	// 1. 非 root + sudoAuth=password → 优先 sudo-password
	a := firewallUninstallAuthForTarget(&config.Target{
		User: "suidao3", Host: "10.0.0.1", Port: 22, Auth: "password", Sudo: true, SudoAuth: "password", Production: true,
	}, "")
	if a.Method != "sudo-password" || !a.Production || a.CanPasswordFallback {
		t.Fatalf("sudo-password 解析异常：%+v", a)
	}

	// 2. root + auth=password → ssh-password
	a = firewallUninstallAuthForTarget(&config.Target{
		User: "root", Host: "10.0.0.2", Port: 2222, Auth: "password",
	}, "")
	if a.Method != "ssh-password" || a.Port != 2222 || a.CanPasswordFallback {
		t.Fatalf("ssh-password 解析异常：%+v", a)
	}

	// 3. 加密私钥 + 免密 sudo → key-passphrase
	a = firewallUninstallAuthForTarget(&config.Target{
		User: "deploy", Host: "10.0.0.3", Auth: "key", Sudo: true, SudoAuth: "nopasswd",
	}, "/home/u/.ssh/id_ed25519")
	if a.Method != "key-passphrase" || a.Port != 22 || a.CanPasswordFallback {
		t.Fatalf("key-passphrase 解析异常：%+v", a)
	}

	// 4. 免密私钥 + root → ssh-key-challenge（允许切换为 ssh-password）
	a = firewallUninstallAuthForTarget(&config.Target{
		User: "root", Host: "10.0.0.4", Auth: "key",
	}, "")
	if a.Method != "ssh-key-challenge" || !a.CanPasswordFallback {
		t.Fatalf("ssh-key-challenge 解析异常：%+v", a)
	}
}

func TestResolveFirewallAuthMethodNoDowngrade(t *testing.T) {
	pwReq := firewallUninstallAuthForTarget(&config.Target{User: "root", Host: "10.0.0.1", Auth: "password"}, "")
	if _, err := resolveFirewallAuthMethod(pwReq, "ssh-key-challenge"); err == nil {
		t.Fatal("不应允许从 ssh-password 降级为 ssh-key-challenge")
	}
	if m, err := resolveFirewallAuthMethod(pwReq, ""); err != nil || m != "ssh-password" {
		t.Fatalf("默认方式异常：%q %v", m, err)
	}

	sudoReq := firewallUninstallAuthForTarget(&config.Target{User: "u", Host: "10.0.0.1", Sudo: true, SudoAuth: "password"}, "")
	if _, err := resolveFirewallAuthMethod(sudoReq, "ssh-password"); err == nil {
		t.Fatal("sudo-password 不应允许绕过切换为其他认证方式")
	}

	keyReq := firewallUninstallAuthForTarget(&config.Target{User: "root", Host: "10.0.0.1", Auth: "key"}, "")
	if m, err := resolveFirewallAuthMethod(keyReq, "ssh-password"); err != nil || m != "ssh-password" {
		t.Fatalf("免密密钥应允许升级切换为 ssh-password：%q %v", m, err)
	}
}

func TestFirewallUninstallRequiresToken(t *testing.T) {
	e := &Engine{File: &config.File{Targets: map[string]*config.Target{"t": {Name: "t", Host: "127.0.0.1"}}}}
	_, err := e.FirewallUninstall("t", FirewallUninstallOptions{Kind: "firewalld"})
	var fae *FirewallAuthError
	if !errors.As(err, &fae) {
		t.Fatalf("缺少令牌应返回 FirewallAuthError，实际：%v", err)
	}
	if d := fae.JSONDetail(); d["kind"] != "fw-auth-failed" {
		t.Fatalf("JSONDetail 异常：%v", d)
	}
}

func TestRemoteKeyEncrypted(t *testing.T) {
	dir := t.TempDir()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	plainBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	plainPath := filepath.Join(dir, "plain_ed25519")
	os.WriteFile(plainPath, pem.EncodeToMemory(plainBlock), 0o600)
	if remote.KeyEncrypted(plainPath) {
		t.Fatal("未加密私钥不应识别为加密")
	}

	encBlock, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret-pass"))
	if err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(dir, "enc_ed25519")
	os.WriteFile(encPath, pem.EncodeToMemory(encBlock), 0o600)
	if !remote.KeyEncrypted(encPath) {
		t.Fatal("加密私钥应识别为需要密码")
	}
}
