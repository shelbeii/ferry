package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	pub2, priv2, _ := ed25519.GenerateKey(nil)
	sums := []byte("aaaa  Ferry_1.0.0_x64-setup.exe\n")
	sig, err := SignSums(priv.Seed(), sums)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(sig), "ferry-sig v1\nkey: "+KeyID(pub)+"\nsig: ") {
		t.Fatalf("签名文件：%s", sig)
	}
	keys := []ed25519.PublicKey{pub, pub2}
	if err := VerifySums(sums, sig, keys); err != nil {
		t.Fatalf("正确的签名：%v", err)
	}
	// Windows 上被改成 CRLF 也认
	if err := VerifySums(sums, []byte(strings.ReplaceAll(string(sig), "\n", "\r\n")), keys); err != nil {
		t.Fatalf("CRLF：%v", err)
	}
	// 备用钥签的也认
	sig2, _ := SignSums(priv2.Seed(), sums)
	if err := VerifySums(sums, sig2, keys); err != nil {
		t.Fatalf("备用钥：%v", err)
	}
	bad := map[string]struct {
		sums, sig []byte
		keys      []ed25519.PublicKey
		want      string
	}{
		"改了一个字节": {[]byte("aaab  Ferry_1.0.0_x64-setup.exe\n"), sig, keys, "对不上"},
		"不认识的钥":  {sums, sig, []ed25519.PublicKey{pub2}, "不认识"},
		"空的":     {sums, nil, keys, "格式"},
		"签名坏了":   {sums, []byte("ferry-sig v1\nkey: " + KeyID(pub) + "\nsig: xx\n"), keys, "看不懂"},
		"换了签名":   {sums, []byte(strings.Replace(string(sig), "sig: ", "sig: A", 1)), keys, "看不懂"},
	}
	for name, c := range bad {
		err := VerifySums(c.sums, c.sig, c.keys)
		if !errors.Is(err, ErrBadSig) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：%v", name, err)
		}
	}
	if _, err := SignSums([]byte("short"), sums); err == nil {
		t.Error("私钥长度不对时应报错")
	}
}

// 编进程序的公钥都能解开、指纹不重复
func TestTrustedKeys(t *testing.T) {
	if len(trustedKeys) < 2 {
		t.Fatal("应有平时用的和备用的两把钥")
	}
	seen := map[string]bool{}
	for _, k := range trustedKeys {
		id := KeyID(k)
		if seen[id] {
			t.Fatalf("重复的钥 %s", id)
		}
		seen[id] = true
	}
}

// 没有签名文件、签名不对：不装；安装包和校验和被一起换掉（签名还是原来的）：不装
func TestDownloadSignature(t *testing.T) {
	ctx := context.Background()
	f := newFake(t, "0.13.70")
	if f.asset() == "" {
		t.Skip("这种系统没有安装包")
	}
	f.seed = nil
	if _, err := DownloadFile(ctx, f.source(), "0.13.70", f.asset(), t.TempDir(), nil); !errors.Is(err, ErrBadSig) || !strings.Contains(err.Error(), "取不到签名文件") {
		t.Fatalf("没有签名：%v", err)
	}

	f = newFake(t, "0.13.70")
	f.original = f.sums()
	f.body = []byte("evil ferry")
	dir := t.TempDir()
	_, err := DownloadFile(ctx, f.source(), "0.13.70", f.asset(), dir, nil)
	if !errors.Is(err, ErrBadSig) || !strings.Contains(err.Error(), "可能被篡改") || f.downloads.Load() != 0 {
		t.Fatalf("一起换掉：%v（下载了 %d 次）", err, f.downloads.Load())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("签名不对时留下了 %d 个文件", len(ents))
	}

	// 用别的钥签的（不是编进程序的）
	f = newFake(t, "0.13.70")
	_, other, _ := ed25519.GenerateKey(nil)
	f.seed = other.Seed()
	if _, err := DownloadFile(ctx, f.source(), "0.13.70", f.asset(), t.TempDir(), nil); !errors.Is(err, ErrBadSig) || !strings.Contains(err.Error(), "不认识") {
		t.Fatalf("别的钥：%v", err)
	}
}
