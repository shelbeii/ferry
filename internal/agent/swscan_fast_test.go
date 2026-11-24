package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// fastScan 一台只有几个进程的假服务器：记下运行过的命令。
type fastScan struct {
	fp  *fakeProc
	mu  sync.Mutex
	ran []string
}

func newFastScan(t *testing.T) *fastScan {
	fp := &fakeProc{dir: t.TempDir(), links: map[string]string{}}
	os.WriteFile(filepath.Join(fp.dir, "stat"), []byte("btime 1700000000\n"), 0o644)
	return &fastScan{fp: fp}
}

func (f *fastScan) scanner(cache *verCache) *swScan {
	return &swScan{
		proc: f.fp.dir, unitDir: f.fp.dir, cache: cache,
		run: func(cmd string, _ time.Duration) (string, error) {
			f.mu.Lock()
			f.ran = append(f.ran, cmd)
			f.mu.Unlock()
			switch cmd {
			case "'/usr/sbin/mysqld' --version 2>&1":
				return "/usr/sbin/mysqld  Ver 10.6.19-MariaDB for Linux on x86_64 (MariaDB Server)\n", nil
			case "'/usr/sbin/nginx' -v 2>&1":
				return "nginx version: nginx/1.24.0\n", nil
			case "'/opt/redis/bin/redis-server' --version 2>&1":
				return "Redis server v=8.0.1\n", nil
			}
			return "", errors.New("exit status 127")
		},
		unitProps: func(string) UnitProps { return UnitProps{Load: "loaded", Active: "active"} },
		readlink: func(p string) (string, error) {
			if v, ok := f.fp.links[p]; ok {
				return v, nil
			}
			return "", os.ErrNotExist
		},
		listening: func(int) []string { return nil },
	}
}

