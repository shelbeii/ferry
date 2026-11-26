package approve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type result struct {
	d   Decision
	o   *Outcome
	err error
	log []string
}

func ask(ctx context.Context, file string, req Request) chan result {
	ch := make(chan result, 1)
	go func() {
		var r result
		r.d, r.o, r.err = Ask(ctx, file, req, nil, func(s string) { r.log = append(r.log, s) })
		ch <- r
	}()
	return ch
}

func TestAskDecide(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mcp", "approver.json")
	ctx := context.Background()
	if _, _, err := Ask(ctx, file, Request{Action: "deploy"}, nil, nil); !errors.Is(err, ErrNoDesktop) {
		t.Fatalf("桌面版没打开：%v", err)
	}

	h := NewHub(file)
	h.Prepare = func(r *Request) error {
		if r.Target == "prod" {
			return errors.New("生产服务器不行")
		}
		return nil
	}
	got := make(chan Request, 4)
	gone := make(chan string, 4)
	unsub, err := h.Subscribe(func(kind string, v any) {
		switch kind {
		case "request":
			got <- v.(Request)
		case "gone":
			gone <- v.(map[string]any)["id"].(string)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("没写端口文件")
	}

	// 桌面版这边再检查一遍：不交给用户
	if r := <-ask(ctx, file, Request{Action: "restart", Target: "prod"}); r.err != nil || r.d.Approve || r.d.Note != "生产服务器不行" {
		t.Fatalf("%+v", r)
	}

	// 同意 → 执行中的进展 → 结果
	res := ask(ctx, file, Request{ID: "r1", Action: "deploy", Target: "test", Apps: []string{"api"}})
	r := <-got
	if r.ID != "r1" || r.Action != "deploy" || r.Time.IsZero() {
		t.Fatalf("%+v", r)
	}
	if err := h.Decide(r.ID, Decision{Approve: true}); err != nil {
		t.Fatal(err)
	}
	if h.Decide(r.ID, Decision{Approve: true}) == nil {
		t.Fatal("已经决定过的不能再决定")
	}
	time.Sleep(50 * time.Millisecond)
	h.Progress(r.ID, "==> 构建")
	time.Sleep(50 * time.Millisecond)
	h.Done(r.ID, Outcome{OK: true, Data: json.RawMessage(`{"release":"x"}`), Log: []string{"完成"}})
	out := <-res
	if out.err != nil || !out.d.Approve || out.o == nil || !out.o.OK || string(out.o.Data) != `{"release":"x"}` || len(out.log) != 1 {
		t.Fatalf("%+v %+v", out, out.o)
	}
	h.Progress(r.ID, "结束以后的进展不出错")

	// 拒绝
	res = ask(ctx, file, Request{Action: "stop", Target: "test"})
	r = <-got
	h.Decide(r.ID, Decision{Note: "你在 Ferry 里拒绝了"})
	if out := <-res; out.err != nil || out.d.Approve || out.o != nil || out.d.Note == "" {
		t.Fatalf("%+v", out)
	}
	<-gone

	// 编程软件取消：桌面版收到 gone
	cctx, cancel := context.WithCancel(ctx)
	res = ask(cctx, file, Request{Action: "restart", Target: "test"})
	r = <-got
	cancel()
	if id := <-gone; id != r.ID {
		t.Fatalf("gone %s", id)
	}
	<-res

	// 令牌不对：不交给桌面版
	b, _ := os.ReadFile(file)
	os.WriteFile(file, []byte(`{"port":`+portOf(t, b)+`,"token":"x"}`), 0o600)
	if out := <-ask(ctx, file, Request{Action: "stop"}); out.err != nil || out.d.Approve || out.d.Note != "令牌不对" {
		t.Fatalf("%+v", out)
	}
	os.WriteFile(file, b, 0o600)

	// 桌面版关闭：还在等的算拒绝，端口文件删掉
	res = ask(ctx, file, Request{Action: "stop", Target: "test"})
	<-got
	unsub()
	select {
	case out := <-res:
		if out.d.Approve || out.d.Note == "" {
			t.Fatalf("%+v", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("桌面版关了还在等")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("端口文件应该删掉")
	}
}

func portOf(t *testing.T, b []byte) string {
	var s struct{ Port int }
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(s.Port)
}

func TestTwoPhasePrepareCommitAndSecurity(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mcp", "approver.json")
	ctx := context.Background()
	if _, err := Prepare(ctx, file, Request{Action: "deploy"}); !errors.Is(err, ErrNoDesktop) {
		t.Fatalf("桌面版没打开时 Prepare 应报 ErrNoDesktop：%v", err)
	}

	h := NewHub(file)
	h.Prepare = func(r *Request) error {
		if r.Target == "prod" {
			return errors.New("生产服务器不行")
		}
		r.Detail = []string{"权威预览：" + r.Target}
		return nil
	}
	h.PrepareClient = func(r *Request) (bool, string, error) {
		if r.Action == "secret" {
			return false, "密码只能在桌面端填", nil
		}
		return true, "", nil
	}
	h.VerifyCommit = func(r *Request, ev ConfirmEvidence) error {
		if ev.Channel == "bad" {
			return errors.New("渠道无效")
		}
		return nil
	}
	got := make(chan Request, 8)
	unsub, err := h.Subscribe(func(kind string, v any) {
		if kind == "request" {
			got <- v.(Request)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	// 1. 直接 Ask 绝不允许伪造 ClientConfirmed
	res := ask(ctx, file, Request{ID: "spoof", Session: "s1", Action: "restart", Target: "test", ClientConfirmed: true, ConfirmChannel: "elicitation"})
	r := <-got
	if r.ClientConfirmed || r.ConfirmChannel != "" {
		t.Fatalf("Ask 通道不能信任外部 ClientConfirmed：%+v", r)
	}
	h.Decide(r.ID, Decision{Note: "拒绝"})
	<-res

	// 2. Prepare → Commit（客户端已确认）：下发给桌面端时由 Hub 置位 ClientConfirmed，且一次性消费防重放
	prep, err := Prepare(ctx, file, Request{ID: "p1", Session: "s1", Action: "restart", Target: "test", Apps: []string{"api"}})
	if err != nil || !prep.Eligible || prep.Token == "" || len(prep.Request.Detail) != 1 {
		t.Fatalf("Prepare：%v %+v", err, prep)
	}
	// 跨会话 Commit 被拦住
	if d, _, err := Commit(ctx, file, prep.Token, "other-session", ConfirmEvidence{Channel: "elicitation"}, nil, nil); err != nil || d.Approve {
		t.Fatalf("跨会话应该被拒绝：%v %+v", err, d)
	}
	// 上面已经消费过一次令牌，重放应失效
	if d, _, err := Commit(ctx, file, prep.Token, "s1", ConfirmEvidence{Channel: "elicitation"}, nil, nil); err != nil || d.Approve {
		t.Fatalf("令牌不可重放：%v %+v", err, d)
	}

	// 正常 Prepare → Commit（验证渠道失败拦截 → 重新 Prepare → 成功）
	prep, err = Prepare(ctx, file, Request{ID: "p2", Session: "s1", Action: "restart", Target: "test", Apps: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	if d, _, err := Commit(ctx, file, prep.Token, "s1", ConfirmEvidence{Channel: "bad"}, nil, nil); err != nil || d.Approve || d.Note != "渠道无效" {
		t.Fatalf("VerifyCommit 应拒绝：%v %+v", err, d)
	}

	prep, err = Prepare(ctx, file, Request{ID: "p3", Session: "s1", Action: "restart", Target: "test", Apps: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	doneCh := make(chan result, 1)
	go func() {
		var out result
		out.d, out.o, out.err = Commit(ctx, file, prep.Token, "s1", ConfirmEvidence{Channel: "elicitation", Action: "accept", TookMs: 3000, Note: "已在 Cursor 确认"}, nil, nil)
		doneCh <- out
	}()
	r = <-got
	if !r.ClientConfirmed || r.ConfirmChannel != "elicitation" || r.ConfirmNote != "已在 Cursor 确认" {
		t.Fatalf("桌面端应收到 Hub 置位的 ClientConfirmed：%+v", r)
	}
	h.Decide(r.ID, Decision{Approve: true, Note: r.ConfirmNote})
	h.Done(r.ID, Outcome{OK: true, Data: json.RawMessage(`{"ok":true}`)})
	out := <-doneCh
	if out.err != nil || !out.d.Approve || out.o == nil || !out.o.OK {
		t.Fatalf("Commit 执行失败：%+v %+v", out, out.o)
	}

	// 3. Prepare → FallbackToDesktop：保留冻结 Request 但 ClientConfirmed=false，附加回退原因
	prep, err = Prepare(ctx, file, Request{ID: "p4", Session: "s1", Action: "secret", Target: "test"})
	if err != nil || prep.Eligible || prep.ForceDesktopReason == "" {
		t.Fatalf("secret 应标记不可在客户端确认：%v %+v", err, prep)
	}
	go func() {
		_, _, _ = Commit(ctx, file, prep.Token, "s1", ConfirmEvidence{FallbackToDesktop: true, FallbackReason: "强制回退桌面端"}, nil, nil)
	}()
	r = <-got
	if r.ClientConfirmed || len(r.Detail) == 0 || r.Detail[0] != "强制回退桌面端" {
		t.Fatalf("回退桌面端应为非 ClientConfirmed 且带原因：%+v", r)
	}
	h.Decide(r.ID, Decision{Note: "拒绝"})

	// 4. Prepare → Cancel 销毁令牌
	prep, err = Prepare(ctx, file, Request{ID: "p5", Session: "s1", Action: "restart", Target: "test"})
	if err != nil {
		t.Fatal(err)
	}
	Cancel(ctx, file, prep.Token, "s1")
	if d, _, err := Commit(ctx, file, prep.Token, "s1", ConfirmEvidence{Channel: "elicitation"}, nil, nil); err != nil || d.Approve {
		t.Fatalf("Cancel 后令牌应失效：%v %+v", err, d)
	}

	// 5. DecideWithBlock 拉黑会话后，后续 Prepare 与 Ask 直接拒绝
	res = ask(ctx, file, Request{ID: "b1", Session: "bad-sess", Action: "restart", Target: "test"})
	r = <-got
	if err := h.DecideWithBlock(r.ID, Decision{Note: "拉黑"}, true); err != nil {
		t.Fatal(err)
	}
	<-res
	if _, err := Prepare(ctx, file, Request{ID: "b2", Session: "bad-sess", Action: "restart", Target: "test"}); err == nil {
		t.Fatal("已拉黑的会话应被 Prepare 拒绝")
	}
	if out := <-ask(ctx, file, Request{ID: "b3", Session: "bad-sess", Action: "restart", Target: "test"}); out.d.Approve || out.d.Note == "" {
		t.Fatalf("已拉黑的会话应被 Ask 拒绝：%+v", out)
	}
}

func TestHubSessionsUnblockCancelAndBye(t *testing.T) {
	file := filepath.Join(t.TempDir(), "approver.json")
	h := NewHub(file)
	closed := make(chan string, 2)
	h.OnSessionClose = func(sess string) { closed <- sess }

	cancels := make(chan string, 2)
	unblocks := make(chan string, 2)
	reqs := make(chan Request, 4)
	unsub, err := h.Subscribe(func(kind string, v any) {
		switch kind {
		case "request":
			reqs <- v.(Request)
		case "cancel":
			cancels <- v.(map[string]any)["id"].(string)
		case "unblock":
			unblocks <- v.(map[string]any)["session"].(string)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	ctx := context.Background()
	Hello(ctx, file, "s-diag", os.Getpid(), "/work/shop", "Cursor 1.0", "2026-07-28", "mcp-app", []string{"mcp-app", "roots"})
	sessions := h.Sessions()
	if len(sessions) != 1 || sessions[0].Session != "s-diag" || sessions[0].Channel != "mcp-app" || sessions[0].ProtocolVersion != "2026-07-28" || sessions[0].PID != os.Getpid() || sessions[0].Dir != "/work/shop" {
		t.Fatalf("Hello 上报的会话诊断信息不对：%+v", sessions)
	}

	// 已退出/被强杀的进程（未发 Bye）在查询 Sessions 时自动剔除，并触发 OnSessionClose 清理内存密码
	oldAlive := processAlive
	processAlive = func(pid int) bool { return pid != 999999 }
	t.Cleanup(func() { processAlive = oldAlive })
	Hello(ctx, file, "s-dead", 999999, "/work/old", "Codex 1.0", "2026-07-28", "mcp-app", []string{"mcp-app"})
	sessions = h.Sessions()
	if len(sessions) != 1 || sessions[0].Session != "s-diag" {
		t.Fatalf("已退出的进程会话应被自动清理：%+v", sessions)
	}
	select {
	case s := <-closed:
		if s != "s-dead" {
			t.Fatalf("清理失效进程应触发 OnSessionClose(s-dead)，实际 %q", s)
		}
	case <-time.After(time.Second):
		t.Fatal("清理失效进程未触发 OnSessionClose")
	}

	// 拉黑 → UnblockSession 解除拉黑并通知订阅方
	h.BlockSession("s-diag")
	if !h.IsSessionBlocked("s-diag") {
		t.Fatal("应已拉黑")
	}
	if !h.UnblockSession("s-diag") || h.IsSessionBlocked("s-diag") {
		t.Fatal("UnblockSession 应解除拉黑")
	}
	select {
	case s := <-unblocks:
		if s != "s-diag" {
			t.Fatalf("unblock 事件会话不对：%q", s)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到 unblock 事件")
	}

	// CancelRun 只允许所属会话取消正在执行的任务
	res := ask(ctx, file, Request{ID: "run-1", Session: "s-diag", Action: "deploy", Target: "test"})
	r := <-reqs
	if err := h.Decide(r.ID, Decision{Approve: true}); err != nil {
		t.Fatal(err)
	}
	if err := CancelRun(ctx, file, "run-1", "other-sess"); err == nil {
		t.Fatal("其他会话不能取消不属于它的任务")
	}
	if err := CancelRun(ctx, file, "run-1", "s-diag"); err != nil {
		t.Fatalf("所属会话 CancelRun 失败：%v", err)
	}
	select {
	case cid := <-cancels:
		if cid != "run-1" {
			t.Fatalf("cancel 事件 ID 不对：%q", cid)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到 cancel 事件")
	}
	h.Done("run-1", Outcome{OK: false, Error: "已停止"})
	<-res

	// Bye 清理预检令牌并触发 OnSessionClose 清理内存密码
	prep, err := Prepare(ctx, file, Request{ID: "p-bye", Session: "s-diag", Action: "restart", Target: "test"})
	if err != nil {
		t.Fatal(err)
	}
	Bye(file, "s-diag")
	select {
	case s := <-closed:
		if s != "s-diag" {
			t.Fatalf("OnSessionClose 会话不对：%q", s)
		}
	case <-time.After(time.Second):
		t.Fatal("Bye 未触发 OnSessionClose")
	}
	if d, _, err := Commit(ctx, file, prep.Token, "s-diag", ConfirmEvidence{Channel: "mcp-app"}, nil, nil); err != nil || d.Approve {
		t.Fatalf("Bye 后该会话的预检令牌应已失效：%v %+v", err, d)
	}
}
