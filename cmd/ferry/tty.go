package main

import (
	"bytes"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// ttyIO ferry serve 里一个请求的终端输入输出（只有 terminal 命令用到）。
type ttyIO struct {
	in     *ttyInput
	out    *ttyOutput
	resize chan [2]int // 列、行；只留最新的一个
}

func newTTY(emit func(string)) *ttyIO {
	return &ttyIO{in: newTTYInput(), out: newTTYOutput(emit), resize: make(chan [2]int, 1)}
}

// setSize 记下新的窗口大小（来不及处理时，旧的丢掉）。
func (t *ttyIO) setSize(cols, rows int) {
	for {
		select {
		case t.resize <- [2]int{cols, rows}:
			return
		default:
			select {
			case <-t.resize:
			default:
			}
		}
	}
}

// close 请求结束：发出剩下的输出，不再接收输入。
func (t *ttyIO) close() {
	t.in.Close()
	t.out.Close()
}

// ttyInput 键盘输入的队列：serve 收到就放进来（不阻塞读请求的循环），远程会话一边读一边发。
type ttyInput struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newTTYInput() *ttyInput {
	t := &ttyInput{}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *ttyInput) push(b []byte) {
	if len(b) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.buf.Write(b)
		t.cond.Signal()
	}
}

func (t *ttyInput) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for t.buf.Len() == 0 && !t.closed {
		t.cond.Wait()
	}
	if t.closed {
		return 0, io.EOF
	}
	return t.buf.Read(p)
}

func (t *ttyInput) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.buf.Reset()
	t.cond.Broadcast()
}

// ttyOutput 终端输出：攒一小会儿（最多 ttyWait，或攒满 ttyMax 字节）一起发，末尾不完整的 UTF-8 字符留到下一批。
// 安静了一阵之后来的输出（打字的回显）马上发，不等；接着来的才攒。
// 桌面版来不及显示时会暂停（flow），这时 Write 等着：SSH 不再读远程的输出，远程程序也跟着停下，界面不会被刷爆。
type ttyOutput struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	timer  *time.Timer
	paused bool
	closed bool
	emit   func(string)
	last   time.Time // 上一次发出的时间
}

const (
	ttyWait = 10 * time.Millisecond // 打字的回显要快
	ttyMax  = 64 << 10
)

func newTTYOutput(emit func(string)) *ttyOutput {
	t := &ttyOutput{emit: emit}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *ttyOutput) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for t.paused && !t.closed {
		t.cond.Wait()
	}
	if t.closed {
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) >= ttyMax || (t.timer == nil && time.Since(t.last) >= ttyWait) {
		t.flushLocked(false)
	}
	// 没发完的（接着来的输出、被切断的半个字符）一会儿再发
	if len(t.buf) > 0 && t.timer == nil {
		t.timer = time.AfterFunc(ttyWait, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.timer = nil
			t.flushLocked(false)
		})
	}
	return len(p), nil
}

func (t *ttyOutput) flushLocked(final bool) {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	n := len(t.buf)
	if !final {
		n = utf8Complete(t.buf)
	}
	if n == 0 {
		return
	}
	s := string(t.buf[:n])
	t.buf = t.buf[:copy(t.buf, t.buf[n:])]
	t.last = time.Now()
	t.emit(s)
}

func (t *ttyOutput) setPaused(v bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.paused = v
	t.cond.Broadcast()
}

// Close 发出剩下的输出；之后的输出丢弃（Write 不再等）。
func (t *ttyOutput) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.flushLocked(true)
		t.closed = true
	}
	t.cond.Broadcast()
}

// utf8Complete b 里到哪里为止都是完整的字符：末尾被切断的多字节字符不算。
func utf8Complete(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}
