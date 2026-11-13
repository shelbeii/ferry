package agent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/notify"
	"github.com/captain/ferry/internal/proto"
)

// 测试里不读这台机器真的 /proc（内存、负载随机器变，会误报）：要测的地方自己放假的
func init() { procDir = filepath.Join(os.TempDir(), "ferry-test-no-proc") }

// probeSys 健康检查结果可控的 fakeSys。
type probeSys struct {
	*fakeSys
	down   bool // 健康检查不通
	probes int
}

func (p *probeSys) Probe(proto.Health) error {
	p.probes++
	if p.down {
		return errors.New("http://127.0.0.1:8080/health：connection refused")
	}
	return nil
}

func TestMonitorHealthCheck(t *testing.T) {
	f := newFake(t)
	sys := &probeSys{fakeSys: f}
	unit := "shop-api.service"
	os.WriteFile(filepath.Join(f.unitDir, unit), []byte(ManagedMarker+" — 由 ferry 生成\n# project=shop app=api target=prod\n[Service]\n"), 0o644)
	f.shellOut = map[string]string{"systemctl is-enabled 'shop-api.service' 2>/dev/null": "enabled\n"}
	f.units[unit] = &fakeUnit{active: "active", sub: "running", pid: 10}
	a := &Agent{Sys: sys, Out: io.Discard, req: &proto.Request{Root: f.root, FerryDir: t.TempDir()}}
	h := proto.Health{HTTP: "http://127.0.0.1:8080/health"}
	a.req.Monitor = &proto.MonitorSpec{Action: "on", Project: "shop", Target: "prod",
		Channels: []proto.MonitorChannel{{Name: "群", Kind: "webhook", URL: "http://x"}},
		Apps:     []proto.MonitorApp{{App: "api", Unit: unit, Health: h}}}
	if rc := a.monitor(); rc != 0 {
		t.Fatal("打开监控失败")
	}
	var sent []notify.Event
	send := func(_ []proto.MonitorChannel, ev notify.Event) map[string]string {
		sent = append(sent, ev)
		return nil
	}
	a.monitorRun(send)
	if len(sent) != 0 || sys.probes != 1 {
		t.Fatalf("通的时候探一次、不发：%d %+v", sys.probes, sent)
	}

	// 进程在、接口不通：探 3 次都不通才告警
	sys.down, sys.probes = true, 0
	a.monitorRun(send)
	if sys.probes != 3 || len(sent) != 1 || !strings.Contains(sent[0].Lines[0], "应用 api 在运行，但健康检查不通过") {
		t.Fatalf("%d %+v", sys.probes, sent)
	}
	sys.down = false
	a.monitorRun(send)
	if len(sent) != 2 || sent[1].Action != "recovered" {
		t.Fatalf("%+v", sent)
	}

	// 部署时记下的健康检查优先（换了单元：用新的那个；记下的单元不是它时用打开告警时传来的）
	a.saveAppHealth("api", unit, proto.Health{TCP: "127.0.0.1:9000"})
	cfg, _ := a.loadMonitor()
	if got, ok := healthOf(cfg.Entries[0], "api", unit); !ok || got.TCP != "127.0.0.1:9000" {
		t.Fatalf("%+v", got)
	}
	a.saveAppHealth("api", "shop-api-b.service", proto.Health{TCP: "127.0.0.1:9001"})
	if got, _ := healthOf(cfg.Entries[0], "api", unit); got.HTTP != h.HTTP {
		t.Fatalf("记下的是别的槽位：%+v", got)
	}
	// 没有健康检查：删掉记录，也不用告警时传来的以外的
	a.saveAppHealth("api", unit, proto.Health{})
	if exists(healthFile(a.meta(), "api")) {
		t.Fatal("没有健康检查时应删掉记录")
	}
	if _, ok := healthOf(monitorEntry{Root: f.root}, "api", unit); ok {
		t.Fatal("什么都没有时不检查")
	}
}

