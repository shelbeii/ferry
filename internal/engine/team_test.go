package engine

import (
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/settings"
)

func TestActorAndDeployers(t *testing.T) {
	env := map[string]string{}
	e := &Engine{Getenv: func(k string) string { return env[k] }}
	if strings.Contains(e.actor(), "（") {
		t.Fatalf("没填名字时只有 用户@电脑名：%s", e.actor())
	}
	e.Settings = settings.Settings{Team: settings.Team{Name: "张三"}}
	if a := e.actor(); !strings.HasPrefix(a, "张三（") || !strings.Contains(a, "@") {
		t.Fatalf("填了名字：%s", a)
	}
	tg := &config.Target{Name: "prod", Deployers: []string{"李四", "张三"}}
	if err := e.checkDeployer(tg, "部署"); err != nil {
		t.Fatalf("名单里有张三：%v", err)
	}
	e.Settings.Team.Name = "王五"
	err := e.checkDeployer(tg, "部署")
	if err == nil || !strings.Contains(err.Error(), "prod 只允许 李四、张三 部署") || !strings.Contains(err.Error(), "你是 王五") {
		t.Fatalf("不在名单里：%v", err)
	}
	// CI：GitHub Actions 触发的人；FERRY_ACTOR 优先
	e.Settings.Team.Name = ""
	env["GITHUB_ACTOR"] = "zhangsan"
	tg.Deployers = []string{"github:zhangsan"}
	if err := e.checkDeployer(tg, "部署"); err != nil {
		t.Fatalf("CI 里按 GitHub 用户：%v", err)
	}
	env["FERRY_ACTOR"] = "发布机器人"
	if !strings.HasPrefix(e.actor(), "发布机器人（") {
		t.Fatalf("FERRY_ACTOR：%s", e.actor())
	}
	// 没写名单：谁都可以
	if err := e.checkDeployer(&config.Target{Name: "test"}, "部署"); err != nil {
		t.Fatal(err)
	}
}
