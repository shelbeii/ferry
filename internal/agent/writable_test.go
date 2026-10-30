package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritableKey(t *testing.T) {
	for k, want := range map[string]bool{
		"JM.LOG.PATH": true, "JM.SNAPSHOT.PATH": true, "logging.file.path": true, "logging.file.name": true,
		"csp.sentinel.log.dir": true, "LOG_HOME": true, "LOG_PATH": true, "upload.dir": true,
		"logging.config": false, "log4j.configurationFile": false, "spring.config.location": false,
		"file.encoding": false, "csp.sentinel.dashboard.server": false, "user.home": false, "": false,
	} {
		if writableKey(k) != want {
			t.Errorf("%s：应为 %v", k, want)
		}
	}
	if !fileKey("logging.file.name") || fileKey("logging.file.path") || fileKey("JM.LOG.PATH") {
		t.Error("fileKey")
	}
}

func TestUnitWritableNeeds(t *testing.T) {
	unit := "[Service]\nEnvironment=\"LOG_HOME=/data/logs/app\" \"SPRING_PROFILES_ACTIVE=prod\"\n" +
		"ExecStart=/usr/bin/java -DJM.LOG.PATH=/home/app/logs/nacos -Dlogging.file.name=/var/log/app/app.log -Dlogging.config=/etc/app/logback.xml -jar /opt/a.jar\n"
	needs, _ := unitAccessNeeds(unit)
	got := map[string]accessNeed{}
	for _, n := range needs {
		got[n.path] = n
	}
	for p, prop := range map[string]string{"/home/app/logs/nacos": "-DJM.LOG.PATH", "/var/log/app": "-Dlogging.file.name", "/data/logs/app": "环境变量 LOG_HOME"} {
		if n, ok := got[p]; !ok || !n.writable || n.mode != "rw" || n.prop != prop {
			t.Errorf("%s：%+v", p, n)
		}
	}
	if n := got["/etc/app/logback.xml"]; n.writable || n.mode != "r" {
		t.Errorf("配置文件只要能读：%+v", n)
	}
}

// 172 上遇到的：-DJM.LOG.PATH 指向的目录本身可写，但里面以前用 root 运行时留下的日志文件写不了。
func TestWritableTreeFix(t *testing.T) {
	f, s, _, _, _ := permFixture(t)
	base, _ := os.Getwd()
	root := filepath.ToSlash(strings.TrimPrefix(base, filepath.VolumeName(base)))
	logs := root + "/app/logs/nacos-client"
	os.MkdirAll(filepath.FromSlash(logs+"/nacos"), 0o755)
	os.WriteFile(filepath.FromSlash(logs+"/nacos/ai.log"), []byte("x"), 0o644)
	os.WriteFile(filepath.FromSlash(logs+"/nacos/config.log"), []byte("x"), 0o644)
	s.UnitText = strings.Replace(s.UnitText, " -Djava.io.tmpdir=", " -DJM.LOG.PATH="+logs+" -Djava.io.tmpdir=", 1)
	dir := os.ModeDir
	f.owners = map[string]Ownership{
		logs + "/nacos":            {UID: 1000, GID: 1000, User: "svc", Group: "svc", Perm: dir | 0o755},
		logs + "/nacos/ai.log":     {UID: 0, GID: 0, User: "root", Group: "root", Perm: 0o644},
		logs + "/nacos/config.log": {UID: 0, GID: 0, User: "root", Group: "root", Perm: 0o644},
	}
	f.denied = map[string]bool{logs + "/nacos/ai.log|w": true, logs + "/nacos/config.log|w": true}

	// 不自动修正：报问题，说清楚是哪些文件、属于谁
	p := runAccess(f, &s, "")
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "-DJM.LOG.PATH") || !strings.Contains(p.Problems[0], "属于 root 的 2 个") ||
		!strings.Contains(p.Problems[0], "chown -R svc: "+logs) {
		t.Fatalf("%v", p.Problems)
	}
	// plan：列出将要做的修改，不动文件
	p = runAccess(f, &s, FixPlan)
	if len(p.Problems) != 0 || len(p.Fixable) != 1 || len(f.calls) != 0 {
		t.Fatalf("plan：%v %v %v", p.Problems, p.Fixable, f.calls)
	}
	// apply：只改写不了的两个文件，改完复查通过
	p = runAccess(f, &s, FixApply)
	if len(p.Problems) != 0 || len(p.Fixed) != 1 || len(f.calls) != 2 || !strings.Contains(f.calls[0], "chown 1000:1000 "+logs+"/nacos/") {
		t.Fatalf("apply：%v %v %v", p.Problems, p.Fixed, f.calls)
	}

	// 属于别的用户（可能是别的服务在用）：不自动改，只报问题
	f.calls = nil
	f.owners[logs+"/nacos/ai.log"] = Ownership{UID: 1005, GID: 1005, User: "other", Group: "other", Perm: 0o644}
	f.denied = map[string]bool{logs + "/nacos/ai.log|w": true}
	if p := runAccess(f, &s, FixApply); len(p.Problems) != 1 || len(f.calls) != 0 || !strings.Contains(p.Problems[0], "属于 other 的 1 个") {
		t.Fatalf("%v %v", p.Problems, f.calls)
	}
}

// 要写入的目录还不存在：检查最近的已有上级能不能写（程序启动时自己创建）。
func TestWritableDirNotYetCreated(t *testing.T) {
	f, s, _, _, _ := permFixture(t)
	base, _ := os.Getwd()
	root := filepath.ToSlash(strings.TrimPrefix(base, filepath.VolumeName(base)))
	parent := root + "/app/logs"
	os.MkdirAll(filepath.FromSlash(parent), 0o755)
	s.UnitText = strings.Replace(s.UnitText, " -Djava.io.tmpdir=", " -DJM.LOG.PATH="+parent+"/nacos-client -Djava.io.tmpdir=", 1)
	f.denied = map[string]bool{parent + "|w": true}
	f.owners = map[string]Ownership{parent: {UID: 0, GID: 0, User: "root", Group: "root", Perm: os.ModeDir | 0o755}}
	p := runAccess(f, &s, "")
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "要创建 "+parent+"/nacos-client") {
		t.Fatalf("%v", p.Problems)
	}
	if p := runAccess(f, &s, FixApply); len(p.Problems) != 0 || len(p.Fixed) != 1 {
		t.Fatalf("%v %v", p.Problems, p.Fixed)
	}
}
