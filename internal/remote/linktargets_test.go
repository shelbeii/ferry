package remote

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseLinkTargets(t *testing.T) {
	got := parseLinkTargets([]string{
		"d\t4096\tlibdir",
		"f\t123\tlibfile",
		"l\t7\tbroken",
		"f\t9\t名\t带制表符",
		"坏行",
		"f\t不是数字\tx",
	})
	if len(got) != 4 {
		t.Fatalf("解析出 %d 条：%v", len(got), got)
	}
	if v := got["libdir"]; !v.dir || v.size != 4096 {
		t.Errorf("指向目录：%+v", v)
	}
	if v := got["libfile"]; v.dir || v.size != 123 {
		t.Errorf("指向文件：%+v", v)
	}
	if v := got["broken"]; v.dir || v.size != 7 {
		t.Errorf("断链按链接本身：%+v", v)
	}
	if v := got["名\t带制表符"]; v.dir || v.size != 9 {
		t.Errorf("名字里有制表符：%+v", v)
	}
}

// 几百个链接只发一条命令；没问到的才留给逐个 Stat。
func TestLinkTargetsOneCommand(t *testing.T) {
	calls := 0
	c := &FileClient{run: func(_ context.Context, cmd string, _ []byte, onLine func(string)) (RunResult, error) {
		calls++
		if !strings.Contains(cmd, "find -L '/usr/lib64'") || !strings.Contains(cmd, "-maxdepth 1") {
			t.Errorf("命令不对：%s", cmd)
		}
		for _, l := range []string{"d\t4096\tlibdir", "f\t123\tlibfile"} {
			onLine(l)
		}
		return RunResult{Code: 1}, nil // 有断链时 find 返回非零，已列出的照样用
	}}
	out := []FileEntry{{Name: "libdir", Link: true}, {Name: "libfile", Link: true}, {Name: "没问到", Link: true}}
	rest := c.linkTargets("/usr/lib64", out, []int{0, 1, 2})
	if calls != 1 {
		t.Fatalf("发了 %d 条命令", calls)
	}
	if !out[0].Dir || out[0].Size != 4096 || out[1].Dir || out[1].Size != 123 {
		t.Fatalf("没套用命令的结果：%+v", out)
	}
	if len(rest) != 1 || rest[0] != 2 {
		t.Fatalf("没问到的应当留给逐个 Stat：%v", rest)
	}
}

// 不能执行命令（提权的文件会话）或命令失败：全部退回逐个 Stat，结果和以前一样。
func TestLinkTargetsFallback(t *testing.T) {
	out := []FileEntry{{Name: "a", Link: true}, {Name: "b", Link: true}}
	links := []int{0, 1}
	noRun := &FileClient{}
	if rest := noRun.linkTargets("/x", out, links); len(rest) != 2 {
		t.Fatalf("没有命令通道时应全部退回：%v", rest)
	}
	broken := &FileClient{run: func(context.Context, string, []byte, func(string)) (RunResult, error) {
		return RunResult{}, errors.New("通道断了")
	}}
	if rest := broken.linkTargets("/x", out, links); len(rest) != 2 {
		t.Fatalf("命令失败时应全部退回：%v", rest)
	}
	if out[0].Dir || out[0].Size != 0 {
		t.Fatalf("退回时不该改条目：%+v", out[0])
	}
}

// 走真正的 SSH 通道：一条命令的输出能逐行收回来。
func TestLinkTargetsOverSSH(t *testing.T) {
	d := newFakeSSHD(t, 4)
	d.out = "d\t4096\tlibdir\nf\t123\tlibfile\n"
	client := d.dial(t)
	c := &FileClient{run: client.RunContext}
	out := []FileEntry{{Name: "libdir", Link: true}, {Name: "libfile", Link: true}}
	if rest := c.linkTargets("/usr/lib64", out, []int{0, 1}); len(rest) != 0 {
		t.Fatalf("还有没问到的：%v", rest)
	}
	if !out[0].Dir || out[0].Size != 4096 || out[1].Dir || out[1].Size != 123 {
		t.Fatalf("%+v", out)
	}
}