func (f *fastScan) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.ran {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// 版本按程序文件的身份缓存：文件没变不再运行程序；换了文件（身份变了）重新取。MariaDB 靠缓存里的输出也认得出。
func TestSoftwareVersionCache(t *testing.T) {
	f := newFastScan(t)
	f.fp.add(t, 1, 0, "/usr/lib/systemd/systemd", "0::/init.scope\n", "net:[1]")
	f.fp.add(t, 100, 1, "/usr/sbin/mysqld", "0::/system.slice/mariadb.service\n", "net:[1]")
	ids := map[string]string{"/usr/sbin/mysqld": "ino-1"}
	saved := fileID
	fileID = func(p string) string { return ids[p] }
	defer func() { fileID = saved }()
	path := filepath.Join(t.TempDir(), "cache", "versions.json")

	scan := func() proto.Software {
		c := loadVerCache(path)
		rep := f.scanner(c).scan()
		c.save()
		if len(rep.Items) != 1 {
			t.Fatalf("应认出一个软件：%+v", rep.Items)
		}
		return rep.Items[0]
	}
	if it := scan(); it.Version != "10.6.19" || it.Kind != "mariadb" {
		t.Fatalf("第一次：%+v", it)
	}
	if it := scan(); it.Version != "10.6.19" || it.Kind != "mariadb" {
		t.Fatalf("用缓存：%+v", it)
	}
	if n := f.count("--version"); n != 1 {
		t.Fatalf("程序没变应只运行一次取版本，运行了 %d 次", n)
	}
	ids["/usr/sbin/mysqld"] = "ino-2" // 升级替换了文件
	scan()
	if n := f.count("--version"); n != 2 {
		t.Fatalf("文件换了应重新取版本，运行了 %d 次", n)
	}
	var stored map[string]verCached
	b, _ := os.ReadFile(path)
	if json.Unmarshal(b, &stored) != nil || len(stored) != 1 {
		t.Fatalf("缓存只留这次用到的：%s", b)
	}
}

// Ferry 装的软件用安装时记下的版本，不运行程序；运行中的程序文件被换掉的标出来。
func TestSoftwareKnownAndReplaced(t *testing.T) {
	f := newFastScan(t)
	f.fp.add(t, 1, 0, "/usr/lib/systemd/systemd", "0::/init.scope\n", "net:[1]")
	f.fp.add(t, 100, 1, "/opt/redis/bin/redis-server", "0::/system.slice/ferry-redis.service\n", "net:[1]")
	f.fp.add(t, 200, 1, "/usr/sbin/nginx (deleted)", "0::/system.slice/nginx.service\n", "net:[1]", "nginx: master process")
	f.fp.add(t, 201, 200, "/usr/sbin/nginx (deleted)", "0::/system.slice/nginx.service\n", "net:[1]", "nginx: worker process")
	sc := f.scanner(nil)
	sc.instances = []proto.SoftwareInstance{{Name: "redis", Kind: "redis", Title: "Redis", Unit: "ferry-redis.service", Version: "8.0.1"}}
	sc.exists = func(string) bool { return true }
	rep := sc.scan()
	got := map[string]proto.Software{}
	for _, it := range rep.Items {
		got[it.Unit] = it
	}
	if it := got["ferry-redis.service"]; it.Version != "8.0.1" || it.Instance != "redis" {
		t.Errorf("Ferry 装的：%+v", it)
	}
	if n := f.count("redis-server' --version"); n != 0 {
		t.Errorf("安装时记了版本的不该再运行程序取版本")
	}
	if it := got["nginx.service"]; it.Replaced != "replaced" || it.Path != "/usr/sbin/nginx" || it.Version != "1.24.0" {
		t.Errorf("程序文件被替换：%+v", it)
	}
	sc = f.scanner(nil)
	sc.exists = func(string) bool { return false }
	for _, it := range sc.scan().Items {
		if it.Unit == "nginx.service" && it.Replaced != "deleted" {
			t.Errorf("程序文件删了：%+v", it)
		}
	}
	for _, k := range []string{"读进程", "取版本、读容器和系统服务", "整理进程", "整理容器", "整理系统服务", "整理Ferry实例", "排序", "读监听端口"} {
		if _, ok := rep.Took[k]; !ok {
			t.Errorf("没有记下 %s 用的时间：%v", k, rep.Took)
		}
	}
}

// 单元枚举按规则里的单元名过滤（全量列一次要 0.6~1.5 秒）。
func TestListUnitsFiltered(t *testing.T) {
	sc := newFastScan(t).scanner(nil)
	var cmds []string
	sc.run = func(cmd string, _ time.Duration) (string, error) {
		cmds = append(cmds, cmd)
		if strings.HasPrefix(cmd, "systemctl list-unit-files --type=service --no-legend --no-pager ") {
			return "docker.service enabled disabled\n", nil
		}
		return "", errors.New("unexpected")
	}
	out, err := sc.listUnits()
	if err != nil || !strings.Contains(out, "docker.service") {
		t.Fatalf("listUnits：%q %v", out, err)
	}
	if len(cmds) != 1 || !strings.Contains(cmds[0], "'docker.service'") || !strings.Contains(cmds[0], "'postgresql*.service'") {
		t.Errorf("没有按规则里的单元名过滤：%q", cmds)
	}
}

// 老 systemd 不认参数：退回全量枚举。
func TestListUnitsFallback(t *testing.T) {
	sc := newFastScan(t).scanner(nil)
	var cmds []string
	sc.run = func(cmd string, _ time.Duration) (string, error) {
		cmds = append(cmds, cmd)
		if strings.HasSuffix(cmd, "--no-pager") {
			return "nginx.service enabled disabled\n", nil
		}
		return "", errors.New("invalid option")
	}
	out, err := sc.listUnits()
	if err != nil || !strings.Contains(out, "nginx.service") {
		t.Fatalf("退回全量后：%q %v", out, err)
	}
	if len(cmds) != 2 {
		t.Errorf("应该先过滤、失败后全量：%q", cmds)
	}
}

type fakeExitCode int

func (c fakeExitCode) Error() string { return fmt.Sprintf("exit status %d", c) }
func (c fakeExitCode) ExitCode() int { return int(c) }

var _ interface{ ExitCode() int } = (*exec.ExitError)(nil)

// systemd 246+ 按规则过滤未匹配到任何单元时，返回退出码 1 且无输出：不应退回全量扫描。
func TestListUnitsNoMatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
	}{
		{"空输出", "", fakeExitCode(1)},
		{"仅空白且包装错误", "\n", fmt.Errorf("systemctl: %w", fakeExitCode(1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := newFastScan(t).scanner(nil)
			var cmds []string
			sc.run = func(cmd string, _ time.Duration) (string, error) {
				cmds = append(cmds, cmd)
				return tc.out, tc.err
			}
			out, err := sc.listUnits()
			if err != nil || out != "" {
				t.Fatalf("0 匹配应直接返回空结果：%q %v", out, err)
			}
			if len(cmds) != 1 {
				t.Errorf("0 匹配不应退回全量枚举：%q", cmds)
			}
		})
	}
}

// 退出码 1 但有错误输出（如老版本拒绝过滤参数）或退出码非 1 时，仍退回全量枚举。
func TestListUnitsFallbackOnRealError(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
	}{
		{"退出码1带报错输出", "Too many arguments.\n", fakeExitCode(1)},
		{"非1退出码空输出", "", fakeExitCode(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := newFastScan(t).scanner(nil)
			var cmds []string
			sc.run = func(cmd string, _ time.Duration) (string, error) {
				cmds = append(cmds, cmd)
				if strings.HasSuffix(cmd, "--no-pager") {
					return "nginx.service enabled disabled\n", nil
				}
				return tc.out, tc.err
			}
			out, err := sc.listUnits()
			if err != nil || !strings.Contains(out, "nginx.service") {
				t.Fatalf("退回全量后：%q %v", out, err)
			}
			if len(cmds) != 2 {
				t.Errorf("应退回全量枚举：%q", cmds)
			}
		})
	}
}

