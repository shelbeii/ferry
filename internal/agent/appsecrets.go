package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func validateAppSecrets(s *proto.AppSpec) error {
	if s.Secrets == nil {
		return nil
	}
	if s.Type == proto.TypeStatic {
		return errors.New("静态站点不能设置应用密钥")
	}
	for k, v := range *s.Secrets {
		if !proto.ValidAppSecretName(k) {
			return errors.New("应用密钥的变量名无效")
		}
		if err := proto.ValidateAppSecretValue(v); err != nil {
			return fmt.Errorf("%s：%w", k, err)
		}
	}
	return nil
}

var envFileEscapes = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")

func appSecretsContent(values proto.AppSecretValues) ([]byte, []string) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s=\"%s\"\n", name, envFileEscapes.Replace(values[name]))
	}
	return []byte(b.String()), names
}

func writeAppSecrets(file string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(file), proto.AppSecretsFile+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), file)
}

func (a *Agent) appSecretsPath(app, id string) string {
	return filepath.Join(a.releaseDir(app, id), proto.AppSecretsFile)
}

func readAppSecrets(file string) ([]byte, error) {
	fi, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return nil, errors.New("应用密钥文件必须是权限为 0600 的普通文件")
	}
	return os.ReadFile(file)
}

func appSecretNames(data []byte) []string {
	// 只取每行等号前的变量名。值不会出现在状态、事件或错误中。
	var names []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		if name, _, ok := bytes.Cut(line, []byte("=")); ok && proto.ValidAppSecretName(string(name)) {
			names = append(names, string(name))
		}
	}
	return names
}

// slotSecretsUnit 用将要启动的版本确定槽位是否需要 Ferry 的 EnvironmentFile。
func slotSecretsUnit(unitText, slot string, hasFile bool) string {
	directive := proto.AppSecretsDirective(filepath.ToSlash(slot))
	lines := strings.Split(unitText, "\n")
	if i := slices.Index(lines, directive); i >= 0 {
		if hasFile {
			return unitText
		}
		return strings.Join(slices.Delete(lines, i, i+1), "\n")
	}
	if !hasFile {
		return unitText
	}
	// 放在 [Service] 的最后，覆盖用户在 envFile 或 unitExtra 中指定的同名值。
	start := slices.Index(lines, "[Service]")
	if start < 0 {
		return unitText
	}
	end := start + 1
	for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "[") {
		end++
	}
	if end > start+1 && lines[end-1] == "" {
		end--
	}
	return strings.Join(slices.Insert(lines, end, directive), "\n")
}

func (a *Agent) secretset() int {
	unlock, err := a.lock(time.Duration(a.req.LockWaitSec) * time.Second)
	if errors.Is(err, ErrLocked) {
		return a.result(proto.CodeLocked, err.Error(), nil)
	}
	if err != nil {
		return a.result(proto.CodeFailed, "加锁失败："+err.Error(), nil)
	}
	defer unlock()
	if len(a.req.Apps) == 0 {
		return a.result(proto.CodeBadInput, "请选择配置了应用密钥的应用", nil)
	}
	var out proto.DeployOutcome
	failed := false
	for i := range a.req.Apps {
		s := &a.req.Apps[i]
		o := proto.AppOutcome{App: s.Name, Release: a.current(s.Name), Result: "SKIPPED"}
		if !failed {
			o.Result, o.Msg = "OK", ""
			msg, err := a.updateAppSecrets(s, o.Release)
			if err != nil {
				o.Result, o.Msg, failed = "FAILED", err.Error(), true
				a.fail(proto.EvFail, s.Name, "%s", o.Msg)
			} else {
				o.Msg = msg
			}
			a.addHistory(proto.HistoryEntry{Action: "secrets", App: s.Name, Release: o.Release, Result: o.Result, Msg: o.Msg})
		}
		out.Apps = append(out.Apps, o)
	}
	return a.finishControl(out)
}

func (a *Agent) updateAppSecrets(s *proto.AppSpec, id string) (string, error) {
	if s.Secrets == nil || id == "" {
		return "", errors.New("请先声明应用密钥并部署一次")
	}
	running := a.runningDir(s.Name)
	if i := a.activeSlot(s); i >= 0 {
		if s.ZD == nil {
			return "", errors.New("当前版本使用不停机发布，请先按当前配置部署")
		}
		running = a.slotDir(s.Name, s.ZD.Slots[i].Name)
	}
	unit, err := os.ReadFile(a.unitPath(s.Unit))
	if err != nil || !slices.Contains(strings.Split(string(unit), "\n"), proto.AppSecretsDirective(filepath.ToSlash(running))) {
		return "", errors.New("当前版本没有启用此应用的密钥，请先部署")
	}
	file := a.appSecretsPath(s.Name, id)
	old, err := readAppSecrets(file)
	if err != nil {
		return "", fmt.Errorf("读取当前版本的密钥文件失败：%w", err)
	}
	data, names := appSecretsContent(*s.Secrets)
	if err := writeAppSecrets(file, data); err != nil {
		return "", fmt.Errorf("写入当前版本的密钥文件失败：%w", err)
	}
	if a.req.RestartSecrets && a.jobFor(s, id) == nil && isRunning(a.Sys.UnitProps(s.Unit)) {
		if err := a.restartWithAppSecrets(s); err != nil {
			if restoreErr := writeAppSecrets(file, old); restoreErr != nil {
				return "", fmt.Errorf("使用新密钥重启失败，恢复旧密钥文件也失败：%w", restoreErr)
			}
			if restoreErr := a.restartWithAppSecrets(s); restoreErr != nil {
				return "", errors.New("使用新密钥重启失败。已恢复旧密钥文件，但服务未能恢复运行")
			}
			return "", errors.New("使用新密钥重启失败。已恢复旧密钥并重启服务")
		}
		a.info(proto.EvReady, s.Name, "服务已重启")
	}
	a.info(proto.EvInfo, s.Name, "已更新 %s 版本的应用密钥（%s）", id, orNone(strings.Join(names, "、")))
	return "应用密钥：" + orNone(strings.Join(names, "、")), nil
}

func (a *Agent) restartWithAppSecrets(s *proto.AppSpec) error {
	if err := a.Sys.Systemctl("restart", s.Unit); err != nil {
		return err
	}
	spec := *s
	var deployed appHealth
	if readJSON(healthFile(a.meta(), s.Name), &deployed) && deployed.Unit == s.Unit {
		spec.Health = deployed.Health
		spec.Health.StableSec = max(spec.Health.StableSec, 2)
	} else {
		spec.Health = proto.Health{StableSec: 2}
	}
	return a.waitReady(&spec, s.Unit)
}

func (a *Agent) hasAppSecrets(s *proto.AppSpec, unit string) bool {
	if s.Secrets != nil {
		return true
	}
	text, err := os.ReadFile(a.unitPath(unit))
	if err != nil {
		return false
	}
	for _, f := range unitValues(string(text), "EnvironmentFile") {
		if proto.IsAppSecretsPath(f) {
			return true
		}
	}
	return false
}

func isAppSecretsFile(p string) bool {
	if proto.IsAppSecretsPath(filepath.ToSlash(p)) {
		return true
	}
	real, err := filepath.EvalSymlinks(p)
	return err == nil && proto.IsAppSecretsPath(filepath.ToSlash(real))
}
