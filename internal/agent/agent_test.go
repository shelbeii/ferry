package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// ---------------------------------------------------------------- 假系统

type fakeUnit struct {
	active, sub string
	pid         int
	enabled     bool
	since       string // ActiveEnterTimestamp
}

type fakeSys struct {
	root       string
	unitDir    string
	now        time.Time
	units      map[string]*fakeUnit
	links      map[string]string
	appOf      map[string]string // unit → 应用（用来判断 current 指向哪个版本）
	bad        map[string]bool   // 这些版本启动后崩溃
	unitBad    map[string]bool   // 这些单元启动后崩溃（不看版本）
	calls      []string
	locked     bool
	namedLocks map[string]bool
	nextPID    int
	shellFail  map[string]bool
	denied     map[string]bool // 运行用户无权访问的路径（"路径" 全部拒绝，"路径|w" 只拒绝写）
	owners     map[string]Ownership
	javas      map[string]string  // java 路径 → 版本
	acl        bool               // 支持 setfacl
	listen     map[int][]string   // pid → 监听的地址
	args       map[int][][]string // pid → 进程及子进程的命令行
	nginx      string             // nginx -T 的输出；空 = 没有 nginx
	users      map[string]bool    // useradd 创建的用户
	hooks      []HookExec         // 执行过的部署钩子
	hookFail   map[string]bool    // 这些钩子命令执行失败
	shellOut   map[string]string  // 这些命令的输出
	// Pipe：pipeOut 命令的输出、pipeFail 命令失败的错误；收到的标准输入记在 pipeIn，环境变量记在 pipeEnv
	pipeOut, pipeFail, pipeIn map[string]string
	pipeEnv                   map[string][]string
}

func newFake(t *testing.T) *fakeSys {
	root := filepath.Join(t.TempDir(), "srv", "app")
	unitDir := filepath.Join(t.TempDir(), "units")
	os.MkdirAll(unitDir, 0o755)
	return &fakeSys{root: root, unitDir: unitDir, now: time.Date(2026, 10, 15, 10, 0, 0, 0, time.Local),
		units: map[string]*fakeUnit{}, links: map[string]string{}, appOf: map[string]string{},
		bad: map[string]bool{}, unitBad: map[string]bool{}, nextPID: 100, shellFail: map[string]bool{}}
}

func (f *fakeSys) unit(n string) *fakeUnit {
	u, ok := f.units[n]
	if !ok {
		u = &fakeUnit{active: "inactive", sub: "dead"}
		f.units[n] = u
	}
	return u
}

func (f *fakeSys) current(app string) string { return curOf(f.root, app, f) }

// curOf 运行中的版本：部署目录的 .ferry/apps/<app>/running；还没换成新结构的看 current 链接。
func curOf(root, app string, f *fakeSys) string {
	if b, err := os.ReadFile(filepath.Join(f.root, ".ferry", "apps", app, "running")); err == nil {
		return strings.TrimSpace(string(b))
	}
	t, ok := f.links[filepath.Join(root, "apps", app, "current")]
	if !ok {
		return ""
	}
	return filepath.Base(t)
}

func (f *fakeSys) Systemctl(args ...string) error {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) < 2 {
		return nil
	}
	u := f.unit(args[1])
	switch args[0] {
	case "start", "restart":
		if _, err := os.Stat(filepath.Join(f.unitDir, args[1])); err != nil {
			return fmt.Errorf("Unit %s not found", args[1])
		}
		f.nextPID++
		u.pid = f.nextPID
		u.active, u.sub = "active", "running"
		if f.unitBad[args[1]] || f.bad[f.current(f.appOf[args[1]])] && f.isManaged(args[1]) {
			u.active, u.sub, u.pid = "activating", "auto-restart", 0
		}
	case "stop":
		u.active, u.sub, u.pid = "inactive", "dead", 0
	case "enable":
		u.enabled = true
	case "disable":
		u.enabled = false
	}
	return nil
}

// isManaged 只有 ferry 生成的单元才会跑 current 里的版本。
func (f *fakeSys) isManaged(unit string) bool {
	b, _ := os.ReadFile(filepath.Join(f.unitDir, unit))
	return strings.HasPrefix(string(b), ManagedMarker)
}

func (f *fakeSys) UnitProps(unit string) UnitProps {
	u := f.unit(unit)
	load := "loaded"
	if _, err := os.Stat(filepath.Join(f.unitDir, unit)); err != nil {
		load = "not-found"
	}
	return UnitProps{Load: load, Active: u.active, Sub: u.sub, PID: u.pid, Since: u.since}
}