func TestMonitorResources(t *testing.T) {
	f := newFake(t)
	proc := t.TempDir()
	old, oldCPU := procDir, cpuCount
	procDir, cpuCount = proc, func() int { return 2 }
	defer func() { procDir, cpuCount = old, oldCPU }()
	os.WriteFile(filepath.Join(proc, "meminfo"), []byte("MemTotal:        8000000 kB\nMemFree:          100000 kB\nMemAvailable:     400000 kB\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "loadavg"), []byte("9.10 6.20 5.00 3/400 1234\n"), 0o644)
	os.MkdirAll(filepath.Join(proc, "42"), 0o755)
	os.WriteFile(filepath.Join(proc, "42", "status"), []byte("Name:\tjava\nVmRSS:\t 6291456 kB\n"), 0o644)
	os.MkdirAll(filepath.Join(proc, "43"), 0o755)
	os.WriteFile(filepath.Join(proc, "43", "status"), []byte("Name:\tmysqld\nVmRSS:\t 1048576 kB\n"), 0o644)
	f.now = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	since := f.now.Add(-5 * time.Minute)
	oom := "journalctl -k --since=@" + strconv.FormatInt(since.Unix(), 10) + " --no-pager -q -o cat 2>/dev/null | grep -E 'Out of memory|out of memory' | tail -n 20"
	f.shellOut = map[string]string{oom: "Out of memory: Killed process 4321 (java) total-vm:9000000kB, anon-rss:3355443kB, file-rss:0kB, shmem-rss:0kB, UID:1000 pgtables:8000kB oom_score_adj:0\n"}
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root}}

	got := a.resourceFindings([]monitorEntry{{MonitorSpec: proto.MonitorSpec{MemPct: 90, LoadX: 2}}}, since)
	text := ""
	for _, g := range got {
		text += g.Key + "：" + g.Text + "\n"
	}
	for _, want := range []string{"mem：内存用了 95%", "java 6.00 GB、mysqld 1.00 GB", "load：负载太高：15 分钟平均 5.0，是 CPU 核数（2）的 2.5 倍", "系统杀掉了进程：java（pid 4321，占 3.20 GB）"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少 %q：\n%s", want, text)
		}
	}
	// 阈值放宽：都不报（OOM 照报）
	got = a.resourceFindings([]monitorEntry{{MonitorSpec: proto.MonitorSpec{MemPct: 99, LoadX: 3}}}, since)
	if len(got) != 1 || got[0].Key != "" {
		t.Fatalf("%+v", got)
	}
}

func TestMonitorPing(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root, FerryDir: t.TempDir()}}
	a.req.Monitor = &proto.MonitorSpec{Action: "on", Project: "shop", Target: "prod", PingURL: "ftp://x",
		Channels: []proto.MonitorChannel{{Name: "群", Kind: "webhook", URL: "http://x"}}}
	if rc := a.monitor(); rc == 0 {
		t.Fatal("心跳地址不是 http(s) 时应拒绝")
	}
	a.req.Monitor.PingURL = "https://hc-ping.com/abc"
	if rc := a.monitor(); rc != 0 {
		t.Fatal("打开监控失败")
	}
	var pinged []string
	fail := false
	old := pingURL
	pingURL = func(u string) error {
		pinged = append(pinged, u)
		if fail {
			return errors.New("心跳地址返回 503")
		}
		return nil
	}
	defer func() { pingURL = old }()
	send := func([]proto.MonitorChannel, notify.Event) map[string]string { return nil }
	a.monitorRun(send)
	if len(pinged) != 1 || pinged[0] != "https://hc-ping.com/abc" {
		t.Fatalf("%v", pinged)
	}
	fail = true
	a.monitorRun(send)
	cfg, st := a.loadMonitor()
	s := a.monitorStatus(cfg, st, "shop/prod")
	if s.PingURL != "https://hc-ping.com/abc" || s.PingError != "心跳地址返回 503" || s.MemPct != 90 || s.LoadX != 2 {
		t.Fatalf("%+v", s)
	}
	fail = false
	a.monitorRun(send)
	_, st = a.loadMonitor()
	if s := a.monitorStatus(cfg, st, "shop/prod"); s.PingError != "" {
		t.Fatalf("成功后清掉错误：%+v", s)
	}
}

func TestShortErrPlain(t *testing.T) {
	for in, want := range map[string]string{
		`Get "http://127.0.0.1:18080/health": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`: "超时，没有响应",
		`dial tcp 127.0.0.1:8080: connect: connection refused`:                                                            "连不上（没有程序在监听这个端口）",
		`read tcp 127.0.0.1:1->127.0.0.1:2: read: connection reset by peer`:                                               "连接被对方断开",
		`dial tcp: lookup nope.invalid: no such host`:                                                                     "域名解析不了",
		`x: something else`: "something else",
	} {
		if got := shortErr(errors.New(in)); got != want {
			t.Errorf("%s：%s", in, got)
		}
	}
}
