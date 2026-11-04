package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

type appInfoResponse struct {
	out string
	err error
}

type appInfoCommandSys struct {
	*fakeSys
	responses map[string]appInfoResponse
}

func (f *appInfoCommandSys) Shell(command string, timeout time.Duration) (string, error) {
	if response, ok := f.responses[command]; ok {
		f.calls = append(f.calls, "sh "+command)
		return response.out, response.err
	}
	return f.fakeSys.Shell(command, timeout)
}

func infoValue(info proto.AppInfo, label string) string {
	for _, section := range info.Sections {
		for _, field := range section.Fields {
			if field.Label == label {
				return field.Value
			}
		}
	}
	return ""
}

func decodeAppInfo(t *testing.T, result run) proto.AppInfo {
	t.Helper()
	if result.code != 0 {
		t.Fatal(result.dump())
	}
	var info proto.AppInfo
	if err := json.Unmarshal(result.result.Data, &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func TestAppInfoUsesRunningJavaChildInsteadOfPendingConfig(t *testing.T) {
	f := newFake(t)
	s := javaApp("api")
	s.UnitText = ManagedMarker + "\n[Service]\nUser=svc\nExecStart=/pending/java-25/bin/java -jar api.jar\n"
	f.unit(s.Unit).active, f.unit(s.Unit).pid = "active", 40
	f.processes = map[int][]ProcessSnapshot{40: {
		{PID: 40, Exe: "/usr/bin/bash", Args: []string{"bash", "start.sh"}},
		{PID: 41, Parent: 40, Exe: "/installed/java-17/bin/java", User: "svc", UID: "1001", Cwd: "/opt/demo/apps/api/running", Args: []string{"/installed/java-17/bin/java", "-jar", "api.jar", "--db.password=hidden-value"}, Threads: 12, Memory: 128 * 1024 * 1024, Libraries: []string{"/installed/java-17/lib/server/libjvm.so"}},
	}}
	f.shellOut = map[string]string{
		appInfoUnitCommand(s.Unit):                         "ExecStart={ path=/loaded/java-21/bin/java ; argv[]=/loaded/java-21/bin/java -jar api.jar ; ignore_errors=no ; }\nFragmentPath=/etc/systemd/system/demo-api.service\nUser=svc\nRestart=always\nNRestarts=3\n",
		"runuser -u 'svc' -- '/proc/41/exe' -version 2>&1": "openjdk version 17.0.9\n",
	}
	info := decodeAppInfo(t, exec1(t, f, "appinfo", &proto.Request{Apps: []proto.AppSpec{s}}))
	if infoValue(info, "运行中的程序路径") != "/installed/java-17/bin/java" || infoValue(info, "服务配置的启动程序") != "/loaded/java-21/bin/java" {
		t.Fatalf("混淆了进程、已加载服务和草稿配置：%+v", info)
	}
	if !strings.Contains(infoValue(info, "运行环境版本"), "17.0.9") || infoValue(info, "运行进程 PID") != "41" || infoValue(info, "主进程 PID") != "40" {
		t.Fatalf("未识别运行中的 Java 子进程：%+v", info)
	}
	if strings.Contains(infoValue(info, "实际命令行"), "hidden-value") {
		t.Fatal("命令行密码未隐藏")
	}
	for _, call := range f.calls {
		if strings.Contains(call, "pending/java") || strings.HasPrefix(call, "restart ") || strings.HasPrefix(call, "start ") {
			t.Fatal("详情读取不应执行配置里的程序或启停服务：", call)
		}
	}
}

func TestAppInfoStoppedUsesDeployedUnitAndOwnRoot(t *testing.T) {
	f := newFake(t)
	s := javaApp("api")
	s.Root = filepath.Join(t.TempDir(), "custom-app-root")
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root}, appRoots: map[string]string{s.Name: s.Root}}
	if err := a.writeRunning(s.Name, id1); err != nil {
		t.Fatal(err)
	}
	file := a.verFile(s.Name, id1, "unit.service")
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("[Service]\nUser=olduser\nWorkingDirectory=/old/work\nExecStart=/old/jdk/bin/java -jar old.jar\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s.UnitText = ManagedMarker + "\n[Service]\nExecStart=/new/jdk/bin/java -jar new.jar\n"
	info := decodeAppInfo(t, exec1(t, f, "appinfo", &proto.Request{Apps: []proto.AppSpec{s}}))
	if infoValue(info, "服务配置的启动程序") != "/old/jdk/bin/java" || infoValue(info, "服务用户") != "olduser" {
		t.Fatal("停止时应显示已部署配置：", info)
	}
	if infoValue(info, "运行中的程序路径") != "" || infoValue(info, "应用目录") != filepath.Join(s.Root, "apps", s.Name) {
		t.Fatal("停止的服务不应有运行进程，应使用应用自己的目录：", info)
	}
}

func TestAppInfoHostedNginxUsesSavedBinding(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	if err := a.writeRunning(s.Name, id1); err != nil {
		t.Fatal(err)
	}
	file := a.nginxSitePath(&s, &in)
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("server { listen 9443; server_name old.example.test; root /opt/web/www; }"), 0644)
	binding, _ := json.Marshal(nginxSiteBinding{Instance: in.Name, File: file})
	os.WriteFile(a.siteBindingFile(s.Name), binding, 0644)
	f.unit(in.Unit).pid = 51
	f.processes = map[int][]ProcessSnapshot{51: {{PID: 51, Exe: "/opt/edge/sbin/nginx", User: "root", UID: "0", Args: []string{"nginx: master process /opt/edge/sbin/nginx -c /opt/edge/nginx.conf"}}}}
	f.shellOut = map[string]string{"'/proc/51/exe' -v 2>&1": "nginx version: nginx/1.26.3"}
	s.NginxInstance = "pending-other-instance"
	info := decodeAppInfo(t, execConfigured(t, a, "appinfo", &proto.Request{FerryDir: a.req.FerryDir, Project: a.req.Project, Apps: []proto.AppSpec{s}}))
	if infoValue(info, "Nginx 实例") != in.Name || infoValue(info, "本站点配置") != file || infoValue(info, "运行中的程序路径") != "/opt/edge/sbin/nginx" {
		t.Fatalf("没有使用已发布的站点绑定：%+v", info)
	}
	if !strings.Contains(infoValue(info, "站点监听"), "9443") || infoValue(info, "站点域名") != "old.example.test" {
		t.Fatal("没有读取实际站点配置：", info)
	}
}