func (f *fakeSys) Journal(string, int, time.Time) []string {
	return []string{"Starting...", "Caused by: java.net.BindException: Address already in use", "\tat x.y.Z(Z.java:1)"}
}

func (f *fakeSys) LookupUser(name string) (int, int, error) {
	if name == "svc" || f.users[name] {
		return 1000, 1000, nil
	}
	return 0, 0, errors.New("用户 " + name + " 不存在")
}

func (f *fakeSys) Lchown(p string, uid, gid int) error {
	if f.denied != nil {
		f.calls = append(f.calls, fmt.Sprintf("chown %d:%d %s", uid, gid, p))
		f.grant(p)
	}
	return nil
}

func (f *fakeSys) Ownership(p string) (Ownership, error) {
	if o, ok := f.owners[p]; ok {
		return o, nil
	}
	return Ownership{}, errors.New("未知")
}

func (f *fakeSys) Listening(pid int) []string { return f.listen[pid] }

func (f *fakeSys) ProcArgs(pid int) [][]string { return f.args[pid] }

func (f *fakeSys) NginxDump() (string, error) {
	if f.nginx == "" {
		return "", ErrNoNginx
	}
	return f.nginx, nil
}

// Chmod / SetFACL：真的改（临时目录里），并让对应的「拒绝」失效
func (f *fakeSys) Chmod(p string, mode os.FileMode) error {
	f.calls = append(f.calls, fmt.Sprintf("chmod %o %s", mode.Perm(), p))
	f.grant(p)
	return os.Chmod(p, mode)
}

func (f *fakeSys) SetFACL(user, p string, flag byte) (bool, error) {
	if !f.acl {
		return false, nil
	}
	f.calls = append(f.calls, fmt.Sprintf("setfacl u:%s:%c %s", user, flag, p))
	f.grant(p)
	return true, nil
}

func (f *fakeSys) grant(p string) {
	for _, k := range []string{p, p + "|r", p + "|w", p + "|x"} {
		delete(f.denied, k)
	}
}

func (f *fakeSys) JavaVersion(java string) (string, error) {
	if v, ok := f.javas[java]; ok {
		return `openjdk version "` + v + `" 2024-01-16`, nil
	}
	return "", errors.New("没有 " + java)
}

// svc 的 uid 1000，在组 1000（svc）与 1001（app）里
func (f *fakeSys) UserGroups(string) (int, []int, error) { return 1000, []int{1000, 1001}, nil }

// AccessAs 拒绝的目录连同其下的一切都访问不了（进不去）。
func (f *fakeSys) AccessAs(_, p string, flag byte) (bool, error) {
	if f.denied[p+"|"+string(flag)] {
		return false, nil
	}
	for d := p; ; d = path.Dir(d) {
		if f.denied[d] {
			return false, nil
		}
		if d == path.Dir(d) {
			return true, nil
		}
	}
}

func (f *fakeSys) Pipe(cmd string, env []string, stdin io.Reader, stdout io.Writer, _ time.Duration) error {
	f.calls = append(f.calls, "pipe "+cmd)
	if f.pipeIn == nil {
		f.pipeIn, f.pipeEnv = map[string]string{}, map[string][]string{}
	}
	f.pipeEnv[cmd] = env
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.pipeIn[cmd] = string(b)
	}
	if msg, ok := f.pipeFail[cmd]; ok {
		return errors.New(msg)
	}
	io.WriteString(stdout, f.pipeOut[cmd])
	return nil
}

func (f *fakeSys) Shell(cmd string, _ time.Duration) (string, error) {
	f.calls = append(f.calls, "sh "+cmd)
	if out, ok := f.shellOut[cmd]; ok {
		return out, nil
	}
	if f.shellFail[cmd] {
		return "nginx: [emerg] bad config\n", errors.New("exit status 1")
	}
	if strings.HasPrefix(cmd, "useradd ") {
		fs := strings.Fields(cmd)
		if f.users == nil {
			f.users = map[string]bool{}
		}
		f.users[strings.Trim(fs[len(fs)-1], "'")] = true
	}
	return "", nil
}

func (f *fakeSys) RunHook(h HookExec, line func(string)) error {
	f.hooks = append(f.hooks, h)
	line("running " + h.Cmd)
	if f.hookFail[h.Cmd] {
		line("ERROR: migration failed")
		return errors.New("退出码 1")
	}
	return nil
}

