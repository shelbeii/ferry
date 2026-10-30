package engine

import (
	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/secret"
)

// DNSCred 这台电脑上保存的 DNS 服务商密钥（HTTPS 证书的 DNS 验证用）：先看环境变量（CI），再看系统凭据库。
// 少了哪一项就返回 nil：部署时不带，服务器上用以前存过的。
func (e *Engine) DNSCred(provider string) map[string]string {
	fields, ok := agent.DNSProviders[provider]
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, f := range fields {
		v, _, err := secret.Lookup(e.getenv, secret.DNSEnv(provider, f), secret.DNSAccount(provider, f))
		if err != nil || v == "" {
			return nil
		}
		out[f] = v
	}
	return out
}
