package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRolloutConfig(t *testing.T) {
	head := "version: 1\nproject: p\ntargets:\n  a: {host: 1.2.3.4, runAs: demo}\napps:\n  - name: api\n    type: java\n    artifact: {path: api.jar}\n"
	load := func(s string) (*File, error) {
		p := filepath.Join(t.TempDir(), "ferry.yaml")
		os.WriteFile(p, []byte(head+s), 0o644)
		return Load(p)
	}
	f, err := load("rollout:\n  batch: 2\n  canary: 1\n  observe: 2m\n  onFailure: rollback\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := f.Rollout; r == nil || r.Batch != 2 || r.Canary != 1 || r.Observe.Std() != 2*time.Minute || r.OnFailure != OnFailureRollback {
		t.Fatalf("%+v", f.Rollout)
	}
	for what, c := range map[string]string{
		"batch 太多":          "rollout: {batch: 51}\n",
		"canary 是负数":        "rollout: {canary: -1}\n",
		"observe 太长":        "rollout: {canary: 1, observe: 2h}\n",
		"observe 没有 canary": "rollout: {observe: 1m}\n",
		"onFailure 写错":      "rollout: {onFailure: retry}\n",
	} {
		if _, err := load(c); err == nil || !strings.Contains(err.Error(), "rollout.") {
			t.Errorf("%s：%v", what, err)
		}
	}
}