func (f *fakeSys) Probe(h proto.Health) error {
	if h.TCP == "" {
		return nil
	}
	for unit, u := range f.units {
		if f.appOf[unit] != "" && strings.HasSuffix(h.TCP, ":"+f.appOf[unit]) && u.active == "active" {
			return nil
		}
	}
	return errors.New(h.TCP + " 未监听")
}

func (f *fakeSys) SwitchLink(link, target string) error {
	f.links[link] = target
	return nil
}

func (f *fakeSys) ReadLink(link string) (string, error) {
	t, ok := f.links[link]
	if !ok {
		return "", os.ErrNotExist
	}
	return t, nil
}

func (f *fakeSys) Lock(name string, _ time.Duration) (func(), error) {
	if f.locked || f.namedLocks[name] {
		return nil, ErrLocked
	}
	if f.namedLocks == nil {
		f.namedLocks = map[string]bool{}
	}
	f.namedLocks[name] = true
	return func() { delete(f.namedLocks, name) }, nil
}

func (f *fakeSys) UnitDir() string       { return f.unitDir }
func (f *fakeSys) Now() time.Time        { return f.now }
func (f *fakeSys) Sleep(d time.Duration) { f.now = f.now.Add(d) }

func (f *fakeSys) RemoveLink(link string) error {
	delete(f.links, link)
	return nil
}

// ---------------------------------------------------------------- 辅助

type run struct {
	code   int
	events []proto.Event
	result proto.Result
}

func (r run) has(code, substr string) bool {
	for _, e := range r.events {
		if e.Code == code && strings.Contains(e.Msg, substr) {
			return true
		}
	}
	return false
}

func (r run) dump() string {
	var b strings.Builder
	for _, e := range r.events {
		fmt.Fprintf(&b, "%s %-8s %-6s %s\n", e.Level, e.Code, e.App, e.Msg)
	}
	fmt.Fprintf(&b, "result %d %s %s\n", r.result.RC, r.result.Msg, r.result.Data)
	return b.String()
}

func exec1(t *testing.T, f *fakeSys, cmd string, req *proto.Request) run {
	t.Helper()
	var out bytes.Buffer
	a := &Agent{Sys: f, Out: &out}
	req.Proto, req.Root, req.Actor = proto.Version, f.root, "tester@pc"
	code := a.Run(cmd, req)
	r := run{code: code}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var head struct{ T string }
		json.Unmarshal([]byte(line), &head)
		if head.T == "ev" {
			var e proto.Event
			json.Unmarshal([]byte(line), &e)
			r.events = append(r.events, e)
		} else {
			json.Unmarshal([]byte(line), &r.result)
		}
	}
	return r
}

func javaApp(name string) proto.AppSpec {
	unit := "demo-" + strings.ToLower(name) + ".service"
	return proto.AppSpec{
		Name: name, Type: proto.TypeJava, FileName: name + ".jar", Unit: unit, User: "svc",
		UnitText:     ManagedMarker + "\n[Service]\nExecStart=/usr/bin/java -jar current/" + name + ".jar\n",
		SharedDirs:   []string{"logs"},
		Health:       proto.Health{TCP: "127.0.0.1:" + name, TimeoutSec: 20},
		KeepReleases: 3,
	}
}

