package config

import (
	"strings"
	"testing"
	"time"
)

func TestRestartConfig(t *testing.T) {
	yaml := func(service string) string {
		return `
version: 1
project: shop
targets:
  t1: {host: 10.0.0.1, root: /data/shop, runAs: web}
apps:
  - name: api
    type: binary
    artifact: {path: api}
    service:
` + service
	}
	f, err := load(t, yaml("      restart: always\n      after: [mysqld, redis-server.service]\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := f.App("api").Service
	if s.Restart.WhenOr() != RestartAlways || s.Restart.DelayOr().Seconds() != 5 || !s.AutostartOn() {
		t.Errorf("简写：%+v", s.Restart)
	}
	if strings.Join(s.After, ",") != "mysqld.service,redis-server.service" {
		t.Errorf("after 补 .service：%v", s.After)
	}
	if f, err := load(t, yaml("      restart: false\n      autostart: false\n")); err != nil || f.App("api").Service.Restart.WhenOr() != RestartNo || f.App("api").Service.AutostartOn() {
		t.Errorf("restart: false = no：%v", err)
	}

	if f, err := load(t, yaml("      autostartDelay: 90s\n")); err != nil || f.App("api").Service.AutostartDelay.Std() != 90*time.Second {
		t.Errorf("autostartDelay：%v", err)
	}

	for bad, want := range map[string]string{
		"      restart: sometimes\n":                          "on-failure",
		"      restart: {delay: 20m}\n":                       "delay",
		"      restart: {max: 500}\n":                         "max",
		"      restart: {within: 5m}\n":                       "和 max 一起写",
		"      restart: {when: no, delay: 5s}\n":              "不用写 delay",
		"      after: [\"bad name\"]\n":                       "after",
		"      unit: shop-api\n      after: [shop-api]\n":     "不能写应用自己",
		"      autostartDelay: 2h\n":                          "1s～1h",
		"      autostart: false\n      autostartDelay: 30s\n": "不用写 service.autostartDelay",
	} {
		if _, err := load(t, yaml(bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q：期望含 %q，得到 %v", bad, want, err)
		}
	}
}
