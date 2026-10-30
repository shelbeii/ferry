package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/scaffold"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/ui"
)

// aiConfig 用哪个模型：modelID 为空时用默认模型；effort 为空时用模型设置的思考强度（none = 不设置）。
// API Key 从凭据库取（环境变量 FERRY_AI_KEY 优先）。设置里「测试」用 FERRY_AI_PROVIDER 等环境变量传表单里还没保存的值。
func aiConfig(st settings.Settings, cio *cliIO, modelID, effort string) (ai.Config, error) {
	var c ai.Config
	keyID := modelID
	if v := cio.getenv("FERRY_AI_PROVIDER"); v != "" {
		c = ai.Config{Provider: v, BaseURL: strings.TrimSpace(cio.getenv("FERRY_AI_BASE_URL")),
			Model: strings.TrimSpace(cio.getenv("FERRY_AI_MODEL")), Effort: cio.getenv("FERRY_AI_EFFORT"),
			Thinking: cio.getenv("FERRY_AI_THINKING")}
		if keyID == "" {
			keyID = cio.getenv("FERRY_AI_MODEL_ID")
		}
	} else {
		m, ok := st.AI.FindModel(modelID)
		if !ok {
			if len(st.AI.Models) == 0 {
				return c, errors.New("还没有配置 AI 模型：在「设置 → AI」里添加")
			}
			return c, fmt.Errorf("没有这个模型：%s（在「设置 → AI」里看看）", modelID)
		}
		c = ai.Config{Provider: m.Provider, BaseURL: m.BaseURL, Model: m.Model, Effort: m.Effort, Thinking: m.Thinking}
		c.Images = ai.SupportsImages(m.Images, m.Provider, m.Model)
		keyID = m.ID
	}
	switch effort {
	case "":
	case "none":
		c.Effort = ""
	case "off", "on", "low", "medium", "high", "xhigh", "max":
		c.Effort = effort
	default:
		return c, fmt.Errorf("思考强度只能是 none、off、on、low、medium、high、xhigh、max：%s", effort)
	}
	c.APIKey = cio.getenv("FERRY_AI_KEY")
	if c.APIKey == "" && keyID != "" {
		k, err := aiModelKey(keyID)
		if err != nil {
			return c, err
		}
		c.APIKey = k
	}
	return c, nil
}

// aiModelKey 模型的 API Key。旧版本只有一个模型，它的 Key 在旧账户里：第一次用到时搬到新账户。
func aiModelKey(id string) (string, error) {
	k, ok, err := secret.Get(secret.AIModelKeyAccount(id))
	if err != nil || ok || id != settings.LegacyModelID {
		return k, err
	}
	old, ok, err := secret.Get(secret.AIKeyAccount)
	if err != nil || !ok {
		return "", err
	}
	if secret.Set(secret.AIModelKeyAccount(id), old) == nil {
		secret.Delete(secret.AIKeyAccount)
	}
	return old, nil
}

// aiKeyCmd ferry ai-key set|delete|status --model <ID>：各模型的 API Key 存在系统凭据库（值从 FERRY_SECRET_VALUE 读）。
// status 不带 --model 时列出每个模型有没有保存。
func aiKeyCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	action := "status"
	if len(c.pos) > 0 {
		action = c.pos[0]
	}
	id := c.flags["model"]
	if id == "" && action != "status" {
		return fail(errors.New("用法：ferry ai-key set|delete|status --model <模型 ID>"))
	}
	var res map[string]any
	switch action {
	case "set":
		v := strings.TrimSpace(cio.getenv("FERRY_SECRET_VALUE"))
		if v == "" && con != nil {
			v, _ = con.Secret("大模型的 API Key：")
		}
		if v == "" {
			return fail(errors.New("没有输入：在终端中运行，或通过环境变量 FERRY_SECRET_VALUE 提供"))
		}
		if err := secret.Set(secret.AIModelKeyAccount(id), v); err != nil {
			return fail(err)
		}
		if id == settings.LegacyModelID {
			secret.Delete(secret.AIKeyAccount)
		}
		res = map[string]any{"stored": true}
	case "delete":
		if err := secret.Delete(secret.AIModelKeyAccount(id)); err != nil {
			return fail(err)
		}
		if id == settings.LegacyModelID {
			secret.Delete(secret.AIKeyAccount)
		}
		res = map[string]any{"stored": false}
	case "status":
		if id != "" {
			k, err := aiModelKey(id)
			if err != nil {
				return fail(err)
			}
			res = map[string]any{"stored": k != ""}
			break
		}
		models := map[string]bool{}
		for _, m := range st.AI.Models {
			k, err := aiModelKey(m.ID)
			if err != nil {
				return fail(err)
			}
			models[m.ID] = k != ""
		}
		res = map[string]any{"models": models}
		if js == nil {
			for _, m := range st.AI.Models {
				fmt.Fprintf(cio.stdout, "%-16s %s：%s\n", m.ID, m.Name, map[bool]string{true: "已保存", false: "未保存"}[models[m.ID]])
			}
			return 0
		}
	default:
		return fail(errors.New("用法：ferry ai-key set|delete|status --model <模型 ID>"))
	}
	if js != nil {
		js.Result(true, res, nil)
	} else {
		fmt.Fprintf(cio.stdout, "API Key：%s\n", map[bool]string{true: "已保存", false: "未保存"}[res["stored"] == true])
	}
	return 0
}