// stage 走一遍 prepare，往上传目录放好文件，返回 deploy 请求。
func (f *fakeSys) stage(t *testing.T, apps []proto.AppSpec, id string, content map[string][]byte) *proto.Request {
	t.Helper()
	r := exec1(t, f, "prepare", &proto.Request{Apps: apps})
	if r.code != 0 {
		t.Fatalf("prepare：\n%s", r.dump())
	}
	var p proto.Prepared
	json.Unmarshal(r.result.Data, &p)
	req := &proto.Request{Apps: apps, Stage: p.Stage}
	for _, s := range apps {
		file := s.FileName
		data := content[s.Name]
		if s.Type == proto.TypeStatic {
			file = s.Name + ".tar.gz"
			data = tgz(t, map[string]string{"index.html": string(content[s.Name])})
		}
		if proto.DirService(s.Type) {
			file = s.Name + ".tar.gz" // content 是打好的 tar.gz
		}
		os.WriteFile(filepath.Join(p.Stage, file), data, 0o644)
		sum := sha256.Sum256(data)
		req.Releases = append(req.Releases, proto.ReleaseSpec{App: s.Name, ID: id, File: file, SHA256: hex.EncodeToString(sum[:]),
			Size: int64(len(data)), Archive: s.Type == proto.TypeStatic, Meta: proto.ReleaseMeta{Commit: "abc123"}})
		if s.Unit != "" {
			f.appOf[s.Unit] = s.Name
		}
	}
	f.now = f.now.Add(time.Second) // 每次 prepare 的上传目录名不同
	return req
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

const (
	id1 = "20261015-100000-aaaa"
	id2 = "20261015-110000-bbbb"
	id3 = "20261015-120000-cccc"
	id4 = "20261015-130000-dddd"
)

// ---------------------------------------------------------------- 测试

func TestDeployFreshThenCrashRollsBack(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")}))
	if r.code != 0 || f.current("System") != id1 {
		t.Fatalf("首次部署：\n%s", r.dump())
	}
	jar := filepath.Join(f.root, "apps", "System", "running", "System.jar")
	if b, _ := os.ReadFile(jar); string(b) != "v1" {
		t.Fatal("版本文件内容错误")
	}
	if !sameFile(filepath.Join(f.unitDir, sys.Unit), []byte(sys.UnitText)) || !f.units[sys.Unit].enabled {
		t.Fatal("单元文件未写入或未启用")
	}
	for _, d := range []string{"data/logs", "running/logs", "../../.ferry/apps/System/versions/" + id1 + "/release.json", "../../.ferry/apps/System/versions/" + id1 + "/unit.service", "../../.ferry/apps/System/run.json", "../../.ferry/apps/System/running"} {
		if _, err := os.Stat(filepath.Join(f.root, "apps", "System", filepath.FromSlash(d))); err != nil {
			t.Errorf("%s 未创建", d)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, ".ferry", "incoming")); len(entries) != 0 {
		t.Fatal("上传目录未清理")
	}

	f.bad[id2] = true
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}))
	if r.code != proto.CodeFailed || !r.has(proto.EvCrash, "进程已退出") || !r.has(proto.EvRollback, "已回滚到 "+id1) {
		t.Fatalf("崩溃应回滚：\n%s", r.dump())
	}
	if !r.has(proto.EvDiag, "BindException") || r.has(proto.EvDiag, "Z.java") {
		t.Fatalf("诊断应输出原因、去掉堆栈：\n%s", r.dump())
	}
	if f.current("System") != id1 || f.units[sys.Unit].active != "active" {
		t.Fatal("应切回并运行旧版本")
	}
	var out proto.DeployOutcome
	json.Unmarshal(r.result.Data, &out)
	if out.Apps[0].Result != "ROLLED_BACK" {
		t.Fatalf("结果：%+v", out)
	}
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root}}
	if !a.isFailed("System", id2) || a.previousGood("System", "") != id1 {
		t.Fatal("失败版本应被标记，默认回滚目标跳过它")
	}
	hist := exec1(t, f, "history", &proto.Request{Apps: []proto.AppSpec{sys}})
	var hs []proto.HistoryEntry
	json.Unmarshal(hist.result.Data, &hs)
	if len(hs) != 2 || hs[0].Result != "ROLLED_BACK" || hs[1].Result != "OK" || hs[1].Commit != "abc123" {
		t.Fatalf("历史：%+v", hs)
	}
}

func TestFailureSkipsRemainingApps(t *testing.T) {
	f := newFake(t)
	a, b := javaApp("Auth"), javaApp("Gateway")
	f.unitBad[a.Unit] = true
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{a, b}, id1, map[string][]byte{"Auth": []byte("a"), "Gateway": []byte("g")}))
	if r.code != proto.CodeFailed || !r.has(proto.EvAbort, "因 Auth 失败未部署") || !r.has(proto.EvRollback, "首次部署失败") {
		t.Fatalf("\n%s", r.dump())
	}
	if f.current("Gateway") != "" {
		t.Fatalf("Gateway 不应部署：%q", f.current("Gateway"))
	}
	if _, err := os.Stat(filepath.Join(f.unitDir, a.Unit)); err == nil {
		t.Fatal("首次部署失败应删除单元文件")
	}
}

func TestVerifyFailureTouchesNothing(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	req := f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})
	req.Releases[0].SHA256 = strings.Repeat("0", 64)
	r := exec1(t, f, "deploy", req)
	if r.code != proto.CodeVerify || len(f.calls) != 0 {
		t.Fatalf("校验失败不应调用 systemctl：%v\n%s", f.calls, r.dump())
	}
}

