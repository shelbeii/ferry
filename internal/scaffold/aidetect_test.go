package scaffold

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/captain/ferry/internal/ai"
)

// 假模型（OpenAI 兼容、不支持流式）：按顺序调用工具，记下每次请求的内容。
type fakeAI struct {
	mu      sync.Mutex
	replies []string
	bodies  []string
}

func (f *fakeAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	f.bodies = append(f.bodies, string(b))
	if len(f.replies) == 0 {
		http.Error(w, `{"error":{"message":"没有更多回复"}}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, f.replies[0])
	f.replies = f.replies[1:]
}

// call 模型这一轮调用一个工具。
func call(id, name string, args any) string {
	a, _ := json.Marshal(args)
	m := map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
		"role": "assistant", "content": "",
		"tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(a)}}},
	}}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10}}
	b, _ := json.Marshal(m)
	return string(b)
}

func say(text string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}}})
	return string(b)
}

func TestAIDetect(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("server/Cargo.toml", "[package]\nname = \"shop-api\"\n")
	write("server/src/main.rs", "fn main() {}\n")
	write("server/config.toml", "port = 9090\n")
	write(".env", "DB_PASSWORD=super-secret-value\n")
	write("node_modules/x/index.js", "x")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("outside-content"), 0o644)

	app := map[string]any{"name": "shop-api", "type": "binary", "kind": "Rust（Cargo）", "workdir": "server",
		"command": "cargo build --release --target x86_64-unknown-linux-gnu", "artifact": "server/target/x86_64-unknown-linux-gnu/release/shop-api",
		"port": 9090, "reason": "server/Cargo.toml 是 Rust 工程，config.toml 里 port = 9090", "notes": []string{"要装 Linux 交叉编译工具链"}}
	bad := map[string]any{}
	for k, v := range app {
		bad[k] = v
	}
	bad["workdir"] = "srv"
	fm := &fakeAI{replies: []string{
		call("c1", "read_file", map[string]any{"path": ".env"}),
		call("c2", "read_file", map[string]any{"path": "../" + filepath.Base(filepath.Dir(outside)) + "/outside.txt"}),
		call("c3", "find_files", map[string]any{"pattern": "*.toml"}),
		call("c4", "read_file", map[string]any{"path": "server/config.toml"}),
		call("c5", "submit_apps", map[string]any{"apps": []any{bad}, "summary": "x"}),
		call("c6", "submit_apps", map[string]any{"apps": []any{app}, "summary": "找到一个 Rust 后端。"}),
		say("找到一个 Rust 后端，要确认交叉编译。"),
	}}
	srv := httptest.NewServer(fm)
	defer srv.Close()
	p, err := ai.NewProvider(ai.Config{Provider: ai.OpenAI, BaseURL: srv.URL + "/v1", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	r, err := AIDetect(context.Background(), dir, AIOptions{Provider: p, Hint: "后端是 Rust", Emit: func(ev ai.Event) {
		if ev.Type == "output" {
			progress = append(progress, ev.Text)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Apps) != 1 {
		t.Fatalf("应有 1 个应用：%+v", r.Apps)
	}
	a := r.Apps[0]
	if !a.AI || a.Reason == "" || a.Type != "binary" || a.Workdir != "server" || a.Port != 9090 || len(a.Notes) != 1 {
		t.Fatalf("应用不对：%+v", a)
	}
	if r.Summary != "找到一个 Rust 后端。" || r.Usage.Input == 0 {
		t.Fatalf("总结、用量不对：%q %+v", r.Summary, r.Usage)
	}
	all := strings.Join(fm.bodies, "\n")
	for _, leak := range []string{"super-secret-value", "outside-content"} {
		if strings.Contains(all, leak) {
			t.Fatalf("不该发给模型：%s", leak)
		}
	}
	for _, want := range []string{"不能读", "只能看项目目录里的东西", "server/Cargo.toml", "port = 9090", "workdir srv 不是项目里的目录", "后端是 Rust", "node_modules/（没有展开）"} {
		if !strings.Contains(all, want) && !strings.Contains(all, jsonEscape(want)) {
			t.Fatalf("请求里应有 %q", want)
		}
	}
	if len(progress) == 0 {
		t.Fatal("应有读文件的进展")
	}
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// 模型说完话却没有交回结果：报错，带上它说的话。
func TestAIDetectNoSubmit(t *testing.T) {
	dir := t.TempDir()
	fm := &fakeAI{replies: []string{say("这个目录是空的，没有能部署的东西。")}}
	srv := httptest.NewServer(fm)
	defer srv.Close()
	p, _ := ai.NewProvider(ai.Config{Provider: ai.OpenAI, BaseURL: srv.URL + "/v1", Model: "m", APIKey: "k"})
	_, err := AIDetect(context.Background(), dir, AIOptions{Provider: p})
	if err == nil || !strings.Contains(err.Error(), "目录是空的") {
		t.Fatalf("应报没有结果：%v", err)
	}
}

// 规则认对了的应用原样保留（不标 AI）。
func TestMergeAIAppsKeepsRules(t *testing.T) {
	rules := []Suggestion{{Name: "api", Type: "binary", Workdir: ".", Command: "go build -o build/api .", Artifact: "build/api", Kind: "Go 程序", Notes: []string{"规则的提醒"}}}
	got := mergeAIApps(t.TempDir(), rules, []aiApp{
		{Name: "api", Type: "binary", Workdir: "./", Command: "go build -o build/api .", Artifact: "build/api", Reason: "go.mod"},
		{Name: "API", Type: "static", Workdir: ".", Artifact: "web/dist", Reason: "vite"},
	})
	if len(got) != 2 || got[0].AI || got[0].Notes[0] != "规则的提醒" || !got[1].AI || got[1].Name != "API-2" {
		t.Fatalf("合并不对：%+v", got)
	}
}

func TestSensitive(t *testing.T) {
	for _, n := range []string{".env", ".env.production", "id_rsa", "server.key", "cert.pem", ".npmrc", "keystore.jks"} {
		if !sensitive(n) {
			t.Errorf("%s 应不能读", n)
		}
	}
	for _, n := range []string{".env.example", "application.yml", "pom.xml", "Makefile"} {
		if sensitive(n) {
			t.Errorf("%s 应能读", n)
		}
	}
}
