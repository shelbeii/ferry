package engine

import "testing"

func TestSoftwareFolder(t *testing.T) {
	for _, c := range [][3]string{
		{"nacos", "3.2.4", "nacos-3.2.4"},
		{"mysql", "8.4.10", "mysql-8.4.10"},
		{"redis", "", "redis"},                    // 从软件源装：不知道版本
		{"app", "1.0+build.7", "app-1.0_build.7"}, // 目录名里不能用的字符换掉
		{"nacos-3.2.4", "3.2.4", "nacos-3.2.4"},   // 名称里已经带了版本
	} {
		if got := SoftwareFolder(c[0], c[1]); got != c[2] {
			t.Errorf("SoftwareFolder(%q, %q) = %q，应为 %q", c[0], c[1], got, c[2])
		}
	}
}
