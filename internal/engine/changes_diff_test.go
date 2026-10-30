package engine

import (
	"slices"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestUnitDiff(t *testing.T) {
	old := "# managed by ferry\n[Service]\nUser=shop\nEnvironment=\"A=1\"\nExecStart=/opt/shop/apps/api/running/api\n"
	now := "# managed by ferry\n[Service]\nUser=shop\nEnvironment=\"A=2\"\nExecStart=/opt/shop/apps/api/running/api\nLimitNOFILE=65536\n"
	got := unitDiff(old, now, nil)
	want := []string{`- Environment="A=1"`, `+ Environment="A=2"`, "+ LimitNOFILE=65536"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if d := unitDiff(old, old, nil); len(d) != 0 {
		t.Fatalf("没变：%q", d)
	}
	// 不停机发布：线上在槽位 b，这次在槽位 a，路径和端口不一样不算变化
	zd := &proto.ZeroDowntime{Slots: [2]proto.Slot{{Port: 9201}, {Port: 19201}}}
	b := "ExecStart=/opt/x/apps/api/slots/b/api --server.port=19201\nDescription=x（槽位 b）\n"
	a := "ExecStart=/opt/x/apps/api/slots/a/api --server.port=9201\nDescription=x（槽位 a）\n"
	if d := unitDiff(b, a, zd); len(d) != 0 {
		t.Fatalf("换槽位不算变化：%q", d)
	}
}

func TestDiffFiles(t *testing.T) {
	added, changed, removed := diffFiles(
		map[string]string{"app.yml": "1", "conf/a.conf": "2", "old.sh": "3"},
		map[string]string{"app.yml": "1", "conf/a.conf": "9", "new.yml": "4"},
	)
	if !slices.Equal(added, []string{"new.yml"}) || !slices.Equal(changed, []string{"conf/a.conf"}) || !slices.Equal(removed, []string{"old.sh"}) {
		t.Fatal(added, changed, removed)
	}
}