func TestTakeoverForeignUnitAndLegacyRollback(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	legacy := "[Service]\nExecStart=/usr/bin/java -jar /home/x/old.jar\n"
	os.WriteFile(filepath.Join(f.unitDir, sys.Unit), []byte(legacy), 0o644)
	f.unit(sys.Unit).active = "active"
	f.unit(sys.Unit).pid = 5

	// 接管失败：恢复原单元文件并拉起旧服务
	f.bad[id1] = true
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")}))
	if r.code != proto.CodeFailed || !sameFile(filepath.Join(f.unitDir, sys.Unit), []byte(legacy)) {
		t.Fatalf("应恢复原单元：\n%s", r.dump())
	}
	if f.units[sys.Unit].active != "active" || !r.has(proto.EvRollback, "接管前") {
		t.Fatalf("旧服务应运行：\n%s", r.dump())
	}
	if !sameFile(filepath.Join(f.root, ".ferry", "apps", "System", "legacy", sys.Unit), []byte(legacy)) {
		t.Fatal("原单元文件应保存")
	}

	// 接管成功，再回到接管前
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}))
	if r.code != 0 || !sameFile(filepath.Join(f.unitDir, sys.Unit), []byte(sys.UnitText)) {
		t.Fatalf("接管：\n%s", r.dump())
	}
	r = exec1(t, f, "releases", &proto.Request{Apps: []proto.AppSpec{sys}})
	var rel []proto.AppReleases
	json.Unmarshal(r.result.Data, &rel)
	if len(rel[0].Releases) != 2 || !rel[0].Legacy || !rel[0].Releases[0].Current || !rel[0].Releases[1].Failed {
		t.Fatalf("版本列表：%+v", rel)
	}
	r = exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{sys}})
	if r.code != 0 || !sameFile(filepath.Join(f.unitDir, sys.Unit), []byte(legacy)) || f.units[sys.Unit].active != "active" {
		t.Fatalf("回到接管前：\n%s", r.dump())
	}
}

func TestConflictsStoppedAndDisabled(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	sys.Conflict = []string{"old-native-system.service"}
	os.WriteFile(filepath.Join(f.unitDir, "old-native-system.service"), []byte("[Service]\n"), 0o644)
	old := f.unit("old-native-system.service")
	old.active, old.pid, old.enabled = "active", 9, true
	f.appOf["old-native-system.service"] = "System"

	f.bad[id1] = true
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")}))
	if r.code != proto.CodeFailed || old.active != "active" || !old.enabled {
		t.Fatalf("失败时应恢复冲突的旧服务：\n%s", r.dump())
	}
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("v2")}))
	if r.code != 0 || old.active != "inactive" || old.enabled {
		t.Fatalf("成功后旧服务应停止并取消自启：\n%s", r.dump())
	}
}

func TestDeltaAssemble(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	lib := []byte("lib")
	sum := sha256.Sum256(lib)
	libSHA := hex.EncodeToString(sum[:])
	plan := "RAW\t0\t4\nLIB\t" + libSHA + "\t3\nRAW\t4\t4\n"
	toDelta := func(req *proto.Request, withLib bool) {
		os.Remove(filepath.Join(req.Stage, "System.jar"))
		if withLib {
			os.MkdirAll(filepath.Join(req.Stage, "libs"), 0o755)
			os.WriteFile(filepath.Join(req.Stage, "libs", libSHA+".jar"), lib, 0o644)
		}
		os.WriteFile(filepath.Join(req.Stage, "System.jar.pack"), []byte("HEADBODY"), 0o644)
		os.WriteFile(filepath.Join(req.Stage, "System.jar.plan"), []byte(plan), 0o644)
		req.Releases[0].Plan, req.Releases[0].Pack = "System.jar.plan", "System.jar.pack"
	}
	req := f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("HEADlibBODY")})
	toDelta(req, true)
	r := exec1(t, f, "deploy", req)
	if r.code != 0 || !r.has(proto.EvAssemble, "System.jar") {
		t.Fatalf("\n%s", r.dump())
	}
	cached := filepath.Join(f.root, ".ferry", "cache", "lib", libSHA+".jar")
	if _, err := os.Stat(cached); err != nil {
		t.Fatal("新依赖应进入缓存")
	}
	// prepare 报告缓存
	pr := exec1(t, f, "prepare", &proto.Request{Apps: []proto.AppSpec{sys}})
	var p proto.Prepared
	json.Unmarshal(pr.result.Data, &p)
	if len(p.Libs) != 1 || p.Libs[0] != libSHA {
		t.Fatalf("prepare 应列出缓存：%+v", p)
	}
	// 缓存被篡改：校验失败，引擎随后全量重试
	os.WriteFile(cached, []byte("LIB"), 0o644)
	req = f.stage(t, []proto.AppSpec{sys}, id2, map[string][]byte{"System": []byte("HEADlibBODY")})
	toDelta(req, false)
	if r = exec1(t, f, "deploy", req); r.code != proto.CodeVerify {
		t.Fatalf("缓存损坏应校验失败：\n%s", r.dump())
	}
	if _, err := os.Stat(cached); !os.IsNotExist(err) || !r.has(proto.EvCache, "已损坏") {
		t.Fatalf("损坏的缓存依赖应删除：\n%s", r.dump())
	}
}

