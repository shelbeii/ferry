package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/rundist"
)

type dotnetFramework struct {
	Name, Version, RollForward string
	ApplyPatches               *bool
}

type dotnetRuntimeConfig struct {
	RuntimeOptions struct {
		TFM, RollForward           string
		RollForwardOnNoCandidateFx json.RawMessage
		ApplyPatches               *bool
		Framework                  dotnetFramework
		Frameworks                 []dotnetFramework
	}
}

// checkDotnetRelease 检查以 Ferry 管理的 dotnet 直接启动的框架依赖应用，切换前即可发现缺少框架。
func (a *Agent) checkDotnetRelease(unit, running, release string) error {
	args := unitExecArgs(unit)
	if len(args) < 2 || !path.IsAbs(args[0]) || path.Base(args[0]) != "dotnet" {
		return nil
	}
	host, ok := a.readManagedRuntime(filepath.Dir(args[0]))
	if !ok || host.Kind != "dotnet" {
		return nil
	}
	dll := args[1]
	if dll == "exec" && len(args) >= 3 {
		dll = args[2]
	}
	if !strings.HasSuffix(dll, ".dll") || strings.HasPrefix(dll, "-") {
		return nil
	}
	if path.IsAbs(dll) {
		rel, ok := strings.CutPrefix(dll, running+"/")
		if !ok {
			return nil
		}
		dll = rel
	}
	name := path.Clean(strings.TrimSuffix(dll, ".dll") + ".runtimeconfig.json")
	if !runtimeArchivePath(name) {
		return nil
	}
	root, err := os.OpenRoot(release)
	if err != nil {
		return err
	}
	defer root.Close()
	st, err := root.Stat(name)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 256<<10 {
		return nil
	}
	raw, err := root.ReadFile(name)
	if err != nil {
		return err
	}
	var config dotnetRuntimeConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("读取 %s 失败：%w", name, err)
	}
	options := config.RuntimeOptions
	// 外部环境文件和旧式 hostfxr 选项可能覆写策略，由 .NET 宿主在启动时判断。
	if len(unitValues(unit, "EnvironmentFile")) != 0 || len(options.RollForwardOnNoCandidateFx) != 0 {
		return nil
	}
	policy := ""
	for _, row := range unitValues(unit, "Environment") {
		for _, kv := range sdSplit(row) {
			if v, ok := strings.CutPrefix(kv, "DOTNET_ROLL_FORWARD="); ok {
				policy = v
			}
		}
	}
	frameworks := options.Frameworks
	if options.Framework.Name != "" {
		frameworks = append(frameworks, options.Framework)
	}
	for _, framework := range frameworks {
		if framework.Name == "" || strings.ContainsAny(framework.Name, "/\\") ||
			!rundist.ValidVersion(framework.Version) || framework.ApplyPatches != nil ||
			options.ApplyPatches != nil {
			continue
		}
		rule := options.RollForward
		if framework.RollForward != "" {
			rule = framework.RollForward
		}
		if policy != "" {
			rule = policy
		}
		switch dotnet := strings.ToLower(rule); dotnet {
		case "", "disable", "latestpatch", "minor", "latestminor", "major", "latestmajor":
		default:
			continue
		}
		entries, _ := os.ReadDir(filepath.Join(host.Home, "shared", framework.Name))
		match := false
		for _, entry := range entries {
			if entry.IsDir() && dotnetVersionMatches(entry.Name(), framework.Version, rule) {
				match = true
				break
			}
		}
		if !match {
			return fmt.Errorf("%s 需要 %s %s，所选 dotnet（%s）中没有兼容版本，请更换运行环境", name, framework.Name, framework.Version, host.Path)
		}
	}
	return nil
}

func dotnetVersionMatches(installed, requested, rule string) bool {
	if !rundist.ValidVersion(installed) || (installed != requested && !rundist.Newer(installed, requested)) {
		return false
	}
	got, want := rundist.VersionParts(installed), rundist.VersionParts(requested)
	switch strings.ToLower(rule) {
	case "disable":
		return installed == requested
	case "latestpatch":
		return got[0] == want[0] && got[1] == want[1]
	case "major", "latestmajor":
		return true
	default:
		return got[0] == want[0]
	}
}