// 请求取消时不退回全量枚举。
func TestListUnitsCanceled(t *testing.T) {
	sc := newFastScan(t).scanner(nil)
	var cmds []string
	sc.run = func(cmd string, _ time.Duration) (string, error) {
		cmds = append(cmds, cmd)
		return "", errCanceled
	}
	_, err := sc.listUnits()
	if !errors.Is(err, errCanceled) {
		t.Fatalf("应返回取消错误：%v", err)
	}
	if len(cmds) != 1 {
		t.Errorf("取消后不应退回全量：%q", cmds)
	}
}

// 同一个容器里同一种软件有多个顶层进程（EMQX 会另起一个 beam）：只列一条。
func TestContainerDuplicateRule(t *testing.T) {
	f := newFastScan(t)
	id := strings.Repeat("a", 64)
	cg := "0::/system.slice/docker-" + id + ".scope\n"
	f.fp.add(t, 1, 0, "/usr/lib/systemd/systemd", "0::/init.scope\n", "net:[1]")
	f.fp.add(t, 300, 1, "/opt/emqx/bin/beam.smp", cg, "net:[2]", "beam.smp -e emqx")
	f.fp.add(t, 400, 1, "/opt/emqx/bin/beam.smp", cg, "net:[2]", "beam.smp -e emqx")
	items := f.scanner(nil).scan().Items
	n := 0
	for _, it := range items {
		if it.Kind != "emqx" {
			continue
		}
		n++
		if it.PID != 300 {
			t.Errorf("留的不是最先看到的那个：%+v", it)
		}
	}
	if n != 1 {
		t.Fatalf("同一容器同一种软件应只列一条：%+v", items)
	}
}

// ss -lntp 一次读出进程的监听地址；认不出的地址不要。
func TestSSListeners(t *testing.T) {
	sc := newFastScan(t).scanner(nil)
	sc.run = func(cmd string, _ time.Duration) (string, error) {
		if cmd != "ss -lntp 2>/dev/null" {
			return "", errors.New("unexpected")
		}
		return "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n" +
			"LISTEN 0 128 0.0.0.0:8080 0.0.0.0:* users:((\"java\",pid=123,fd=45))\n" +
			"LISTEN 0 511 [::]:8000 [::]:* users:((\"nginx\",pid=200,fd=6),(\"nginx\",pid=201,fd=6))\n" +
			"LISTEN 0 128 127.0.0.1:9201 0.0.0.0:* users:((\"java\",pid=123,fd=46))\n" +
			"LISTEN 0 128 [::1]:9000 [::]:* users:((\"minio\",pid=300,fd=3))\n" +
			"LISTEN 0 128 ::ffff:127.0.0.1:8005 [::]:* users:((\"tomcat\",pid=500,fd=8))\n" +
			"LISTEN 0 128 [fe80::1%eth0]:7000 [::]:* users:((\"x\",pid=400,fd=7))\n", nil
	}
	got := sc.ssListeners()
	if !reflect.DeepEqual(got[123], []string{"*:8080", "127.0.0.1:9201"}) {
		t.Errorf("123：%v", got[123])
	}
	if !reflect.DeepEqual(got[200], []string{"*:8000"}) || !reflect.DeepEqual(got[201], []string{"*:8000"}) {
		t.Errorf("同一端口多个进程：%v %v", got[200], got[201])
	}
	if !reflect.DeepEqual(got[300], []string{"[::1]:9000"}) {
		t.Errorf("300：%v", got[300])
	}
	if !reflect.DeepEqual(got[500], []string{"127.0.0.1:8005"}) {
		t.Errorf("IPv4 映射地址：%v", got[500])
	}
	if _, ok := got[400]; ok {
		t.Errorf("带网卡的地址不该认：%v", got[400])
	}
}

// 没有 ss 时返回 nil，调用方退回逐个进程扫 /proc。
func TestSSListenersFallback(t *testing.T) {
	sc := newFastScan(t).scanner(nil)
	sc.run = func(string, time.Duration) (string, error) { return "", errors.New("no ss") }
	if got := sc.ssListeners(); got != nil {
		t.Errorf("没有 ss：%v", got)
	}
}

func TestMergeListenAddrs(t *testing.T) {
	got := mergeListenAddrs([]string{"127.0.0.1:8080", "*:8080", "::1:9000", "127.0.0.1:9000"})
	want := []string{"*:8080", "127.0.0.1:9000", "::1:9000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

func TestDescendants(t *testing.T) {
	children := map[int][]int{1: {10, 20}, 10: {11}, 11: {12}, 20: {21}}
	got := descendants(children, 10)
	if !reflect.DeepEqual(got, map[int]bool{10: true, 11: true, 12: true}) {
		t.Fatalf("%v", got)
	}
}

func TestEachParallel(t *testing.T) {
	var mu sync.Mutex
	done := map[int]bool{}
	eachParallel(20, 3, func(i int) {
		mu.Lock()
		done[i] = true
		mu.Unlock()
	})
	if len(done) != 20 {
		t.Fatalf("应全部做完：%d", len(done))
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "出错了") {
			t.Fatalf("其中一件出错时应在调用方 panic：%v", r)
		}
	}()
	eachParallel(5, 2, func(i int) {
		if i == 3 {
			panic("出错了")
		}
	})
}
