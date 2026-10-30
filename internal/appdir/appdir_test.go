package appdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 用临时目录模拟用户目录与配置目录。
func sandbox(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("FERRY_HOME", "")
	Reset()
	t.Cleanup(Reset)
	return home
}

// installAt 让「程序所在目录」指向 dir（模拟安装在那里）。按 Windows 的规则（数据放在安装目录下），在 macOS 上也测这套逻辑。
func installAt(t *testing.T, dir string) {
	t.Helper()
	old, oldNo := ExeDir, noInstallData
	ExeDir = func() string { return dir }
	noInstallData = false
	Reset()
	t.Cleanup(func() { ExeDir, noInstallData = old, oldNo; Reset() })
}

// macOS：不在 .app 包里放数据，用用户目录。
func TestNoInstallDataUsesUserDir(t *testing.T) {
	home := sandbox(t)
	inst := filepath.Join(home, "Applications", "Ferry.app", "Contents", "MacOS")
	os.MkdirAll(inst, 0o755)
	installAt(t, inst)
	noInstallData = true
	Reset()
	cur := Current()
	if cur.Mode != ModeUser || cur.Dir != UserDir() || cur.Note == "" {
		t.Fatalf("%+v", cur)
	}
	if _, err := os.Stat(filepath.Join(inst, "data")); err == nil {
		t.Fatal("不应在程序目录下建 data")
	}
}

func TestResolveOrder(t *testing.T) {
	home := sandbox(t)
	inst := filepath.Join(home, "D-drive", "Programs", "Ferry")
	os.MkdirAll(inst, 0o755)
	installAt(t, inst)
	// 默认：安装目录下的 data
	if c := Current(); c.Mode != ModeInstall || c.Dir != filepath.Join(inst, "data") || c.DefaultDir != c.Dir {
		t.Fatalf("默认：%+v", c)
	}
	// 手动设置的位置：记录在安装目录的 data 里
	custom := filepath.Join(home, "data-elsewhere")
	if err := SetLocation(custom); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inst, "data", "location.json")); err != nil {
		t.Fatal("位置记录应在安装目录的 data 里")
	}
	if c := Current(); c.Mode != ModeCustom || c.Dir != custom {
		t.Fatalf("自定义：%+v", c)
	}
	t.Setenv("FERRY_HOME", filepath.Join(home, "env"))
	Reset()
	if c := Current(); c.Mode != ModeEnv {
		t.Fatalf("环境变量优先：%+v", c)
	}
	t.Setenv("FERRY_HOME", "")
	if err := SetLocation(""); err != nil {
		t.Fatal(err)
	}
	if c := Current(); c.Mode != ModeInstall {
		t.Fatalf("恢复默认：%+v", c)
	}
}

func TestResolveFallsBackToUserDir(t *testing.T) {
	home := sandbox(t)
	// 开发时程序在临时目录里：用用户目录
	installAt(t, filepath.Join(os.TempDir(), "go-build123", "exe"))
	if c := Current(); c.Mode != ModeUser || !strings.HasPrefix(c.Dir, home) || c.Note == "" {
		t.Fatalf("临时目录：%+v", c)
	}
}

func TestOldPointerMovesToInstallDir(t *testing.T) {
	home := sandbox(t)
	user := UserDir()
	os.MkdirAll(user, 0o700)
	custom := filepath.Join(home, "mydata")
	os.WriteFile(filepath.Join(user, "location.json"), []byte(`{"dataDir":"`+filepath.ToSlash(custom)+`"}`), 0o600)
	inst := filepath.Join(home, "inst")
	os.MkdirAll(inst, 0o755)
	installAt(t, inst)
	if c := Current(); c.Mode != ModeCustom || c.Dir != abs(custom) {
		t.Fatalf("旧的位置记录应继续有效：%+v", c)
	}
	if _, err := os.Stat(filepath.Join(user, "location.json")); err == nil {
		t.Fatal("旧的位置记录应已挪到安装目录")
	}
}

