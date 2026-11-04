package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type runtimeScanFixture struct {
	*fakeSys
	listing string
}

func (s *runtimeScanFixture) Shell(cmd string, d time.Duration) (string, error) {
	if strings.HasPrefix(cmd, "for p in ") {
		return s.listing, nil
	}
	return s.fakeSys.Shell(cmd, d)
}

func TestRuntimeChoicesDeduplicateAndCheckUserAccess(t *testing.T) {
	f := newFake(t)
	app := javaApp("api")
	f.unit(app.Unit).pid = 42
	f.processes = map[int][]ProcessSnapshot{42: {{PID: 42, Exe: "/opt/jdk17/bin/java"}}}
	f.denied = map[string]bool{"/root/jdk21/bin/java": true}
	f.shellOut = map[string]string{"runuser -u 'svc' -- '/opt/jdk17/bin/java' -version 2>&1": "openjdk version 17.0.12"}
	a := &Agent{Sys: &runtimeScanFixture{fakeSys: f, listing: "/usr/bin/java\t/opt/jdk17/bin/java\n/link/java\t/opt/jdk17/bin/java\n/root/jdk21/bin/java\t/root/jdk21/bin/java\n"}, req: &proto.Request{Root: f.root}}
	result := execConfigured(t, a, "appruntimes", &proto.Request{Apps: []proto.AppSpec{app}})
	if result.code != 0 {
		t.Fatal(result.dump())
	}
	var data proto.AppRuntimeChoices
	if err := json.Unmarshal(result.result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !data.CanSwitch || len(data.Choices) != 2 || !data.Choices[0].Current || data.Choices[0].Version == "" || data.Choices[1].Disabled == "" {
		t.Fatalf("候选去重、当前标记或权限检查错误：%+v", data)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "/root/jdk21/bin/java") {
			t.Fatal("不能执行运行用户无权使用的解释器：", call)
		}
	}
}

func TestNativeRuntimeCannotPretendToSwitchInterpreter(t *testing.T) {
	f := newFake(t)
	s := javaApp("api")
	s.Type = proto.TypeBinary
	result := exec1(t, f, "appruntimes", &proto.Request{Apps: []proto.AppSpec{s}})
	var data proto.AppRuntimeChoices
	json.Unmarshal(result.result.Data, &data)
	if result.code != 0 || data.CanSwitch || data.Reason == "" || len(data.Choices) != 0 {
		t.Fatalf("原生程序不应伪造解释器候选：%+v", data)
	}
}

func TestStoppedRuntimeChoicesUseDeployedUnit(t *testing.T) {
	f := newFake(t)
	s := javaApp("api")
	a := &Agent{Sys: &runtimeScanFixture{fakeSys: f}, req: &proto.Request{Root: f.root}}
	if err := a.writeRunning(s.Name, id1); err != nil {
		t.Fatal(err)
	}
	file := a.verFile(s.Name, id1, "unit.service")
	os.MkdirAll(filepath.Dir(file), 0755)
	os.WriteFile(file, []byte("[Service]\nExecStart=/old/java17/bin/java -jar api.jar\n"), 0644)
	s.UnitText = ManagedMarker + "\n[Service]\nExecStart=/pending/java21/bin/java -jar api.jar\n"
	result := execConfigured(t, a, "appruntimes", &proto.Request{Apps: []proto.AppSpec{s}})
	var data proto.AppRuntimeChoices
	json.Unmarshal(result.result.Data, &data)
	if result.code != 0 || data.Current != "/old/java17/bin/java" {
		t.Fatal("停止时不应把待部署的环境标成当前：", result.dump())
	}
}
