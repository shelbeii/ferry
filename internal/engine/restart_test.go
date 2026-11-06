package engine

import (
	"strings"
	"testing"
)

const restartYAML = `
version: 1
project: shop
targets:
  t1: {host: 10.0.0.1, root: /data/shop, runAs: web}
apps:
  - name: plain
    type: binary
    artifact: {path: plain}
  - name: always
    type: binary
    artifact: {path: always}
    service:
      restart: {when: always, delay: 10s, max: 3, within: 5m}
      autostart: false
      after: [mysqld, docker.socket]
  - name: fast
    type: binary
    artifact: {path: fast}
    service:
      restart: {delay: 500ms}
  - name: never
    type: binary
    artifact: {path: never}
    service:
      restart: no
  - name: late
    type: binary
    artifact: {path: late}
    service:
      autostartDelay: 2m
`

func TestRestartUnit(t *testing.T) {
	f, err := loadYAML(t, restartYAML)
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("t1")
	unit := func(app string) (string, bool) {
		s, err := Spec(f, tg, f.App(app))
		if err != nil {
			t.Fatal(err)
		}
		return s.UnitText, s.NoAutostart
	}
	check := func(app, text string, want, not []string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(text, w+"\n") {
				t.Errorf("%s 缺少 %q：\n%s", app, w, text)
			}
		}
		for _, w := range not {
			if strings.Contains(text, w) {
				t.Errorf("%s 不该有 %q：\n%s", app, w, text)
			}
		}
	}

	// 不写：和以前一样
	text, no := unit("plain")
	check("plain", text, []string{"After=network-online.target", "Restart=on-failure", "RestartSec=5", "WantedBy=multi-user.target", "# project=shop app=plain target=t1"},
		[]string{"StartLimit", "autostart=no"})
	if no {
		t.Error("plain 默认开机自启")
	}

	text, no = unit("always")
	check("always", text, []string{
		"After=network-online.target mysqld.service docker.socket",
		"StartLimitIntervalSec=300", "StartLimitBurst=3", // [Unit]
		"Restart=always", "RestartSec=10", "StartLimitInterval=300", // [Service]（老 systemd）
		"# project=shop app=always target=t1 autostart=no",
		"WantedBy=multi-user.target", // 留着：以后改回开机自启时 enable 才有用
	}, nil)
	if !no || strings.Count(text, "StartLimitBurst=3") != 2 {
		t.Errorf("always：NoAutostart=%v\n%s", no, text)
	}
	// [Unit] 里的要在 [Service] 之前
	if u, s := strings.Index(text, "StartLimitIntervalSec"), strings.Index(text, "[Service]"); u < 0 || u > s {
		t.Errorf("StartLimitIntervalSec 要在 [Unit] 段：\n%s", text)
	}

	text, _ = unit("fast")
	check("fast", text, []string{"RestartSec=500ms", "StartLimitIntervalSec=0", "StartLimitInterval=0"}, nil)

	text, _ = unit("never")
	check("never", text, []string{"Restart=no"}, []string{"RestartSec", "StartLimit"})

	// 开机后等 2 分钟再启动：交给服务器上的定时器，单元文件里记下（服务器告警用）
	s, err := Spec(f, tg, f.App("late"))
	if err != nil || s.BootDelaySec != 120 || s.NoAutostart {
		t.Fatalf("late：%+v %v", s.BootDelaySec, err)
	}
	check("late", s.UnitText, []string{"# project=shop app=late target=t1 bootDelay=120", "WantedBy=multi-user.target"}, nil)
	if s, _ := Spec(f, tg, f.App("plain")); s.BootDelaySec != 0 {
		t.Error("plain 不延迟")
	}
}
