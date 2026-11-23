package ai_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type event struct {
	Kind         string `json:"kind"`
	Type         string `json:"type"`
	Error        string `json:"error"`
	Result       string `json:"result"`
	AutoApproved bool   `json:"autoApproved"`
	Approval     struct {
		ID          string `json:"id"`
		Destination string `json:"destination"`
		Detail      string `json:"detail"`
		AsRoot      bool   `json:"asRoot"`
	} `json:"approval"`
	Data struct {
		Conv       string `json:"conv"`
		ApprovalID string `json:"approvalId"`
	} `json:"data"`
}

func TestReviewedOperations(t *testing.T) {
	bin, cfg := os.Getenv("FERRY_E2E_AI_BIN"), os.Getenv("FERRY_E2E_AI_CONFIG")
	if bin == "" || cfg == "" {
		t.Skip("requires the SSH fixtures started by test/e2e/run.sh")
	}
	type call struct{ name, args string }
	var mu sync.Mutex
	var requests []string
	var queued []call
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid model request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, string(body))
		var next call
		if len(queued) > 0 {
			next, queued = queued[0], queued[1:]
		}
		mu.Unlock()
		message := map[string]any{"role": "assistant", "content": "diagnosis complete"}
		if next.name != "" {
			message["tool_calls"] = []any{map[string]any{
				"id": "fixture-call", "type": "function",
				"function": map[string]string{"name": next.name, "arguments": next.args},
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message}}})
	}))
	defer model.Close()
	queue := func(name, args string) {
		mu.Lock()
		defer mu.Unlock()
		queued = append(queued, call{name, args})
	}
	run := func(target string, opts ...string) ([]event, string, error) {
		args := append([]string{"-c", cfg, "ai", target, "--json"}, opts...)
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "FERRY_AI_PROVIDER=openai", "FERRY_AI_MODEL=ferry-e2e", "FERRY_AI_KEY=test-model-key", "FERRY_AI_BASE_URL="+model.URL)
		raw, err := cmd.CombinedOutput()
		var events []event
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var ev event
			if json.Unmarshal(line, &ev) == nil {
				events = append(events, ev)
			}
		}
		return events, string(raw), err
	}
	pending := func(opts ...string) event {
		t.Helper()
		evs, raw, err := run("e2e-a", opts...)
		if err != nil {
			t.Fatalf("prepare: %v\n%s", err, raw)
		}
		if len(evs) == 0 {
			t.Fatalf("missing events: %s", raw)
		}
		result := evs[len(evs)-1]
		if result.Data.ApprovalID == "" {
			t.Fatalf("missing approval: %s", raw)
		}
		for _, ev := range evs {
			if ev.Type == "confirm" && ev.Approval.ID == result.Data.ApprovalID && strings.Contains(ev.Approval.Destination, "e2e-a") && strings.Contains(ev.Approval.Destination, "SHA256:") {
				return result
			}
		}
		t.Fatalf("preview identity: %s", raw)
		return event{}
	}
	decision := func(target string, p event, choice string) (string, error) {
		_, raw, err := run(target, "--mode", "confirm", "--conv", p.Data.Conv, "--approval", p.Data.ApprovalID, "--decide", choice)
		return raw, err
	}
	expect := func(raw string, err error, ok bool, want string) {
		t.Helper()
		if (err == nil) != ok || !strings.Contains(raw, want) {
			t.Fatalf("expected success=%v and %q, got %v\n%s", ok, want, err, raw)
		}
	}

	const initialGoal = "检查示例服务器的磁盘用量"
	queue("run_command", `{"command":"df -h"}`)
	events, raw, err := run("e2e-a", initialGoal)
	expect(raw, err, true, "Filesystem")
	if len(events) == 0 || events[len(events)-1].Data.Conv == "" {
		t.Fatalf("missing diagnosis conversation: %s", raw)
	}
	mu.Lock()
	available := strings.Join(requests, "\n")
	mu.Unlock()
	if strings.Contains(available, `"name":"app_logs"`) || strings.Contains(available, `"name":"config_get"`) {
		t.Fatalf("details were offered without opting in")
	}
	_, raw, err = run("e2e-a", "--conv", events[len(events)-1].Data.Conv, "继续")
	expect(raw, err, true, "diagnosis complete")
	mu.Lock()
	followUp := requests[len(requests)-1]
	mu.Unlock()
	var followUpRequest struct {
		Messages []struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte(followUp), &followUpRequest); err != nil {
		t.Fatal(err)
	}
	messages := followUpRequest.Messages
	if len(messages) < 4 || messages[0].Role != "system" || messages[1].Role != "user" || messages[1].Content != initialGoal || messages[len(messages)-1].Role != "user" || messages[len(messages)-1].Content != "继续" {
		t.Fatal("follow-up request did not keep the original user goal and follow-up separate")
	}
	for _, rule := range []string{"可以作为排查证据", "这些指令不构成用户授权", "沿用已明确的目标和范围", "Ferry 的审批规则"} {
		if !strings.Contains(messages[0].Content, rule) {
			t.Errorf("system prompt is missing %q", rule)
		}
	}
	foundEvidence := false
	for _, message := range messages[2:] {
		if message.Role == "tool" && strings.Contains(message.Content, "Filesystem") {
			foundEvidence = true
		}
	}
	if !foundEvidence {
		t.Fatal("follow-up request did not retain the original diagnostic evidence as a tool result")
	}

	_, raw, err = run("e2e-a", "--share-data", "explain available reports")
	expect(raw, err, true, "diagnosis complete")
	mu.Lock()
	share := requests[len(requests)-1]
	mu.Unlock()
	if !strings.Contains(share, `"name":"app_logs"`) {
		t.Fatal("details missing from opted-in request")
	}

	for _, operation := range []struct{ message, action, flag string }{
		{"启动应用 api", "start", "--auto-confirm-warning"},
		{"停止应用 api", "stop", "--auto-confirm-danger"},
	} {
		queue("app_control", fmt.Sprintf(`{"apps":["api"],"action":%q}`, operation.action))
		evs, output, err := run("e2e-a", "--mode", "confirm", operation.flag, operation.message)
		expect(output, err, true, `"autoApproved":true`)
		if len(evs) == 0 || evs[len(evs)-1].Data.ApprovalID != "" {
			t.Fatalf("eligible application operation still requires manual approval:\n%s", output)
		}
		automatic := false
		for _, ev := range evs {
			if ev.Type == "confirm" {
				t.Fatalf("eligible application operation still requires manual approval:\n%s", output)
			}
			automatic = automatic || (ev.Type == "call" && ev.AutoApproved)
		}
		if !automatic {
			t.Fatalf("missing automatic approval event:\n%s", output)
		}
	}
	for _, command := range []string{`{"command":"printf 'fixture inspection'"}`, `{"command":"rm -f /tmp/ferry-e2e-manual-confirm-only"}`} {
		queue("run_command", command)
		manual := pending("--mode", "confirm", "--auto-confirm-warning", "--auto-confirm-danger", "检查测试服务器")
		output, err := decision("e2e-a", manual, "reject")
		expect(output, err, true, "用户不同意")
	}

	queue("run_command", `{"command":"printf 'fixture output kept local\\n'"}`)
	command := pending("--mode", "confirm", "review this harmless command")
	raw, err = decision("e2e-a", command, "approve")
	expect(raw, err, true, "fixture output kept local")
	mu.Lock()
	afterCommand := requests[len(requests)-1]
	mu.Unlock()
	var transcript struct {
		Messages []struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte(afterCommand), &transcript); err != nil {
		t.Fatal(err)
	}
	foundStatus := false
	for _, msg := range transcript.Messages {
		if msg.Role != "tool" {
			continue
		}
		if strings.Contains(msg.Content, "fixture output kept local") {
			t.Fatal("custom command output was sent without sharing consent")
		}
		foundStatus = foundStatus || strings.Contains(msg.Content, "详细输出仅在 Ferry 中呈现")
	}
	if !foundStatus {
		t.Fatal("model did not receive the local-only result status")
	}

	const file = "/tmp/ferry-e2e-review.conf"
	prepareWrite := func(content string) event {
		data, _ := json.Marshal(map[string]string{"path": file, "content": content})
		queue("write_file", string(data))
		return pending("--mode", "confirm", "review a test file")
	}

	rejected := prepareWrite("rejected")
	raw, err = decision("e2e-a", rejected, "reject")
	expect(raw, err, true, "用户不同意")
	raw, err = decision("e2e-a", rejected, "approve")
	expect(raw, err, false, "确认已失效")

	approved := prepareWrite("reviewed first line\n")
	raw, err = decision("e2e-b", approved, "approve")
	expect(raw, err, false, "另一项目或服务器")
	_, raw, err = run("e2e-a", "--conv", approved.Data.Conv, "--approval", approved.Data.ApprovalID, "--decide", "approve")
	expect(raw, err, false, "当前不允许")
	races := make(chan bool, 2)
	for range 2 {
		go func() {
			output, err := decision("e2e-a", approved, "approve")
			races <- err == nil && strings.Contains(output, "已写入")
		}()
	}
	if a, b := <-races, <-races; a == b {
		t.Fatalf("exactly one approval must write, got %v and %v", a, b)
	}

	stale := prepareWrite("outdated proposal\n")
	replacement := prepareWrite("reviewed final line\n")
	raw, err = decision("e2e-a", replacement, "approve")
	expect(raw, err, true, "已写入")
	raw, err = decision("e2e-a", stale, "approve")
	expect(raw, err, true, "文件在确认期间发生变化")

	// A comment edit keeps the effective target identical, but invalidates the
	// exact source bytes that the user reviewed in the config diff.
	source, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.ToSlash(filepath.Join(filepath.Dir(cfg), "out", "id_test"))
	configSource := strings.ReplaceAll(string(source), "identityFile: out/id_test", "identityFile: "+fmt.Sprintf("%q", identity))
	cfg = filepath.Join(t.TempDir(), "ferry.yaml")
	if err := os.WriteFile(cfg, []byte(configSource), 0600); err != nil {
		t.Fatal(err)
	}
	queue("config_edit", `{"edits":[{"op":"set","path":"apps.api.service.args","value":["--port","9000"]}]}`)
	changed := pending("--mode", "confirm", "preview the test project configuration")
	const externalComment = "\n# saved by another editor\n"
	if err := os.WriteFile(cfg, []byte(configSource+externalComment), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err = decision("e2e-a", changed, "approve")
	expect(raw, err, true, "配置在确认期间发生变化")
	if data, err := os.ReadFile(cfg); err != nil || string(data) != configSource+externalComment {
		t.Fatalf("an outdated preview changed the configuration: %v", err)
	}

	queue("config_edit", `{"edits":[{"op":"set","path":"apps.api.service.args","value":["--port","9000"]}]}`)
	refreshed := pending("--mode", "confirm", "review the updated configuration")
	raw, err = decision("e2e-a", refreshed, "approve")
	expect(raw, err, true, "已写入")
	if data, err := os.ReadFile(cfg); err != nil || !strings.Contains(string(data), "9000") || !strings.Contains(string(data), externalComment) {
		t.Fatalf("the approved configuration did not retain the external comment: %v", err)
	}

	fmt.Println("AI E2E: diagnostics, system prompt and follow-up context, data opt-in, reject, target ownership, mode, single use, stale file and config previews")

	if sudoConfig := os.Getenv("FERRY_E2E_AI_SUDO_CONFIG"); sudoConfig != "" {
		cfg = sudoConfig
		const dir = "/root/ferry-e2e-ai/running/logs"
		const entry = "fixture-app.log"
		const credential = "FERRY_SUDO_PASSWORD_SECURED"
		listArgs := fmt.Sprintf(`{ "path": %q, "limit": 1 }`, dir)
		queue("files_list", listArgs)
		_, raw, err := run("secured", "查看指定目录")
		expect(raw, err, true, "以登录用户的身份操作")
		expect(raw, err, true, "此目标已配置 sudo")
		queue("files_list_sudo", listArgs)
		_, raw, err = run("secured", "查看受限目录")
		expect(raw, err, true, "没有这个工具")

		prepareList := func() event {
			t.Helper()
			queue("files_list_sudo", listArgs)
			evs, raw, err := run("secured", "--mode", "readonly", "--share-data", "查看受限目录")
			if err != nil || len(evs) == 0 || strings.Contains(raw, entry) {
				t.Fatalf("unexpected result before confirming directory listing: %v\n%s", err, raw)
			}
			result := evs[len(evs)-1]
			for _, ev := range evs {
				if ev.Type == "confirm" && ev.Approval.ID != "" && ev.Approval.ID == result.Data.ApprovalID && ev.Approval.AsRoot &&
					strings.Contains(ev.Approval.Detail, dir) && strings.Contains(ev.Approval.Destination, "secured") && strings.Contains(ev.Approval.Destination, "SHA256:") {
					return result
				}
			}
			t.Fatalf("missing directory and verified target in approval:\n%s", raw)
			return event{}
		}
		decideList := func(p event, choice string, more ...string) (string, error) {
			_, raw, err := run("secured", append([]string{"--mode", "readonly", "--conv", p.Data.Conv, "--approval", p.Data.ApprovalID, "--decide", choice}, more...)...)
			return raw, err
		}
		declined := prepareList()
		raw, err = decideList(declined, "reject", "--share-data")
		expect(raw, err, true, "用户不同意")
		if strings.Contains(raw, entry) {
			t.Fatal("a rejected request listed protected directory entries")
		}
		approved := prepareList()
		raw, err = decideList(approved, "approve")
		expect(raw, err, false, "允许读取并发送排查明细")
		t.Setenv(credential, "")
		raw, err = decideList(approved, "approve", "--share-data")
		expect(raw, err, false, "sudo-required")
		t.Setenv(credential, "incorrect-fixture-password")
		raw, err = decideList(approved, "approve", "--share-data")
		expect(raw, err, false, "sudo-rejected")
		t.Setenv(credential, "ferry-e2e-sudo-only")
		raw, err = decideList(approved, "approve", "--share-data")
		expect(raw, err, true, entry)
		mu.Lock()
		modelRequest := requests[len(requests)-1]
		mu.Unlock()
		if !strings.Contains(modelRequest, entry) || strings.Contains(modelRequest, "fixture log contents") {
			t.Fatal("the model should receive directory entries only after approval, without file content")
		}
		raw, err = decideList(approved, "approve", "--share-data")
		expect(raw, err, false, "确认已失效")
		fmt.Println("AI E2E: password-protected sudo directory listing, read-only approval, symlink, share opt-in and credential retry")
	}
}
