package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/settings"
)

func sample(ok bool) Event {
	ev := Event{Action: "deploy", OK: ok, Project: "shop", Target: "prod", Production: true, Release: "20261025-100000-abcd",
		Commit: "abc1234", Branch: "main", Actor: "zhangsan@PC（经 Claude Code · MCP）", ElapsedSec: 83, Time: time.Now(),
		Apps: []App{{Name: "api", Result: "OK"}, {Name: "web", Result: "OK"}}}
	if !ok {
		ev.Apps = []App{{Name: "api", Result: "ROLLED_BACK", Msg: "api 启动后退出"}, {Name: "web", Result: "SKIPPED"}}
		ev.Error = "prod：部署失败：api 部署失败"
	}
	return ev
}

type got struct {
	path, query string
	body        map[string]any
}

func server(t *testing.T, reply string) (*httptest.Server, *got) {
	g := &got{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.path, g.query = r.URL.Path, r.URL.RawQuery
		json.Unmarshal(b, &g.body)
		io.WriteString(w, reply)
	}))
	t.Cleanup(s.Close)
	return s, g
}

func TestMarkdown(t *testing.T) {
	md := sample(true).Markdown()
	for _, want := range []string{"### ✅ shop 部署成功 · prod（生产）", "- 应用：api、web", "- 版本：20261025-100000-abcd", "- 提交：abc1234 · main", "用时 1 分 23 秒"} {
		if !strings.Contains(md, want) {
			t.Errorf("缺 %q：\n%s", want, md)
		}
	}
	md = sample(false).Markdown()
	for _, want := range []string{"❌ shop 部署失败", "- api：失败，已回滚到原来的版本（api 启动后退出）", "- web：没有部署（前面的应用失败了）", "- 原因："} {
		if !strings.Contains(md, want) {
			t.Errorf("缺 %q：\n%s", want, md)
		}
	}
}

func TestSendKinds(t *testing.T) {
	ctx := context.Background()
	s, g := server(t, `{"errcode":0,"errmsg":"ok"}`)
	if err := Send(ctx, Target{Channel: settings.NotifyChannel{Kind: "wecom"}, URL: s.URL + "/send?key=K"}, sample(true)); err != nil {
		t.Fatal(err)
	}
	if g.body["msgtype"] != "markdown" || !strings.Contains(g.body["markdown"].(map[string]any)["content"].(string), "部署成功") {
		t.Fatalf("企业微信：%v", g.body)
	}

	if err := Send(ctx, Target{Channel: settings.NotifyChannel{Kind: "dingtalk"}, URL: s.URL + "/robot/send?access_token=T", Sign: "SEC"}, sample(false)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.query, "access_token=T") || !strings.Contains(g.query, "timestamp=") || !strings.Contains(g.query, "sign=") {
		t.Fatalf("钉钉加签：%s", g.query)
	}
	if md := g.body["markdown"].(map[string]any); !strings.Contains(md["title"].(string), "部署失败") {
		t.Fatalf("钉钉：%v", g.body)
	}

	fs, fg := server(t, `{"code":0,"msg":"success"}`)
	if err := Send(ctx, Target{Channel: settings.NotifyChannel{Kind: "feishu"}, URL: fs.URL + "/hook/X", Sign: "SEC"}, sample(false)); err != nil {
		t.Fatal(err)
	}
	card := fg.body["card"].(map[string]any)
	if fg.body["msg_type"] != "interactive" || fg.body["sign"] == nil || card["header"].(map[string]any)["template"] != "red" {
		t.Fatalf("飞书：%v", fg.body)
	}

	ws, wg := server(t, `whatever`)
	if err := Send(ctx, Target{Channel: settings.NotifyChannel{Kind: "webhook"}, URL: ws.URL + "/ferry"}, sample(true)); err != nil {
		t.Fatal(err)
	}
	if wg.body["project"] != "shop" || wg.body["ok"] != true || !strings.Contains(wg.body["markdown"].(string), "部署成功") {
		t.Fatalf("自定义接口：%v", wg.body)
	}
}

func TestSendErrorsHideURL(t *testing.T) {
	ctx := context.Background()
	s, _ := server(t, `{"errcode":310000,"errmsg":"keywords not in content"}`)
	err := Send(ctx, Target{Channel: settings.NotifyChannel{Kind: "dingtalk"}, URL: s.URL + "/robot/send?access_token=SECRET"}, sample(true))
	if err == nil || !strings.Contains(err.Error(), "关键词") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
	err = Send(ctx, Target{Channel: settings.NotifyChannel{Kind: "wecom"}, URL: "http://127.0.0.1:1/send?key=SECRET"}, sample(true))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("连不上时不能带出地址：%v", err)
	}
	if err := Send(ctx, Target{Channel: settings.NotifyChannel{Kind: "wecom"}, URL: "ftp://x"}, sample(true)); err == nil {
		t.Fatal("地址无效应报错")
	}
}

func TestMatch(t *testing.T) {
	ok, bad := sample(true), sample(false)
	ok.Production = false
	cases := []struct {
		ch   settings.NotifyChannel
		ev   Event
		want bool
	}{
		{settings.NotifyChannel{}, ok, true},
		{settings.NotifyChannel{Off: true}, ok, false},
		{settings.NotifyChannel{On: []string{"failure"}}, ok, false},
		{settings.NotifyChannel{On: []string{"failure"}}, bad, true},
		{settings.NotifyChannel{ProductionOnly: true}, ok, false},
		{settings.NotifyChannel{ProductionOnly: true}, bad, true},
		{settings.NotifyChannel{Scope: []string{"other"}}, ok, false},
		{settings.NotifyChannel{Scope: []string{"shop/prod"}}, ok, true},
		{settings.NotifyChannel{Scope: []string{"shop"}}, ok, true},
	}
	for i, c := range cases {
		if Match(c.ch, c.ev) != c.want {
			t.Errorf("%d：%+v", i, c.ch)
		}
	}
}

func TestEnvTargets(t *testing.T) {
	env := map[string]string{"FERRY_NOTIFY": "dingtalk:https://x/robot?access_token=1 webhook:https://y/hook", "FERRY_NOTIFY_SECRET": "S"}
	ts, err := EnvTargets(func(k string) string { return env[k] })
	if err != nil || len(ts) != 2 || ts[0].Channel.Kind != "dingtalk" || ts[0].Sign != "S" || ts[1].Sign != "" || ts[1].URL != "https://y/hook" {
		t.Fatalf("%v %+v", err, ts)
	}
	env["FERRY_NOTIFY"] = "slack:https://x"
	if _, err := EnvTargets(func(k string) string { return env[k] }); err == nil {
		t.Fatal("不认识的方式应报错")
	}
}

func TestCutBytes(t *testing.T) {
	s := strings.Repeat("部", 10) // 30 字节
	if c := cutBytes(s, 7); c != "部部…" {
		t.Fatalf("%q", c)
	}
}