func TestRuntimeProcessSelectionAndSafeVersionProbes(t *testing.T) {
	processes := []ProcessSnapshot{{PID: 1, Exe: "/bin/sh"}, {PID: 2, Exe: "/opt/python3.12"}, {PID: 3, Exe: "/opt/node"}, {PID: 4, Exe: "/opt/MyApp", Libraries: []string{"/usr/share/dotnet/shared/Microsoft.NETCore.App/8.0.12/libcoreclr.so"}}, {PID: 5, Exe: "/opt/api"}}
	for _, test := range []struct {
		kind, file string
		pid        int
	}{{proto.TypePython, "", 2}, {proto.TypeNode, "", 3}, {proto.TypeDotnet, "", 4}, {proto.TypeBinary, "api", 5}} {
		if p := runtimeProcess(test.kind, processes, test.file); p.PID != test.pid {
			t.Fatalf("%s 识别错进程：%+v", test.kind, p)
		}
	}
	for _, test := range []struct{ kind, exe string }{{proto.TypeBinary, "/opt/api"}, {proto.TypeDotnet, "/opt/MyApp"}, {proto.TypeDotnet, "/usr/bin/dotnet"}, {proto.TypePython, "/opt/server.py"}} {
		if flag := runtimeVersionFlag(test.kind, test.exe); flag != "" {
			t.Fatalf("不能执行应用自身来猜版本，或把 SDK 版本当成运行时：%+v %s", test, flag)
		}
	}
	if runtimeVersionFlag(proto.TypePython, "/opt/python3.12") != "--version" || runtimeVersionFlag(proto.TypeNode, "/opt/node") != "--version" {
		t.Fatal("解释器版本参数错误")
	}
}