func TestPruneKeepsCurrentAndPrevious(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	sys.KeepReleases = 2
	for i, id := range []string{id1, id2, id3, id4} {
		r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id, map[string][]byte{"System": {byte(i)}}))
		if r.code != 0 {
			t.Fatal(r.dump())
		}
	}
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root}}
	if got := a.listReleases("System"); strings.Join(got, ",") != id4+","+id3 {
		t.Fatalf("应保留 2 个：%v", got)
	}
}

func TestManualRollbackAndControl(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	for _, id := range []string{id1, id2} {
		if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id, map[string][]byte{"System": []byte(id)})); r.code != 0 {
			t.Fatal(r.dump())
		}
	}
	r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{sys}})
	if r.code != 0 || f.current("System") != id1 {
		t.Fatalf("\n%s", r.dump())
	}
	// 指定版本、启动失败时切回
	f.bad[id2] = true
	r = exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{sys}, To: id2})
	if r.code != proto.CodeFailed || f.current("System") != id1 || f.units[sys.Unit].active != "active" {
		t.Fatalf("回滚失败应切回：\n%s", r.dump())
	}
	r = exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "stop"})
	if r.code != 0 || f.units[sys.Unit].active != "inactive" {
		t.Fatal(r.dump())
	}
	r = exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys}, Action: "restart"})
	if r.code != 0 || !r.has(proto.EvReady, "") {
		t.Fatal(r.dump())
	}
	r = exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{sys}})
	var st []proto.AppStatus
	json.Unmarshal(r.result.Data, &st)
	if st[0].Current == nil || st[0].Current.ID != id1 || st[0].Healthy == nil || !*st[0].Healthy || st[0].Releases != 2 {
		t.Fatalf("状态：%+v", st[0])
	}
}

func TestControlSkipsNotDeployed(t *testing.T) {
	f := newFake(t)
	sys, other := javaApp("System"), javaApp("Other")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	for _, action := range []string{"restart", "stop", "start"} {
		r := exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{sys, other}, Action: action})
		if r.code != 0 || !r.has(proto.EvInfo, "未通过 ferry 部署") {
			t.Fatalf("%s：\n%s", action, r.dump())
		}
	}
	if f.units[sys.Unit].active != "active" {
		t.Fatal("System 应在运行")
	}
}

func TestControlAllNotDeployedFails(t *testing.T) {
	f := newFake(t)
	r := exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{javaApp("Job")}, Action: "restart"})
	if r.code != proto.CodeFailed || !strings.Contains(r.result.Msg, "还没有通过 ferry 部署过") {
		t.Fatalf("全部未部署时不能报成功：\n%s", r.dump())
	}
}

func TestLocked(t *testing.T) {
	f := newFake(t)
	sys := javaApp("System")
	req := f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})
	f.locked = true
	if r := exec1(t, f, "deploy", req); r.code != proto.CodeLocked {
		t.Fatal(r.dump())
	}
	f.locked = false
}

// 别人正在部署：拿不到锁时说是谁在做什么；状态里也看得到
func TestLockHolder(t *testing.T) {
	old := pidAlive
	defer func() { pidAlive = old }()
	pidAlive = func(int) bool { return true }
	f := newFake(t)
	sys := javaApp("System")
	a := &Agent{Sys: f, Out: io.Discard, cmd: "deploy", req: &proto.Request{Root: f.root, Actor: "张三（zs@PC）", Apps: []proto.AppSpec{sys}}}
	unlock, err := a.lock(0)
	if err != nil {
		t.Fatal(err)
	}
	b := &Agent{Sys: f, Out: io.Discard, cmd: "control", req: &proto.Request{Root: f.root, Actor: "李四", Action: "restart"}}
	if _, err := b.lock(0); !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "张三（zs@PC）正在部署 System") {
		t.Fatalf("要说是谁在部署：%v", err)
	}
	r := exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{sys}})
	var st []proto.AppStatus
	json.Unmarshal(r.result.Data, &st)
	if len(st) != 1 || st[0].Busy == nil || st[0].Busy.Actor != "张三（zs@PC）" || st[0].Busy.Action != "部署" {
		t.Fatalf("状态里要有正在部署的人：%+v", st)
	}
	unlock()
	if a.lockHolder() != nil {
		t.Fatal("做完后不再显示")
	}
	// 崩溃留下的记录（进程已经不在了）：不算
	unlock, _ = a.lock(0)
	pidAlive = func(int) bool { return false }
	if a.lockHolder() != nil {
		t.Fatal("进程不在了就不算")
	}
	unlock()
}

