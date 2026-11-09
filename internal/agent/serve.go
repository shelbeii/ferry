package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/captain/ferry/internal/proto"
)

// serveParallel 常驻时同时做几个请求。
const serveParallel = 8

// Serve 常驻（ferry-agent serve）：一行行读请求，同时处理，输出每行前面加请求号（协议见 proto/serve.go）。
// 读到结尾（本机关了这个会话、连接断了）时等手上的做完再返回。只处理 proto.ServeCmds 里的只读命令。
func Serve(sys System, in io.Reader, out io.Writer) int {
	w := &serveOut{w: out}
	sem := make(chan struct{}, serveParallel)
	var wg sync.WaitGroup
	r := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var sr proto.ServeRequest
			switch {
			case json.Unmarshal(line, &sr) != nil || sr.ID == "" || sr.Req == nil:
				fmt.Fprintf(os.Stderr, "ferry-agent serve：无法解析的请求（%d 字节）\n", len(line))
			case !proto.ServeCmds[sr.Cmd]:
				w.line(sr.ID, resultLine(proto.CodeBadInput, "常驻的 agent 不处理 "+sr.Cmd))
				w.end(sr.ID, proto.CodeBadInput)
			default:
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					lw := &serveLines{id: sr.ID, out: w}
					a := &Agent{Sys: sys, Out: lw}
					code := a.Run(sr.Cmd, sr.Req) // Run 自己拦下 panic，返回失败的结果
					lw.flush()
					w.end(sr.ID, code)
				}()
			}
		}
		if err != nil {
			break
		}
	}
	wg.Wait()
	return 0
}

// resultLine 一次性运行时 agent 输出的结果行（不经 Agent 时用）。
func resultLine(code int, msg string) string {
	b, _ := json.Marshal(proto.Result{T: "result", RC: code, Msg: msg})
	return string(b)
}

// serveOut 几个请求共用的输出：一次写完整的一行，不会和别的请求的行交错在一行里。
type serveOut struct {
	mu sync.Mutex
	w  io.Writer
}

func (o *serveOut) line(id, s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	io.WriteString(o.w, id+" "+s+"\n")
}

func (o *serveOut) end(id string, code int) {
	o.line(id, proto.ServeEnd+strconv.Itoa(code))
}

// serveLines 一个请求的输出：按行切开，每行加上请求号。
type serveLines struct {
	id  string
	out *serveOut
	buf []byte
}

func (l *serveLines) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		l.out.line(l.id, string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

func (l *serveLines) flush() {
	if len(l.buf) > 0 {
		l.out.line(l.id, string(l.buf))
		l.buf = nil
	}
}
