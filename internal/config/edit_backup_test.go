package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddBackup(t *testing.T) {
	head := "version: 1\nproject: p\ntargets:\n  a:\n    host: 1.2.3.4\n    runAs: demo\n"
	apps := "apps:\n  - {name: Sys, type: java, artifact: {path: sys.jar}}\n"
	b := SoftwareBackup{Name: "mysql", Kind: "mysql", Database: "*", User: "root", Port: 3306, Bin: "/opt/ferry-software/mysql/mysql/bin",
		EnvFile: "/opt/ferry-software/mysql/ferry-backup.env", Target: "a", Comment: "Ferry 装的 MySQL（mysql）"}
	cases := map[string]string{
		"没有 backups":        head + apps,
		"已有 backups，后面还有字段": head + "backups:\n  - name: files\n    paths: [/opt/p/data]\n# 注释\n\n" + apps,
		"backups 在最后":       head + apps + "backups:\n  - name: files\n    paths: [/opt/p/data]\n",
	}
	for what, content := range cases {
		p := filepath.Join(t.TempDir(), "ferry.yaml")
		os.WriteFile(p, []byte(content), 0o644)
		if err := AddBackup(p, b); err != nil {
			t.Fatalf("%s：%v", what, err)
		}
		f, err := Load(p)
		if err != nil {
			t.Fatalf("%s：%v", what, err)
		}
		var got *Backup
		for _, x := range f.Backups {
			if x.Name == "mysql" {
				got = x
			}
		}
		if got == nil || got.MySQL == nil || got.MySQL.Database != "*" || got.MySQL.Bin != b.Bin || got.MySQL.Port != 3306 || len(got.EnvFile) != 1 || !got.AppliesTo("a") || got.AppliesTo("b") {
			out, _ := os.ReadFile(p)
			t.Errorf("%s：\n%s", what, out)
		}
		if strings.Contains(content, "files") && len(f.Backups) != 2 {
			t.Errorf("%s：原来的备份项丢了", what)
		}
	}
}

// 变量层层重复引用：展开到一定长度就报错
func TestVarsBomb(t *testing.T) {
	vars := map[string]string{"v0": "x"}
	for i := 1; i <= 6; i++ {
		vars["v"+itoa(i)] = strings.Repeat("${v"+itoa(i-1)+"}", 40)
	}
	sc := &Scope{Layers: []map[string]string{vars}}
	if _, err := sc.Expand("${v6}"); err == nil || !strings.Contains(err.Error(), "太长") {
		t.Errorf("%v", err)
	}
}