func TestAppInfoAllTypesAndMissingDeployment(t *testing.T) {
	for _, kind := range []string{proto.TypeJava, proto.TypeBinary, proto.TypeStatic, proto.TypePython, proto.TypeNode, proto.TypeDotnet} {
		t.Run(kind, func(t *testing.T) {
			f := newFake(t)
			s := javaApp("app")
			s.Type = kind
			if kind == proto.TypeStatic {
				s.Unit, s.UnitText, s.FileName = "", "", ""
			}
			info := decodeAppInfo(t, exec1(t, f, "appinfo", &proto.Request{Apps: []proto.AppSpec{s}}))
			if info.Type != kind || len(info.Sections) < 2 || len(info.Notes) == 0 || infoValue(info, "运行中的程序路径") != "" {
				t.Fatalf("未部署类型 %s 的详情有误：%+v", kind, info)
			}
		})
	}
	f := newFake(t)
	if result := exec1(t, f, "appinfo", &proto.Request{}); result.code != proto.CodeBadInput {
		t.Fatal("必须指定单个应用")
	}
}

func TestProcessStatusAndRuntimeLibraries(t *testing.T) {
	var p ProcessSnapshot
	parseProcessStatus("Uid:\t1000\t1001\t1001\t1001\nPPid:\t30\nThreads:\t8\nVmRSS:\t4096 kB\n", &p)
	if p.UID != "1001" || p.Parent != 30 || p.Threads != 8 || p.Memory != 4096*1024 {
		t.Fatalf("进程状态解析错误：%+v", p)
	}
	libs := runtimeLibraries("a b c d /usr/lib/libc.so\na b c d /opt/jdk/lib/server/libjvm.so\na b c d /opt/jdk/lib/server/libjvm.so\na b c d /opt/dotnet/shared/Microsoft.NETCore.App/8.0.12/libcoreclr.so\n")
	if len(libs) != 2 || !strings.HasSuffix(libs[0], "libcoreclr.so") {
		t.Fatal("运行时动态库去重错误：", libs)
	}
}

func TestAppInfoDirectoryRuntimesReportActualVersions(t *testing.T) {
	for _, kind := range []string{proto.TypePython, proto.TypeNode, proto.TypeDotnet} {
		t.Run(kind, func(t *testing.T) {
			f := newFake(t)
			s := javaApp("api")
			s.Type = kind
			exe := map[string]string{proto.TypePython: "/opt/python/bin/python3.12", proto.TypeNode: "/opt/node/bin/node", proto.TypeDotnet: "/opt/app/MyApp"}[kind]
			f.unit(s.Unit).active, f.unit(s.Unit).pid = "active", 60
			f.processes = map[int][]ProcessSnapshot{60: {{PID: 60, Exe: exe, UID: "1001", User: "svc", Args: []string{exe, "main"}, Libraries: []string{"/usr/share/dotnet/shared/Microsoft.NETCore.App/8.0.12/libcoreclr.so"}}}}
			f.shellOut = map[string]string{}
			if kind != proto.TypeDotnet {
				f.shellOut["runuser -u 'svc' -- '/proc/60/exe' --version 2>&1"] = map[string]string{proto.TypePython: "Python 3.12.7", proto.TypeNode: "v22.9.0"}[kind]
			}
			info := decodeAppInfo(t, exec1(t, f, "appinfo", &proto.Request{Apps: []proto.AppSpec{s}}))
			if infoValue(info, "运行中的程序路径") != exe {
				t.Fatalf("%s 程序路径错误：%+v", kind, info)
			}
			if kind == proto.TypeDotnet {
				if infoValue(info, "实际 CoreCLR 版本") != "8.0.12" {
					t.Fatal("没有使用实际加载的 CoreCLR：", info)
				}
				for _, call := range f.calls {
					if strings.Contains(call, "/proc/60/exe") {
						t.Fatal("不应执行 .NET apphost 获取版本", call)
					}
				}
			} else if infoValue(info, "运行环境版本") == "" {
				t.Fatal("没有读取解释器版本：", info)
			}
		})
	}
}

func TestLoadedPropertyClearsOldSettings(t *testing.T) {
	props := map[string]string{"User": "", "WorkingDirectory": "", "EnvironmentFiles": ""}
	if loadedProperty(props, "User", "old-user", "root") != "root" || loadedProperty(props, "WorkingDirectory", "/old/path", "/") != "/" || loadedProperty(props, "EnvironmentFiles", "/old.env", "未设置") != "未设置" {
		t.Fatal("systemd 已清空的设置不能回退到旧部署记录")
	}
}

