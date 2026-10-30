package main

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

// 用一个测试专用的键，不碰真正的用户环境变量。
func TestUserPathRegistry(t *testing.T) {
	old := userEnvKey
	userEnvKey = `Software\FerryTest\Environment`
	t.Cleanup(func() {
		userEnvKey = old
		registry.DeleteKey(registry.CURRENT_USER, `Software\FerryTest\Environment`)
		registry.DeleteKey(registry.CURRENT_USER, `Software\FerryTest`)
	})
	if v, err := userPath(); err == nil || v != "" {
		t.Fatalf("键不存在时应该报错：%q %v", v, err)
	}
	if err := setUserPath(`%USERPROFILE%\bin;E:\Ferry`); err != nil {
		t.Fatal(err)
	}
	k, _ := registry.OpenKey(registry.CURRENT_USER, userEnvKey, registry.QUERY_VALUE)
	_, typ, _ := k.GetStringValue("Path")
	k.Close()
	if typ != registry.EXPAND_SZ {
		t.Fatalf("新建的 PATH 应该是可展开的类型：%d", typ)
	}
	v, err := userPath()
	if err != nil || v != `%USERPROFILE%\bin;E:\Ferry` {
		t.Fatalf("读回 %q %v", v, err)
	}
}
