package config

import "testing"

// ferry.yaml 由用户、AI 写：解析、校验不管给什么都不能 panic。
func FuzzParse(f *testing.F) {
	f.Add([]byte("version: 1\nproject: p\ntargets:\n  a: {host: h, runAs: x}\napps:\n  - {name: A, type: java, artifact: {path: a.jar}}\nbackups:\n  - {name: db, mysql: {database: '*', bin: /x}}\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		Parse(b)
	})
}
