package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func TestHookBeforeSwitchFailureKeepsOldVersion(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})); r.code != 0 {
		t.Fatalf("首次部署：\n%s", r.dump())
	}
	pid := f.units[sys.Unit].pid
	sys.Hooks = &proto.Hooks{BeforeSwitch: []proto.Hook{{Run: "./migrate.sh", User: "svc", TimeoutSec: 60}}}
	f.hookFail = map[string]bool{"./migrate.sh": true}
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}))
	if r.code != proto.CodeFailed || !r.has(proto.EvFail, "切换前的命令失败：退出码 1；没有切换，线上还是 "+id1) {
		t.Fatalf("切换前的命令失败应中止：\n%s", r.dump())
	}
	if !r.has(proto.EvHook, "ERROR: migration failed") {
		t.Fatalf("应显示命令的输出：\n%s", r.dump())
	}
	if f.current("System") != id1 || f.units[sys.Unit].pid != pid {
		t.Fatal("旧版本不应停止或切换")
	}
	h := f.hooks[0]
	if h.Dir != filepath.Join(f.root, "apps", "System", "versions", id2) || h.UID != 1000 || h.Timeout.Seconds() != 60 {
		t.Fatalf("执行方式：%+v", h)
	}
	for _, want := range []string{"FERRY_RELEASE=" + id2, "FERRY_PREVIOUS=" + id1, "FERRY_PHASE=beforeSwitch", "FERRY_APP=System", "USER=svc"} {
		if !slices.Contains(h.Env, want) {
			t.Errorf("环境变量缺 %s：%v", want, h.Env)
		}
	}
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root}}
	if !a.isFailed("System", id2) {
		t.Fatal("失败的版本应标记为失败")
	}
	var out proto.DeployOutcome
	json.Unmarshal(r.result.Data, &out)
	if out.Apps[0].Result != "FAILED" {
		t.Fatalf("结果：%+v", out)
	}
}

func TestHookAfterSwitchFailureRollsBack(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})); r.code != 0 {
		t.Fatalf("首次部署：\n%s", r.dump())
	}
	sys.Hooks = &proto.Hooks{AfterSwitch: []proto.Hook{
		{Run: "notify.sh", User: "root", Optional: true},
		{Run: "warmup.sh", User: "svc"},
	}}
	f.hookFail = map[string]bool{"notify.sh": true, "warmup.sh": true}
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}))
	if !r.has(proto.EvHook, "切换后的命令第 1 条失败（设了 optional，接着部署）") {
		t.Fatalf("optional 的失败只提示：\n%s", r.dump())
	}
	if r.code != proto.CodeFailed || !r.has(proto.EvRollback, "已回滚到 "+id1) || f.current("System") != id1 {
		t.Fatalf("切换后的命令失败应回滚：\n%s", r.dump())
	}
	if len(f.hooks) != 2 || f.hooks[0].UID != 0 || f.hooks[1].Dir != filepath.Join(f.root, "apps", "System", "running") {
		t.Fatalf("执行方式：%+v", f.hooks)
	}
}

func TestHookOKAndEnvFile(t *testing.T) {
	f := newFake(t)
	env := filepath.Join(t.TempDir(), "api.env")
	os.WriteFile(env, []byte("# 注释\nDB_URL=jdbc:mysql://db/app\nexport DB_PASS=\"p a\\\"ss\"\nNAME='x y'\n"), 0o600)
	sys := javaApp("System")
	sys.RuntimeJava = 17
	sys.Hooks = &proto.Hooks{BeforeSwitch: []proto.Hook{{Run: "java -jar flyway.jar migrate", User: "svc"}}}
	sys.HookEnv = map[string]string{"SPRING_PROFILES_ACTIVE": "prod"}
	sys.HookEnvFiles = []string{env, "-/nonexistent/x.env"}
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")}))
	if r.code != 0 || !r.has(proto.EvHook, "切换前的命令完成") || f.current("System") != id1 {
		t.Fatalf("\n%s", r.dump())
	}
	e := f.hooks[0].Env
	for _, want := range []string{"DB_URL=jdbc:mysql://db/app", `DB_PASS=p a"ss`, "NAME=x y", "SPRING_PROFILES_ACTIVE=prod", "FERRY_PREVIOUS="} {
		if !slices.Contains(e, want) {
			t.Errorf("环境变量缺 %s：%v", want, e)
		}
	}
	var path string
	for _, kv := range e {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	if i := strings.Index(path, "java-17"); i < 0 || i > strings.Index(path, "/usr/bin") {
		t.Errorf("PATH 应先找 Ferry 提供的 JDK：%s", path)
	}

	// 必需的 envFile 不存在：不执行，部署失败
	sys.HookEnvFiles = []string{"/nonexistent/x.env"}
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}))
	if r.code != proto.CodeFailed || !r.has(proto.EvFail, "读取 envFile") || len(f.hooks) != 1 {
		t.Fatalf("\n%s", r.dump())
	}
}

func TestHookStaticAfterSwitchFailureSwitchesBack(t *testing.T) {
	f := newFake(t)
	site := proto.AppSpec{Name: "web", Type: proto.TypeStatic, User: "svc", KeepReleases: 3}
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{site}, id1, map[string][]byte{"web": []byte("v1")})); r.code != 0 {
		t.Fatalf("\n%s", r.dump())
	}
	site.Hooks = &proto.Hooks{AfterSwitch: []proto.Hook{{Run: "purge-cdn.sh", User: "svc"}}}
	f.hookFail = map[string]bool{"purge-cdn.sh": true}
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{site}, id2, map[string][]byte{"web": []byte("v2")}))
	if r.code != proto.CodeFailed || !r.has(proto.EvRollback, "已切回 "+id1) || f.current("web") != id1 {
		t.Fatalf("\n%s", r.dump())
	}
}

func TestReadEnvFileFormats(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.env")
	os.WriteFile(p, []byte("A=1\n; c\n  B = two \nC=\"multi\\nline\"\nbad line\nD=$HOME\n"), 0o600)
	kv, err := readEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if kv["A"] != "1" || kv["B"] != "two" || kv["C"] != "multi\nline" || kv["D"] != "$HOME" || len(kv) != 4 {
		t.Fatalf("%q", kv)
	}
}

func TestHumanDur(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Minute: "5 分钟", 30 * time.Second: "30 秒", 90 * time.Minute: "1 小时 30 分钟", 0: "0 秒"} {
		if got := humanDur(d); got != want {
			t.Errorf("%v：%s", d, got)
		}
	}
}

func TestReadyTimeCompared(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")}))
	if !r.has(proto.EvReady, "这次从启动到就绪用了 1 秒") {
		t.Fatalf("第一次只说用了多久：\n%s", r.dump())
	}
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}))
	var out proto.DeployOutcome
	json.Unmarshal(r.result.Data, &out)
	if !r.has(proto.EvReady, "（上次 1 秒）") || out.Apps[0].ReadySec != 1 || out.Apps[0].PrevReadySec != 1 {
		t.Fatalf("第二次和上次比：%+v\n%s", out, r.dump())
	}
}