// aiTestCmd ferry ai-test [--model <ID>] [--effort …]：用这个模型发一句话，确认配置能用。
func aiTestCmd(c *cli, cio *cliIO, js *ui.JSON, st settings.Settings, fail func(error) int) int {
	cfg, err := aiConfig(st, cio, c.flags["model"], c.flags["effort"])
	if err != nil {
		return fail(err)
	}
	p, err := ai.NewProvider(cfg)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(cio.ctx, 2*time.Minute)
	defer cancel()
	start := time.Now()
	reply, err := p.Step(ctx, "你在测试连接。只回复「连接正常」四个字。", []ai.Message{{Role: "user", Text: "你好"}}, nil)
	if err != nil {
		return fail(err)
	}
	res := map[string]any{"reply": strings.TrimSpace(reply.Text), "seconds": time.Since(start).Seconds(), "model": cfg.Model}
	if cfg.Model == "" {
		res["model"] = ai.DefaultModel(cfg.Provider)
	}
	if js != nil {
		js.Result(true, res, nil)
	} else {
		fmt.Fprintf(cio.stdout, "%s（%s，%.1f 秒）\n", res["reply"], res["model"], res["seconds"])
	}
	return 0
}

// readImage 读一张图片文件：按内容认格式（PNG、JPEG、GIF、WebP）。
func readImage(file string) (ai.Image, error) {
	fi, err := os.Stat(file)
	if err != nil {
		return ai.Image{}, err
	}
	if fi.IsDir() {
		return ai.Image{}, fmt.Errorf("不是图片：%s", file)
	}
	// 桌面版会把大图缩小后再发，这里只拦住明显不是截图、照片的大文件
	if fi.Size() > 50<<20 {
		return ai.Image{}, fmt.Errorf("图片太大：%s（%.0f MB）", filepath.Base(file), float64(fi.Size())/(1<<20))
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return ai.Image{}, err
	}
	media := http.DetectContentType(b)
	switch media {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return ai.Image{}, fmt.Errorf("不是 PNG、JPEG、GIF、WebP 图片：%s", filepath.Base(file))
	}
	return ai.Image{Media: media, Data: base64.StdEncoding.EncodeToString(b)}, nil
}

// aiImageCmd ferry ai-image <图片…>：读图片文件，输出 [{"name","media","data"}]（桌面版把拖进来的图片显示出来、缩小后再发）。
func aiImageCmd(c *cli, cio *cliIO, js *ui.JSON, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(errors.New("用法：ferry ai-image <图片…>"))
	}
	type named struct {
		Name string `json:"name"`
		ai.Image
	}
	var out []named
	for _, f := range c.pos {
		im, err := readImage(f)
		if err != nil {
			return fail(err)
		}
		out = append(out, named{filepath.Base(f), im})
	}
	if js != nil {
		js.Result(true, map[string]any{"images": out}, nil)
		return 0
	}
	for _, im := range out {
		fmt.Fprintf(cio.stdout, "%s  %s  %d 字节\n", im.Name, im.Media, len(im.Data)*3/4)
	}
	return 0
}

