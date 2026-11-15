package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/secret"
)

type AppSecretsRequired struct {
	Target  string
	App     string
	Names   []string
	Problem string
}

func (e *AppSecretsRequired) Error() string {
	if e.Problem != "" {
		return fmt.Sprintf("%s 上的 %s 无法读取团队密钥：%s", e.Target, e.App, e.Problem)
	}
	return fmt.Sprintf("%s 上的 %s 缺少应用密钥 %s：请在「应用密钥」中填写，或设置 %s",
		e.Target, e.App, strings.Join(e.Names, "、"), strings.Join(e.envNames(), "、"))
}

func (e *AppSecretsRequired) envNames() []string {
	env := make([]string, len(e.Names))
	for i, name := range e.Names {
		env[i] = secret.AppEnv(e.App, name)
	}
	return env
}

func (e *AppSecretsRequired) JSONDetail() map[string]any {
	return map[string]any{"kind": "app-secrets-required", "target": e.Target, "app": e.App, "names": e.Names, "envNames": e.envNames()}
}

func (e *Engine) keyringAppSecret(t *config.Target, app, name string) (value, source string) {
	v, ok, err := secret.Get(secret.AppAccount(e.File.Project, t.Name, app, name))
	switch {
	case err != nil:
		return "", "unavailable"
	case ok:
		return v, "target"
	}
	v, ok, err = secret.Get(secret.AppAccount(e.File.Project, "*", app, name))
	switch {
	case err != nil:
		return "", "unavailable"
	case ok:
		return v, "shared"
	default:
		return "", ""
	}
}

type appSecretLookup func(app, name string) (value, source string, err error)

// 每个目标每次操作最多读一次加密文件。完全由环境变量供值的 CI 可以直接部署。
func (e *Engine) appSecretLookup(t *config.Target) appSecretLookup {
	load := sync.OnceValues(func() (*teamSecretFile, error) {
		id, err := e.teamReadIdentity()
		if err != nil {
			return nil, err
		}
		return e.readTeamSecrets(t, id)
	})
	return func(app, name string) (string, string, error) {
		if v := e.getenv(secret.AppEnv(app, name)); v != "" {
			return v, "env", nil
		}
		if t.AppSecretsFile() == "" {
			v, source := e.keyringAppSecret(t, app, name)
			return v, source, nil
		}
		f, err := load()
		if err != nil {
			return "", "team-unavailable", err
		}
		if v, ok := f.Apps[app][name]; ok {
			if v == "" {
				return "", "invalid", nil
			}
			return v, "team", nil
		}
		return "", "", nil
	}
}

func appSecretValues(t *config.Target, a *config.App, lookup appSecretLookup) (*proto.AppSecretValues, error) {
	if len(a.Service.Secrets) == 0 {
		return nil, nil
	}
	values := make(proto.AppSecretValues, len(a.Service.Secrets))
	missing := &AppSecretsRequired{Target: t.Name, App: a.Name}
	for _, ref := range a.Service.Secrets {
		v, source, err := lookup(a.Name, ref.Name)
		if err != nil {
			missing.Names = append(missing.Names, ref.Name)
			missing.Problem = err.Error()
			continue
		}
		if source == "invalid" {
			return nil, fmt.Errorf("%s / %s / %s：密钥值不能为空", t.Name, a.Name, ref.Name)
		}
		if v == "" {
			if !ref.Optional {
				missing.Names = append(missing.Names, ref.Name)
			}
			continue
		}
		if err := proto.ValidateAppSecretValue(v); err != nil {
			return nil, fmt.Errorf("%s / %s / %s：%w", t.Name, a.Name, ref.Name, err)
		}
		values[ref.Name] = v
	}
	if len(missing.Names) > 0 {
		return nil, missing
	}
	return &values, nil
}

// AppSecrets 查询本机配置和供值情况，无需连接服务器。
func (e *Engine) AppSecrets(target string, names []string) ([]proto.AppSecretStatus, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	apps, err := e.File.Select(t, names)
	if err != nil {
		return nil, err
	}
	rows := []proto.AppSecretStatus{}
	lookup := e.appSecretLookup(t)
	for _, a := range apps {
		for _, ref := range a.Service.Secrets {
			v, source, _ := lookup(a.Name, ref.Name)
			if v != "" && proto.ValidateAppSecretValue(v) != nil {
				source = "invalid"
			}
			rows = append(rows, proto.AppSecretStatus{App: a.Name, Name: ref.Name, Doc: ref.Doc,
				Optional: ref.Optional, Available: v != "" && source != "invalid", Source: source})
		}
	}
	return rows, nil
}

// AppSecretAvailability 供 MCP 和 AI 查询，结果只有应用、变量名和供值状态。
func (e *Engine) AppSecretAvailability(target string, names []string) (map[string]map[string]bool, error) {
	rows, err := e.AppSecrets(target, names)
	if err != nil {
		return nil, err
	}
	apps := map[string]map[string]bool{}
	for _, row := range rows {
		if apps[row.App] == nil {
			apps[row.App] = map[string]bool{}
		}
		apps[row.App][row.Name] = row.Available
	}
	return apps, nil
}

// SaveAppSecret 按目标的密钥来源写入或删除一项。
func (e *Engine) SaveAppSecret(target, app, name, value string, shared bool) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	a := e.File.App(app)
	if a == nil || !t.Allows(a.Name) {
		return fmt.Errorf("%s 上没有可部署的应用 %q", t.Name, app)
	}
	for _, ref := range a.Service.Secrets {
		if ref.Name == name {
			if value != "" {
				if err := proto.ValidateAppSecretValue(value); err != nil {
					return err
				}
			}
			if t.AppSecretsFile() != "" {
				if shared {
					return fmt.Errorf("这个目标使用团队密钥文件，无需传 --all-targets")
				}
				return e.saveTeamSecret(t, a.Name, name, value)
			}
			scope := t.Name
			if shared {
				scope = "*"
			}
			account := secret.AppAccount(e.File.Project, scope, a.Name, name)
			if value == "" {
				return secret.Delete(account)
			}
			return secret.Set(account, value)
		}
	}
	return fmt.Errorf("%s 未在 service.secrets 声明 %s", a.Name, name)
}

// PushAppSecrets 只更新当前发布版本的密钥。
func (e *Engine) PushAppSecrets(target string, names []string, restart bool) ([]proto.AppOutcome, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	if err := e.checkDeployer(t, "更新应用密钥"); err != nil {
		return nil, err
	}
	apps, err := e.File.Select(t, names)
	if err != nil {
		return nil, err
	}
	var specs []proto.AppSpec
	lookup := e.appSecretLookup(t)
	for _, a := range apps {
		if len(a.Service.Secrets) == 0 {
			continue
		}
		s, err := Spec(e.File, t, a)
		if err != nil {
			return nil, err
		}
		if s.Secrets, err = appSecretValues(t, a, lookup); err != nil {
			return nil, err
		}
		specs = append(specs, s)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("%s 上没有配置应用密钥的应用", t.Name)
	}
	if t.Production && !e.Yes && !e.confirmProduction("更新应用密钥", []string{t.Name}) {
		return nil, ErrCanceled
	}
	session, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	defer session.close()
	r, err := session.call("secretset", &proto.Request{Apps: specs, RestartSecrets: restart, LockWaitSec: 300})
	if err != nil {
		return nil, err
	}
	var out proto.DeployOutcome
	if len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, &out); err != nil {
			return nil, err
		}
	}
	if r.RC != proto.CodeOK {
		return out.Apps, &AgentError{Cmd: "secretset", RC: r.RC, Msg: r.Msg}
	}
	return out.Apps, nil
}
