package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 「服务器信息」第二次读：只读一次 CPU 计数，和上次的比，Window 是这段时间的秒数。
func TestServerInfoCPUFromPrevious(t *testing.T) {
	lines := strings.Split(serverInfoFixture, "\n")
	first, sample, err := parseServerInfoWith(lines, nil, 0)
	if err != nil || first.CPU.Window != 1 || len(sample) != 3 || !strings.HasPrefix(sample[0], "cpu 120 ") {
		t.Fatalf("第一次：%+v %v %v", first.CPU, sample, err)
	}
	// 第二次：服务器只输出 cpu1（这次的计数）
	var quick []string
	skip := false
	for _, l := range lines {
		switch {
		case l == "@@cpu1":
			quick = append(quick, l, "cpu 220 0 210 1460 105 21 21 63 45 0", "cpu0 110 0 105 730 52 10 10 31 22 0", "cpu1 110 0 105 730 53 11 11 32 23 0")
			skip = true
			continue
		case l == "@@disks":
			skip = false
		}
		if !skip {
			quick = append(quick, l)
		}
	}
	r, next, err := parseServerInfoWith(quick, sample, 61*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 总量（前 8 项）1100→2100，空闲（idle+iowait）815→1565：(1000-750)/1000 = 25%
	if r.CPU.Usage == nil || *r.CPU.Usage != 25 || r.CPU.Window != 61 || r.CPU.Cores != 2 {
		t.Fatalf("和上次比：%+v", r.CPU)
	}
	if len(next) != 3 || !strings.HasPrefix(next[0], "cpu 220 ") {
		t.Fatalf("下次用这次的计数：%v", next)
	}
}

func TestAgentVersionOK(t *testing.T) {
	out := fmt.Sprintf(`{"version":"x","proto":%d,"arch":"amd64","sha256":"abc"}`+"\n\n", proto.Version)
	if !agentVersionOK(out, "abc") || agentVersionOK(out, "abd") || agentVersionOK("", "abc") {
		t.Fatal("校验和、协议版本都要对上")
	}
	if agentVersionOK(fmt.Sprintf(`{"proto":%d,"sha256":"abc"}`, proto.Version-1), "abc") {
		t.Fatal("协议版本不对的不能用")
	}
}

// 经 sudo 运行慢（主机名解析不了）时提示；root 登录、量不到时间时不提示。
func TestWarnSlowSudo(t *testing.T) {
	rep := &recRep{}
	e := &Engine{Rep: rep}
	sudo := &conn{sudo: "sudo -n ", label: "deploy@10.0.0.1:22"}
	e.warnSlowSudo(sudo, "1000000000 3500000000")
	if len(rep.warns) != 1 || !strings.Contains(rep.warns[0], "2.5 秒") || !strings.Contains(rep.warns[0], "/etc/hosts") {
		t.Fatalf("慢的要提示：%v", rep.warns)
	}
	e.warnSlowSudo(sudo, "1000000000 1200000000")
	e.warnSlowSudo(&conn{label: "root@x"}, "1000000000 9000000000")
	e.warnSlowSudo(sudo, "%s%N %s%N")
	if len(rep.warns) != 1 {
		t.Fatalf("快的、root、量不到的不提示：%v", rep.warns)
	}
}