func TestAppInfoOldSystemdKeepsSupportedProperties(t *testing.T) {
	f := newFake(t)
	s := javaApp("api")
	s.UnitText = ManagedMarker + "\n[Service]\nUser=olduser\nEnvironmentFile=/old.env\nExecStart=/pending/java/bin/java -jar api.jar\n"
	a := &Agent{req: &proto.Request{Root: f.root}, Sys: &appInfoCommandSys{fakeSys: f, responses: map[string]appInfoResponse{
		appInfoUnitCommand(s.Unit): {
			out: "LoadState=loaded\nExecStart={ path=/loaded/java/bin/java ; argv[]=/loaded/java/bin/java -jar api.jar ; ignore_errors=no ; }\nFragmentPath=/etc/systemd/system/demo-api.service\nUser=svc\nGroup=\nWorkingDirectory=/srv/live\nEnvironmentFile=/home/svc/service.env (ignore_errors=no)\nRestart=on-failure\nNeedDaemonReload=no\n",
			err: errors.New("exit status 1"),
		},
	}}}
	info := decodeAppInfo(t, execConfigured(t, a, "appinfo", &proto.Request{Apps: []proto.AppSpec{s}}))
	if infoValue(info, "服务配置的启动程序") != "/loaded/java/bin/java" || infoValue(info, "服务用户") != "svc" || infoValue(info, "服务工作目录") != "/srv/live" {
		t.Fatalf("旧 systemd 的有效信息不能因可选属性缺失而丢弃：%+v", info)
	}
	if !strings.Contains(infoValue(info, "环境变量文件"), "/home/svc/service.env") || strings.Contains(infoValue(info, "环境变量文件"), "/old.env") || infoValue(info, "自动重启次数") != "" {
		t.Fatal("应兼容旧属性名称，不编造缺失的重启次数：", info)
	}
	for _, note := range info.Notes {
		if strings.Contains(note, "无法读取 systemd") {
			t.Fatal("可用的旧版服务属性不应显示整体读取失败：", note)
		}
	}
}

func TestAppInfoSystemdFailureShowsActualReason(t *testing.T) {
	for _, test := range []struct{ out, reason string }{
		{"Failed to connect to bus: Access denied", "Access denied"},
		{"LoadState=not-found\nFragmentPath=\n", "不存在"},
		{"LoadState=error\nExecStart={ path=/java ; argv[]=/java -jar app.jar --password=private-value ; }\n", "LoadState=error"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			f := newFake(t)
			s := javaApp("api")
			s.UnitText = ManagedMarker + "\n[Service]\nExecStart=/saved/java/bin/java -jar api.jar\n"
			a := &Agent{req: &proto.Request{Root: f.root}, Sys: &appInfoCommandSys{fakeSys: f, responses: map[string]appInfoResponse{
				appInfoUnitCommand(s.Unit): {out: test.out, err: errors.New("exit status 1")},
			}}}
			info := decodeAppInfo(t, execConfigured(t, a, "appinfo", &proto.Request{Apps: []proto.AppSpec{s}}))
			if infoValue(info, "服务配置的启动程序") != "/saved/java/bin/java" || !strings.Contains(strings.Join(info.Notes, "\n"), test.reason) {
				t.Fatalf("真实读取失败应保留回退信息并显示具体原因：%+v", info)
			}
			if strings.Contains(strings.Join(info.Notes, "\n"), "private-value") {
				t.Fatal("错误提示不能暴露原始服务启动参数")
			}
		})
	}
}

