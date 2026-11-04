package config

import (
	"strings"
	"testing"
)

func TestInvalidTargetRuntimeOverrides(t *testing.T) {
	base := `version: 1
project: demo
targets:
  a: {host: 192.0.2.1, runAs: app, appRuntimes: {api: OVERRIDE}}
apps:
  - {name: api, type: java, artifact: {path: api.jar}}
`
	for _, value := range []string{"{}", "{java: relative}", "{java: /bin/java, node: /bin/node}", "{python: /bin/python}", "{java: /bin/java, port: 8081}"} {
		if _, err := load(t, strings.Replace(base, "OVERRIDE", value, 1)); err == nil {
			t.Fatalf("应拒绝无效覆盖：%s", value)
		}
	}
}