func TestStaticDeployAndReloadFailure(t *testing.T) {
	f := newFake(t)
	web := proto.AppSpec{Name: "web", Type: proto.TypeStatic, Link: filepath.Join(t.TempDir(), "html", "web"),
		Reload: "nginx -s reload", KeepReleases: 3}
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{web}, id1, map[string][]byte{"web": []byte("<h1>1</h1>")}))
	if r.code != 0 || f.current("web") != id1 || f.links[web.Link] != filepath.Join(f.root, "apps", "web", "running") {
		t.Fatalf("\n%s", r.dump())
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, "apps", "web", "running", "index.html")); string(b) != "<h1>1</h1>" {
		t.Fatal("静态文件未解压")
	}
	f.shellFail["nginx -s reload"] = true
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{web}, id2, map[string][]byte{"web": []byte("<h1>2</h1>")}))
	if r.code != proto.CodeFailed || f.current("web") != id1 || !r.has(proto.EvDiag, "emerg") {
		t.Fatalf("reload 失败应切回：\n%s", r.dump())
	}
}

func TestBadInput(t *testing.T) {
	f := newFake(t)
	bad := javaApp("System")
	bad.User = "root"
	if r := exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{bad}}); r.code != proto.CodeBadInput {
		t.Fatal("root 运行应拒绝")
	}
	sys := javaApp("System")
	req := f.stage(t, []proto.AppSpec{sys}, id1, map[string][]byte{"System": []byte("v1")})
	req.Stage = filepath.Join(f.root, "elsewhere")
	if r := exec1(t, f, "deploy", req); r.code != proto.CodeBadInput {
		t.Fatal("上传目录必须在 incoming 下")
	}
	var out bytes.Buffer
	a := &Agent{Sys: f, Out: &out}
	if code := a.Run("deploy", &proto.Request{Proto: 1, Root: f.root}); code != proto.CodeBadInput {
		t.Fatal("协议版本不一致应拒绝")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.tar.gz")
	os.WriteFile(src, tgz(t, map[string]string{"../evil": "x"}), 0o644)
	if err := extractTarGz(src, filepath.Join(dir, "out")); err == nil {
		t.Fatal("应拒绝 ..")
	}
}

func TestAppOwnRoot(t *testing.T) {
	f := newFake(t)
	other := filepath.Join(t.TempDir(), "data", "demo")
	job := javaApp("Job")
	job.Root = other
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{job}, id1, map[string][]byte{"Job": []byte("v1")}))
	if r.code != 0 {
		t.Fatalf("部署：\n%s", r.dump())
	}
	if b, _ := os.ReadFile(filepath.Join(other, "apps", "Job", "running", "Job.jar")); string(b) != "v1" {
		t.Fatal("版本应放在应用自己的部署根目录下")
	}
	if _, err := os.Stat(filepath.Join(f.root, "apps", "Job")); err == nil {
		t.Fatal("目标的部署根目录下不应有该应用")
	}
	if curOf(other, "Job", f) != id1 {
		t.Fatalf("运行中的版本：%q", curOf(other, "Job", f))
	}
	if _, err := os.Stat(filepath.Join(f.root, ".ferry", "history.jsonl")); err != nil {
		t.Fatal("部署历史仍记在目标的 .ferry 下")
	}

	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{job}, id2, map[string][]byte{"Job": []byte("v2")}))
	if r.code != 0 {
		t.Fatalf("第二次部署：\n%s", r.dump())
	}
	r = exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{job}})
	if r.code != 0 || curOf(other, "Job", f) != id1 {
		t.Fatalf("回滚：\n%s", r.dump())
	}
	rs := exec1(t, f, "releases", &proto.Request{Apps: []proto.AppSpec{job}})
	var list []proto.AppReleases
	json.Unmarshal(rs.result.Data, &list)
	if len(list) != 1 || len(list[0].Releases) != 2 {
		t.Fatalf("版本列表：%s", rs.result.Data)
	}

	// 以前在目标的部署根目录下部署过：预检提示不会搬过去
	os.MkdirAll(filepath.Join(f.root, "apps", "Job", "releases", id3), 0o755)
	pf := exec1(t, f, "preflight", &proto.Request{Apps: []proto.AppSpec{job}})
	var p proto.Preflight
	json.Unmarshal(pf.result.Data, &p)
	if len(p.AppRoots) != 1 || p.AppRoots[0].Path != other || p.AppRoots[0].Apps[0] != "Job" {
		t.Fatalf("appRoots：%+v", p.AppRoots)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "不会搬过去") {
		t.Fatalf("应提示旧版本不会搬：%v", p.Warnings)
	}

	bad := javaApp("Job")
	bad.Root = "relative/dir"
	if r := exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{bad}}); r.code != proto.CodeBadInput {
		t.Fatalf("相对路径应拒绝：%d %s", r.code, r.result.Msg)
	}
}

