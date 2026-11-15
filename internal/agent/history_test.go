package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

var histBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))

// fakeHistory n 条按时间先后的记录（每条隔一分钟）。
func fakeHistory(n int, seed int64) []proto.HistoryEntry {
	r := rand.New(rand.NewSource(seed))
	apps := []string{"api", "web", "api2", "job<1>"}
	actions := []string{"deploy", "deploy", "deploy", "rollback", "start", "stop", "restart", "run"}
	results := []string{"OK", "OK", "OK", "FAILED", "ROLLED_BACK", "ROLLBACK_FAILED"}
	actors := []string{"张三（zs@PC-1）", "李四（ls@PC-2）", "CI（runner@build）", "Wang Wu（ww@Mac）"}
	msgs := []string{"", "", "从 20260101-1 回滚", `健康检查不通："app":"api" 返回 502`, "Deploy FAILED: timeout", "连不上 <db>"}
	out := make([]proto.HistoryEntry, n)
	for i := range out {
		out[i] = proto.HistoryEntry{
			Time: histBase.Add(time.Duration(i) * time.Minute), Action: actions[r.Intn(len(actions))], App: apps[r.Intn(len(apps))],
			Release: fmt.Sprintf("2026%04d-%d", i, r.Intn(100)), Result: results[r.Intn(len(results))], Commit: fmt.Sprintf("%040x", r.Int63()),
			Actor: actors[r.Intn(len(actors))], Msg: msgs[r.Intn(len(msgs))],
		}
	}
	return out
}

