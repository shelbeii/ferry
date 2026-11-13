package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/captain/ferry/internal/engine"
)

// rolloutFlags deploy 的 --batch、--canary、--observe、--on-failure（没给的用 ferry.yaml 的 rollout）。
func rolloutFlags(flags map[string]string) (engine.RolloutOptions, error) {
	var o engine.RolloutOptions
	if v := flags["batch"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return o, fmt.Errorf("--batch 写一次切换几台（≥1）：%q", v)
		}
		o.Batch = n
	}
	if v := flags["canary"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return o, fmt.Errorf("--canary 写先切换几台（0 = 不暂停）：%q", v)
		}
		o.Canary = &n
	}
	if v := flags["observe"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			n, err2 := strconv.Atoi(v)
			if err2 != nil {
				return o, fmt.Errorf("--observe 写观察多久，如 2m、90s：%q", v)
			}
			d = time.Duration(n) * time.Second
		}
		o.Observe = &d
	}
	o.OnFailure = flags["on-failure"]
	return o, engine.CheckRollout(o)
}
