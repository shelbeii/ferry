package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
)

// 工具的参数定义里不能有 null（DeepSeek 等接口会拒绝：null is not of type "array"）
func TestAIToolSchemasHaveNoNull(t *testing.T) {
	e := &Engine{}
	for _, mode := range []string{AIReadOnly, AIConfirm} {
		for _, tg := range []*config.Target{{Name: "a", User: "root"}, {Name: "b", Plain: true, User: "root"}} {
			for _, tool := range e.aiTools(nil, tg, mode, "c") {
				b, err := json.Marshal(tool.Schema)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), "null") {
					t.Errorf("%s 的参数定义里有 null：%s", tool.Name, b)
				}
				if tool.Schema["type"] != "object" {
					t.Errorf("%s 的参数定义应是 object：%s", tool.Name, b)
				}
			}
		}
	}
}

// 只读模式没有改动的工具；确认模式里改动的工具都要确认，只读的命令不用
func TestAIToolsByMode(t *testing.T) {
	e := &Engine{}
	tg := &config.Target{Name: "a", User: "root"}
	names := func(mode string) map[string]bool {
		m := map[string]bool{}
		for _, tool := range e.aiTools(nil, tg, mode, "c") {
			m[tool.Name] = true
		}
		return m
	}
	ro, cf := names(AIReadOnly), names(AIConfirm)
	for _, n := range []string{"write_file", "app_control", "app_rollback", "software_install", "software_control", "save_software_template"} {
		if ro[n] {
			t.Errorf("只读模式不应有 %s", n)
		}
		if !cf[n] {
			t.Errorf("确认模式应有 %s", n)
		}
	}
	plain := map[string]bool{}
	for _, tool := range e.aiTools(nil, &config.Target{Name: "p", Plain: true, User: "root"}, AIConfirm, "c") {
		plain[tool.Name] = true
	}
	if plain["software_install"] || plain["app_control"] || !plain["write_file"] {
		t.Errorf("连接（没有 agent）的工具：%v", plain)
	}

	for _, tool := range e.aiTools(nil, tg, AIConfirm, "c") {
		switch tool.Name {
		case "run_command":
			if tool.Approve(context.Background(), json.RawMessage(`{"command":"df -h"}`)) != nil {
				t.Error("只读命令不用确认")
			}
			ap := tool.Approve(context.Background(), json.RawMessage(`{"command":"yum install -y redis"}`))
			if ap == nil || ap.Kind != "命令：yum install" || !ap.AsRoot || ap.Danger != "" {
				t.Errorf("yum install 要确认：%+v", ap)
			}
			if ap := tool.Approve(context.Background(), json.RawMessage(`{"command":"rm -rf /opt/x"}`)); ap == nil || ap.Danger == "" {
				t.Errorf("rm 是危险命令：%+v", ap)
			}
		case "app_control":
			if ap := tool.Approve(context.Background(), json.RawMessage(`{"apps":["System"],"action":"stop"}`)); ap == nil || ap.Danger == "" {
				t.Errorf("停止应用要确认并标危险：%+v", ap)
			}
		case "save_software_template":
			if tool.Approve(context.Background(), json.RawMessage(`{"yaml":"id: X"}`)) != nil {
				t.Error("格式不对的模板不用打扰用户，执行时把错误交回模型")
			}
		}
	}
	for _, tool := range e.aiTools(nil, tg, AIReadOnly, "c") {
		if tool.Name == "run_command" && tool.Approve(context.Background(), json.RawMessage(`{"command":"yum install -y redis"}`)) != nil {
			t.Error("只读模式下不会请求确认（直接拒绝）")
		}
	}
}

// 长输出保留开头和结尾（出错信息一般在最后）
func TestHeadTail(t *testing.T) {
	h := &headTail{headMax: 20, tailMax: 20}
	for i := 0; i < 100; i++ {
		h.add(fmt.Sprintf("line %d", i))
	}
	h.add("ERROR: 安装失败")
	out := h.String()
	if !strings.HasPrefix(out, "line 0\n") || !strings.Contains(out, "中间省略") || !strings.HasSuffix(out, "ERROR: 安装失败\n") {
		t.Fatalf("输出：%s", out)
	}
}

