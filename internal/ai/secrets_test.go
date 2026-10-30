package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestSecretFields(t *testing.T) {
	ok := []SecretField{{Name: "mysql_root", Label: "MySQL root 新密码", Confirm: true}, {Name: "user1", Label: "账号", Kind: "text"}}
	if err := CheckSecretFields(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]SecretField{
		nil,
		{{Name: "MySQL", Label: "x"}},
		{{Name: "a", Label: ""}},
		{{Name: "a", Label: "x"}, {Name: "a", Label: "y"}},
		{{Name: "a", Label: "x", Kind: "number"}},
		make([]SecretField, MaxSecretFields+1),
	} {
		if CheckSecretFields(bad) == nil {
			t.Errorf("应该不通过：%+v", bad)
		}
	}
}

func TestSecretsFillAndCommand(t *testing.T) {
	conv := "t-fill"
	defer ForgetSecrets(conv)
	SetSecret(conv, "db_pass", "p@ss 'w0rd'")

	if got := SecretNames(`mysql -p"$FERRY_SECRET_DB_PASS" {{secret:db_pass}} {{ secret:other }}`); strings.Join(got, ",") != "db_pass,other" {
		t.Fatalf("SecretNames = %v", got)
	}
	out, err := FillSecrets(conv, "requirepass {{secret:db_pass}}\n")
	if err != nil || out != "requirepass p@ss 'w0rd'\n" {
		t.Fatalf("FillSecrets = %q, %v", out, err)
	}
	if _, err := FillSecrets(conv, "{{secret:nope}}"); err == nil || !strings.Contains(err.Error(), "ask_secret") {
		t.Fatalf("没填过的应该报错：%v", err)
	}

	cmd, env, err := CommandSecrets(conv, `echo {{secret:db_pass}} | x --pw "$FERRY_SECRET_DB_PASS"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "p@ss") || cmd != `echo ${FERRY_SECRET_DB_PASS} | x --pw "$FERRY_SECRET_DB_PASS"` {
		t.Fatalf("命令里不应该有值：%q", cmd)
	}
	if len(env) != 1 || env["FERRY_SECRET_DB_PASS"] != "p@ss 'w0rd'" {
		t.Fatalf("env = %v", env)
	}
	if c, e, err := CommandSecrets(conv, "ls -l"); err != nil || e != nil || c != "ls -l" {
		t.Fatalf("没用到代号的命令不变：%q %v %v", c, e, err)
	}
	if _, _, err := CommandSecrets(conv, `echo "$FERRY_SECRET_MISSING"`); err == nil {
		t.Fatal("没填过的应该报错")
	}

	line := strings.TrimSuffix(string(SecretEnvLine(env)), "\n")
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil || string(b) != `export FERRY_SECRET_DB_PASS='p@ss '\''w0rd'\'''`+"\n" {
		t.Fatalf("SecretEnvLine = %q, %v", b, err)
	}
}

func TestMaskSecrets(t *testing.T) {
	conv := "t-mask"
	defer ForgetSecrets(conv)
	SetSecret(conv, "a", "Secr3t-Value")
	SetSecret(conv, "short", "abc") // 太短的不遮
	in := "pass=Secr3t-Value b64=" + base64.StdEncoding.EncodeToString([]byte("Secr3t-Value")) + " abc"
	got := MaskSecrets(conv, in)
	if strings.Contains(got, "Secr3t") || strings.Contains(got, base64.StdEncoding.EncodeToString([]byte("Secr3t-Value"))) || !strings.HasSuffix(got, " abc") {
		t.Fatalf("MaskSecrets = %q", got)
	}
	if MaskSecrets("other", in) != in {
		t.Fatal("别的对话不受影响")
	}
}

// 模型要用户填 → 用户填好 → 命令里用代号：交给模型的内容里始终没有值。
func TestAskSecretLoop(t *testing.T) {
	conv := &Conversation{ID: "t-loop"}
	defer ForgetSecrets(conv.ID)
	fields := []SecretField{{Name: "new_pw", Label: "新密码"}}
	ask, _ := json.Marshal(map[string]any{"fields": fields})
	var ran string
	tools := []Tool{{
		Name: "ask_secret",
		Approve: func(ctx context.Context, in json.RawMessage) *Approval {
			return &Approval{Kind: "填写", Title: "请填写", Once: "要你填写", Fields: fields}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			return "已填：" + SecretToken("new_pw"), nil
		},
	}, {
		Name: "run_command",
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct{ Command string }
			json.Unmarshal(in, &a)
			_, env, err := CommandSecrets(conv.ID, a.Command)
			ran = env["FERRY_SECRET_NEW_PW"]
			return "设置成功，新密码是 " + ran, err
		},
	}}
	p := &scripted{replies: []Message{
		{Role: "assistant", Calls: []ToolCall{{ID: "1", Name: "ask_secret", Input: ask}}},
		{Role: "assistant", Calls: []ToolCall{{ID: "2", Name: "run_command", Input: json.RawMessage(`{"command":"chpasswd \"$FERRY_SECRET_NEW_PW\""}`)}}},
		{Role: "assistant", Text: "好了"},
	}}
	var events []Event
	l := &Loop{Provider: p, Tools: tools, Emit: func(e Event) { events = append(events, e) }}
	if err := l.Send(context.Background(), conv, "改密码", nil); err != nil {
		t.Fatal(err)
	}
	if conv.Pending == nil || len(conv.Pending.Approval.Fields) != 1 {
		t.Fatalf("应该停下来等用户填：%+v", conv.Pending)
	}
	SetSecret(conv.ID, "new_pw", "Hunter2-very-secret")
	if err := l.Decide(context.Background(), conv, Approve); err != nil {
		t.Fatal(err)
	}
	if ran != "Hunter2-very-secret" {
		t.Fatalf("命令拿到的值 = %q", ran)
	}
	b, _ := json.Marshal(conv)
	if strings.Contains(string(b), "Hunter2") {
		t.Fatalf("对话记录里不应该有值：%s", b)
	}
	for _, m := range p.seen {
		if strings.Contains(m, "Hunter2") {
			t.Fatalf("发给模型的内容里不应该有值：%s", m)
		}
	}
}

func TestAskSecretCancel(t *testing.T) {
	conv := &Conversation{ID: "t-cancel"}
	fields := []SecretField{{Name: "pw", Label: "密码"}}
	tools := []Tool{{Name: "ask_secret",
		Approve: func(ctx context.Context, in json.RawMessage) *Approval {
			return &Approval{Kind: "填写", Once: "x", Fields: fields}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) { return "", nil }}}
	p := &scripted{replies: []Message{
		{Role: "assistant", Calls: []ToolCall{{ID: "1", Name: "ask_secret", Input: json.RawMessage(`{}`)}}},
		{Role: "assistant", Text: "好的"},
	}}
	l := &Loop{Provider: p, Tools: tools, Emit: func(Event) {}}
	l.Send(context.Background(), conv, "x", nil)
	if err := l.Decide(context.Background(), conv, Reject); err != nil {
		t.Fatal(err)
	}
	var last string
	for _, m := range conv.Messages {
		if m.Role == "tool" {
			last = m.Text
		}
	}
	if !strings.Contains(last, "没有填写") {
		t.Fatalf("取消填写的结果 = %q", last)
	}
}

// scripted 按顺序给出回复的假模型，记下每次收到的内容。
type scripted struct {
	replies []Message
	seen    []string
}

func (s *scripted) Step(ctx context.Context, system string, msgs []Message, tools []Tool) (Message, error) {
	b, _ := json.Marshal(msgs)
	s.seen = append(s.seen, string(b))
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}
