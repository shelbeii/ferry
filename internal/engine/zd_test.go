package engine

import (
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestZeroDowntimeSpec(t *testing.T) {
	f, err := loadYAML(t, `
version: 1
project: shop
targets:
  prod: {host: 10.0.0.1, runAs: shop}
apps:
  - name: gateway
    type: java
    vars: {port: "8080"}
    artifact: {path: gw.jar}
    service:
      jvmArgs: ["-Dlogging.file.path=${logs}", "-Dslot=${slot}"]
      args: ["--server.port=${port}"]
      health: {http: "http://127.0.0.1:${port}/actuator/health"}
      zeroDowntime: {ports: [18080, 18081], listen: "0.0.0.0:8080", drain: 5s}
  - name: system
    type: java
    artifact: {path: sys.jar}
    service:
      env: {SERVER_PORT: "9999"}
      zeroDowntime: {ports: [9201, 19201]}
`)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("prod")
	s, err := Spec(f, tg, f.App("gateway"))
	if err != nil {
		t.Fatal(err)
	}
	zd := s.ZD
	if zd == nil || zd.DrainSec != 5 || zd.Slots[0].Unit != "shop-gateway.service" || zd.Slots[1].Unit != "shop-gateway-b.service" {
		t.Fatalf("%+v", zd)
	}
	a, b := zd.Slots[0], zd.Slots[1]
	for _, want := range []string{"--server.port=18080", "/opt/shop/apps/gateway/slots/a/gw.jar", "-Dlogging.file.path=/opt/shop/apps/gateway/slots/a/logs",
		"-Dslot=a", "WorkingDirectory=/opt/shop/apps/gateway/slots/a", `Environment="SERVER_PORT=18080"`, `Environment="PORT=18080"`} {
		if !strings.Contains(a.UnitText, want) {
			t.Errorf("槽位 a 缺 %q：\n%s", want, a.UnitText)
		}
	}
	if !strings.Contains(b.UnitText, "--server.port=18081") || !strings.Contains(b.UnitText, "slots/b/gw.jar") || b.Health.HTTP != "http://127.0.0.1:18081/actuator/health" {
		t.Errorf("槽位 b：%+v\n%s", b.Health, b.UnitText)
	}
	if s.Unit != a.Unit || s.UnitText != a.UnitText {
		t.Error("AppSpec 的单元是槽位 a 的")
	}
	if zd.ProxyUnit != "shop-gateway-proxy.service" || !strings.Contains(zd.ProxyConf, "listen 0.0.0.0:8080;") ||
		!strings.Contains(zd.ProxyConf, "server 127.0.0.1:"+proto.ProxyPort) || s.RuntimeNginx != "nginx" {
		t.Errorf("前面的 nginx：%s\n%s", zd.ProxyUnit, zd.ProxyConf)
	}
	if !strings.Contains(zd.ProxyUnitText, "/opt/shop/runtimes/nginx/nginx") || !contains(s.SharedDirs, "proxy/logs") {
		t.Errorf("%s %v", zd.ProxyUnitText, s.SharedDirs)
	}

	sys, err := Spec(f, tg, f.App("system"))
	if err != nil {
		t.Fatal(err)
	}
	if sys.ZD.DrainSec != 30 || sys.ZD.ProxyUnit != "" || sys.RuntimeNginx != "" {
		t.Errorf("默认 drain 30 秒、没有 listen 不加 nginx：%+v", sys.ZD)
	}
	if h := sys.ZD.Slots[1].Health; h.TCP != "127.0.0.1:19201" {
		t.Errorf("没有健康检查时检查槽位的端口：%+v", h)
	}
	if !strings.Contains(sys.ZD.Slots[1].UnitText, `Environment="SERVER_PORT=9999"`) {
		t.Error("配置里写了 SERVER_PORT 就用配置的")
	}
}

func TestZeroDowntimeConfigErrors(t *testing.T) {
	for zd, want := range map[string]string{
		`{ports: [9201]}`:                       "两个不同的端口",
		`{ports: [9201, 9201]}`:                 "两个不同的端口",
		`{ports: [9201, 70000]}`:                "端口 70000 不对",
		`{ports: [9201, 19201], listen: 9201}`:  "不能和 ports 里的端口一样",
		`{ports: [9201, 19201], listen: "x:y"}`: "listen 写端口",
		`{ports: [9201, 19201], drain: 11m}`:    "drain 最多 10m",
		`{ports: [9201, 19201], nginx: "1.30"}`: "只在设置了 listen 时有用",
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
    service: {zeroDowntime: `+zd+`}
`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s：期望含 %q，得到 %v", zd, want, err)
		}
	}
}
