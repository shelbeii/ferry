package engine

import (
	"strings"
	"testing"
)

func TestHookSpec(t *testing.T) {
	f, err := loadYAML(t, `
version: 1
project: shop
vars: {profile: prod}
targets:
  prod: {host: 10.0.0.1, runAs: shop}
apps:
  - name: api
    type: java
    artifact: {path: api.jar}
    service:
      env: {SPRING_PROFILES_ACTIVE: "${profile}"}
      envFile: -/etc/shop/api.env
    hooks:
      beforeSwitch:
        - java -jar ${running}/../versions/$FERRY_RELEASE/flyway.jar -url=x migrate
        - {run: "echo ${FERRY_RELEASE} $${HOME}", user: root, timeout: 30s}
      afterSwitch:
        - {run: curl -fsS http://127.0.0.1:8080/warmup, optional: true}
  - name: web
    type: static
    artifact: {path: dist}
    hooks:
      afterSwitch: [./purge.sh]
`)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("prod")
	s, err := Spec(f, tg, f.App("api"))
	if err != nil {
		t.Fatal(err)
	}
	h := s.Hooks
	if h == nil || len(h.BeforeSwitch) != 2 || len(h.AfterSwitch) != 1 {
		t.Fatalf("%+v", h)
	}
	b0, b1, a0 := h.BeforeSwitch[0], h.BeforeSwitch[1], h.AfterSwitch[0]
	if !strings.HasPrefix(b0.Run, "java -jar /opt/shop/apps/api/running/../versions/$FERRY_RELEASE/") || b0.User != "shop" || b0.TimeoutSec != 0 {
		t.Errorf("%+v", b0)
	}
	if b1.Run != "echo ${FERRY_RELEASE} ${HOME}" || b1.User != "root" || b1.TimeoutSec != 30 {
		t.Errorf("${FERRY_*} 应原样交给 shell：%+v", b1)
	}
	if !a0.Optional || a0.User != "shop" {
		t.Errorf("%+v", a0)
	}
	if s.HookEnv["SPRING_PROFILES_ACTIVE"] != "prod" || len(s.HookEnvFiles) != 1 || s.HookEnvFiles[0] != "-/etc/shop/api.env" {
		t.Errorf("钩子应拿到服务的 env：%v %v", s.HookEnv, s.HookEnvFiles)
	}

	w, err := Spec(f, tg, f.App("web"))
	if err != nil || w.Hooks == nil || w.Hooks.AfterSwitch[0].User != "shop" || w.HookEnv != nil {
		t.Fatalf("%v %+v", err, w)
	}
	if n, _ := Spec(f, tg, f.App("api")); n.Hooks == nil {
		t.Fatal("每次渲染都应带上钩子")
	}
}

func TestHookConfigErrors(t *testing.T) {
	for yaml, want := range map[string]string{
		`hooks: {beforeSwitch: [""]}`:                        "run 不能为空",
		`hooks: {afterSwitch: [{run: x, user: "Bad User"}]}`: "user",
		`hooks: {afterSwitch: [{run: x, timeout: 3h}]}`:      "timeout 最多 2h",
		`hooks: {afterSwitch: [{run: x, retry: 3}]}`:         "retry",
		`hooks: {beforeDeploy: [x]}`:                         "beforeDeploy",
	} {
		_, err := loadYAML(t, `
version: 1
project: shop
targets:
  prod: {host: 10.0.0.1, runAs: shop}
apps:
  - name: api
    type: java
    artifact: {path: api.jar}
    `+yaml+`
`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s：期望含 %q 的错误，得到 %v", yaml, want, err)
		}
	}
	// 静态站点没有运行用户时，钩子要写 user
	_, err := loadYAML(t, `
version: 1
project: shop
targets:
  prod: {host: 10.0.0.1}
apps:
  - name: web
    type: static
    artifact: {path: dist}
    hooks: {afterSwitch: [./purge.sh]}
`)
	if err == nil || !strings.Contains(err.Error(), "不知道以哪个用户执行") {
		t.Errorf("%v", err)
	}
}