// aiCmd ferry ai <目标> <要问的话…> [--conv <对话>] [--mode readonly|confirm] [--decide approve|always|reject] [--image <图片;…>]：
// 和服务器助手对话一轮。桌面版用环境变量 FERRY_AI_MESSAGE 传话，FERRY_AI_IMAGES 传图片（[{"media","data"}] 的 JSON）。确认模式下遇到改动服务器的操作会停下（结果的 pending），用 --decide 接着执行。
func aiCmd(e *engine.Engine, c *cli, cio *cliIO, js *ui.JSON, st settings.Settings, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(errors.New("用法：ferry ai <目标> <要问的话> [--conv <对话 ID>] [--mode confirm]"))
	}
	decide := c.flags["decide"]
	msg := strings.TrimSpace(cio.getenv("FERRY_AI_MESSAGE"))
	if msg == "" {
		msg = strings.TrimSpace(strings.Join(c.pos[1:], " "))
	}
	var images []ai.Image
	if v := cio.getenv("FERRY_AI_IMAGES"); v != "" && decide == "" {
		if err := json.Unmarshal([]byte(v), &images); err != nil {
			return fail(fmt.Errorf("图片的内容无效：%v", err))
		}
	}
	if v := c.flags["image"]; v != "" && decide == "" {
		for _, f := range filepath.SplitList(v) {
			im, err := readImage(f)
			if err != nil {
				return fail(err)
			}
			images = append(images, im)
		}
	}
	if msg == "" && decide == "" && len(images) == 0 {
		return fail(errors.New("要问什么？"))
	}
	if decide != "" && c.flags["conv"] == "" {
		return fail(errors.New("--decide 要和 --conv 一起用"))
	}
	cfg, err := aiConfig(st, cio, c.flags["model"], c.flags["effort"])
	if err != nil {
		return fail(err)
	}
	emit := aiEmit(js, cio)
	rewind := 0
	if v := c.flags["rewind"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fail(errors.New("--rewind 要写第几句话（从 1 数）"))
		}
		rewind = n
	}
	r, err := e.AIChat(c.pos[0], engine.AIOptions{Conv: c.flags["conv"], Message: msg, Images: images, Decide: decide, Mode: c.flags["mode"],
		Rewind: rewind, Model: cfg, Emit: emit})
	if err != nil {
		if js != nil {
			js.Result(false, r, err)
			return 1
		}
		return fail(err)
	}
	if js != nil {
		js.Result(true, r, nil)
		return 0
	}
	mode := ""
	if c.flags["mode"] != "" {
		mode = " --mode " + c.flags["mode"]
	}
	if r.Pending {
		fmt.Fprintf(cio.stdout, "\n（执行：ferry ai %s --conv %s%s --decide approve；同类不再询问用 always，不执行用 reject）\n", c.pos[0], r.Conv, mode)
	} else {
		fmt.Fprintf(cio.stdout, "\n（接着问：ferry ai %s <问题> --conv %s%s）\n", c.pos[0], r.Conv, mode)
	}
	return 0
}

// aiDetectCmd ferry detect <目录> --ai [--model <ID>] [--effort …]：先按规则识别，再让 AI 看项目里的文件检查、修正、补充。
// 用户补充的说明从环境变量 FERRY_AI_MESSAGE 读；识别过程和 ferry ai 一样按 ai 事件输出。
func aiDetectCmd(c *cli, cio *cliIO, js *ui.JSON, st settings.Settings, fail func(error) int) int {
	cfg, err := aiConfig(st, cio, c.flags["model"], c.flags["effort"])
	if err != nil {
		return fail(err)
	}
	p, err := ai.NewProvider(cfg)
	if err != nil {
		return fail(err)
	}
	r, err := scaffold.AIDetect(cio.ctx, c.pos[0], scaffold.AIOptions{Provider: p, Emit: aiEmit(js, cio), Hint: cio.getenv("FERRY_AI_MESSAGE")})
	if err != nil {
		if js != nil {
			js.Result(false, r, err)
			return 1
		}
		return fail(err)
	}
	if js != nil {
		js.Result(true, r, nil)
		return 0
	}
	fmt.Fprintln(cio.stdout)
	for _, a := range r.Apps {
		mark := ""
		if a.AI {
			mark = "（AI）"
		}
		fmt.Fprintf(cio.stdout, "%-20s %-24s %s → %s%s\n", a.Name, a.Kind, a.Command, a.Artifact, mark)
		if a.Reason != "" {
			fmt.Fprintf(cio.stdout, "    依据：%s\n", a.Reason)
		}
	}
	if r.Summary != "" {
		fmt.Fprintf(cio.stdout, "\n%s\n", r.Summary)
	}
	return 0
}

