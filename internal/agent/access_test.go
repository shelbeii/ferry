package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestSdSplit(t *testing.T) {
	got := sdSplit(`/usr/bin/bash "/a b/x.sh" 'q "1"' -Dk=\"v\" plain`)
	want := []string{"/usr/bin/bash", "/a b/x.sh", `q "1"`, `-Dk="v"`, "plain"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestArgPaths(t *testing.T) {
	for arg, want := range map[string][]string{
		"/opt/x/a.jar":                  {"/opt/x/a.jar"},
		"-Djava.io.tmpdir=/data/tmp/":   {"/data/tmp"},
		"--spring.profiles.active=prod": nil,
		"-cp":                           nil,
		"/a.jar:/b.jar":                 {"/a.jar", "/b.jar"},
		"/a.jar:lib/b.jar":              nil,
		"tomcat*.9201.*":                nil,
		"$HOME/x":                       nil,
		"/var/log/%n":                   nil,
		"/var/100%%":                    {"/var/100%"},
		"{}":                            nil,
	} {
		if got := argPaths(arg); !reflect.DeepEqual(got, want) {
			t.Errorf("%q：%q，期望 %q", arg, got, want)
		}
	}
}

func TestUnitAccessNeeds(t *testing.T) {
	unit := ManagedMarker + `
[Service]
User=svc
WorkingDirectory=/srv/work
ExecStartPre=-/usr/bin/find /data/tmp -name tomcat*.1.* -exec rm -rf {} +
ExecStartPre=+/usr/bin/chown svc /data
ExecStart=/usr/bin/bash /app/with-env.sh /jdk/bin/java -Djava.io.tmpdir=/data/tmp -jar /opt/p/apps/A/current/a.jar
ProtectHome=read-only
[Install]
ExecStart=/not/service/section
`
	needs, ph := unitAccessNeeds(unit)
	if ph != "read-only" {
		t.Errorf("ProtectHome：%q", ph)
	}
	var got []string
	for _, n := range needs {
		got = append(got, n.where+" "+n.path+" "+n.mode+" "+n.reason+map[bool]string{true: " soft", false: ""}[n.soft])
	}
	want := []string{
		"WorkingDirectory /srv/work x dir",
		"ExecStartPre /usr/bin/find x program soft",
		"ExecStartPre /data/tmp r arg soft",
		"ExecStart /usr/bin/bash x program",
		"ExecStart /app/with-env.sh r arg",
		"ExecStart /jdk/bin/java r arg",
		"ExecStart /data/tmp rw arg",
		"ExecStart /opt/p/apps/A/current/a.jar r arg",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n%s", strings.Join(got, "\n"))
	}
}

// 172 上遇到的情况：包装脚本所在目录运行用户进不去 → 预检就报出来，不用等到启动失败。
func TestServiceAccessProblems(t *testing.T) {
	f := newFake(t)
	base := t.TempDir()
	// 服务器路径以 / 开头：Windows 上切到临时目录所在的盘，去掉盘符后同样能访问
	t.Chdir(base)
	mk := func(rel string, data string, mode os.FileMode) string {
		p := filepath.Join(base, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if data == "" {
			os.MkdirAll(p, 0o755)
		} else {
			os.WriteFile(p, []byte(data), mode)
		}
		return filepath.ToSlash(strings.TrimPrefix(p, filepath.VolumeName(p)))
	}
	base = filepath.ToSlash(strings.TrimPrefix(base, filepath.VolumeName(base)))
	script := mk("app/scripts/with-env.sh", "#!/bin/sh\nexec \"$@\"\n", 0o755)
	tmp := mk("app/data/tmp", "", 0)
	java := mk("jdk/bin/java", "ELF", 0o755)
	bash := mk("usr/bin/bash", "ELF", 0o755)
	find := mk("usr/bin/find", "ELF", 0o755)
	scripts := filepath.ToSlash(filepath.Dir(filepath.FromSlash(script)))

	s := javaApp("System")
	s.UnitText = ManagedMarker + "\n[Service]\nUser=svc\n" +
		"ExecStartPre=-" + find + " " + tmp + " -name x -exec rm -rf {} +\n" +
		"ExecStart=" + bash + " " + script + " " + java + " -Djava.io.tmpdir=" + tmp + " -jar /nonexistent/current/System.jar\n"

	run := func() *proto.Preflight {
		a := &Agent{Sys: f, req: &proto.Request{Root: f.root}}
		p := &proto.Preflight{}
		a.checkServiceAccess(&s, p, map[string]error{})
		return p
	}

	if p := run(); len(p.Problems)+len(p.Warnings) != 0 {
		t.Fatalf("都能访问时不应报告：%+v", p)
	}

	has := func(t *testing.T, text string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("缺少「%s」：\n%s", w, text)
			}
		}
	}
	dir := os.ModeDir

	// 上级目录进不去（其他用户没有任何权限）
	f.denied = map[string]bool{scripts: true}
	f.owners = map[string]Ownership{scripts: {UID: 0, GID: 0, User: "root", Group: "root", Perm: dir | 0o700}}
	p := run()
	if len(p.Problems) != 1 {
		t.Fatalf("problems：%+v", p.Problems)
	}
	has(t, p.Problems[0], "System 以 svc 用户运行，启动命令要读取 "+script, "但进不去上级目录 "+scripts,
		"这个目录属于 root（组 root），svc 既不是属主也不在这个组里，其他用户的权限是「无」",
		"\n修复：chown svc: "+scripts, "chmod o+x "+scripts)

	// 172 上的情况：临时目录属于 root、组是运行用户的组、755 → 能进不能写
	f.denied = map[string]bool{tmp + "|w": true}
	f.owners = map[string]Ownership{tmp: {UID: 0, GID: 1001, User: "root", Group: "app", Perm: dir | 0o755}}
	p = run()
	if len(p.Problems) != 1 || len(p.Warnings) != 0 {
		t.Fatalf("problems：%+v warnings：%+v", p.Problems, p.Warnings)
	}
	has(t, p.Problems[0], "要往 "+tmp+" 写临时文件（启动参数 -Djava.io.tmpdir），但没有写入的权限",
		"这个目录属于 root（组 app），svc 在这个组里，组的权限是「列出、进入」", "\n修复：chown svc: "+tmp+"（把它交给运行用户）")

	// 带 - 的启动前命令：只提示
	f.denied = map[string]bool{tmp + "|r": true}
	f.owners = map[string]Ownership{tmp: {UID: 0, GID: 0, User: "root", Group: "root", Perm: dir | 0o700}}
	p = run()
	if len(p.Warnings) != 1 || len(p.Problems) != 1 {
		t.Fatalf("problems：%+v warnings：%+v", p.Problems, p.Warnings)
	}
	has(t, p.Warnings[0], "启动前命令要访问目录 "+tmp, "没有列出的权限", "（这条启动前命令允许失败，不影响启动，但建议修复）")

	// 权限位允许但仍然访问不了：ACL / 安全策略
	f.denied = map[string]bool{script + "|r": true}
	f.owners = map[string]Ownership{script: {UID: 0, GID: 0, User: "root", Group: "root", Perm: 0o755}}
	p = run()
	if len(p.Problems) != 1 {
		t.Fatalf("problems：%+v", p.Problems)
	}
	has(t, p.Problems[0], "没有读的权限", "其他用户的权限是「读、执行」", "按权限位是允许的", "KYSEC", "getfacl "+script)

	// 属于运行用户自己但去掉了权限
	f.denied = map[string]bool{script + "|r": true}
	f.owners = map[string]Ownership{script: {UID: 1000, GID: 1000, User: "svc", Group: "svc", Perm: 0o300}}
	has(t, run().Problems[0], "这个文件属于 svc 自己，属主的权限是「写、执行」", "修复：chmod u+r "+script)

	if runtime.GOOS != "windows" { // Windows 上文件没有可执行位
		f.denied = map[string]bool{java + "|x": true}
		f.owners = nil
		if p := run(); len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "启动命令要执行 "+java) {
			t.Fatalf("交给脚本执行的 java 也要能执行：%+v", p.Problems)
		}
	}

	f.denied = nil
	s.UnitText = ManagedMarker + "\n[Service]\nExecStart=" + base + "/jdk-missing/bin/java -jar x.jar\n"
	if p := run(); len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "但服务器上没有这个文件") || !strings.Contains(p.Problems[0], "修复：检查配置里的路径") {
		t.Fatalf("程序不存在：%+v", p.Problems)
	}

	// 162 上的情况：包装脚本在新服务器上没有，bash 找不到脚本（status=127），以前要等构建、上传完才发现
	missing := base + "/home/shop/app/scripts/with-env.sh"
	s.UnitText = ManagedMarker + "\n[Service]\nExecStart=" + bash + " " + missing + " " + java + " -jar x.jar\n"
	p = run()
	if len(p.Problems) != 1 {
		t.Fatalf("包装脚本不存在：%+v", p.Problems)
	}
	has(t, p.Problems[0], "要用 "+bash+" 执行脚本 "+missing+"，但服务器上没有这个文件（启动会以 status=127 退出）", "artifact.files")

	// 脚本在，交给它执行的 java 不在
	s.UnitText = ManagedMarker + "\n[Service]\nExecStart=" + bash + " " + script + " " + base + "/jdk-missing/bin/java -Xmx1g -jar x.jar\n"
	p = run()
	if len(p.Problems) != 1 {
		t.Fatalf("java 不存在：%+v", p.Problems)
	}
	has(t, p.Problems[0], "要通过 "+script+" 执行 "+base+"/jdk-missing/bin/java", "status=127")

	// bash -c 后面是命令，不是脚本；参数里的普通路径可能由程序自己创建，不要求存在
	s.UnitText = ManagedMarker + "\n[Service]\nExecStart=" + bash + " -c \"exec " + java + "\"\n" +
		"ExecStartPost=" + find + " " + base + "/created/later -delete\n"
	if p := run(); len(p.Problems) != 0 {
		t.Fatalf("不应报告：%+v", p.Problems)
	}
}