func TestAppInfoVersionFallbackUsesSameExecutableAndUser(t *testing.T) {
	f := newFake(t)
	p := ProcessSnapshot{PID: 51, Exe: "/opt/nginx instance/sbin/nginx", UID: "1001", User: "svc"}
	primary := "runuser -u 'svc' -- '/proc/51/exe' -v 2>&1"
	fallback := appInfoVersionFallback(proto.TypeStatic, p)
	if !strings.Contains(fallback, "[ '/proc/51/exe' -ef '/opt/nginx instance/sbin/nginx' ]") || !strings.Contains(fallback, "then runuser -u 'svc' -- '/opt/nginx instance/sbin/nginx' -v 2>&1") {
		t.Fatal("回退必须核对同一个可执行文件，并保留运行用户与固定版本参数：", fallback)
	}
	a := &Agent{Sys: &appInfoCommandSys{fakeSys: f, responses: map[string]appInfoResponse{
		primary:  {out: "runuser: failed to execute /proc/51/exe: Permission denied", err: errors.New("exit status 1")},
		fallback: {out: "nginx version: nginx/1.20.1\n"},
	}}}
	version, err := a.appInfoRuntimeVersion(proto.TypeStatic, p)
	if err != nil || !strings.Contains(version, "1.20.1") || len(f.calls) != 2 {
		t.Fatalf("应从核对过的实际程序读取版本：%q, %v, %v", version, err, f.calls)
	}
}

func TestAppInfoVersionDoesNotUseReplacedOrDeletedProgram(t *testing.T) {
	f := newFake(t)
	p := ProcessSnapshot{PID: 51, Exe: "/usr/sbin/nginx", UID: "1001", User: "svc"}
	primary := "runuser -u 'svc' -- '/proc/51/exe' -v 2>&1"
	fallback := appInfoVersionFallback(proto.TypeStatic, p)
	a := &Agent{Sys: &appInfoCommandSys{fakeSys: f, responses: map[string]appInfoResponse{
		primary:  {out: "Permission denied", err: errors.New("exit status 1")},
		fallback: {out: "运行进程已退出或程序文件已替换，无法确认版本", err: errors.New("exit status 1")},
	}}}
	if version, err := a.appInfoRuntimeVersion(proto.TypeStatic, p); version != "" || err == nil || !strings.Contains(err.Error(), "已替换") || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("程序不再一致时，必须保留具体原因，不能报告新文件的版本：%q %v", version, err)
	}
	p.Exe += " (deleted)"
	if command := appInfoVersionFallback(proto.TypeStatic, p); command != "" {
		t.Fatal("已删除的运行程序不能回退到同名的新文件：", command)
	}
	for _, test := range []struct{ kind, executable string }{{proto.TypeBinary, "/opt/api"}, {proto.TypeDotnet, "/opt/MyApp"}, {proto.TypeDotnet, "/usr/bin/dotnet"}} {
		p.Exe = test.executable
		if command := appInfoVersionFallback(test.kind, p); command != "" {
			t.Fatal("原生程序和 dotnet/apphost 不能作为解释器执行版本参数：", command)
		}
	}
}

func TestAppInfoStandaloneNginxIgnoresStaleSharedBinding(t *testing.T) {
	f := newFake(t)
	s := servedApp()
	result := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{s.Name: []byte("<h1>site</h1>")}))
	if result.code != 0 {
		t.Fatal(result.dump())
	}
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root}}
	binding, _ := json.Marshal(nginxSiteBinding{Instance: "removed-old-edge", File: "/old/site.conf"})
	if err := os.WriteFile(a.siteBindingFile(s.Name), binding, 0644); err != nil {
		t.Fatal(err)
	}
	info := decodeAppInfo(t, exec1(t, f, "appinfo", &proto.Request{Apps: []proto.AppSpec{s}}))
	if infoValue(info, "运行环境") != "Ferry 独立 Nginx" || infoValue(info, "Nginx 实例") != "" {
		t.Fatal("不应使用旧版本残留的共享站点绑定：", info)
	}
}

func TestAppInfoDisabledSiteKeepsNginxServiceState(t *testing.T) {
	f, a, in, s := hostedFixture(t)
	if err := a.writeRunning(s.Name, id1); err != nil {
		t.Fatal(err)
	}
	f.unit(in.Unit).pid, f.unit(in.Unit).sub = 70, "running"
	info := decodeAppInfo(t, execConfigured(t, a, "appinfo", &proto.Request{FerryDir: a.req.FerryDir, Project: a.req.Project, Apps: []proto.AppSpec{s}}))
	if info.Status.Active != "inactive" || infoValue(info, "服务状态") != "active / running" || infoValue(info, "主进程 PID") != "70" {
		t.Fatal("站点停用不代表共享 Nginx 服务也已停止：", info)
	}
}
