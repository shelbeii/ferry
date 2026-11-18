package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

var installTaskID = regexp.MustCompile(`^[a-z0-9-]{6,64}$`)

func (a *Agent) softwareTaskPath(id string) (string, error) {
	if !installTaskID.MatchString(id) {
		return "", errors.New("安装任务 ID 无效")
	}
	return filepath.Join(a.swDir(), ".tasks", id+".json"), nil
}

func (a *Agent) beginSoftwareTask(plan *proto.SoftwarePlan) (func(), error) {
	path, err := a.softwareTaskPath(plan.TaskID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := a.Sys.Lock(path+".lock", 0)
	if err != nil {
		return nil, fmt.Errorf("安装任务 %s 正在执行或无法加锁：%w", plan.TaskID, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		unlock()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("安装任务 %s 已提交，请查询原任务结果", plan.TaskID)
	}
	now := a.Sys.Now()
	a.installTask = &proto.SoftwareTask{
		ID: plan.TaskID, Name: plan.Name, Phase: "接收安装计划",
		Status: "running", StartedAt: now, UpdatedAt: now,
	}
	if err := a.writeSoftwareTask(path); err != nil {
		a.installTask = nil
		unlock()
		return nil, err
	}
	// 已退出且保留超过 30 天的记录可以清除；每次安装顺便清理。
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !installTaskID.MatchString(id) || id == plan.TaskID {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < 30*24*time.Hour {
			continue
		}
		old, _ := a.softwareTaskPath(id)
		// 保留锁文件，防止另一个进程同时锁住已经从目录移除的 inode。
		if release, err := a.Sys.Lock(old+".lock", 0); err == nil {
			_ = os.Remove(old)
			release()
		}
	}
	return unlock, nil
}

func (a *Agent) writeSoftwareTask(path string) error {
	b, err := json.Marshal(a.installTask)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

func (a *Agent) saveSoftwareTask() {
	if task := a.installTask; task != nil {
		task.UpdatedAt = a.Sys.Now()
		path, _ := a.softwareTaskPath(task.ID)
		if err := a.writeSoftwareTask(path); err != nil {
			a.warn(proto.EvWarn, task.Name, "保存服务器安装进度失败：%v", err)
		}
	}
}

func (a *Agent) finishSoftwareTask(code int) {
	a.installTask.Code = code
	switch code {
	case proto.CodeOK:
		a.installTask.Status = "ok"
	case proto.CodeConfirm:
		a.installTask.Status = "needs-confirmation"
	default:
		a.installTask.Status = "failed"
	}
	a.saveSoftwareTask()
	a.installTask = nil
}

func (a *Agent) softwareTaskStatus() int {
	path, err := a.softwareTaskPath(a.req.SoftwareTaskID)
	if err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return a.result(proto.CodeOK, "", nil)
	}
	if err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	var task proto.SoftwareTask
	if err := json.Unmarshal(b, &task); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	if task.Status == "running" {
		if unlock, err := a.Sys.Lock(path+".lock", 0); err == nil {
			defer unlock()
			// 安装可能在读取快照和取锁之间刚结束。
			if b, err := os.ReadFile(path); err == nil {
				_ = json.Unmarshal(b, &task)
			}
			if task.Status == "running" {
				task.Status = "interrupted"
			}
		}
	}
	if task.Status == "ok" {
		if instance, err := a.loadInstance(task.Name); err == nil && instance.InstallTaskID == task.ID {
			task.Current = true
			task.PasswordUnset = slices.Contains(instance.Unset, "password")
		}
	}
	return a.result(proto.CodeOK, "", task)
}

func (a *Agent) installPhase(name, phase string) {
	if a.installTask != nil {
		a.installTask.Phase = phase
		a.saveSoftwareTask()
	}
	a.info(proto.EvInstallPhase, name, "%s", phase)
}

func (a *Agent) installShell(name, phase, command string, timeout time.Duration) (string, error) {
	started := time.Now()
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.info(proto.EvHeartbeat, name, "%s，已用时 %s", phase, time.Since(started).Round(time.Second))
			case <-done:
				return
			}
		}
	}()
	defer func() {
		close(done)
		<-exited
	}()
	return a.Sys.Shell(command, timeout)
}
