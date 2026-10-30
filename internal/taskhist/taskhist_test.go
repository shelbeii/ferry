package taskhist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rec(id, status string, started int64) *Record {
	return &Record{ID: id, Title: "部署 " + id, Status: status, StartedAt: started, Lines: json.RawMessage(`[{"kind":"step","msg":"构建"}]`)}
}

func TestSaveLoadList(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, rec("aaaaaa-1", OK, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, rec("aaaaaa-2", Failed, 2000)); err != nil {
		t.Fatal(err)
	}
	r, err := Load(dir, "aaaaaa-1")
	if err != nil || r.Title != "部署 aaaaaa-1" || string(r.Lines) != `[{"kind":"step","msg":"构建"}]` {
		t.Fatalf("load: %+v %v", r, err)
	}
	list, err := List(dir, false)
	if err != nil || len(list) != 2 || list[0].ID != "aaaaaa-2" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := Save(dir, &Record{ID: "../evil", Status: OK}); err == nil {
		t.Fatal("非法 ID 应该被拒绝")
	}
	if err := Save(dir, &Record{ID: "aaaaaa-3", Status: "weird"}); err == nil {
		t.Fatal("非法状态应该被拒绝")
	}
}

func TestListInterrupt(t *testing.T) {
	dir := t.TempDir()
	Save(dir, rec("bbbbbb-1", Running, 1000))
	list, _ := List(dir, false)
	if list[0].Status != Running {
		t.Fatalf("不带 interrupt 不应改动：%+v", list[0])
	}
	list, _ = List(dir, true)
	if list[0].Status != Interrupted || list[0].EndedAt == 0 || list[0].Error != InterruptedMsg {
		t.Fatalf("应改成中断：%+v", list[0])
	}
	r, _ := Load(dir, "bbbbbb-1")
	if r.Status != Interrupted || len(r.Lines) == 0 {
		t.Fatalf("应写回文件且保留输出：%+v", r)
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	Save(dir, rec("cccccc-1", OK, 1))
	Save(dir, rec("cccccc-2", OK, 2))
	n, err := Delete(dir, "cccccc-1", "cccccc-9")
	if err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if c, _ := Usage(dir); c != 1 {
		t.Fatalf("还剩 %d 个", c)
	}
}

func TestClean(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, id := range []string{"dddddd-1", "dddddd-2", "dddddd-3", "dddddd-4"} {
		Save(dir, rec(id, OK, int64(i)))
		// 越前面越旧：40、20、10、5 天前
		days := []int{40, 20, 10, 5}[i]
		os.Chtimes(filepath.Join(dir, id+".json"), now, now.Add(-time.Duration(days)*24*time.Hour))
	}
	// 按天数：只有 40 天前的超过 30 天
	if got := Clean(dir, 30, 0, 0, ""); len(got) != 1 || got[0] != "dddddd-1" {
		t.Fatalf("按天数：%v", got)
	}
	// 按个数：最多 1 个，从最旧的删起；keep 的不删
	if got := Clean(dir, 0, 0, 1, "dddddd-2"); len(got) != 2 || got[0] != "dddddd-3" || got[1] != "dddddd-4" {
		t.Fatalf("按个数：%v", got)
	}
	// 按大小：上限 1 字节，keep 以外的都删
	Save(dir, rec("dddddd-5", OK, 5))
	if got := Clean(dir, 0, 1, 0, "dddddd-5"); len(got) != 1 || got[0] != "dddddd-2" {
		t.Fatalf("按大小：%v", got)
	}
	if c, _ := Usage(dir); c != 1 {
		t.Fatalf("还剩 %d 个", c)
	}
	if CleanAll(dir) != 1 {
		t.Fatal("CleanAll")
	}
}