// 生产服务器：每个改动都要确认，不能「同类不再询问」
func TestAIProductionAlwaysAsks(t *testing.T) {
	e := &Engine{}
	for _, tool := range e.aiTools(nil, &config.Target{Name: "p", User: "root", Production: true}, AIConfirm, "c") {
		if tool.Name == "run_command" {
			ap := tool.Approve(context.Background(), json.RawMessage(`{"command":"yum install -y redis"}`))
			if ap == nil || ap.Once == "" {
				t.Fatalf("生产服务器应每次确认：%+v", ap)
			}
			if tool.Approve(context.Background(), json.RawMessage(`{"command":"df -h"}`)) != nil {
				t.Fatal("只读命令不用确认")
			}
		}
	}
	no := false
	if (&config.Target{AIChanges: &no}).AllowAIChanges() || !(&config.Target{}).AllowAIChanges() {
		t.Fatal("aiChanges")
	}
}

// AI 保存的模板标上 origin: ai
func TestMarkAITemplate(t *testing.T) {
	y := markAITemplate("id: demo\norigin: mine\nname: 演示\n")
	if !strings.HasPrefix(y, "origin: ai") || strings.Count(y, "origin:") != 1 || !strings.Contains(y, "name: 演示") {
		t.Fatalf("%s", y)
	}
}

// 用户填的值经标准输入设成环境变量：不出现在命令行上，命令本身仍接原来的输入（没有时是 /dev/null）。
func TestWrapCommandSecrets(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("没有 sh")
	}
	env := map[string]string{"FERRY_SECRET_PW": `p@ss 'w"0rd' $x`}
	for _, c := range []struct {
		stdin []byte
		want  string
	}{
		{nil, "[p@ss 'w\"0rd' $x]()"},
		{[]byte("文件内容\n"), "[p@ss 'w\"0rd' $x](文件内容)"},
	} {
		line, in := wrapCommand(`printf '[%s](%s)' "$FERRY_SECRET_PW" "$(cat)"`, c.stdin, env, "")
		if strings.Contains(line, "p@ss") {
			t.Fatalf("命令行里不应该有值：%s", line)
		}
		cmd := exec.Command(sh, "-c", line)
		cmd.Stdin = bytes.NewReader(in)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v：%s", err, out)
		}
		got := strings.SplitN(string(out), "\n", 2)
		if len(got) != 2 || !strings.HasPrefix(got[0], pidMark) || got[1] != c.want {
			t.Fatalf("输出 = %q，想要 %q", out, c.want)
		}
	}
}

// ask_secret 只在允许改动时有；用到代号的只读命令也要确认，没填过的不打扰用户
func TestAISecretTools(t *testing.T) {
	e := &Engine{}
	tg := &config.Target{Name: "a", User: "root", Plain: true}
	has := func(mode string) bool {
		for _, tool := range e.aiTools(nil, tg, mode, "c-tools") {
			if tool.Name == "ask_secret" {
				return true
			}
		}
		return false
	}
	if has(AIReadOnly) || !has(AIConfirm) {
		t.Fatal("ask_secret 只在允许改动时有")
	}
	defer ai.ForgetSecrets("c-tools")
	ai.SetSecret("c-tools", "pw", "secret-value")
	var run ai.Tool
	for _, tool := range e.aiTools(nil, tg, AIConfirm, "c-tools") {
		if tool.Name == "run_command" {
			run = tool
		}
	}
	ap := run.Approve(context.Background(), json.RawMessage(`{"command":"echo \"$FERRY_SECRET_PW\""}`))
	if ap == nil || ap.Once == "" {
		t.Fatalf("用到代号的只读命令也要每次确认：%+v", ap)
	}
	if ap := run.Approve(context.Background(), json.RawMessage(`{"command":"echo \"$FERRY_SECRET_NOPE\""}`)); ap != nil {
		t.Fatal("没填过的不用问用户，执行时告诉模型")
	}
	if ap := run.Approve(context.Background(), json.RawMessage(`{"command":"ls /"}`)); ap != nil {
		t.Fatal("普通只读命令不用确认")
	}
}