func writeHistory(t testing.TB, hs []proto.HistoryEntry) string {
	t.Helper()
	var b bytes.Buffer
	for _, h := range hs {
		j, _ := json.Marshal(h)
		b.Write(j)
		b.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// bruteHistory 全部解析后筛：对照用。
func bruteHistory(hs []proto.HistoryEntry, apps []string, q proto.HistoryQuery) proto.HistoryPage {
	m := newHistoryMatcher(apps, q)
	var all []proto.HistoryEntry
	for i := len(hs) - 1; i >= 0; i-- {
		if m.match(hs[i]) {
			all = append(all, hs[i])
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, proto.HistoryMaxLimit)
	page := proto.HistoryPage{Items: []proto.HistoryEntry{}, Total: len(all)}
	if q.Offset < len(all) {
		page.Items = append(page.Items, all[q.Offset:min(len(all), q.Offset+limit)]...)
	}
	return page
}

func sameEntries(a, b []proto.HistoryEntry) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// 和全部解析后筛的结果一样：各种条件、页码，块很小（行跨好几块）和正常大小都试。
func TestReadHistoryMatchesBrute(t *testing.T) {
	hs := fakeHistory(2000, 1)
	p := writeHistory(t, hs)
	r := rand.New(rand.NewSource(2))
	pick := func(vals ...string) []string {
		var out []string
		for _, v := range vals {
			if r.Intn(3) == 0 {
				out = append(out, v)
			}
		}
		return out
	}
	defer func(old int) { backChunk = old }(backChunk)
	// 块很小时每块都要读一次文件，慢：少试几次
	for _, c := range []struct{ chunk, runs int }{{7, 6}, {300, 60}, {4096, 150}, {128 << 10, 300}} {
		chunk := c.chunk
		backChunk = chunk
		for i := 0; i < c.runs; i++ {
			q := proto.HistoryQuery{
				Action: pick("deploy", "rollback", "stop", "run"), Result: pick("OK", "FAILED", "ROLLED_BACK"),
				Offset: r.Intn(4) * r.Intn(400), Limit: []int{0, 20, 50, 100, 1000}[r.Intn(5)],
			}
			switch r.Intn(4) {
			case 0:
				q.Actor = []string{"张三", "wang", "CI", "@PC"}[r.Intn(4)]
			case 1:
				q.Text = []string{"api", "FAILED", "回滚", `"app":"api"`, "<db>", "deploy failed", "2026001"}[r.Intn(7)]
			}
			if r.Intn(3) == 0 {
				q.Since = histBase.Add(time.Duration(r.Intn(3000)) * time.Minute)
			}
			if r.Intn(4) == 0 {
				q.Until = histBase.Add(time.Duration(r.Intn(3000)) * time.Minute)
			}
			apps := pick("api", "job<1>", "web")
			got, err := readHistory(p, apps, q, true)
			if err != nil {
				t.Fatal(err)
			}
			want := bruteHistory(hs, apps, q)
			if got.Total != want.Total || !sameEntries(got.Items, want.Items) {
				t.Fatalf("chunk %d apps %v %+v：得到 %d 条（共 %d），应为 %d 条（共 %d）", chunk, apps, q, len(got.Items), got.Total, len(want.Items), want.Total)
			}
			// 不数总条数时这一页一样
			q.Limit = min(q.Limit, proto.HistoryMaxLimit)
			got2, _ := readHistory(p, apps, q, false)
			if !sameEntries(got2.Items, want.Items) {
				t.Fatalf("不数总数时这一页不一样：%+v", q)
			}
		}
	}
}

func TestReadHistoryEdges(t *testing.T) {
	// 没有文件：空的一页
	page, err := readHistory(filepath.Join(t.TempDir(), "none.jsonl"), nil, proto.HistoryQuery{}, true)
	if err != nil || page.Total != 0 || page.Items == nil {
		t.Fatalf("%+v %v", page, err)
	}
	// 最后一行没有换行、有 \r\n、有坏行、空行
	p := filepath.Join(t.TempDir(), "h.jsonl")
	os.WriteFile(p, []byte(`{"time":"2026-01-01T00:00:00Z","action":"deploy","app":"a","result":"OK"}`+"\r\n"+
		"坏掉的行\n\n"+`{"time":"2026-01-02T00:00:00Z","action":"stop","app":"b","result":"OK"}`), 0o644)
	page, _ = readHistory(p, nil, proto.HistoryQuery{}, true)
	if page.Total != 2 || len(page.Items) != 2 || page.Items[0].App != "b" || page.Items[1].App != "a" {
		t.Fatalf("%+v", page)
	}
	// 太长的一行跳过，前后的照常
	defer func(old int) { backChunk = old }(backChunk)
	backChunk = 1 << 20
	long := `{"time":"2026-01-01T12:00:00Z","app":"x","msg":"` + strings.Repeat("长", backMaxLine/2) + `"}`
	os.WriteFile(p, []byte(`{"time":"2026-01-01T00:00:00Z","app":"a"}`+"\n"+long+"\n"+`{"time":"2026-01-02T00:00:00Z","app":"b"}`+"\n"), 0o644)
	page, _ = readHistory(p, nil, proto.HistoryQuery{}, true)
	if len(page.Items) != 2 || page.Items[0].App != "b" || page.Items[1].App != "a" {
		t.Fatalf("长行：%d 条 %+v", len(page.Items), page.Total)
	}
}

func TestHistoryFacets(t *testing.T) {
	hs := fakeHistory(100, 3)
	page, err := readHistory(writeHistory(t, hs), nil, proto.HistoryQuery{Limit: 1, Facets: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(page.Actors, "张三（zs@PC-1）") || !slices.Contains(page.Apps, "job<1>") || !slices.IsSorted(page.Apps) {
		t.Fatalf("%v %v", page.Actors, page.Apps)
	}
}

func TestHistoryPagesWithConcurrentAppend(t *testing.T) {
	hs := fakeHistory(3, 4)
	hs[1].Time = hs[0].Time.Add(-time.Hour)
	path := writeHistory(t, hs)
	query := proto.HistoryQuery{Limit: 1, Since: hs[0].Time}
	first, err := readHistory(path, nil, query, true)
	if err != nil || first.Total != 2 || !sameEntries(first.Items, hs[2:]) {
		t.Fatalf("第一页：%+v %v", first, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	newEntry := hs[0]
	newEntry.Time = hs[2].Time.Add(time.Minute)
	err = json.NewEncoder(f).Encode(newEntry)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("追加记录：%v %v", err, closeErr)
	}
	query.Snapshot = &first.Snapshot
	query.Offset = 1
	second, err := readHistory(path, nil, query, true)
	if err != nil || second.Total != first.Total || !sameEntries(second.Items, hs[:1]) {
		t.Fatalf("第二页：%+v %v", second, err)
	}
	query.Offset = int(^uint(0) >> 1)
	empty, err := readHistory(path, nil, query, true)
	if err != nil || empty.Total != 2 || len(empty.Items) != 0 {
		t.Fatalf("超出范围：%+v %v", empty, err)
	}
}

func TestReadHistoryCountsValidMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	lines := []string{
		`{"time":"2026-01-01T00:00:00Z", "app": "a", "msg": "旧记录"}`,
		`{"time":"2026-01-01T00:00:00Z","app":"a","msg":"旧记录",`,
		`{"time":"2026-01-01T00:00:00Z","app":"a","msg":"旧记录"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := readHistory(path, []string{"a"}, proto.HistoryQuery{Text: "旧记录", Limit: 1}, true)
	if err != nil || page.Total != 2 || len(page.Items) != 1 {
		t.Fatalf("统计：%+v %v", page, err)
	}
}

func TestContainsFold(t *testing.T) {
	for _, c := range []struct {
		s, sub string
		want   bool
	}{{"Deploy FAILED", "failed", true}, {"张三（ZS@PC）", "zs@pc", true}, {"abc", "abcd", false}, {"x", "", true}, {"ABC", "abd", false}} {
		if got := containsFold([]byte(c.s), lowerASCII([]byte(c.sub))); got != c.want {
			t.Errorf("%q %q = %v", c.s, c.sub, got)
		}
	}
	// 随机的和直接转小写再找的结果一样
	r := rand.New(rand.NewSource(9))
	alpha := []string{"a", "B", "c", "A", "b", "张", "1", "@", " "}
	gen := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(alpha[r.Intn(len(alpha))])
		}
		return b.String()
	}
	for range 20000 {
		s, sub := gen(r.Intn(30)), gen(1+r.Intn(4))
		want := bytes.Contains(lowerASCII([]byte(s)), lowerASCII([]byte(sub)))
		if got := containsFold([]byte(s), lowerASCII([]byte(sub))); got != want {
			t.Fatalf("%q %q = %v", s, sub, got)
		}
	}
}

// go test -run x -bench History ./internal/agent/
func BenchmarkReadHistory(b *testing.B) {
	hs := fakeHistory(300000, 5)
	p := writeHistory(b, hs)
	st, _ := os.Stat(p)
	b.Logf("%d 条，%.1f MB", len(hs), float64(st.Size())/(1<<20))
	cases := []struct {
		name string
		apps []string
		q    proto.HistoryQuery
	}{
		{"第一页带总数", nil, proto.HistoryQuery{Limit: 50, Facets: true}},
		{"第100页", nil, proto.HistoryQuery{Limit: 50, Offset: 5000}},
		{"应用和结果", []string{"api"}, proto.HistoryQuery{Limit: 50, Result: []string{"FAILED"}}},
		{"操作人", nil, proto.HistoryQuery{Limit: 50, Actor: "张三"}},
		{"搜索", nil, proto.HistoryQuery{Limit: 50, Text: "timeout"}},
		{"最近7天", nil, proto.HistoryQuery{Limit: 50, Since: hs[len(hs)-1].Time.Add(-7 * 24 * time.Hour)}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := readHistory(p, c.apps, c.q, true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