// EnvironmentFile 写的文件不存在：systemd 不启动服务，预检拦下；加 - 的、跟着版本发布的不管
func TestEnvFilesMustExist(t *testing.T) {
	f := newFake(t)
	have := filepath.ToSlash(filepath.Join(t.TempDir(), "app.env"))
	os.WriteFile(have, []byte("A=1\n"), 0o600)
	have = strings.TrimPrefix(have, filepath.VolumeName(have))
	t.Chdir(filepath.VolumeName(t.TempDir()) + "/")
	a := &Agent{Sys: f, req: &proto.Request{Root: "/opt/ferry-test-root"}}
	s := javaApp("System")
	s.UnitText = ManagedMarker + "\n[Service]\nEnvironmentFile=/home/shop/secrets/shop.env\n" +
		"EnvironmentFile=-/maybe/missing.env\nEnvironmentFile=" + have + "\n" +
		"EnvironmentFile=" + filepath.ToSlash(a.appDir("System")) + "/running/app.env\nExecStart=/usr/bin/java -jar x.jar\n"
	p := &proto.Preflight{}
	a.checkEnvFiles(&s, p)
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "环境变量文件 /home/shop/secrets/shop.env 在服务器上不存在") {
		t.Fatalf("problems：%+v", p.Problems)
	}
}

