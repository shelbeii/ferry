package engine

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestEnvKey(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----"
	for name, env := range map[string]map[string]string{
		"原文":      {"FERRY_SSH_KEY": key + "\n"},
		"base64":  {"FERRY_SSH_KEY": base64.StdEncoding.EncodeToString([]byte(key))},
		"换行写成 \n": {"FERRY_SSH_KEY": strings.ReplaceAll(key, "\n", `\n`)},
		"按目标":     {"FERRY_SSH_KEY_PROD_A": key, "FERRY_SSH_KEY": "other"},
	} {
		n, data := envKey(func(k string) string { return env[k] }, "prod-a")
		if string(data) != key+"\n" {
			t.Errorf("%s：%q", name, data)
		}
		if name == "按目标" && n != "FERRY_SSH_KEY_PROD_A" {
			t.Errorf("按目标的优先：%s", n)
		}
	}
	if n, data := envKey(func(string) string { return "" }, "x"); n != "" || data != nil {
		t.Fatal("没有设置时不用")
	}
}
