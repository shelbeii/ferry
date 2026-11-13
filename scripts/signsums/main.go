// signsums 给发布的 SHA256SUMS.txt 签名（见 internal/update/sig.go）。
//
//	go run ./scripts/signsums genkey <目录>                       # 生成一对密钥：私钥写进 <目录>/ferry-sign-<指纹>.key（只有自己能读），打印公钥
//	go run ./scripts/signsums sign -key-env FERRY_SIGN_KEY SHA256SUMS.txt   # 用环境变量里的私钥签名，写出 SHA256SUMS.txt.sig
//	go run ./scripts/signsums verify -pub <base64 公钥> SHA256SUMS.txt     # 验签（不写 -pub 时用编进 Ferry 的公钥）
//
// 私钥是 base64 的 Ed25519 种子（32 字节），整串放进 GitHub 仓库的 Secrets（FERRY_SIGN_KEY）。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "signsums:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("用法：signsums genkey <目录> | sign -key-env 变量名 文件 | verify [-pub 公钥] 文件")
	}
	switch args[0] {
	case "genkey":
		if len(args) != 2 {
			return errors.New("用法：signsums genkey <目录>")
		}
		return genkey(args[1])
	case "sign":
		fs := flag.NewFlagSet("sign", flag.ContinueOnError)
		env := fs.String("key-env", "FERRY_SIGN_KEY", "私钥所在的环境变量")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("用法：signsums sign -key-env 变量名 文件")
		}
		return sign(os.Getenv(*env), *env, fs.Arg(0))
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ContinueOnError)
		pub := fs.String("pub", "", "base64 的公钥；不写时用编进 Ferry 的")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("用法：signsums verify [-pub 公钥] 文件")
		}
		return verify(*pub, fs.Arg(0))
	}
	return fmt.Errorf("不认识的子命令：%s", args[0])
}

func genkey(dir string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	id := update.KeyID(pub)
	p := filepath.Join(dir, "ferry-sign-"+id+".key")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("指纹：%s\n公钥：%s\n私钥：%s\n", id, base64.StdEncoding.EncodeToString(pub), p)
	return nil
}

func sign(key64, env, file string) error {
	key64 = strings.TrimSpace(key64)
	if key64 == "" {
		return fmt.Errorf("环境变量 %s 是空的：不允许发布没签名的版本（在仓库 Settings → Secrets 里加上 %s）", env, env)
	}
	seed, err := base64.StdEncoding.DecodeString(key64)
	if err != nil {
		return fmt.Errorf("%s 不是 base64：%w", env, err)
	}
	sums, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	out, err := update.SignSums(seed, sums)
	if err != nil {
		return err
	}
	// 签完马上用编进 Ferry 的公钥验一遍：私钥和代码里的公钥对不上时，发出去的版本谁也装不上
	if err := update.VerifyTrusted(sums, out); err != nil {
		return fmt.Errorf("签好了，但编进 Ferry 的公钥验不过（私钥和 internal/update/keys.go 里的公钥不是一对）：%w", err)
	}
	if err := os.WriteFile(file+".sig", out, 0o644); err != nil {
		return err
	}
	fmt.Print(string(out))
	return nil
}

func verify(pub64, file string) error {
	sums, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(file + ".sig")
	if err != nil {
		return err
	}
	if pub64 == "" {
		err = update.VerifyTrusted(sums, sig)
	} else {
		b, derr := base64.StdEncoding.DecodeString(pub64)
		if derr != nil || len(b) != ed25519.PublicKeySize {
			return errors.New("-pub 不是 base64 的 Ed25519 公钥")
		}
		err = update.VerifySums(sums, sig, []ed25519.PublicKey{b})
	}
	if err != nil {
		return err
	}
	fmt.Println("签名正确")
	return nil
}
