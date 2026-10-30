package agent

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func gzText(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.String()
}

// 按天切分的日志：app.log（今天）、昨天的 app-2026-10-18.log、前天压缩的 app-2026-10-17.log.gz
func dailyLogs(t *testing.T, f *fakeSys) {
	t.Helper()
	day := func(rel, text string, ago int) {
		writeLog(t, f, "Job", rel, text)
		p := logPath(f, "Job", rel)
		mt := time.Date(2026, 10, 19, 10, 0, 0, 0, time.Local).Add(-time.Duration(ago) * 24 * time.Hour)
		os.Chtimes(p, mt, mt)
	}
	day("logs/app-2026-10-17.log.gz", gzText(t, "17 第 1 行\n17 ERROR 连接超时\n17 第 3 行\n"), 2)
	day("logs/app-2026-10-18.log", "18 第 1 行\n18 error 磁盘满\n18 第 3 行\n18 第 4 行\n", 1)
	day("logs/app-2026-10-19.log", "19 第 1 行\n19 第 2 行\n", 0)
}

func grep(t *testing.T, f *fakeSys, s proto.AppSpec, rel, run, file, kw string, ctx int) proto.LogSearch {
	t.Helper()
	r := exec1(t, f, "loggrep", &proto.Request{Apps: []proto.AppSpec{s}, LogRelease: rel, LogRun: run, LogFile: file, Grep: kw, GrepContext: ctx})
	var out proto.LogSearch
	if r.code != 0 || json.Unmarshal(r.result.Data, &out) != nil {
		t.Fatalf("loggrep %s：\n%s", kw, r.dump())
	}
	return out
}

func TestDailyLogs(t *testing.T) {
	f := newFake(t)
	s := logApp()
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"Job": []byte("v1")})); r.code != 0 {
		t.Fatalf("v1：\n%s", r.dump())
	}
	dailyLogs(t, f)

	// 文件按修改时间从新到旧
	rs := runs(t, f, s)
	var names []string
	for _, x := range rs[0].Files {
		names = append(names, x.Path)
	}
	if strings.Join(names, ",") != "logs/app-2026-10-19.log,logs/app-2026-10-18.log,logs/app-2026-10-17.log.gz" || rs[0].Files[0].MTime.IsZero() {
		t.Fatalf("文件顺序：%v", names)
	}
	// 压缩的直接解压
	if got := readLog(t, f, s, "", "", "logs/app-2026-10-17.log.gz"); strings.Join(got.Lines, "|") != "17 ERROR 连接超时|17 第 3 行" {
		t.Fatalf("读 .gz：%+v", got)
	}
	// 通配符：最近修改的那个，并告诉是哪个文件
	if got := readLog(t, f, s, "", "", "logs/app-*.log"); got.File != "logs/app-2026-10-19.log" || strings.Join(got.Lines, "|") != "19 第 1 行|19 第 2 行" {
		t.Fatalf("通配符：%+v", got)
	}
	if r := exec1(t, f, "logread", &proto.Request{Apps: []proto.AppSpec{s}, LogFile: "logs/nope-*.log"}); r.code == 0 || !strings.Contains(r.result.Msg, "没有匹配") {
		t.Fatalf("没有匹配的通配符：%s", r.dump())
	}
	// 某一行附近
	r := exec1(t, f, "logread", &proto.Request{Apps: []proto.AppSpec{s}, LogFile: "logs/app-2026-10-18.log", Around: 2, Lines: 300})
	var around proto.LogText
	json.Unmarshal(r.result.Data, &around)
	if around.First != 1 || len(around.Lines) != 4 {
		t.Fatalf("附近：%+v", around)
	}

	// 搜索全部文件（不区分大小写）：最近修改的文件的结果在前，带前后一行
	res := grep(t, f, s, "", "", "", "error", 1)
	if res.Files != 3 || len(res.Matches) != 2 {
		t.Fatalf("搜索：%+v", res)
	}
	m0, m1 := res.Matches[0], res.Matches[1]
	if m0.File != "logs/app-2026-10-18.log" || m0.Line != 2 || m0.Text != "18 error 磁盘满" || strings.Join(m0.Before, "|") != "18 第 1 行" || strings.Join(m0.After, "|") != "18 第 3 行" {
		t.Fatalf("第一条：%+v", m0)
	}
	if m1.File != "logs/app-2026-10-17.log.gz" || m1.Line != 2 {
		t.Fatalf("第二条（.gz 里）：%+v", m1)
	}
	// 只搜一个文件 / 通配符
	if res := grep(t, f, s, "", "", "logs/app-2026-10-18.log", "第", 0); res.Files != 1 || len(res.Matches) != 3 {
		t.Fatalf("搜一个文件：%+v", res)
	}
	if res := grep(t, f, s, "", "", "logs/*.gz", "第", 0); res.Files != 1 || len(res.Matches) != 2 {
		t.Fatalf("搜通配符：%+v", res)
	}
	// 区分大小写
	r = exec1(t, f, "loggrep", &proto.Request{Apps: []proto.AppSpec{s}, Grep: "ERROR", GrepCase: true})
	var cs proto.LogSearch
	json.Unmarshal(r.result.Data, &cs)
	if len(cs.Matches) != 1 || !strings.Contains(cs.Matches[0].Text, "连接超时") {
		t.Fatalf("区分大小写：%+v", cs)
	}

	// 部署新版本：这些文件随 v1 归档，归档里的也能按时间排、读 .gz、搜索
	f.now = f.now.Add(time.Hour)
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{"Job": []byte("v2")})); r.code != 0 {
		t.Fatalf("v2：\n%s", r.dump())
	}
	old := runs(t, f, s)[1]
	if old.Files[0].Path != "logs/app-2026-10-19.log" || old.Files[0].MTime.IsZero() {
		t.Fatalf("归档的文件顺序：%+v", old.Files)
	}
	if got := readLog(t, f, s, id1, old.ID, "logs/app-2026-10-17.log.gz"); strings.Join(got.Lines, "|") != "17 ERROR 连接超时|17 第 3 行" {
		t.Fatalf("读归档里的 .gz：%+v", got)
	}
	if got := readLog(t, f, s, id1, old.ID, "logs/app-*.log"); got.File != "logs/app-2026-10-19.log" {
		t.Fatalf("归档里的通配符：%+v", got)
	}
	if res := grep(t, f, s, id1, old.ID, "", "error", 0); res.Files != 3 || len(res.Matches) != 2 || res.Matches[0].File != "logs/app-2026-10-18.log" {
		t.Fatalf("搜归档：%+v", res)
	}
}

func TestGrepLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("x hit\n")
	}
	ms, more := grepReader(strings.NewReader(b.String()), "HIT", false, 0, 10)
	if len(ms) != 10 || !more || ms[9].Line != 10 {
		t.Fatalf("%d %v", len(ms), more)
	}
}

func TestDiagGlob(t *testing.T) {
	dir := t.TempDir()
	old, cur := filepath.Join(dir, "app-1.log"), filepath.Join(dir, "app-2.log")
	os.WriteFile(old, []byte("old"), 0o644)
	os.WriteFile(cur, []byte("cur"), 0o644)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	if got := newestGlob(filepath.Join(dir, "app-*.log")); got != cur {
		t.Fatalf("%s", got)
	}
	if got := newestGlob(filepath.Join(dir, "none-*.log")); got != "" {
		t.Fatalf("%s", got)
	}
}
