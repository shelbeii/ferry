package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 常驻的 agent：几个请求同时做，输出按请求号分开，每个都有结果和结束标记；改动服务器的命令不接。
func TestServe(t *testing.T) {
	f := newFake(t)
	var in bytes.Buffer
	req := func(id, cmd string, ver int) {
		b, _ := json.Marshal(proto.ServeRequest{ID: id, Cmd: cmd, Req: &proto.Request{Proto: ver, Root: f.root}})
		in.Write(append(b, '\n'))
	}
	for i := 1; i <= 5; i++ {
		req(fmt.Sprint(i), "history", proto.Version)
	}
	req("old", "history", proto.Version-1)
	req("dep", "deploy", proto.Version)
	in.WriteString("不是 JSON\n")
	b, _ := json.Marshal(proto.ServeRequest{ID: "last", Cmd: "history", Req: &proto.Request{Proto: proto.Version, Root: f.root}})
	in.Write(b) // 最后一行没有换行

	var out bytes.Buffer
	if code := Serve(f, &in, &out); code != 0 {
		t.Fatalf("退出码 %d", code)
	}
	results := map[string]proto.Result{}
	ends := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		id, rest, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("没有请求号：%q", line)
		}
		if code, end := strings.CutPrefix(rest, proto.ServeEnd); end {
			ends[id] = code
			continue
		}
		var r proto.Result
		if json.Unmarshal([]byte(rest), &r) == nil && r.T == "result" {
			results[id] = r
		}
	}
	for _, id := range []string{"1", "2", "3", "4", "5", "last"} {
		if results[id].RC != proto.CodeOK || ends[id] != "0" {
			t.Errorf("请求 %s：%+v 结束 %q", id, results[id], ends[id])
		}
	}
	if results["old"].RC != proto.CodeBadInput || ends["old"] != fmt.Sprint(proto.CodeBadInput) {
		t.Errorf("协议版本不对：%+v %q", results["old"], ends["old"])
	}
	if r := results["dep"]; r.RC != proto.CodeBadInput || !strings.Contains(r.Msg, "不处理 deploy") {
		t.Errorf("改动服务器的命令不该交给常驻的 agent：%+v", r)
	}
	if len(ends) != 8 {
		t.Errorf("结束标记：%v", ends)
	}
}

// blockSys 运行命令时一直等到取消（模拟很慢的扫描）。
type blockSys struct {
	*fakeSys
	started chan string
}

func (b blockSys) ShellContext(ctx context.Context, cmd string, _ time.Duration) (string, error) {
	b.started <- cmd
	<-ctx.Done()
	return "", errCanceled
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) ended(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.b.String(), "\n"+id+" "+proto.ServeEnd) || strings.HasPrefix(l.b.String(), id+" "+proto.ServeEnd)
}

// 慢的扫描占满了也不耽误很快的查看；取消时还在排队的不做了，正在做的停下正在运行的命令。
func TestServeCancelAndQueues(t *testing.T) {
	f := newFake(t)
	sys := blockSys{fakeSys: f, started: make(chan string, 16)}
	pr, pw := io.Pipe()
	out := &lockedBuf{}
	done := make(chan int, 1)
	go func() { done <- Serve(sys, pr, out) }()
	send := func(sr proto.ServeRequest) {
		b, _ := json.Marshal(sr)
		pw.Write(append(b, '\n'))
	}
	req := func(id, cmd string) {
		send(proto.ServeRequest{ID: id, Cmd: cmd, Req: &proto.Request{Proto: proto.Version, Root: f.root}})
	}
	waitEnd := func(id string) {
		t.Helper()
		for start := time.Now(); !out.ended(id); time.Sleep(5 * time.Millisecond) {
			if time.Since(start) > 3*time.Second {
				t.Fatalf("请求 %s 没有结束：\n%s", id, out.b.String())
			}
		}
	}
	for i := 1; i <= serveHeavy; i++ {
		req(fmt.Sprint("p", i), "ports")
		<-sys.started // 都在运行命令（卡着）
	}
	// ports 里读防火墙和 docker ps 同时跑：一个请求可能卡着两条命令，都收掉
	for drained := false; !drained; {
		select {
		case <-sys.started:
		case <-time.After(100 * time.Millisecond):
			drained = true
		}
	}
	req("queued", "ports") // 慢的满了：排队
	req("h", "history")    // 很快的照常做完
	waitEnd("h")
	if out.ended("queued") {
		t.Fatal("排队的不该已经做了")
	}
	send(proto.ServeRequest{ID: "queued", Cancel: true})
	waitEnd("queued")
	select {
	case cmd := <-sys.started:
		t.Fatalf("取消了的请求不该再运行命令：%s", cmd)
	default:
	}
	for i := 1; i <= serveHeavy; i++ {
		send(proto.ServeRequest{ID: fmt.Sprint("p", i), Cancel: true})
		waitEnd(fmt.Sprint("p", i))
	}
	pw.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("输入结束后应该退出")
	}
}