func TestMigrateUserDir(t *testing.T) {
	home := sandbox(t)
	user := UserDir()
	os.MkdirAll(filepath.Join(user, "keys"), 0o700)
	os.WriteFile(filepath.Join(user, "settings.json"), []byte(`{"ui":{}}`), 0o600)
	os.WriteFile(filepath.Join(user, "keys", "id_ed25519"), []byte("KEY"), 0o600)
	os.WriteFile(filepath.Join(user, "known_hosts"), []byte("h1 ssh-ed25519 AAAA"), 0o600)
	inst := filepath.Join(home, "inst")
	os.MkdirAll(inst, 0o755)
	installAt(t, inst)
	msg, from, err := MigrateUserDir()
	if err != nil || !strings.Contains(msg, "搬到安装目录") || from != user {
		t.Fatalf("%q %q %v", msg, from, err)
	}
	data := filepath.Join(inst, "data")
	if b, _ := os.ReadFile(filepath.Join(data, "keys", "id_ed25519")); string(b) != "KEY" {
		t.Fatal("私钥应已搬过来")
	}
	if _, err := os.Stat(filepath.Join(user, "settings.json")); err == nil {
		t.Fatal("旧位置的数据应已删除")
	}
	if msg, _, _ := MigrateUserDir(); msg != "" {
		t.Fatalf("只搬一次：%q", msg)
	}
	// 两边都有设置文件：不合并，只提示
	os.MkdirAll(user, 0o700)
	os.WriteFile(filepath.Join(user, "settings.json"), []byte(`{}`), 0o600)
	if msg, _, _ := MigrateUserDir(); !strings.Contains(msg, "没有自动合并") {
		t.Fatalf("两边都有：%q", msg)
	}
	// 旧位置只剩记录文件与空目录（开发版运行时留下的）：不算旧数据，顺手清掉
	os.RemoveAll(user)
	os.MkdirAll(filepath.Join(user, "tmp"), 0o700)
	os.WriteFile(filepath.Join(user, ".migrated-v1"), nil, 0o600)
	if msg, _, _ := MigrateUserDir(); msg != "" {
		t.Fatalf("只有记录文件不应提示：%q", msg)
	}
	if _, err := os.Stat(user); err == nil {
		t.Fatal("只有记录文件的旧目录应已删除")
	}
}

