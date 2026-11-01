package swtpl

import "testing"

// 模板可能是用户、AI 写的：解析、展开变量不管给什么都不能 panic，也不能一直展开下去。
func FuzzTemplate(f *testing.F) {
	for _, t := range Builtin() {
		b, _ := BuiltinSource(t.ID)
		f.Add(b, "${port+1000} ${public?a|${password}} $${X} ${random:8}")
	}
	f.Fuzz(func(t *testing.T, yaml []byte, s string) {
		Parse(yaml)
		vs := Vars{"port": "8848", "public": "true", "password": "${port}", "x": "${x}${x}"}
		vs.Expand(s)
		vs.Cond(s)
	})
}
