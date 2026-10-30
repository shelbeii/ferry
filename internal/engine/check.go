package engine

import (
	"errors"
	"time"

	"github.com/captain/ferry/internal/remote"
)

// ServerCheck 测试连接的结果。
type ServerCheck struct {
	Target string `json:"target"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	// ConnectMs 连上用了多久（复用已有连接时很短）
	ConnectMs int64 `json:"connectMs"`
	// RTT 到服务器的网络往返时间（毫秒）；Slow 时 3 秒内没有回应
	RTT  float64 `json:"rtt,omitempty"`
	Slow bool    `json:"slow,omitempty"`
}

// ServerCheck 连服务器（有连接池时复用已有连接），量一次网络延迟。连不上时返回说明原因的错误。
func (e *Engine) ServerCheck(target string) (*ServerCheck, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	out := &ServerCheck{Target: t.Name, Host: t.Host, Port: t.Port, User: t.User}
	start := time.Now()
	err = e.withConn(target, func(s *session) error {
		out.ConnectMs = time.Since(start).Milliseconds()
		d, err := s.c.Latency(LatencyTimeout, 0)
		switch {
		case errors.Is(err, remote.ErrSlow):
			out.Slow = true
		case err != nil:
			return err
		default:
			out.RTT = latencyMs(d)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
