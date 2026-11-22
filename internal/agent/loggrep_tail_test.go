package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// 大文件默认只搜最近一段：行号仍是文件里的真实行号；只在前面出现的内容默认搜不到，要搜全部得明说。
func TestLogGrepTailWindow(t *testing.T) {
	f := newFake(t)
	s := logApp()
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"Job": []byte("v1")})); r.code != 0 {
		t.Fatalf("部署：\n%s", r.dump())
	}
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "第 %d 行\n", i)
	}
	b.WriteString("OLD-MARK 早期的错误\n") // 第 201 行
	for i := 202; i <= 400; i++ {
		fmt.Fprintf(&b, "第 %d 行\n", i)
	}
	b.WriteString("NEW-MARK 最近的错误\n") // 第 401 行
	writeLog(t, f, "Job", "logs/app.log", b.String())

	saved := maxGrepBytes
	maxGrepBytes = 300 // 只够最后二十来行
	defer func() { maxGrepBytes = saved }()

	res := grep(t, f, s, "", "", "logs/app.log", "NEW-MARK", 0)
	if len(res.Matches) != 1 || res.Matches[0].Line != 401 {
		t.Fatalf("最近一处的行号应当还是 401：%+v", res)
	}
	if res.TailBytes != 300 {
		t.Fatalf("应当说明只搜了最近一段：%+v", res)
	}

	if res := grep(t, f, s, "", "", "logs/app.log", "OLD-MARK", 0); len(res.Matches) != 0 || res.TailBytes != 300 {
		t.Fatalf("前面那段默认不搜：%+v", res)
	}

	// 搜全部：前面那段也能找到，行号正确，不再标「只搜了最近一段」
	r := exec1(t, f, "loggrep", &proto.Request{Apps: []proto.AppSpec{s}, LogFile: "logs/app.log", Grep: "OLD-MARK", GrepAll: true})
	var all proto.LogSearch
	if r.code != 0 || json.Unmarshal(r.result.Data, &all) != nil {
		t.Fatalf("搜全部：\n%s", r.dump())
	}
	if len(all.Matches) != 1 || all.Matches[0].Line != 201 || all.TailBytes != 0 {
		t.Fatalf("搜全部：%+v", all)
	}
}

// 文件没超过上限时照常整个搜，不标「只搜了最近一段」。
func TestLogGrepSmallFileWhole(t *testing.T) {
	f := newFake(t)
	s := logApp()
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"Job": []byte("v1")})); r.code != 0 {
		t.Fatalf("部署：\n%s", r.dump())
	}
	writeLog(t, f, "Job", "logs/app.log", "第一行\n第二行 ERROR\n第三行\n")
	saved := maxGrepBytes
	maxGrepBytes = 300
	defer func() { maxGrepBytes = saved }()
	res := grep(t, f, s, "", "", "logs/app.log", "ERROR", 0)
	if len(res.Matches) != 1 || res.Matches[0].Line != 2 || res.TailBytes != 0 {
		t.Fatalf("%+v", res)
	}
}
