package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
)

func TestLoadSaveDefaults(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	s, err := Load()
	if err != nil || s.Connect.TimeoutSec != 15 || s.UI.Theme != "system" || !s.UI.ConfirmClose || !s.UI.Glass || s.Logs.BuildLogDays != 30 {
		t.Fatalf("默认值：%+v %v", s, err)
	}
	if s.Connect.StatusMinutes != 1 {
		t.Fatalf("自动读取应用状态默认每分钟：%d", s.Connect.StatusMinutes)
	}
	if s.UI.GlassTransparency != DefaultGlassTransparency {
		t.Fatalf("玻璃透明度默认 90：%d", s.UI.GlassTransparency)
	}
	if s.UI.TaskKeep != 50 || s.UI.TaskKeepDays != 30 || s.UI.TaskMaxMB != 100 || s.AI.KeepDays != 30 || s.AI.MaxMB != 200 {
		t.Fatalf("保留设置的默认值：%+v %+v", s.UI, s.AI)
	}
	// 没有 AI 模型时是空列表：桌面版直接读 models.length
	if b, _ := json.Marshal(s.AI); !strings.Contains(string(b), `"models":[]`) {
		t.Fatalf("models 应为 []：%s", b)
	}
	s.Build.Env = map[string]string{"JAVA_HOME": "/jdk", "PATH": "ignored"}
	s.Build.Path = []string{"/opt/maven/bin"}
	s.UI.Theme = "weird"
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	got, _ := Load()
	if got.UI.Theme != "system" || got.Build.Env["JAVA_HOME"] != "/jdk" {
		t.Fatalf("%+v", got)
	}
	env := strings.Join(got.BuildEnv(), "\n")
	if !strings.Contains(env, "JAVA_HOME=/jdk") || !strings.Contains(env, "PATH=/opt/maven/bin"+string(os.PathListSeparator)) || strings.Contains(env, "ignored") {
		t.Fatalf("构建环境：%s", env)
	}
}

// 自动读取应用状态的间隔：旧文件没有这一项按每分钟，负数是关闭，太大的收到 60
func TestStatusMinutes(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	for in, want := range map[string]int{`{}`: 1, `{"connect":{"statusMinutes":5}}`: 5, `{"connect":{"statusMinutes":-1}}`: StatusOff, `{"connect":{"statusMinutes":-7}}`: StatusOff, `{"connect":{"statusMinutes":600}}`: 60} {
		if err := os.WriteFile(appdir.SettingsFile(), []byte(in), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Load()
		if err != nil || s.Connect.StatusMinutes != want {
			t.Fatalf("%s → %d %v，应为 %d", in, s.Connect.StatusMinutes, err, want)
		}
	}
}

// 玻璃透明度：旧文件没有这一项按默认值，0 保留（最不透明），超出范围的收到 0～100
func TestGlassTransparency(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	for in, want := range map[string]int{`{}`: DefaultGlassTransparency, `{"ui":{"glassTransparency":0}}`: 0, `{"ui":{"glassTransparency":80}}`: 80, `{"ui":{"glassTransparency":-5}}`: 0, `{"ui":{"glassTransparency":300}}`: 100} {
		if err := os.WriteFile(appdir.SettingsFile(), []byte(in), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Load()
		if err != nil || s.UI.GlassTransparency != want {
			t.Fatalf("%s → %d %v，应为 %d", in, s.UI.GlassTransparency, err, want)
		}
	}
}

func TestCleanBuildLogs(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "build-old.log")
	recent := filepath.Join(dir, "build-new.log")
	other := filepath.Join(dir, "keep.txt")
	for _, p := range []string{old, recent, other} {
		os.WriteFile(p, nil, 0o644)
	}
	past := time.Now().AddDate(0, 0, -40)
	os.Chtimes(old, past, past)
	os.Chtimes(other, past, past)
	Defaults().CleanBuildLogs(dir)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("过期日志应删除")
	}
	for _, p := range []string{recent, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s 不应删除", p)
		}
	}
}

// 旧版本只有一个模型：转成模型列表里的一个，成为默认模型
func TestAIModelsMigrate(t *testing.T) {
	s := Settings{AI: AI{Provider: "openai", BaseURL: " https://api.deepseek.com/v1 ", Model: "deepseek-chat"}}
	s.normalize()
	if len(s.AI.Models) != 1 || s.AI.Default != LegacyModelID || s.AI.Provider != "" {
		t.Fatalf("迁移：%+v", s.AI)
	}
	m := s.AI.Models[0]
	if m.ID != LegacyModelID || m.Name != "deepseek-chat" || m.BaseURL != "https://api.deepseek.com/v1" || m.Provider != "openai" {
		t.Fatalf("模型：%+v", m)
	}
	// ID 重复或无效时重新编号；思考强度不认识的清空；默认模型不存在时用第一个
	s = Settings{AI: AI{Default: "nope", Models: []AIModel{
		{ID: "ds", Provider: "openai", Model: "deepseek-chat", Effort: "high"},
		{ID: "ds", Model: "claude-opus-5-5", Effort: "huge"},
		{ID: "Bad ID!", Name: "千问", Provider: "openai", Model: "qwen-plus"},
	}}}
	s.normalize()
	ids := []string{s.AI.Models[0].ID, s.AI.Models[1].ID, s.AI.Models[2].ID}
	if ids[0] != "ds" || ids[1] == "ds" || ids[2] == "" || s.AI.Default != "ds" {
		t.Fatalf("ID：%v 默认 %s", ids, s.AI.Default)
	}
	if s.AI.Models[1].Provider != "anthropic" || s.AI.Models[1].Effort != "" || s.AI.Models[0].Effort != "high" {
		t.Fatalf("%+v", s.AI.Models)
	}
	if m, ok := s.AI.FindModel(""); !ok || m.ID != "ds" {
		t.Fatalf("默认模型：%+v", m)
	}
	if _, ok := s.AI.FindModel("missing"); ok {
		t.Fatal("没有的模型")
	}
}
