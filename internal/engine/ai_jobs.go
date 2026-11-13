package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

// 服务器助手（和经 MCP 的编程软件）用的：现在运行定时任务、把异地的备份取回服务器。

// reBackupFileArg 备份文件名（<名字>-<时间>.<扩展名>，可能带 .age）。
var reBackupFileArg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)

// CheckJob 这个应用是不是这台服务器上的定时任务。
func (e *Engine) CheckJob(t *config.Target, app string) error {
	a := e.File.App(app)
	if app == "" || a == nil || !t.Allows(app) {
		return fmt.Errorf("%s 上没有应用 %q", t.Name, app)
	}
	if a.Job == nil {
		return fmt.Errorf("%s 不是定时任务（配置里没有写 job），启停用 app_control", app)
	}
	return nil
}

// CheckFetch 能不能把异地的这份备份取回来：配置里这一项要设了 upload，文件名要像备份文件。
func (e *Engine) CheckFetch(target, name, file string) error {
	if name == "" {
		return errors.New("要写明是哪一项备份（name）")
	}
	if err := e.checkBackup(target, name); err != nil {
		return err
	}
	if !e.uploads(target, name) {
		return fmt.Errorf("备份 %s 没有设置传到异地（upload）", name)
	}
	if !reBackupFileArg.MatchString(file) || !strings.HasPrefix(file, name+"-") {
		return fmt.Errorf("不是 %s 的备份文件：%q（用 backup_status 的 remote 列出异地的，填 name 字段）", name, file)
	}
	return nil
}

// uploads 这台服务器上的这项备份设了传到异地。
func (e *Engine) uploads(target, name string) bool {
	for _, b := range e.File.Backups {
		if b.Name == name && b.AppliesTo(target) {
			return b.Upload != nil && b.Upload.S3 != nil
		}
	}
	return false
}

// RemoteBackups 传到异地（S3）的备份文件；name 为空 = 这台服务器上设了 upload 的每一项。
func (e *Engine) RemoteBackups(target, name string) (proto.BackupStatus, error) {
	if name != "" {
		if err := e.checkBackup(target, name); err != nil {
			return proto.BackupStatus{}, err
		}
		if !e.uploads(target, name) {
			return proto.BackupStatus{}, fmt.Errorf("备份 %s 没有设置传到异地（upload）", name)
		}
		return e.Backup(target, "remote", BackupOptions{Name: name})
	}
	var out proto.BackupStatus
	found := false
	for _, b := range e.File.Backups {
		if !b.AppliesTo(target) || b.Upload == nil || b.Upload.S3 == nil {
			continue
		}
		found = true
		r, err := e.Backup(target, "remote", BackupOptions{Name: b.Name})
		if err != nil {
			return out, fmt.Errorf("%s：%w", b.Name, err)
		}
		out.On, out.Dir = r.On, r.Dir
		out.Files = append(out.Files, r.Files...)
	}
	if !found {
		return out, fmt.Errorf("%s 上的备份都没有设置传到异地（backups 里的 upload）", target)
	}
	out.Items = []proto.BackupItemStatus{}
	return out, nil
}

// aiJobTools 允许改动时：现在运行定时任务、把异地的备份取回服务器。
func (e *Engine) aiJobTools(t *config.Target) []ai.Tool {
	type jobArgs struct{ App string }
	type fetchArgs struct{ Name, File string }
	return []ai.Tool{{
		Name: "run_job",
		Description: "现在运行一次定时任务（配置里写了 job 的应用）：等它运行完，返回结果（成功、退出码、用了多久），输出实时显示。" +
			"不影响定时器，到点照样运行。先用 app_status 看它上次运行的结果、是不是正在运行。",
		Schema: schema(map[string]any{"app": str("定时任务的应用名")}, "app"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			var a jobArgs
			if parseArgs(in, &a) != nil || e.CheckJob(t, a.App) != nil {
				return nil
			}
			j := e.File.App(a.App).Job
			d := "定时：" + strings.Join(j.Schedule, "、") + "\n最长运行 " + j.TimeoutOr().String() + "，这次运行不影响定时器"
			return &ai.Approval{Kind: "现在运行定时任务", Title: "现在运行定时任务 " + a.App, Detail: d, AsRoot: true}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a jobArgs
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if err := e.CheckJob(t, a.App); err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) { return e.RunJob(t.Name, a.App) })
			if err != nil {
				return toJSON(r), fmt.Errorf("%v（用 app_logs 看它的输出）", err)
			}
			return toJSON(r), nil
		},
	}, {
		Name: "backup_fetch",
		Description: "把传到异地（S3 兼容存储）的一份备份取回服务器的备份目录（加密的自动解密），服务器上那份被清掉了、坏了时用。" +
			"先用 backup_status 的 remote 列出异地的（local 为 true 的服务器上已经有了，不用取回）。取回后恢复要用户在 Ferry 的「数据备份」里做。",
		Schema: schema(map[string]any{
			"name": str("哪一项备份（backup_status 里 items 的 name）"),
			"file": str("异地的备份文件名（remote 列出的 name）"),
		}, "name", "file"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			var a fetchArgs
			if parseArgs(in, &a) != nil || e.CheckFetch(t.Name, a.Name, a.File) != nil {
				return nil
			}
			return &ai.Approval{Kind: "取回备份", Title: "把异地的备份 " + a.File + " 取回服务器", AsRoot: true}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a fetchArgs
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if err := e.CheckFetch(t.Name, a.Name, a.File); err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) {
				return e.Backup(t.Name, "fetch", BackupOptions{Name: a.Name, File: a.File})
			})
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}}
}
