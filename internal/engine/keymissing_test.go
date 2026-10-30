package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/remote"
)

// 指定的私钥不在这台电脑上、默认私钥也登录不了：说明原因和办法，桌面版据 key-missing 给出按钮
func TestMissingKeyError(t *testing.T) {
	tg := &config.Target{Name: "web", IdentityFile: "~/.ssh/id_x", Auth: "key"}
	c := &credentials{t: tg, missingKey: `C:\Users\b\.ssh\id_x`}
	for _, in := range []error{remote.ErrNoKey, &remote.AuthFailedError{Addr: "root@h:22", Err: errors.New("unable to authenticate")}} {
		var mk *MissingKeyError
		if !errors.As(c.translate(in), &mk) {
			t.Fatalf("%v 没有换成 MissingKeyError", in)
		}
		msg := mk.Error()
		for _, want := range []string{"targets.web.identityFile", `C:\Users\b\.ssh\id_x`, "ferry keygen web --local", "ferry local set web --password"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("缺少 %q：%s", want, msg)
			}
		}
		if mk.JSONDetail()["kind"] != "key-missing" {
			t.Fatal(mk.JSONDetail())
		}
	}
	// 私钥在：原样返回
	c.missingKey = ""
	if err := c.translate(remote.ErrNoKey); err != remote.ErrNoKey {
		t.Fatal(err)
	}
}
