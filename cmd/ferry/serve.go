package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/settings"
)

// serve 常驻模式（桌面版用）：从 stdin 逐行读请求，并发执行，到服务器的连接在请求之间复用。
//
//	请求 {"id":"1","op":"run","args":["status","prod","--json"],"env":{"FERRY_PASSWORD_PROD":"…"}}
//	     {"id":"1","op":"cancel"}
//	输出 {"id":"1","outs":["<stdout 的几行>"]} {"id":"1","err":"<stderr 的一行>"} {"id":"1","exit":0}
//	stdout 攒一小会儿（最多 80 毫秒或 1000 行）一起发：日志刷得很快时，桌面版不用每行处理一次
//
// 终端（run terminal <目标>）另有：
//
//	请求 {"id":"1","op":"input","data":"ls\r"}（或 "b64":"…"，原样的字节）
//	     {"id":"1","op":"resize","cols":120,"rows":30}
//	     {"id":"1","op":"flow","pause":true}（桌面版来不及显示时暂停输出，显示完再继续）
//	输出 {"id":"1","tty":"<终端输出>"}
//
// stdin 关闭（桌面版退出）时断开全部连接并退出。
func serve(in io.Reader, out io.Writer) int {
	// 协议独占 stdout：其他地方误写 os.Stdout 的内容改到 stderr，不会打乱协议
	os.Stdout = os.Stderr

	st, _ := settings.Load()
	pool := engine.NewPool(st.IdleTimeout())
	defer pool.Close()

	var mu sync.Mutex
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(v)
	}
	send(map[string]any{"ready": true, "version": Version})

	type running struct {
		cancel context.CancelFunc
		tty    *ttyIO
	}
	var (
		reqMu sync.Mutex
		reqs  = map[string]*running{}
		wg    sync.WaitGroup
	)
	find := func(id string) *running {
		reqMu.Lock()
		defer reqMu.Unlock()
		return reqs[id]
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		var req struct {
			ID    string            `json:"id"`
			Op    string            `json:"op"`
			Args  []string          `json:"args"`
			Env   map[string]string `json:"env"`
			Data  string            `json:"data"`
			B64   []byte            `json:"b64"`
			Cols  int               `json:"cols"`
			Rows  int               `json:"rows"`
			Pause bool              `json:"pause"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil || req.ID == "" {
			// 不原样打印：终端输入里可能有密码
			fmt.Fprintf(os.Stderr, "ferry serve: 无法解析的请求（%d 字节）\n", len(sc.Bytes()))
			continue
		}
		switch req.Op {
		case "cancel":
			if r := find(req.ID); r != nil {
				r.cancel()
			}
		case "input": // 终端的键盘输入：放进队列，不阻塞这里读请求
			if r := find(req.ID); r != nil {
				r.tty.in.push([]byte(req.Data))
				r.tty.in.push(req.B64)
			}
		case "resize":
			if r := find(req.ID); r != nil && req.Cols > 0 && req.Rows > 0 {
				r.tty.setSize(req.Cols, req.Rows)
			}
		case "flow":
			if r := find(req.ID); r != nil {
				r.tty.out.setPaused(req.Pause)
			}
		case "run":
			ctx, cancel := context.WithCancel(context.Background())
			id := req.ID
			r := &running{cancel: cancel, tty: newTTY(func(s string) { send(map[string]any{"id": id, "tty": s}) })}
			reqMu.Lock()
			reqs[id] = r
			reqMu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				code := serveOne(ctx, pool, id, req.Args, req.Env, r.tty, send)
				reqMu.Lock()
				delete(reqs, id)
				reqMu.Unlock()
				cancel()
				send(map[string]any{"id": id, "exit": code})
			}()
		default:
			send(map[string]any{"id": req.ID, "err": "未知操作 " + req.Op, "exit": 2})
		}
	}
	// 桌面版退出：取消进行中的请求，稍等它们收尾
	reqMu.Lock()
	for _, r := range reqs {
		r.cancel()
	}
	reqMu.Unlock()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return 0
}

func serveOne(ctx context.Context, pool *engine.Pool, id string, args []string, env map[string]string, tty *ttyIO, send func(any)) (code int) {
	stdout := &batchWriter{emit: func(ls []string) { send(map[string]any{"id": id, "outs": ls}) }}
	stderr := &lineWriter{emit: func(l string) { send(map[string]any{"id": id, "err": l}) }}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "ferry 内部错误：%v\n%s", r, debug.Stack())
			code = 1
		}
		tty.close() // 终端最后的输出先发，再发结果
		stdout.Flush()
		stderr.Flush()
	}()
	getenv := func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return os.Getenv(k)
	}
	return run(args, &cliIO{ctx: ctx, stdout: stdout, stderr: stderr, getenv: getenv, pool: pool, tty: tty})
}

// batchWriter 按行切分输出，攒一批再回调（最多等 batchWait，或攒满 batchMax 行）。
type batchWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines []string
	timer *time.Timer
	emit  func([]string)
}

const (
	batchWait = 80 * time.Millisecond
	batchMax  = 1000
)

func (w *batchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		w.lines = append(w.lines, string(bytes.TrimRight(w.buf.Next(i+1), "\r\n")))
	}
	switch {
	case len(w.lines) >= batchMax:
		w.flushLocked()
	case len(w.lines) > 0 && w.timer == nil:
		w.timer = time.AfterFunc(batchWait, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.flushLocked()
		})
	}
	return len(p), nil
}

func (w *batchWriter) flushLocked() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	if len(w.lines) > 0 {
		w.emit(w.lines)
		w.lines = nil
	}
}

// Flush 发出攒着的行和最后不带换行的部分（命令结束、发 exit 之前调用）。
func (w *batchWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.lines = append(w.lines, w.buf.String())
		w.buf.Reset()
	}
	w.flushLocked()
}

// lineWriter 按行切分输出，每行回调一次。
type lineWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(w.buf.Next(i+1), "\r\n"))
		w.emit(line)
	}
	return len(p), nil
}

// Flush 输出最后不带换行的部分。
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.emit(w.buf.String())
		w.buf.Reset()
	}
}
