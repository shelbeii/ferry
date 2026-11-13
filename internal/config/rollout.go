package config

import "time"

// Rollout 一次部署到多台服务器时（ferry deploy a,b,c，或桌面版多选）怎么切换。不写 = 一台一台切换，一台失败就停。
type Rollout struct {
	// Batch 一次切换几台（同一批同时切换，批与批之间一批做完再做下一批），默认 1
	Batch int `yaml:"batch"`
	// Canary 先切换几台，成功后暂停，确认了再部署其余的；0 = 不暂停（默认）
	Canary int `yaml:"canary"`
	// Observe 先切换的那几台上线后观察多久（这期间健康检查一直要通过才继续），默认不观察
	Observe *Duration `yaml:"observe"`
	// OnFailure 有一台失败时：stop 停下，后面的不部署（默认）；rollback 再把这次已经切换的回滚到部署前的版本
	OnFailure string `yaml:"onFailure"`
}

// 失败时的做法
const (
	OnFailureStop     = "stop"
	OnFailureRollback = "rollback"
)

// RolloutMaxBatch 一批最多几台。
const RolloutMaxBatch = 50

// RolloutMaxObserve 最长观察多久。
const RolloutMaxObserve = time.Hour

func checkRollout(r *Rollout, bad func(string, ...any)) {
	if r == nil {
		return
	}
	if r.Batch < 0 || r.Batch > RolloutMaxBatch {
		bad("rollout.batch：一次切换几台，写 1～%d（当前 %d）", RolloutMaxBatch, r.Batch)
	}
	if r.Canary < 0 || r.Canary > RolloutMaxBatch {
		bad("rollout.canary：先切换几台，写 0～%d（当前 %d）", RolloutMaxBatch, r.Canary)
	}
	if r.Observe != nil && (r.Observe.Std() < 0 || r.Observe.Std() > RolloutMaxObserve) {
		bad("rollout.observe：写 0～1h，如 2m（当前 %s）", r.Observe.Std())
	}
	if r.Observe != nil && r.Observe.Std() > 0 && r.Canary == 0 {
		bad("rollout.observe：观察的是先切换的那几台，要同时写 canary（如 canary: 1）")
	}
	switch r.OnFailure {
	case "", OnFailureStop, OnFailureRollback:
	default:
		bad("rollout.onFailure：写 stop（停下，默认）或 rollback（把这次已经切换的回滚）（当前 %q）", r.OnFailure)
	}
}
