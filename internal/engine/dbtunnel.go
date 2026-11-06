package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Tunnel 经一台服务器连数据库：占着到服务器的连接（连接池里的连接不会因空闲被断开），
// 从服务器那边连数据库的端口（SSH 本地端口转发），不用在防火墙上放行数据库端口。
type Tunnel struct {
	s *session
}

// OpenTunnel 连上目标，返回经它转发的通道；用完调用 Close。
func (e *Engine) OpenTunnel(target string) (*Tunnel, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	s, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	return &Tunnel{s: s}, nil
}

// Dial 从服务器那边连 addr。
func (tn *Tunnel) Dial(ctx context.Context, addr string) (net.Conn, error) {
	c, err := tn.s.c.DialTCP(ctx, addr)
	if err != nil {
		if !tn.s.c.Alive() {
			return nil, fmt.Errorf("到服务器 %s 的连接已断开：%w", tn.s.label, err)
		}
		var oe *ssh.OpenChannelError
		if errors.As(err, &oe) && oe.Reason == ssh.Prohibited {
			return nil, fmt.Errorf("服务器 %s 不允许 SSH 端口转发（要在 /etc/ssh/sshd_config 里把 AllowTcpForwarding 设成 yes，再重启 sshd）：%w", tn.s.label, err)
		}
		return nil, fmt.Errorf("从服务器 %s 上连不上 %s（程序没在运行，或地址、端口不对）：%w", tn.s.label, addr, err)
	}
	return c, nil
}

// Alive 到服务器的连接还在。
func (tn *Tunnel) Alive() bool { return tn.s.c.Alive() }

// Label user@host:port
func (tn *Tunnel) Label() string { return tn.s.label }

// Close 不再使用（连接池里的连接留着给别的操作用）。
func (tn *Tunnel) Close() { tn.s.close() }

// ContainerAddr Docker 容器在服务器上的 IP（数据库端口没有映射到主机时，从服务器上直接连容器）。
func (tn *Tunnel) ContainerAddr(ctx context.Context, container string, port int) (string, error) {
	if strings.ContainsAny(container, "'\"\\ $`;&|<>") {
		return "", fmt.Errorf("容器名 %q 不合法", container)
	}
	cmd := tn.s.sudo + "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' " + shQuote(container)
	out, r, err := tn.s.c.Output(cmd)
	if err != nil {
		return "", err
	}
	if r.Code != 0 {
		return "", fmt.Errorf("查不到容器 %s 的地址：%s", container, strings.TrimSpace(r.Stderr+" "+out))
	}
	for _, ip := range strings.Fields(out) {
		if net.ParseIP(ip) != nil {
			return net.JoinHostPort(ip, strconv.Itoa(port)), nil
		}
	}
	return "", fmt.Errorf("容器 %s 没有可以连的 IP（可能用的是 host 网络，请直接填 127.0.0.1）", container)
}
