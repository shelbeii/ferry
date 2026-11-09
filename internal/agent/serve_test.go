package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// 常驻的 agent：几个请求同时做，输出按请求号分开，每个都有结果和结束标记；改动服务器的命令不接。
func TestServe(t *testing.T) {
	f := newFake(t)
	var in bytes.Buffer
	req := func(id, cmd string, ver int) {
		b, _ := json.Marshal(proto.ServeRequest{ID: id, Cmd: cmd, Req: &proto.Request{Proto: ver, Root: f.root}})
		in.Write(append(b, '\n'))
	}
	for i := 1; i <= 5; i++ {
		req(fmt.Sprint(i), "history", proto.Version)
	}
	req("old", "history", proto.Version-1)
	req("dep", "deploy", proto.Version)
	in.WriteString("不是 JSON\n")
	b, _ := json.Marshal(proto.ServeRequest{ID: "last", Cmd: "history", Req: &proto.Request{Proto: proto.Version, Root: f.root}})
	in.Write(b) // 最后一行没有换行

	var out bytes.Buffer
	if code := Serve(f, &in, &out); code != 0 {
		t.Fatalf("退出码 %d", code)
	}
	results := map[string]proto.Result{}
	ends := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		id, rest, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("没有请求号：%q", line)
		}
		if code, end := strings.CutPrefix(rest, proto.ServeEnd); end {
			ends[id] = code
			continue
		}
		var r proto.Result
		if json.Unmarshal([]byte(rest), &r) == nil && r.T == "result" {
			results[id] = r
		}
	}
	for _, id := range []string{"1", "2", "3", "4", "5", "last"} {
		if results[id].RC != proto.CodeOK || ends[id] != "0" {
			t.Errorf("请求 %s：%+v 结束 %q", id, results[id], ends[id])
		}
	}
	if results["old"].RC != proto.CodeBadInput || ends["old"] != fmt.Sprint(proto.CodeBadInput) {
		t.Errorf("协议版本不对：%+v %q", results["old"], ends["old"])
	}
	if r := results["dep"]; r.RC != proto.CodeBadInput || !strings.Contains(r.Msg, "不处理 deploy") {
		t.Errorf("改动服务器的命令不该交给常驻的 agent：%+v", r)
	}
	if len(ends) != 8 {
		t.Errorf("结束标记：%v", ends)
	}
}