// aiEmit 把 AI 的进展交给界面：--json 时按 ai 事件输出，否则在终端里打印。
func aiEmit(js *ui.JSON, cio *cliIO) func(ai.Event) {
	streamed := false // 命令行下逐段打印过回答，完整的回答就不再打印
	return func(ev ai.Event) {
		if js != nil {
			f := map[string]any{"type": ev.Type}
			if ev.Text != "" {
				f["text"] = ev.Text
			}
			if ev.Call != nil {
				f["call"] = ev.Call
			}
			if ev.Approval != nil {
				f["approval"] = ev.Approval
				f["allowed"] = ev.Allowed
			}
			if ev.Type == "result" {
				f["result"] = ai.Clip(ev.Result, 64<<10)
				f["isError"] = ev.IsError
			}
			if ev.Usage != nil {
				f["usage"] = ev.Usage
				f["model"] = ev.Model
			}
			js.Emit("ai", f)
			if fl, ok := cio.stdout.(interface{ Flush() }); ok {
				fl.Flush()
			}
			return
		}
		switch ev.Type {
		case "delta":
			fmt.Fprint(cio.stdout, ev.Text)
			streamed = true
		case "text":
			if streamed {
				fmt.Fprintln(cio.stdout)
			} else {
				fmt.Fprintln(cio.stdout, ev.Text)
			}
			streamed = false
		case "usage":
			u := ev.Usage
			fmt.Fprintf(cio.stdout, "    （%s：输入 %d、缓存 %d、输出 %d tokens）\n", ev.Model, u.Input, u.CacheRead+u.CacheWrite, u.Output)
		case "call":
			fmt.Fprintf(cio.stdout, "  ▶ %s %s\n", ev.Call.Name, string(ev.Call.Input))
			if ev.Allowed {
				fmt.Fprintf(cio.stdout, "    （同类已允许：%s）\n", ev.Approval.Kind)
			}
		case "output":
			for _, l := range strings.Split(ev.Text, "\n") {
				fmt.Fprintln(cio.stdout, "    │ "+l)
			}
		case "confirm":
			a := ev.Approval
			fmt.Fprintf(cio.stdout, "\n  ？ 需要确认：%s\n", a.Title)
			if a.AsRoot {
				fmt.Fprintln(cio.stdout, "    以 root 执行")
			}
			if a.Danger != "" {
				fmt.Fprintf(cio.stdout, "    危险：%s\n", a.Danger)
			}
			if d := strings.TrimRight(a.Detail, "\n"); d != "" {
				for _, l := range strings.Split(d, "\n") {
					fmt.Fprintln(cio.stdout, "    "+l)
				}
			}
		case "result":
			out := ai.Clip(strings.TrimSpace(ev.Result), 1500)
			if ev.IsError {
				out = "✗ " + out
			}
			for _, l := range strings.Split(out, "\n") {
				fmt.Fprintln(cio.stdout, "    "+l)
			}
		}
	}
}

