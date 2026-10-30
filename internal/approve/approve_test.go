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
