package engine

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/safe"
)

// Ports 服务器上的端口与防火墙（只读查看）。
func (e *Engine) Ports(target string) (*proto.PortReport, error) {
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		return s.callData("ports", &proto.Request{KeepPorts: keepPorts(s)}, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PortSet 用服务器的防火墙（firewalld / ufw）放行（open）或关闭（close）一个端口，返回改完的情况。
func (e *Engine) PortSet(target string, port int, pr, action string) (*proto.PortReport, error) {
	if action != "open" && action != "close" {
		return nil, fmt.Errorf("未知操作：%s（open 或 close）", action)
	}
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		if s.t.NeedConfirm() && !e.Yes {
			q := fmt.Sprintf("在 %s 的防火墙上放行端口 %d/%s？之后外部都能连这个端口", s.t.Name, port, pr)
			if action == "close" {
				q = fmt.Sprintf("在 %s 的防火墙上关闭端口 %d/%s？之后外部连不上这个端口（服务器本机不受影响）", s.t.Name, port, pr)
			}
			if !e.Rep.Confirm(q) {
				return ErrCanceled
			}
		}
		return s.callData("portset", &proto.Request{Port: port, PortProto: pr, Action: action, KeepPorts: keepPorts(s)}, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// keepPorts Ferry 连这台服务器用的端口：不许关闭。
func keepPorts(s *session) []int {
	if s.t.Port > 0 {
		return []int{s.t.Port}
	}
	return []int{22}
}

// PortProbe 从本机连服务器的一个端口的结果。
type PortProbe struct {
	Port int   `json:"port"`
	OK   bool  `json:"ok"`
	Ms   int64 `json:"ms"`
	// Why 连不上的原因：refused（服务器回绝：没有程序在监听，或防火墙拒绝）| timeout（没有回应：多半被防火墙、安全组丢弃）| error
	Why string `json:"why,omitempty"`
	Msg string `json:"msg,omitempty"`
}

// PortCheck 从本机（运行 Ferry 的电脑）连服务器的这些 TCP 端口，看能不能连上（不连 SSH，不改动服务器）。
func (e *Engine) PortCheck(target string, ports []int, timeout time.Duration) ([]PortProbe, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	if len(ports) == 0 {
		return nil, errors.New("要指定端口")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	out := make([]PortProbe, len(ports))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, p := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer safe.Recover("测端口", nil)
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = probePort(t.Host, p, timeout)
		}()
	}
	wg.Wait()
	return out, nil
}

func probePort(host string, port int, timeout time.Duration) PortProbe {
	r := PortProbe{Port: port}
	start := time.Now()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	r.Ms = time.Since(start).Milliseconds()
	if err == nil {
		c.Close()
		r.OK = true
		return r
	}
	r.Msg = err.Error()
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(r.Msg, "refused"):
		r.Why = "refused"
	case errors.As(err, &ne) && ne.Timeout():
		r.Why = "timeout"
	default:
		r.Why = "error"
	}
	return r
}

// FirewallEnable 打开服务器上装了没开的防火墙（firewalld / ufw）：agent 先放行 SSH 端口和 allow 里的端口、安排自动恢复，
// 再打开；这里用新的连接试 SSH 端口，连得上才取消自动恢复（连不上时防火墙过几分钟自己关掉，恢复原样）。
func (e *Engine) FirewallEnable(target string, allow []string, restartDocker bool) (*proto.PortReport, error) {
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		if s.t.NeedConfirm() && !e.Yes {
			q := fmt.Sprintf("打开 %s 的防火墙？之后只有 SSH 端口和 %s 能从外部连", s.t.Name, strings.Join(allow, "、"))
			if len(allow) == 0 {
				q = fmt.Sprintf("打开 %s 的防火墙？之后外部只能连 SSH 端口", s.t.Name)
			}
			if !e.Rep.Confirm(q) {
				return ErrCanceled
			}
		}
		if err := s.callData("fwenable", &proto.Request{Allow: allow, RestartDocker: restartDocker, KeepPorts: keepPorts(s)}, &out); err != nil {
			return err
		}
		port := s.t.Port
		if port == 0 {
			port = 22
		}
		addr := net.JoinHostPort(s.t.Host, strconv.Itoa(port))
		e.Rep.Info("用新的连接试 %s 能不能连上", addr)
		var last error
		for i := 0; i < 3; i++ {
			c, err := net.DialTimeout("tcp", addr, 10*time.Second)
			if err == nil {
				c.Close()
				last = nil
				break
			}
			last = err
			time.Sleep(2 * time.Second)
		}
		if last != nil {
			return fmt.Errorf("打开防火墙后，新的连接连不上 %s（%v）：几分钟内防火墙会自动关掉，恢复原样", addr, last)
		}
		return s.callData("fwconfirm", &proto.Request{KeepPorts: keepPorts(s)}, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FirewallDisable 关掉服务器上正在运行的防火墙（firewalld / ufw），开机也不再启动；放行规则留着，再打开时照旧。
// restartDocker：Docker 在运行、关的是 firewalld 时要同时重启 Docker。
func (e *Engine) FirewallDisable(target string, restartDocker bool) (*proto.PortReport, error) {
	var out proto.PortReport
	err := e.withTarget(target, func(s *session) error {
		if s.t.NeedConfirm() && !e.Yes {
			q := fmt.Sprintf("关闭 %s 的防火墙？之后所有端口都不拦，服务器上对外监听的端口外部都能连", s.t.Name)
			if !e.Rep.Confirm(q) {
				return ErrCanceled
			}
		}
		return s.callData("fwdisable", &proto.Request{RestartDocker: restartDocker, KeepPorts: keepPorts(s)}, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
