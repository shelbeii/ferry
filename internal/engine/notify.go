package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/notify"
	"github.com/captain/ferry/internal/secret"
)

// NotifyTargets 要发通知的地方：设置里的（地址、加签密钥从凭据库取），加上环境变量 FERRY_NOTIFY 里的。
// 没保存地址的跳过，并说明原因（missing）。
func (e *Engine) NotifyTargets() (out []notify.Target, missing []string, err error) {
	for _, ch := range e.Settings.Notify.Channels {
		if ch.Off {
			continue
		}
		u, _, err := secret.Lookup(e.getenv, "", secret.NotifyURLAccount(ch.ID))
		if err != nil || u == "" {
			missing = append(missing, ch.Name)
			continue
		}
		sign, _, _ := secret.Lookup(e.getenv, "", secret.NotifySignAccount(ch.ID))
		out = append(out, notify.Target{Channel: ch, URL: u, Sign: sign})
	}
	env, err := notify.EnvTargets(e.getenv)
	return append(out, env...), missing, err
}

// sendNotify 发给匹配的各个地方（同时发，各自最多 10 秒）。失败只提示，不影响部署的结果。
// 用单独的上下文：部署中途点了停止（服务器上照常做完），结果也要通知。
func (e *Engine) sendNotify(ev notify.Event) {
	targets, missing, err := e.NotifyTargets()
	if err != nil {
		e.Rep.Warn("部署通知：%v", err)
	}
	var use []notify.Target
	for _, t := range targets {
		if notify.Match(t.Channel, ev) {
			use = append(use, t)
		}
	}
	if len(missing) > 0 && len(e.Settings.Notify.Channels) > 0 {
		e.Rep.Warn("部署通知：%s 没有保存地址，没有发（在设置 → 通知里填上）", strings.Join(missing, "、"))
	}
	if len(use) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	errs := make([]error, len(use))
	var wg sync.WaitGroup
	for i, t := range use {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = notify.Send(ctx, t, ev)
		}()
	}
	wg.Wait()
	var sent []string
	for i, t := range use {
		if errs[i] != nil {
			e.Rep.Warn("部署通知没有发到「%s」：%v", t.Channel.Name, errs[i])
		} else {
			sent = append(sent, t.Channel.Name)
		}
	}
	if len(sent) > 0 {
		e.Rep.Info("已发送部署通知：%s", strings.Join(sent, "、"))
	}
}

// baseEvent 一条通知里和目标有关的部分。
func (e *Engine) baseEvent(action string, t *config.Target) notify.Event {
	ev := notify.Event{Action: action, Project: e.File.Project, Actor: e.actor(), Time: time.Now()}
	if t != nil {
		ev.Target, ev.Host, ev.Production = t.Name, t.Host, t.Production
	}
	return ev
}

// notifyDeploy 部署结束后通知：每台切换过的服务器一条。还没动服务器就停下的（取消、构建失败、上传失败）不发。
func (e *Engine) notifyDeploy(reports []*DeployReport, err error) {
	if errors.Is(err, ErrCanceled) {
		return
	}
	for i, r := range reports {
		t, _ := e.File.Target(r.Target)
		ev := e.baseEvent("deploy", t)
		ev.OK = true
		ev.Release, ev.Commit, ev.Branch, ev.Dirty = r.Release, strings.TrimSuffix(r.Git.Short(), "*"), r.Git.Branch, r.Git.Dirty // 有没提交的修改另外写
		ev.ElapsedSec = int(r.Elapsed / time.Second)
		for _, a := range r.Apps {
			ev.Apps = append(ev.Apps, notify.App{Name: a.App, Result: a.Result, Msg: a.Msg})
			ev.OK = ev.OK && a.Result == "OK"
		}
		// 出错的是最后一台（后面的没有切换）
		if err != nil && i == len(reports)-1 {
			ev.OK = false
			ev.Error = err.Error()
		}
		e.sendNotify(ev)
	}
}

// notifyRollback 手动回滚结束后通知。
func (e *Engine) notifyRollback(t *config.Target, app, to string, start time.Time, err error) {
	if errors.Is(err, ErrCanceled) {
		return
	}
	ev := e.baseEvent("rollback", t)
	ev.OK, ev.Release, ev.ElapsedSec = err == nil, to, int(time.Since(start)/time.Second)
	res := notify.App{Name: app, Result: "OK"}
	if err != nil {
		res.Result, ev.Error = "FAILED", err.Error()
	}
	ev.Apps = []notify.App{res}
	e.sendNotify(ev)
}

// TestNotify 给一个地方发测试消息。
func TestNotify(t notify.Target) error {
	ev := notify.Event{Action: "test", OK: true, Actor: actor(), Time: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return notify.Send(ctx, t, ev)
}
