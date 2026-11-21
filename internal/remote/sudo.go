package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

var ErrSudoRejected = errors.New("sudo 密码被服务器拒绝")

type SudoError struct {
	Kind    string
	Message string
}

func (e *SudoError) Error() string { return e.Message }
func (e *SudoError) JSONDetail() map[string]any {
	return map[string]any{"kind": e.Kind}
}

func ExplainSudoError(output string) error {
	msg := strings.TrimSpace(output)
	switch lower := strings.ToLower(msg); {
	case strings.Contains(lower, "a password is required") || strings.Contains(lower, "a terminal is required to read the password"):
		return &SudoError{"sudo-auth-mode", "服务器要求 sudo 密码，请把此目标的 sudo 认证设为使用密码"}
	case strings.Contains(lower, "must have a tty") || strings.Contains(lower, "no tty present"):
		return &SudoError{"sudo-terminal-required", "服务器 sudo 策略要求交互终端，请在终端执行或请管理员检查该操作的 sudo 策略"}
	case strings.Contains(lower, "not in the sudoers file") || strings.Contains(lower, "not allowed to execute") || strings.Contains(lower, "may not run sudo"):
		return &SudoError{"sudo-policy-denied", "服务器 sudoers 未授权此操作：" + msg}
	case strings.Contains(lower, "sudo: command not found") || strings.Contains(lower, "/usr/bin/sudo: not found"):
		return &SudoError{"sudo-unavailable", "服务器找不到 sudo，请检查管理员提供的提权方式"}
	}
	return nil
}

// StartSudoPipe only sends the credential if sudo prompts for it. Command input
// is sent separately, after the privileged shell acknowledges that it has started.
func (c *Client) StartSudoPipe(ctx context.Context, cmd string, password func() (string, error)) (*Pipe, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stopped := func(err error) error {
		if errors.Is(parent.Err(), context.Canceled) {
			return ErrCanceled
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &SudoError{"sudo-timeout", "等待服务器 sudo 认证超时，请检查连接或服务器的 sudo 策略后重试"}
		}
		return err
	}
	token := rand.Text()
	prompt, ready := "FERRY_SUDO_PROMPT_"+token+":", "FERRY_SUDO_READY_"+token+":"
	signals := &sudoSignals{prompt: []byte(prompt), ready: []byte(ready), events: make(chan bool, 3)}
	script := "printf %s " + sudoQuote(ready) + " >&2; exec /bin/sh -c " + sudoQuote(cmd)
	p, err := c.startPipe(ctx, "LC_ALL=C /usr/bin/sudo -k -S -u root -p "+sudoQuote(prompt)+" -- /bin/sh -c "+sudoQuote(script), signals)
	if err != nil {
		return nil, stopped(err)
	}
	p.markers = []string{prompt, ready}
	p.done = make(chan struct{})
	go func() {
		p.err = p.s.Wait()
		close(p.done)
	}()
	stop := context.AfterFunc(ctx, func() { p.Close() })
	defer stop()
	keep := false
	defer func() {
		if !keep {
			p.Close()
		}
	}()
	sent := false
	for {
		select {
		case isReady := <-signals.events:
			if isReady {
				if !stop() && ctx.Err() != nil {
					return nil, stopped(ErrCanceled)
				}
				keep = true
				return p, nil
			}
			if sent {
				return nil, ErrSudoRejected
			}
			v, err := password()
			if err != nil {
				return nil, stopped(err)
			}
			if v == "" || strings.ContainsAny(v, "\r\n\x00") {
				return nil, errors.New("sudo 密码不能为空，也不能包含换行或 NUL")
			}
			if _, err := io.WriteString(p.In, v+"\n"); err != nil {
				return nil, stopped(err)
			}
			sent = true
		case <-p.done:
			// The command may finish before the waiting goroutine sees the marker.
			if signals.started {
				keep = true
				return p, nil
			}
			msg := p.Stderr()
			if sent && (strings.Contains(msg, "try again") || strings.Contains(msg, "incorrect password")) {
				return nil, ErrSudoRejected
			}
			if err := ExplainSudoError(msg); err != nil {
				return nil, stopped(err)
			}
			msg = strings.TrimSpace(msg)
			if msg == "" {
				msg = "服务器未确认 sudo 命令已启动"
			}
			return nil, stopped(fmt.Errorf("sudo 执行失败：%s", msg))
		case <-ctx.Done():
			return nil, stopped(ErrCanceled)
		}
	}
}

func sudoQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// sudoSignals scans only the authentication prefix; it handles markers split
// across multiple SSH packets and never consumes the command's stdout.
type sudoSignals struct {
	prompt, ready []byte
	tail          []byte
	events        chan bool
	started       bool
}

func (s *sudoSignals) Write(p []byte) (int, error) {
	if s.started {
		return len(p), nil
	}
	s.tail = append(s.tail, p...)
	for {
		i, j := bytes.Index(s.tail, s.prompt), bytes.Index(s.tail, s.ready)
		if j >= 0 && (i < 0 || j < i) {
			s.started = true
			s.tail = nil
			s.events <- true
			break
		}
		if i < 0 {
			n := max(len(s.prompt), len(s.ready)) - 1
			if len(s.tail) > n {
				s.tail = append(s.tail[:0], s.tail[len(s.tail)-n:]...)
			}
			break
		}
		s.tail = s.tail[i+len(s.prompt):]
		select {
		case s.events <- false:
		default:
		}
	}
	return len(p), nil
}

func (c *Client) RunSudo(ctx context.Context, cmd string, input []byte, password func() (string, error), onLine func(string)) (RunResult, error) {
	p, err := c.StartSudoPipe(ctx, cmd, password)
	if err != nil {
		return RunResult{}, err
	}
	defer p.Close()
	stop := context.AfterFunc(ctx, func() { p.Close() })
	defer stop()
	go func() {
		if len(input) > 0 {
			p.In.Write(input)
		}
		p.In.Close()
	}()
	sc := bufio.NewScanner(p.Out)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if onLine != nil {
			onLine(sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		p.Close()
	}
	res, err := sudoResult(p)
	if ctx.Err() != nil {
		return res, ErrCanceled
	}
	if err == nil {
		c.markOK()
		err = sc.Err()
	}
	return res, err
}

func (c *Client) StreamSudo(ctx context.Context, cmd string, dst io.Writer, password func() (string, error)) error {
	p, err := c.StartSudoPipe(ctx, cmd, password)
	if err != nil {
		return err
	}
	defer p.Close()
	stop := context.AfterFunc(ctx, func() { p.Close() })
	defer stop()
	p.In.Close()
	_, copyErr := io.Copy(dst, p.Out)
	if copyErr != nil {
		p.Close()
	}
	res, err := sudoResult(p)
	if ctx.Err() != nil {
		return ErrCanceled
	}
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("退出码 %d：%s", res.Code, strings.TrimSpace(res.Stderr))
	}
	return errors.Join(copyErr, err)
}

func sudoResult(p *Pipe) (RunResult, error) {
	err := p.Wait()
	res := RunResult{Stderr: p.Stderr()}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return RunResult{Code: exit.ExitStatus(), Stderr: res.Stderr}, nil
	}
	return res, err
}
