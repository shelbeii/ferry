package config

import (
	"strings"
	"testing"
)

func TestAppSecretsConfig(t *testing.T) {
	const base = `version: 1
project: demo
targets:
  prod: {host: localhost, runAs: demo, secretsFrom: keyring}
apps:
  - name: api
    type: binary
    artifact: {path: bin/api}
    service:
      secrets: [DB_PASSWORD, {name: SMS_KEY, optional: true, doc: for SMS}]
`
	f, err := load(t, base)
	if err != nil {
		t.Fatal(err)
	}
	refs := f.App("api").Service.Secrets
	if len(refs) != 2 || refs[0] != (SecretRef{Name: "DB_PASSWORD"}) ||
		refs[1] != (SecretRef{Name: "SMS_KEY", Optional: true, Doc: "for SMS"}) {
		t.Fatalf("unexpected refs: %+v", refs)
	}
	team, err := load(t, strings.Replace(base, "secretsFrom: keyring", "secretsFrom: file:secrets/prod.age", 1))
	if err != nil || team.Targets["prod"].AppSecretsFile() != "secrets/prod.age" {
		t.Fatalf("invalid team configuration: %v", err)
	}
	for _, tt := range []struct{ from, to, want string }{
		{"DB_PASSWORD", "1INVALID", "变量名"},
		{"DB_PASSWORD", "SMS_KEY", "重复"},
		{"      secrets:", "      env: {DB_PASSWORD: plaintext}\n      secrets:", "service.env"},
		{"doc: for SMS", "value: forbidden", "value"},
		{"secretsFrom: keyring", "secretsFrom: vault", "只支持 keyring"},
		{"secretsFrom: keyring", "secretsFrom: file:../prod.age", ".age 相对路径"},
		{"secretsFrom: keyring", "secretsFrom: file:/prod.age", ".age 相对路径"},
		{"secretsFrom: keyring", "secretsFrom: file:secrets/prod.yaml", ".age 相对路径"},
		{"secretsFrom: keyring", "secretsFrom: file:.git/prod.age", "项目工作目录"},
		{"type: binary", "type: static", "静态站点的 service"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			_, err := load(t, strings.Replace(base, tt.from, tt.to, 1))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
		})
	}
}
