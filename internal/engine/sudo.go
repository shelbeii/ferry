package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/secret"
)

func (s *session) allowsSudo() bool {
	return s.sudo != "" && s.t != nil && s.t.SudoAuth != "disabled" && (s.t.Sudo || s.t.Plain)
}

func (s *session) passwordSudo() bool {
	return s.allowsSudo() && s.t.SudoAuth == "password"
}

func (s *session) sudoNeed(rejected bool) *secret.NeedError {
	t := s.t
	return &secret.NeedError{
		Kind: secret.KindSudo, Target: t.Name, Rejected: rejected,
		Env: secret.SudoEnv(t.User, t.Host, t.Port), Account: secret.SudoAccount(t.User, t.Host, t.Port),
		Prompt: fmt.Sprintf("%s@%s:%d 的 sudo 密码", t.User, t.Host, t.Port),
	}
}

func (s *session) sudoPassword() (string, error) {
	need := s.sudoNeed(false)
	v, source, err := secret.Lookup(func(key string) string {
		if v := s.e.getenv(key); v != "" {
			return v
		}
		return s.e.getenv(secret.SudoTargetEnv(s.t.Name))
	}, need.Env, need.Account)
	if err != nil {
		s.e.Rep.Warn("%v", err)
	}
	if source != secret.FromNone {
		return v, nil
	}
	// A standalone CLI command may open several SSH channels after one prompt.
	if s.e.Pool == nil {
		s.sudoMu.Lock()
		defer s.sudoMu.Unlock()
		if s.sudoPrompted != "" {
			return s.sudoPrompted, nil
		}
		if v, ok := s.e.Rep.Secret(need.Prompt + "："); ok && v != "" {
			s.sudoPrompted = v
			return v, nil
		}
	}
	return "", need
}

func (s *session) sudoError(err error) error {
	if errors.Is(err, remote.ErrSudoRejected) {
		s.sudoMu.Lock()
		s.sudoPrompted = ""
		s.sudoMu.Unlock()
		return s.sudoNeed(true)
	}
	return err
}

func isPrivilegeError(err error) bool {
	var sudo *remote.SudoError
	var need *secret.NeedError
	return errors.As(err, &sudo) || (errors.As(err, &need) && need.Kind == secret.KindSudo)
}

func (s *session) runPriv(ctx context.Context, cmd string, stdin []byte, onLine func(string)) (remote.RunResult, error) {
	if s.sudo != "" && !s.allowsSudo() {
		return remote.RunResult{}, &remote.SudoError{Kind: "sudo-disabled", Message: "此目标未启用 sudo，无法执行需要提权的操作"}
	}
	if !s.passwordSudo() {
		res, err := s.c.RunContext(ctx, s.sudo+cmd, stdin, onLine)
		if s.sudo != "" && err == nil && res.Code != 0 {
			err = remote.ExplainSudoError(res.Stderr)
		}
		return res, err
	}
	res, err := s.c.RunSudo(ctx, cmd, stdin, s.sudoPassword, onLine)
	return res, s.sudoError(err)
}

func (s *session) outputPriv(ctx context.Context, cmd string) (string, remote.RunResult, error) {
	var out strings.Builder
	res, err := s.runPriv(ctx, cmd, nil, func(line string) { out.WriteString(line); out.WriteByte('\n') })
	return out.String(), res, err
}

func (s *session) streamPriv(cmd string, out io.Writer, pty bool, stop <-chan struct{}) error {
	if s.sudo != "" && !s.allowsSudo() {
		return &remote.SudoError{Kind: "sudo-disabled", Message: "此目标未启用 sudo，无法执行需要提权的操作"}
	}
	if !s.passwordSudo() {
		return s.c.Stream(s.sudo+cmd, out, pty, stop)
	}
	ctx, cancel := context.WithCancel(s.e.ctx())
	defer cancel()
	if stop != nil {
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	err := s.c.StreamSudo(ctx, cmd, out, s.sudoPassword)
	if errors.Is(err, remote.ErrCanceled) && stop != nil {
		select {
		case <-stop:
			return nil
		default:
		}
	}
	return s.sudoError(err)
}

func (s *session) runPrivilegeProbe(ctx context.Context, script string, onLine func(string)) (remote.RunResult, error) {
	if s.sudo != "" && (s.passwordSudo() || s.t.SudoAuth == "nopasswd") && (s.t.Sudo || s.t.Plain) {
		return s.runPriv(ctx, "sh -c "+shQuote("printf 'priv sudo\\n'; exec sh -s"), []byte(script), onLine)
	}
	if s.t.User != "root" && (s.t.SudoAuth == "disabled" || (!s.t.Plain && !s.t.Sudo)) {
		return s.c.RunContext(ctx, "printf 'priv none\\n'; exec sh -s", []byte(script), onLine)
	}
	return s.c.RunContext(ctx, privWrap, []byte(script), onLine)
}

// VerifySudoPassword verifies a new credential before it replaces the saved one.
// A server that already allows NOPASSWD may start the command without checking it.
func (e *Engine) VerifySudoPassword(target, password string) (bool, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return false, err
	}
	if t.User == "root" || t.SudoAuth != "password" || (!t.Plain && !t.Sudo) {
		return false, errors.New("此目标没有启用 sudo 密码认证")
	}
	s, err := e.connectSSH(t)
	if err != nil {
		return false, err
	}
	defer s.close()
	used := false
	res, err := s.c.RunSudo(e.ctx(), "true", nil, func() (string, error) {
		used = true
		return password, nil
	}, nil)
	if err != nil {
		return false, s.sudoError(err)
	}
	if res.Code != 0 {
		return false, fmt.Errorf("验证 sudo 密码失败：%s", strings.TrimSpace(res.Stderr))
	}
	return used, nil
}
