package engine

import (
	"errors"
	"fmt"
	"slices"

	"github.com/captain/ferry/internal/config"
)

type TargetDraftCheck struct {
	RunAsApps     []string            `json:"runAsApps"`
	HasAppSecrets bool                `json:"hasAppSecrets"`
	MissingVars   map[string][]string `json:"missingVars"`
	Problems      []string            `json:"problems"`
}

// CheckTargetDraft 展开新增服务器上的应用配置。只读本机配置，不建立 SSH 会话。
func CheckTargetDraft(f *config.File, name string) (TargetDraftCheck, error) {
	result := TargetDraftCheck{RunAsApps: []string{}, MissingVars: map[string][]string{}, Problems: []string{}}
	t, err := f.Target(name)
	if err != nil || len(f.Apps) == 0 {
		return result, err
	}
	apps, err := f.Select(t, nil)
	if err != nil {
		return result, err
	}
	record := func(err error) {
		if err == nil {
			return
		}
		var missing *config.UndefinedVarError
		if errors.As(err, &missing) && missing.CanDefine() {
			if !slices.Contains(result.MissingVars[missing.Name], err.Error()) {
				result.MissingVars[missing.Name] = append(result.MissingVars[missing.Name], err.Error())
			}
		} else if !slices.Contains(result.Problems, err.Error()) {
			result.Problems = append(result.Problems, err.Error())
		}
	}
	badRoot := make(map[string]bool)
	for _, a := range apps {
		if a.RequiresTargetRunAs() {
			result.RunAsApps = append(result.RunAsApps, a.Name)
		}
		result.HasAppSecrets = result.HasAppSecrets || len(a.Service.Secrets) > 0
		// 正式加载先校验 YAML 的应用根目录，再套用本机设置。
		if _, err := f.AppRoot(t, a); err != nil {
			record(fmt.Errorf("%s root：%w", a.Name, err))
			badRoot[a.Name] = true
		}
	}
	local, err := config.ReadLocal(f.Project, f.Path)
	if err != nil {
		return result, err
	}
	f.ApplyLocal(local)
	for _, a := range apps {
		if !badRoot[a.Name] {
			_, err := Spec(f, t, a)
			record(err)
		}
		_, err := LocalArtifact(f, t, a)
		record(err)
		_, err = LocalFiles(f, t, a)
		record(err)
	}
	return result, nil
}