// aiHistoryCmd ferry ai-history list [-c <配置>] [<目标>] | show <对话> | delete <对话> | clean [--all]：AI 助手的历史对话。
func aiHistoryCmd(c *cli, cio *cliIO, js *ui.JSON, st settings.Settings, fail func(error) int) int {
	dir := engine.AIConvDir()
	action := "list"
	if len(c.pos) > 0 {
		action = c.pos[0]
	}
	arg := ""
	if len(c.pos) > 1 {
		arg = c.pos[1]
	}
	usage := func() map[string]any {
		n, b := ai.DirUsage(dir)
		return map[string]any{"count": n, "bytes": b, "keepDays": st.AI.KeepDays, "maxMB": st.AI.MaxMB}
	}
	var res map[string]any
	switch action {
	case "list":
		list, err := ai.List(dir)
		if err != nil {
			return fail(err)
		}
		cfg := c.flags["c"]
		if cfg == "" {
			cfg = c.flags["config"]
		}
		items := []ai.Summary{}
		for _, s := range list {
			if (cfg == "" || samePath(s.Config, cfg)) && (arg == "" || s.Target == arg) {
				items = append(items, s)
			}
		}
		res = usage()
		res["items"] = items
		if js == nil {
			for _, s := range items {
				fmt.Fprintf(cio.stdout, "%s  %s  %-16s %s\n", s.ID, s.Updated.Local().Format("01-02 15:04"), s.Target, s.Title)
			}
			fmt.Fprintf(cio.stdout, "共 %d 个对话，%.1f MB（保留 %d 天、最多 %d MB）\n", res["count"], float64(res["bytes"].(int64))/(1<<20), st.AI.KeepDays, st.AI.MaxMB)
			return 0
		}
	case "show":
		if arg == "" {
			return fail(errors.New("用法：ferry ai-history show <对话 ID>"))
		}
		conv, err := ai.Load(dir, arg)
		if err != nil {
			return fail(err)
		}
		if len(conv.Messages) == 0 {
			return fail(fmt.Errorf("没有这个对话（可能已经被清理）：%s", arg))
		}
		conv.Recover() // 只用于显示：执行到一半退出的操作显示为结果未知
		res = map[string]any{"id": conv.ID, "config": conv.Config, "target": conv.Target, "items": conv.Items(), "pending": conv.Pending != nil,
			"usage": conv.UsageByModel()}
		if js == nil {
			for _, it := range conv.Items() {
				switch it.Kind {
				case "user":
					fmt.Fprintln(cio.stdout, "> "+it.Text)
				case "text":
					fmt.Fprintln(cio.stdout, it.Text)
				case "tool":
					fmt.Fprintf(cio.stdout, "  ▶ %s %s\n", it.Call.Name, string(it.Call.Input))
				}
			}
			return 0
		}
	case "export":
		if arg == "" {
			return fail(errors.New("用法：ferry ai-history export <对话 ID> [--out <文件>]"))
		}
		conv, err := ai.Load(dir, arg)
		if err != nil {
			return fail(err)
		}
		if len(conv.Messages) == 0 {
			return fail(fmt.Errorf("没有这个对话（可能已经被清理）：%s", arg))
		}
		conv.Recover()
		md := conv.Markdown()
		out := c.flags["out"]
		if out == "" {
			if js == nil {
				fmt.Fprint(cio.stdout, md)
				return 0
			}
			res = map[string]any{"markdown": md}
			break
		}
		if err := os.WriteFile(out, []byte(md), 0o600); err != nil {
			return fail(err)
		}
		res = map[string]any{"file": out}
		if js == nil {
			fmt.Fprintf(cio.stdout, "已导出到 %s\n", out)
			return 0
		}
	case "delete":
		if arg == "" {
			return fail(errors.New("用法：ferry ai-history delete <对话 ID>"))
		}
		if err := ai.Delete(dir, arg); err != nil {
			return fail(err)
		}
		res = usage()
	case "clean":
		n := 0
		if c.has("all") {
			n = ai.CleanAll(dir)
		} else {
			// 设置里「清理」用表单上（可能还没保存）的值
			if v, err := strconv.Atoi(c.flags["days"]); err == nil && v > 0 {
				st.AI.KeepDays = v
			}
			if v, err := strconv.Atoi(c.flags["max-mb"]); err == nil && v > 0 {
				st.AI.MaxMB = v
			}
			n = engine.CleanAIHistory(st, "")
		}
		res = usage()
		res["removed"] = n
		if js == nil {
			fmt.Fprintf(cio.stdout, "删除了 %d 个对话\n", n)
			return 0
		}
	default:
		return fail(errors.New("用法：ferry ai-history list|show|delete|clean"))
	}
	if js != nil {
		js.Result(true, res, nil)
	}
	return 0
}

// samePath 两个本机路径是否指同一个文件（Windows 上不分大小写、不分 / 和 \）。
func samePath(a, b string) bool {
	norm := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return filepath.Clean(p)
	}
	a, b = norm(a), norm(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
