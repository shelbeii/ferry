package engine

import (
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

// 换新服务器时预检报的路径：说清楚来自哪个变量、在哪里给这台服务器单独设置（162 上的情况）。
func TestVarHints(t *testing.T) {
	f := &config.File{
		Project: "gz",
		Vars: map[string]string{
			"base":    "/home/shop",
			"withEnv": "${base}/app/scripts/with-env.sh",
			"jdk":     "/home/appuser/jdk-25",
			"profile": "prod",
		},
		Targets: map[string]*config.Target{
			"t162": {Name: "t162", Root: "/opt/x", Vars: map[string]string{"secrets": "/home/appuser/secrets/gz.env"}, LocalVars: []string{"secrets"}},
			"t41":  {Name: "t41", Root: "/home/y"},
		},
		LocalVars: []string{"jdk"},
	}
	tg, app := f.Targets["t162"], &config.App{Name: "System", Vars: map[string]string{"logDir": "/data/logs"}}
	items := []*item{{app: app}}
	got := withVarHints(f, tg, items, []string{
		"System 的启动命令要用 /usr/bin/bash 执行脚本 /home/shop/app/scripts/with-env.sh，但服务器上没有这个文件",
		"System 的启动命令要通过 /x.sh 执行 /home/appuser/jdk-25/bin/java，但服务器上没有这个文件",
		"System 的环境变量文件 /home/appuser/secrets/gz.env 在服务器上不存在",
		"System 运行时要往 /data/logs/a 写入",
		"Other 的启动命令要执行 /home/shop/x",
		"System 的工作目录 /srv/w 不存在",
	})
	want := [][]string{
		{"来自变量 ${withEnv}（/home/shop/app/scripts/with-env.sh，ferry.yaml 里通用的值）", "在顶部选中 t162，打开「本机设置」，在变量里把 withEnv 改成", "其他服务器不受影响"},
		{"来自变量 ${jdk}", "本机设置的「所有服务器共用」里", "改成只对 t162"},
		{"来自本机设置里 t162 的变量 ${secrets}"},
		{"来自应用 System 的变量 ${logDir}"},
	}
	for i, w := range want {
		for _, s := range w {
			if !strings.Contains(got[i], s) {
				t.Errorf("第 %d 条缺少「%s」：\n%s", i+1, s, got[i])
			}
		}
	}
	for _, i := range []int{4, 5} {
		if strings.Contains(got[i], "\n") {
			t.Errorf("第 %d 条不应补说明：\n%s", i+1, got[i])
		}
	}

	// 本机设置改了服务器路径：部署时提醒对别的服务器也生效
	if n := localPathVarsNote(f, []string{"t162"}); !strings.Contains(n, "改了变量 jdk") || !strings.Contains(n, "对 t41 也生效") {
		t.Fatalf("提醒：%q", n)
	}
	f.Targets["t41"].Vars = map[string]string{"jdk": "/usr/lib/jvm/jdk-25"}
	if n := localPathVarsNote(f, []string{"t162"}); n != "" {
		t.Fatalf("别的服务器自己写了这个变量，不应提醒：%q", n)
	}
}
