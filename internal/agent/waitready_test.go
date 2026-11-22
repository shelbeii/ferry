package agent

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func TestWaitBackoff(t *testing.T) {
	want := []time.Duration{
		300 * time.Millisecond, 300 * time.Millisecond,
		500 * time.Millisecond, 500 * time.Millisecond,
		time.Second, time.Second, time.Second, time.Second,
		2 * time.Second, 2 * time.Second,
	}
	for i, w := range want {
		if got := waitBackoff(i); got != w {
			t.Errorf("第 %d 次等 %v，应当 %v", i, got, w)
		}
	}
}

// lateHealthySys 到某个时刻健康检查才通过。
type lateHealthySys struct {
	*fakeSys
	readyAt time.Time
}

func (s *lateHealthySys) Probe(proto.Health) error {
	if s.now.Before(s.readyAt) {
		return errors.New("还没起来")
	}
	return nil
}

// 应用 1.1 秒就绪时不该等到 2 秒开外（以前固定 2 秒一轮，每个应用平均白等一秒）。
func TestWaitReadyDetectsEarly(t *testing.T) {
	f := newFake(t)
	sys := &lateHealthySys{fakeSys: f, readyAt: f.now.Add(1100 * time.Millisecond)}
	a := &Agent{Sys: sys, Out: io.Discard, req: &proto.Request{}}
	s := &proto.AppSpec{Name: "api", Unit: "demo-api.service", Health: proto.Health{TCP: "127.0.0.1:8080", TimeoutSec: 30}}
	u := f.unit(s.Unit)
	u.active, u.sub, u.pid = "active", "running", 100
	start := f.now
	if err := a.waitReady(s, s.Unit); err != nil {
		t.Fatalf("等就绪：%v", err)
	}
	if elapsed := f.now.Sub(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("就绪判定用了 %v，应当紧跟在 1.1 秒之后", elapsed)
	}
}

// 一直起不来：到超时就停，不能无限等。
func TestWaitReadyTimesOut(t *testing.T) {
	f := newFake(t)
	sys := &lateHealthySys{fakeSys: f, readyAt: f.now.Add(time.Hour)}
	a := &Agent{Sys: sys, Out: io.Discard, req: &proto.Request{}}
	s := &proto.AppSpec{Name: "api", Unit: "demo-api.service", Health: proto.Health{TCP: "127.0.0.1:8080", TimeoutSec: 5}}
	u := f.unit(s.Unit)
	u.active, u.sub, u.pid = "active", "running", 100
	start := f.now
	if err := a.waitReady(s, s.Unit); err == nil {
		t.Fatal("应当超时报错")
	}
	if elapsed := f.now.Sub(start); elapsed < 5*time.Second || elapsed > 8*time.Second {
		t.Fatalf("超时用了 %v，应当在 5 秒出头", elapsed)
	}
}
