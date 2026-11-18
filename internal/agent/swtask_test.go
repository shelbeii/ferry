package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestSoftwareTaskResumesByID(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	id := "install-task-123"
	req := &proto.Request{FerryDir: dir, SoftwareTaskID: id}
	worker := &Agent{Sys: f, Out: io.Discard, req: req}
	plan := &proto.SoftwarePlan{TaskID: id, Name: "nacos", Env: map[string]string{"PASSWORD": "not-for-history"}}
	release, err := worker.beginSoftwareTask(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	worker.installPhase(plan.Name, "初始化")
	client := func(want string) {
		t.Helper()
		got := exec1(t, f, "swtask", &proto.Request{Root: dir, FerryDir: dir, SoftwareTaskID: id})
		var task proto.SoftwareTask
		if err := json.Unmarshal(got.result.Data, &task); err != nil || task.Status != want || task.Phase != "初始化" {
			t.Fatalf("恢复状态：%+v %v", task, err)
		}
	}
	client("running")
	if _, err := worker.beginSoftwareTask(plan); err == nil {
		t.Fatal("运行中重复提交")
	}
	worker.finishSoftwareTask(proto.CodeOK)
	release()
	client("ok")
	if _, err := worker.beginSoftwareTask(plan); err == nil {
		t.Fatal("已完成的任务重复提交")
	}
	body, err := os.ReadFile(filepath.Join(dir, "software", ".tasks", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || len(fields) != 7 {
		t.Fatalf("持久化应仅保留任务元数据，字段数 %d：%v", len(fields), err)
	}
}