// 运行用户不存在：单独预检只报告；自动修正时 plan 列出、apply 创建
func TestPreflightRunUser(t *testing.T) {
	f := newFake(t)
	app := javaApp("Job")
	app.User = "shop"
	var p proto.Preflight
	json.Unmarshal(exec1(t, f, "preflight", &proto.Request{Apps: []proto.AppSpec{app}}).result.Data, &p)
	if !strings.Contains(strings.Join(p.Problems, "\n"), "运行用户 shop 不存在") || len(p.Fixable) != 0 {
		t.Fatalf("不修正时应报告问题：%v %v", p.Problems, p.Fixable)
	}
	p = proto.Preflight{}
	json.Unmarshal(exec1(t, f, "preflight", &proto.Request{Apps: []proto.AppSpec{app}, FixPerms: FixPlan}).result.Data, &p)
	if !slices.Contains(p.Fixable, "创建运行用户 shop") || strings.Contains(strings.Join(p.Problems, "\n"), "shop") {
		t.Fatalf("plan：%v %v", p.Fixable, p.Problems)
	}
	if f.users["shop"] {
		t.Fatal("plan 不应创建用户")
	}
	p = proto.Preflight{}
	json.Unmarshal(exec1(t, f, "preflight", &proto.Request{Apps: []proto.AppSpec{app}, FixPerms: FixApply}).result.Data, &p)
	if !slices.Contains(p.Fixed, "创建运行用户 shop") || !p.Users["shop"] || !f.users["shop"] {
		t.Fatalf("apply：%v %v %v", p.Fixed, p.Users, p.Problems)
	}
}

// storedJar 依赖不压缩存放的 fat jar（和 Spring Boot 打的一样，才能抽取依赖）。
func storedJar(t *testing.T, libs map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	names := []string{"META-INF/MANIFEST.MF"}
	for n := range libs {
		names = append(names, n)
	}
	sort.Strings(names[1:])
	for _, n := range names {
		w, err := z.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(libs[n] + "-content"))
	}
	z.Close()
	return b.Bytes()
}

// 部署失败：这次上传的依赖留在缓存里（失败的、因它没部署的应用都算），改好配置再部署时只传变了的部分。
func TestFailedDeployKeepsLibs(t *testing.T) {
	f := newFake(t)
	a, b := javaApp("Auth"), javaApp("Gateway")
	f.unitBad[a.Unit] = true
	ja := storedJar(t, map[string]string{"BOOT-INF/lib/common.jar": "common", "BOOT-INF/lib/auth-only.jar": "auth"})
	jb := storedJar(t, map[string]string{"BOOT-INF/lib/common.jar": "common", "BOOT-INF/lib/gw-only.jar": "gw"})
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{a, b}, id1, map[string][]byte{"Auth": ja, "Gateway": jb}))
	if r.code != proto.CodeFailed || !r.has(proto.EvCache, "下次部署不用再传") {
		t.Fatalf("\n%s", r.dump())
	}
	libs := func(seed bool) []string {
		pr := exec1(t, f, "prepare", &proto.Request{Apps: []proto.AppSpec{a, b}, Seed: seed})
		var p proto.Prepared
		json.Unmarshal(pr.result.Data, &p)
		return p.Libs
	}
	if got := libs(false); len(got) != 3 {
		t.Fatalf("应留下 3 个依赖：%v", got)
	}

	// 以前的版本失败时没留（缓存是空的）：从还没切换过去的最新版本里抽
	os.RemoveAll(filepath.Join(f.root, ".ferry", "cache", "lib"))
	if got := libs(true); len(got) != 2 {
		t.Fatalf("应从失败的 Auth 版本抽出 2 个依赖：%v", got)
	}
}
