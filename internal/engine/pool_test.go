package engine

import (
	"errors"
	"testing"

	"github.com/captain/ferry/internal/config"
)

// 后台自动刷新：没有连着时直接返回，不去连服务器（否则空闲断开后又被自动连上）
func TestBackgroundNoDial(t *testing.T) {
	p := NewPool(0)
	defer p.Close()
	f := &config.File{Path: "ferry.yaml"}
	tg := &config.Target{Name: "prod", Host: "192.0.2.1", Port: 22, User: "root"}
	e := &Engine{File: f, Pool: p, Background: true}
	if _, _, err := p.Acquire(e, tg); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("应该是 ErrNotConnected：%v", err)
	}
	if len(p.entries) != 0 {
		t.Fatalf("不应该开始连接：%d", len(p.entries))
	}
}
