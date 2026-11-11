package remote

import (
	"context"
	"errors"

	"golang.org/x/crypto/ssh"

	"github.com/captain/ferry/internal/safe"
)

// 服务器的 sshd 限制每条连接同时开的会话数（OpenSSH 的 MaxSessions，默认 10）。终端、文件、实时日志、
// 各种查看都在同一条连接上开会话，开多了服务器会拒绝（OpenSSH 回 open failed）。
// 这时再建一条连接（参数和原来的一样），新的会话开在那里；最多另建 maxExtra 条，随主连接一起关闭。

// maxExtra 会话开满后最多另建几条连接。
const maxExtra = 3

// isSessionLimit 是不是服务器因会话数到了上限拒绝开会话。OpenSSH 这时回的是 connect failed（open failed），
// 有的服务器回 administratively prohibited：都算。只用在开会话（session 通道）时，不会和端口转发连不上混淆。
func isSessionLimit(err error) bool {
	var oe *ssh.OpenChannelError
	return errors.As(err, &oe) && (oe.Reason == ssh.ConnectionFailed || oe.Reason == ssh.Prohibited)
}

// newSession 开一个会话：主连接开满了就开在另建的连接上。
func (c *Client) newSession() (*ssh.Session, error) {
	var s *ssh.Session
	err := c.onSessionConn(func(conn *ssh.Client) (err error) {
		s, err = conn.NewSession()
		return err
	})
	return s, err
}

// newSessionCtx 同 newSession，ctx 取消时不再等：服务器不回应开会话的请求（卡住了）时，取消也能马上返回。
// 取消以后才开好的会话随即关掉。
func (c *Client) newSessionCtx(ctx context.Context) (*ssh.Session, error) {
	if ctx.Done() == nil {
		return c.newSession()
	}
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}
	type opened struct {
		s   *ssh.Session
		err error
	}
	ch := make(chan opened, 1)
	safe.Go("开会话", func() {
		s, err := c.newSession()
		ch <- opened{s, err}
	})
	select {
	case o := <-ch:
		return o.s, o.err
	case <-ctx.Done():
		safe.Go("关掉取消后才开好的会话", func() {
			if o := <-ch; o.s != nil {
				o.s.Close()
			}
		})
		return nil, ErrCanceled
	}
}

// onSessionConn 用主连接执行 open（开会话、开 SFTP）；服务器因会话数上限拒绝时，换另建的连接（没有或都满了就再建一条）。
func (c *Client) onSessionConn(open func(conn *ssh.Client) error) error {
	err := open(c.ssh)
	if err == nil || !isSessionLimit(err) {
		return err
	}
	c.extraMu.Lock()
	defer c.extraMu.Unlock()
	for i := 0; i < len(c.extra); i++ {
		e := c.extra[i]
		err2 := open(e)
		if err2 == nil {
			return nil
		}
		if !isSessionLimit(err2) { // 这条另建的连接断了：去掉，试下一条
			e.Close()
			c.extra = append(c.extra[:i], c.extra[i+1:]...)
			i--
		}
	}
	if len(c.extra) >= maxExtra || c.dead.Load() {
		return err
	}
	e, derr := dialExtra(c.opts)
	if derr != nil {
		return err // 再建连接也失败：报原来「会话数到了上限」的错，比连接错误好懂
	}
	if err2 := open(e); err2 != nil {
		e.Close()
		return err2
	}
	c.extra = append(c.extra, e)
	return nil
}

// dialExtra 用同样的参数再建一条连接。保活和 Dial 里的一样，连接关闭后自己结束。
func dialExtra(o Options) (*ssh.Client, error) {
	extra, err := Dial(o)
	if err != nil {
		return nil, err
	}
	return extra.ssh, nil
}

// closeExtra 关闭另建的连接（主连接关闭时）。
func (c *Client) closeExtra() {
	c.extraMu.Lock()
	defer c.extraMu.Unlock()
	for _, e := range c.extra {
		e.Close()
	}
	c.extra = nil
}