// 旧 systemd（CentOS 7 是 219）不认识的设置：只提醒会被忽略；ReadWritePaths 里不存在的目录也就不会导致启动失败
func TestOldSystemdDirectives(t *testing.T) {
	f := newFake(t)
	s := javaApp("System")
	s.UnitText = ManagedMarker + "\n[Service]\nExecStart=/usr/bin/java -jar x.jar\nProtectSystem=full\nNoNewPrivileges=true\n" +
		"ReadWritePaths=/opt/shop-missing\nProtectKernelTunables=true\n"
	a := &Agent{Sys: f, req: &proto.Request{Root: "/opt/ferry-test-root"}}
	p := &proto.Preflight{Systemd: 219}
	a.checkUnitSupport(&s, p)
	a.checkSandboxPaths(&s, p)
	if len(p.Problems) != 0 || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "ReadWritePaths、ProtectKernelTunables") ||
		!strings.Contains(p.Warnings[0], "systemd 219") {
		t.Fatalf("problems：%+v warnings：%+v", p.Problems, p.Warnings)
	}
	p = &proto.Preflight{Systemd: 252}
	a.checkUnitSupport(&s, p)
	a.checkSandboxPaths(&s, p)
	if len(p.Warnings) != 0 || len(p.Problems) != 1 {
		t.Fatalf("新 systemd：problems：%+v warnings：%+v", p.Problems, p.Warnings)
	}
}

// 启动失败的退出码：说明是什么意思，指出缺的文件
func TestExitHints(t *testing.T) {
	unit := ManagedMarker + "\n[Service]\nExecStart=/usr/bin/bash /home/shop/app/scripts/shop-with-env.sh /home/shop/runtime/jdk-25/bin/java -jar x.jar\n"
	got := strings.Join(exitHints([]string{"shop-system.service: main process exited, code=exited, status=127/n/a"}, unit), "\n")
	for _, w := range []string{"status=127：找不到要执行的命令", "服务器上没有 /home/shop/app/scripts/shop-with-env.sh", "服务器上没有 /home/shop/runtime/jdk-25/bin/java"} {
		if !strings.Contains(got, w) {
			t.Errorf("缺少「%s」：\n%s", w, got)
		}
	}
	if h := exitHints([]string{"Started x."}, unit); len(h) != 0 {
		t.Fatalf("没有退出码时不应说明：%v", h)
	}
}

// ReadWritePaths 里不存在的目录：systemd 会拒绝启动（226/NAMESPACE），预检拦下；加 - 的、部署目录下的不管
func TestSandboxPathsMustExist(t *testing.T) {
	f := newFake(t)
	s := javaApp("System")
	s.UnitText = ManagedMarker + "\n[Service]\nExecStart=/usr/bin/java -jar x.jar\nProtectSystem=full\n" +
		"ReadWritePaths=/opt/shop-missing/apps / -/maybe/missing\nReadOnlyPaths=+/opt/also-missing\n" +
		"ReadWritePaths=/opt/ferry-test-root/apps\n"
	if got := unitSandboxPaths(s.UnitText); len(got) != 4 {
		t.Fatalf("应找出 4 个必须存在的路径：%+v", got)
	}
	p := &proto.Preflight{}
	a := &Agent{Sys: f, req: &proto.Request{Root: "/opt/ferry-test-root"}}
	a.checkSandboxPaths(&s, p)
	all := strings.Join(p.Problems, "\n")
	if !strings.Contains(all, "/opt/shop-missing/apps") || !strings.Contains(all, "/opt/also-missing") || !strings.Contains(all, "226/NAMESPACE") {
		t.Fatalf("不存在的目录应报出来：%v", p.Problems)
	}
	if len(p.Problems) != 2 || strings.Contains(all, "maybe") || strings.Contains(all, "ferry-test-root") {
		t.Fatalf("存在的、加 - 的、部署目录下的不应报：%v", p.Problems)
	}
}
