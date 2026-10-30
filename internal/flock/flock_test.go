package flock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(p, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	waited := false
	if _, err := Lock(p, 600*time.Millisecond, func() { waited = true }); err != ErrTimeout || !waited {
		t.Fatalf("第二把锁应超时：%v %v", err, waited)
	}
	unlock()
	u2, err := Lock(p, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	u2()
}
