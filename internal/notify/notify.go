// Package notify 部署通知：部署、回滚结束后把结果发到企业微信、钉钉、飞书的群机器人，或自己的接口。
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/settings"
)

// Event 一次部署或回滚的结果。自定义接口收到的就是它（外加 title、markdown）。
type Event struct {
	Action     string    `json:"action"` // deploy | rollback | test | alert（服务器告警）| recovered（告警的问题恢复了）
	OK         bool      `json:"ok"`
	Project    string    `json:"project"`
	Target     string    `json:"target"`
	Host       string    `json:"host,omitempty"`
	Production bool      `json:"production,omitempty"`
	Release    string    `json:"release,omitempty"` // 部署的版本；回滚时为回到的版本（读不到时为空）
	Commit     string    `json:"commit,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Dirty      bool      `json:"dirty,omitempty"` // 有没提交的修改
	Actor      string    `json:"actor"`
	Apps       []App     `json:"apps"`
	Error      string    `json:"error,omitempty"`
	ElapsedSec int       `json:"elapsedSec,omitempty"`
	Time       time.Time `json:"time"`
	// Lines 服务器告警：发现的问题（或恢复了的问题），每条一句
	Lines []string `json:"lines,omitempty"`
}

// App 一个应用的结果。Result：OK | ROLLED_BACK | FAILED | ROLLBACK_FAILED | SKIPPED
type App struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Msg    string `json:"msg,omitempty"`
}

var resultText = map[string]string{
	"OK":              "成功",
	"ROLLED_BACK":     "失败，已回滚到原来的版本",
	"FAILED":          "失败",
	"ROLLBACK_FAILED": "失败，回滚也失败了，需要人工处理",
	"SKIPPED":         "没有部署（前面的应用失败了）",
}

// Title 一行标题：✅ shop 部署成功 · prod
func (ev Event) Title() string {
	what := map[string]string{"deploy": "部署", "rollback": "回滚", "test": "测试消息"}[ev.Action]
	if ev.Action == "test" {
		return "🔔 Ferry 部署通知" + what
	}
	where := ev.Project + " · " + ev.Target
	if ev.Project == "" {
		where = ev.Target
	}
	switch ev.Action {
	case "alert":
		return fmt.Sprintf("⚠️ %s 服务器告警（%d 项）", where, len(ev.Lines))
	case "recovered":
		return fmt.Sprintf("✅ %s 恢复正常（%d 项）", where, len(ev.Lines))
	}
	mark, res := "✅", "成功"
	if !ev.OK {
		mark, res = "❌", "失败"
	}
	prod := ""
	if ev.Production {
		prod = "（生产）"
	}
	return fmt.Sprintf("%s %s %s%s · %s%s", mark, ev.Project, what, res, ev.Target, prod)
}

// lines 正文各行（不含标题）。
func (ev Event) lines() []string {
	if ev.Action == "test" {
		return []string{"这是一条测试消息：部署、回滚结束后会在这里通知。", "发自：" + ev.Actor}
	}
	if ev.Action == "alert" || ev.Action == "recovered" {
		out := append([]string{}, ev.Lines...)
		if ev.Host != "" {
			out = append(out, "服务器："+ev.Host)
		}
		return append(out, "时间："+ev.Time.Local().Format("2006-01-02 15:04"))
	}
	var out []string
	var names []string
	allOK := true
	for _, a := range ev.Apps {
		names = append(names, a.Name)
		allOK = allOK && a.Result == "OK"
	}
	if allOK && len(names) > 0 {
		out = append(out, "应用："+strings.Join(names, "、"))
	} else {
		for _, a := range ev.Apps {
			l := a.Name + "：" + orRaw(resultText[a.Result], a.Result)
			if a.Msg != "" && a.Result != "OK" && a.Result != "SKIPPED" {
				l += "（" + cut(oneLine(a.Msg), 200) + "）"
			}
			out = append(out, l)
		}
	}
	if ev.Release != "" {
		v := "版本：" + ev.Release
		if ev.Action == "rollback" {
			v = "回到版本：" + ev.Release
		}
		out = append(out, v)
	}
	if ev.Commit != "" {
		c := "提交：" + ev.Commit
		if ev.Branch != "" {
			c += " · " + ev.Branch
		}
		if ev.Dirty {
			c += "（有没提交的修改）"
		}
		out = append(out, c)
	}
	if ev.Error != "" && !ev.OK {
		out = append(out, "原因："+cut(oneLine(ev.Error), 300))
	}
	who := "操作人：" + ev.Actor
	if ev.ElapsedSec > 0 {
		who += " · 用时 " + humanSec(ev.ElapsedSec)
	}
	out = append(out, who)
	if ev.Host != "" {
		out = append(out, "服务器："+ev.Host)
	}
	return out
}

// Markdown 标题 + 正文（企业微信、钉钉、自定义接口用）。
func (ev Event) Markdown() string {
	var b strings.Builder
	b.WriteString("### " + ev.Title() + "\n")
	for _, l := range ev.lines() {
		b.WriteString("- " + mdEscape(l) + "\n")
	}
	return b.String()
}

// Target 一个要发的地方：设置里的通知方式和它的地址、加签密钥。
type Target struct {
	Channel settings.NotifyChannel
	URL     string
	Sign    string
}

// Match 这条通知要不要发给 ch。
func Match(ch settings.NotifyChannel, ev Event) bool {
	if ch.Off {
		return false
	}
	if ev.Action == "test" {
		return true
	}
	want := "failure"
	if ev.OK {
		want = "success"
	}
	if ev.Action == "alert" || ev.Action == "recovered" {
		want = "" // 服务器告警：开了监控、选了这个通知就发
	}
	if want != "" && len(ch.On) > 0 && !slices.Contains(ch.On, want) {
		return false
	}
	if ch.ProductionOnly && !ev.Production {
		return false
	}
	if len(ch.Scope) > 0 && !slices.Contains(ch.Scope, ev.Project) && !slices.Contains(ch.Scope, ev.Project+"/"+ev.Target) {
		return false
	}
	return true
}

// EnvTargets 环境变量里的通知（CI 里用，不用设置、凭据库）：
// FERRY_NOTIFY="dingtalk:https://oapi.dingtalk.com/robot/send?access_token=…"，多个用空格分开；
// 加签密钥放 FERRY_NOTIFY_SECRET（只对第一个）。成功、失败都发。
func EnvTargets(getenv func(string) string) ([]Target, error) {
	var out []Target
	for i, f := range strings.Fields(getenv("FERRY_NOTIFY")) {
		kind, u, ok := strings.Cut(f, ":")
		if _, known := settings.NotifyKinds[kind]; !ok || !known || !strings.HasPrefix(u, "http") {
			return nil, fmt.Errorf("环境变量 FERRY_NOTIFY 的第 %d 项要写成 <方式>:<地址>，方式是 wecom、dingtalk、feishu、webhook", i+1)
		}
		t := Target{Channel: settings.NotifyChannel{ID: fmt.Sprintf("env%d", i+1), Name: "FERRY_NOTIFY（" + settings.NotifyKinds[kind] + "）", Kind: kind}, URL: u}
		if i == 0 {
			t.Sign = getenv("FERRY_NOTIFY_SECRET")
		}
		out = append(out, t)
	}
	return out, nil
}

// Client 发送用的 HTTP 客户端（测试时换掉）。
var Client = &http.Client{Timeout: 10 * time.Second}

// Send 发一条通知。错误信息里不带地址（地址里有令牌）。
func Send(ctx context.Context, t Target, ev Event) error {
	if t.URL == "" {
		return errors.New("没有保存地址")
	}
	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("地址无效（要以 https:// 开头）")
	}
	now := time.Now()
	var body any
	switch t.Channel.Kind {
	case "wecom":
		body = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": cutBytes(ev.Markdown(), 4000)}}
	case "dingtalk":
		if t.Sign != "" {
			ts := strconv.FormatInt(now.UnixMilli(), 10)
			q := u.Query()
			q.Set("timestamp", ts)
			q.Set("sign", hmacB64(t.Sign, ts+"\n"+t.Sign))
			u.RawQuery = q.Encode()
		}
		body = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": ev.Title(), "text": cutBytes(ev.Markdown(), 18000)}}
	case "feishu":
		color := "green"
		if (!ev.OK && ev.Action != "test" && ev.Action != "recovered") || ev.Action == "alert" {
			color = "red"
		}
		var md []string
		for _, l := range ev.lines() {
			md = append(md, mdEscape(l))
		}
		m := map[string]any{"msg_type": "interactive", "card": map[string]any{
			"config":   map[string]any{"wide_screen_mode": true},
			"header":   map[string]any{"title": map[string]string{"tag": "plain_text", "content": ev.Title()}, "template": color},
			"elements": []any{map[string]string{"tag": "markdown", "content": strings.Join(md, "\n")}},
		}}
		if t.Sign != "" {
			ts := strconv.FormatInt(now.Unix(), 10)
			m["timestamp"], m["sign"] = ts, hmacB64(ts+"\n"+t.Sign, "")
		}
		body = m
	default:
		body = struct {
			Event
			Title    string `json:"title"`
			Markdown string `json:"markdown"`
		}{ev, ev.Title(), ev.Markdown()}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil {
		return errors.New("地址无效")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "ferry")
	resp, err := Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // 去掉地址
		}
		return fmt.Errorf("发送失败：%v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("接口返回 %d：%s", resp.StatusCode, cut(oneLine(string(rb)), 200))
	}
	if t.Channel.Kind == "webhook" {
		return nil
	}
	// 群机器人返回 200 也可能没发出去：errcode / code 不为 0
	var r struct {
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Code    *int   `json:"code"`
		Msg     string `json:"msg"`
		SCode   *int   `json:"StatusCode"`
	}
	if json.Unmarshal(rb, &r) != nil {
		return nil
	}
	switch {
	case r.ErrCode != nil && *r.ErrCode != 0:
		return fmt.Errorf("机器人拒绝了：%s（%d）%s", r.ErrMsg, *r.ErrCode, hint(t.Channel.Kind, *r.ErrCode))
	case r.Code != nil && *r.Code != 0:
		return fmt.Errorf("机器人拒绝了：%s（%d）%s", r.Msg, *r.Code, hint(t.Channel.Kind, *r.Code))
	case r.SCode != nil && *r.SCode != 0:
		return fmt.Errorf("机器人拒绝了（%d）", *r.SCode)
	}
	return nil
}

// hint 常见错误码的说明。
func hint(kind string, code int) string {
	switch {
	case kind == "dingtalk" && code == 310000:
		return "：钉钉机器人的安全设置没通过——设了「自定义关键词」的，关键词加上「部署」或「Ferry」；设了「加签」的，把加签密钥填上"
	case kind == "feishu" && (code == 19021 || code == 19022):
		return "：飞书机器人设了签名校验，把密钥填上，并确认这台电脑的时间准确"
	case kind == "feishu" && code == 19024:
		return "：飞书机器人设了「自定义关键词」，关键词加上「部署」或「Ferry」"
	case kind == "wecom" && code == 93000:
		return "：企业微信机器人的地址无效或已删除"
	}
	return ""
}

func hmacB64(key, msg string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func humanSec(s int) string {
	if s < 60 {
		return fmt.Sprintf("%d 秒", s)
	}
	return fmt.Sprintf("%d 分 %d 秒", s/60, s%60)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// cutBytes 按字节截断（企业微信的 markdown 最多 4096 字节），不截断半个字符。
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// mdEscape 去掉会被当成 markdown 格式的字符（应用名、错误信息里的 * _ ` 等）。
func mdEscape(s string) string {
	return strings.NewReplacer("*", "＊", "`", "'", "<", "‹", ">", "›", "[", "［", "]", "］").Replace(s)
}

func orRaw(s, raw string) string {
	if s == "" {
		return raw
	}
	return s
}
