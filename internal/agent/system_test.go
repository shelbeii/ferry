package agent

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// 有 since 时不给 -n（老版本 systemd 会从 since 往后取），读完留最后 n 条。
func TestJournalArgsLastLines(t *testing.T) {
	since := time.Date(2026, 10, 21, 17, 59, 32, 0, time.Local)
	args := strings.Join(journalArgs("app.service", 60, since), " ")
	if strings.Contains(args, " -n ") || !strings.Contains(args, "--since 2026-10-21 17:59:32") {
		t.Errorf("有 since 时：%s", args)
	}
	if args := strings.Join(journalArgs("app.service", 60, time.Time{}), " "); !strings.Contains(args, "-n 60") || strings.Contains(args, "--since") {
		t.Errorf("没有 since 时：%s", args)
	}
	if got := keepLast([]string{"a", "b", "c", "d"}, 2); !slices.Equal(got, []string{"c", "d"}) {
		t.Errorf("最后 2 行：%q", got)
	}
	if got := keepLast([]string{"a"}, 2); !slices.Equal(got, []string{"a"}) {
		t.Errorf("不够 2 行：%q", got)
	}
}