func TestMigrateLegacyAndProject(t *testing.T) {
	home := sandbox(t)
	legacy := filepath.Join(home, ".ferry")
	os.MkdirAll(legacy, 0o700)
	os.WriteFile(filepath.Join(legacy, "known_hosts"), []byte("[h]:22 ssh-ed25519 AAAA\n[h2]:22 ssh-ed25519 BBBB\n"), 0o600)
	os.WriteFile(KnownHosts(), []byte("[h]:22 ssh-ed25519 AAAA\n"), 0o600)
	msg, err := MigrateLegacy()
	if err != nil || !strings.Contains(msg, "1 条") {
		t.Fatalf("%q %v", msg, err)
	}
	b, _ := os.ReadFile(KnownHosts())
	if strings.Count(string(b), "ssh-ed25519") != 2 {
		t.Fatalf("合并结果：%s", b)
	}
	if msg, _ := MigrateLegacy(); msg != "" {
		t.Fatal("只迁移一次")
	}

	cfgDir := filepath.Join(home, "Desktop")
	old := filepath.Join(cfgDir, ".ferry")
	os.MkdirAll(filepath.Join(old, "logs"), 0o755)
	os.WriteFile(filepath.Join(old, "logs", "build-1.log"), []byte("log"), 0o644)
	os.WriteFile(filepath.Join(old, "history.jsonl"), []byte("{}\n"), 0o644)
	cfg := filepath.Join(cfgDir, "ferry.yaml")
	msg, err = MigrateProject("demo", cfg)
	if err != nil || msg == "" {
		t.Fatalf("%q %v", msg, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("搬完后旧 .ferry 应删除")
	}
	pd := ProjectDir("demo", cfg)
	if _, err := os.Stat(filepath.Join(pd, "logs", "build-1.log")); err != nil {
		t.Fatal("日志应搬到项目目录")
	}
	if b, _ := os.ReadFile(filepath.Join(pd, "history.jsonl")); string(b) != "{}\n" {
		t.Fatal("历史应搬到项目目录")
	}
}

func TestMove(t *testing.T) {
	home := sandbox(t)
	from := Dir()
	os.MkdirAll(filepath.Join(from, "keys"), 0o700)
	os.WriteFile(filepath.Join(from, "keys", "k"), []byte("secret"), 0o600)
	os.WriteFile(filepath.Join(from, "known_hosts"), []byte("a\n"), 0o600)
	to := filepath.Join(home, "new")
	os.MkdirAll(to, 0o700)
	os.WriteFile(filepath.Join(to, "known_hosts"), []byte("b\n"), 0o600)
	if err := Move(from, to); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "keys", "k")); string(b) != "secret" {
		t.Fatal("密钥应复制过去")
	}
	if b, _ := os.ReadFile(filepath.Join(to, "known_hosts")); string(b) != "b\na\n" {
		t.Fatalf("known_hosts 应合并：%q", b)
	}
	if _, err := os.Stat(filepath.Join(from, "keys")); !os.IsNotExist(err) {
		t.Fatal("原位置的数据应删除")
	}
	if err := Move(to, filepath.Join(to, "inner")); err == nil {
		t.Fatal("不能搬到自己里面")
	}
}

// 配置文件移动后，项目目录跟着改名；新位置已有项目目录时合并
func TestMoveProject(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	Reset()
	t.Cleanup(Reset)
	cfg := t.TempDir()
	a, b, c := filepath.Join(cfg, "a", "ferry.yaml"), filepath.Join(cfg, "b", "ferry.yaml"), filepath.Join(cfg, "c", "ferry.yaml")
	old := ProjectDir("demo", a)
	os.MkdirAll(filepath.Join(old, "logs"), 0o700)
	os.WriteFile(filepath.Join(old, "local.json"), []byte(`{"vars":{"x":"1"}}`), 0o600)
	os.WriteFile(filepath.Join(old, "history.jsonl"), []byte("a1\n"), 0o600)
	os.WriteFile(filepath.Join(old, "logs", "build-1.log"), []byte("log"), 0o600)

	to, err := MoveProject("demo", a, b)
	if err != nil || to != ProjectPath("demo", b) {
		t.Fatalf("%q %v", to, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("旧目录还在")
	}
	if got, _ := os.ReadFile(filepath.Join(to, "local.json")); string(got) != `{"vars":{"x":"1"}}` {
		t.Fatalf("local.json：%s", got)
	}

	// 合并：c 那里以前用过（有部署历史和本机设置）
	dst := ProjectDir("demo", c)
	os.WriteFile(filepath.Join(dst, "history.jsonl"), []byte("c1\n"), 0o600)
	os.WriteFile(filepath.Join(dst, "local.json"), []byte(`{}`), 0o600)
	if _, err := MoveProject("demo", b, c); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "history.jsonl")); string(got) != "c1\na1\n" {
		t.Fatalf("history：%q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "local.json")); string(got) != `{}` {
		t.Fatalf("新位置的本机设置不动：%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "logs", "build-1.log")); string(got) != "log" {
		t.Fatalf("构建日志：%q", got)
	}
	if _, err := os.Stat(ProjectPath("demo", b)); !os.IsNotExist(err) {
		t.Fatal("合并后旧目录还在")
	}
	// 原来没有数据
	if to, err := MoveProject("demo", a, b); err != nil || to != "" {
		t.Fatalf("%q %v", to, err)
	}
}
