package agent

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// 服务器上已有的软件（software 命令，只读，不改动服务器）：
//   1. 扫描 /proc 里的进程，按程序名 / Java 命令行认出软件；同一软件的子进程（nginx worker、postgres 后端）并到父进程。
//      进程所在的 cgroup 说明它是怎么运行的：systemd 单元、Docker 容器，或直接启动的。
//   2. docker ps -a：补上停止的容器，按镜像名认出进程没认出的容器，取主机端口映射与 1Panel 等标签。
//   3. systemctl list-unit-files：补上装了但没在运行的服务。

// swRule 一种软件怎么认。
type swRule struct {
	kind, name string
	exe        []string // 程序文件名
	java       []string // Java 进程：命令行里有其中之一（主类、jar 名）
	// has 命令行（含程序路径）里还要有其中之一：nginx 是不是 OpenResty、beam.smp 跑的是哪个。
	// exe、java 都为空时只看它（宝塔面板是 python 跑的）
	has    []string
	images []string // 镜像名（去掉仓库地址与标签后的最后一段），也匹配 <名>-xxx
	units  []string // systemd 单元名（不含 .service）；末尾 * 表示前缀
	verArg string   // 取版本：运行 <程序> <verArg>；空 = 不运行程序取版本
	verRe  *regexp.Regexp
}

var (
	reVerNum   = regexp.MustCompile(`(\d+\.\d+(?:\.\d+)*)`)
	reVerMinIO = regexp.MustCompile(`RELEASE\.([0-9TZ:-]+)`)
)

// swRules 认得的软件（顺序 = 列表里的顺序：数据库在前）。
var swRules = []swRule{
	{kind: "mysql", name: "MySQL", exe: []string{"mysqld"}, images: []string{"mysql", "percona"}, units: []string{"mysqld", "mysql"}, verArg: "--version"},
	{kind: "mariadb", name: "MariaDB", exe: []string{"mariadbd"}, images: []string{"mariadb"}, units: []string{"mariadb"}, verArg: "--version"},
	{kind: "postgresql", name: "PostgreSQL", exe: []string{"postgres", "postmaster"}, images: []string{"postgres", "postgis"}, units: []string{"postgresql*"}, verArg: "--version"},
	{kind: "mongodb", name: "MongoDB", exe: []string{"mongod"}, images: []string{"mongo"}, units: []string{"mongod"}, verArg: "--version"},
	{kind: "dm", name: "达梦数据库", exe: []string{"dmserver"}, units: []string{"DmService*"}},
	{kind: "kingbase", name: "人大金仓", exe: []string{"kingbase"}, units: []string{"kingbase*"}},
	{kind: "clickhouse", name: "ClickHouse", exe: []string{"clickhouse-server"}, images: []string{"clickhouse-server", "clickhouse"}, units: []string{"clickhouse-server"}, verArg: "--version"},
	{kind: "elasticsearch", name: "Elasticsearch", java: []string{"org.elasticsearch.bootstrap.Elasticsearch"}, images: []string{"elasticsearch"}, units: []string{"elasticsearch"}},
	{kind: "redis", name: "Redis", exe: []string{"redis-server"}, images: []string{"redis"}, units: []string{"redis", "redis-server", "redis_*"}, verArg: "--version"},
	{kind: "valkey", name: "Valkey", exe: []string{"valkey-server"}, images: []string{"valkey"}, units: []string{"valkey", "valkey-server"}, verArg: "--version"},
	{kind: "memcached", name: "Memcached", exe: []string{"memcached"}, images: []string{"memcached"}, units: []string{"memcached"}, verArg: "-V"},
	{kind: "etcd", name: "etcd", exe: []string{"etcd"}, images: []string{"etcd"}, units: []string{"etcd"}, verArg: "--version"},
	{kind: "nacos", name: "Nacos", java: []string{"nacos-server", "-Dnacos.home=", "com.alibaba.nacos.Nacos"}, images: []string{"nacos-server", "nacos"}, units: []string{"nacos"}},
	{kind: "sentinel", name: "Sentinel 控制台", java: []string{"sentinel-dashboard"}, images: []string{"sentinel-dashboard", "sentinel"}, units: []string{"sentinel*"}},
	{kind: "seata", name: "Seata", java: []string{"io.seata.server.ServerApplication", "seata-server"}, images: []string{"seata-server"}, units: []string{"seata*"}},
	{kind: "xxljob", name: "XXL-JOB 调度中心", java: []string{"xxl-job-admin"}, images: []string{"xxl-job-admin"}, units: []string{"xxl-job*"}},
	{kind: "rabbitmq", name: "RabbitMQ", exe: []string{"beam.smp"}, has: []string{"rabbit"}, images: []string{"rabbitmq"}, units: []string{"rabbitmq-server"}},
	{kind: "emqx", name: "EMQX", exe: []string{"beam.smp"}, has: []string{"emqx"}, images: []string{"emqx"}, units: []string{"emqx"}},
	{kind: "kafka", name: "Kafka", java: []string{"kafka.Kafka"}, images: []string{"kafka"}, units: []string{"kafka"}},
	{kind: "zookeeper", name: "ZooKeeper", java: []string{"org.apache.zookeeper.server.quorum.QuorumPeerMain"}, images: []string{"zookeeper"}, units: []string{"zookeeper"}},
	{kind: "rocketmq", name: "RocketMQ", java: []string{"org.apache.rocketmq.namesrv.NamesrvStartup", "org.apache.rocketmq.broker.BrokerStartup"}, images: []string{"rocketmq"}, units: []string{"rocketmq*"}},
	{kind: "minio", name: "MinIO", exe: []string{"minio"}, images: []string{"minio"}, units: []string{"minio"}, verArg: "--version", verRe: reVerMinIO},
	{kind: "zlmediakit", name: "ZLMediaKit", exe: []string{"MediaServer"}, images: []string{"zlmediakit"}},
	{kind: "openresty", name: "OpenResty", exe: []string{"nginx", "openresty"}, has: []string{"openresty"}, images: []string{"openresty"}, units: []string{"openresty"}, verArg: "-v"},
	{kind: "nginx", name: "Nginx", exe: []string{"nginx"}, images: []string{"nginx"}, units: []string{"nginx"}, verArg: "-v"},
	{kind: "httpd", name: "Apache", exe: []string{"httpd", "apache2"}, images: []string{"httpd"}, units: []string{"httpd", "apache2"}, verArg: "-v"},
	{kind: "haproxy", name: "HAProxy", exe: []string{"haproxy"}, images: []string{"haproxy"}, units: []string{"haproxy"}, verArg: "-v"},
	{kind: "tomcat", name: "Tomcat", java: []string{"org.apache.catalina.startup.Bootstrap"}, images: []string{"tomcat"}, units: []string{"tomcat*"}},
	{kind: "jenkins", name: "Jenkins", java: []string{"jenkins.war"}, images: []string{"jenkins"}, units: []string{"jenkins"}},
	{kind: "frps", name: "frp 服务端", exe: []string{"frps"}, images: []string{"frps"}, units: []string{"frps"}, verArg: "-v"},
	{kind: "frpc", name: "frp 客户端", exe: []string{"frpc"}, images: []string{"frpc"}, units: []string{"frpc"}, verArg: "-v"},
	{kind: "prometheus", name: "Prometheus", exe: []string{"prometheus"}, images: []string{"prometheus"}, units: []string{"prometheus"}, verArg: "--version"},
	{kind: "grafana", name: "Grafana", exe: []string{"grafana-server", "grafana"}, images: []string{"grafana", "grafana-oss"}, units: []string{"grafana-server", "grafana"}, verArg: "-v"},
	{kind: "docker", name: "Docker", exe: []string{"dockerd"}, units: []string{"docker"}, verArg: "--version"},
	// 版本：1panel-core version（从它自己的数据库读）；1panel-agent 用同目录的 1panel-core
	{kind: "1panel", name: "1Panel 面板", exe: []string{"1panel", "1panel-core", "1panel-agent"}, units: []string{"1panel", "1panel-core", "1panel-agent"}, verArg: "version"},
	{kind: "bt", name: "宝塔面板", has: []string{"BT-Panel"}, units: []string{"bt"}},
}

// swProc /proc 里的一个进程。
type swProc struct {
	pid, ppid int
	exe       string   // 程序路径（容器里的进程为容器内路径）
	args      []string // 命令行
	unit      string   // 所在的 systemd 单元
	container string   // 所在的 Docker 容器 ID
	rule      int      // 认出的软件（swRules 下标），-1 = 不认识
}

// swScan 一次查找。依赖的系统操作都是字段，测试时换成假的。
type swScan struct {
	proc      string // /proc
	unitDir   string // 判断单元是不是 Ferry 生成的
	run       func(cmd string, timeout time.Duration) (string, error)
	unitProps func(unit string) UnitProps
	readlink  func(string) (string, error)
	listening func(pid int) []string
	instances []proto.SoftwareInstance // Ferry 装的软件
	appUnits  map[string][3]string     // Ferry 部署的应用的单元：里面的进程属于应用，不算软件

	procs   map[int]*swProc
	hostNet string            // /proc/1/ns/net：判断容器是不是用主机网络
	vers    map[string]string // 程序路径 → 版本（同一个程序只运行一次）
}

func (a *Agent) software() int {
	sc := &swScan{
		proc:      "/proc",
		unitDir:   a.Sys.UnitDir(),
		run:       a.Sys.Shell,
		unitProps: a.Sys.UnitProps,
		readlink:  os.Readlink,
		listening: func(pid int) []string { return listeningFromProc("/proc", pid) },
		instances: a.instances(),
		appUnits:  a.managedUnits(),
	}
	rep := sc.scan()
	// 安装软件时用：服务器架构、已有的 JDK、各目录的剩余空间、已占用的端口
	rep.Arch = goruntime.GOARCH
	rep.OS = osInfo("/etc/os-release")
	rep.Runtimes = a.installedRuntimes()
	rep.FerryDir = a.ferryDir()
	for _, d := range []string{"/opt", "/data", "/home", "/srv", "/www"} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			rep.Disks = append(rep.Disks, proto.Disk{Path: d, FreeMB: diskFreeMB(d)})
		}
	}
	for p := range listeningPorts() {
		rep.Ports = append(rep.Ports, p)
	}
	sort.Ints(rep.Ports)
	return a.result(proto.CodeOK, "", rep)
}

func (sc *swScan) scan() *proto.SoftwareReport {
	rep := &proto.SoftwareReport{Items: []proto.Software{}}
	sc.vers = map[string]string{}
	sc.readProcs()
	sc.hostNet, _ = sc.readlink(filepath.Join(sc.proc, "1", "ns", "net"))

	// 1. 进程：每种软件只取最上层的进程（父进程是同一种软件的不算）
	byContainer := map[string][]int{} // 容器 → 其中认出的软件（items 下标）
	pids := make([]int, 0, len(sc.procs))
	for pid := range sc.procs {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		p := sc.procs[pid]
		if p.rule < 0 {
			continue
		}
		if pp := sc.procs[p.ppid]; pp != nil && pp.rule == p.rule {
			continue
		}
		if _, ok := sc.appUnits[p.unit]; ok && p.unit != "" {
			continue // 前端站点自带的 nginx 等：在「应用」里看
		}
		it := sc.fromProc(p)
		if p.container != "" {
			byContainer[p.container] = append(byContainer[p.container], len(rep.Items))
		}
		rep.Items = append(rep.Items, it)
	}

	// 2. Docker 容器
	rep.Docker = sc.docker(rep, byContainer)

	// 3. 装了但没在运行（或进程没认出来）的系统服务
	sc.units(rep)

	// 4. Ferry 装的软件：标上实例名（可以启停、卸载）；没在运行的也列出来
	for _, in := range sc.instances {
		i := -1
		for j := range rep.Items {
			if rep.Items[j].Unit == in.Unit {
				i = j
				break
			}
		}
		if i < 0 {
			p := sc.unitProps(in.Unit)
			it := proto.Software{State: "stopped", Via: "systemd", Unit: in.Unit, PID: p.PID}
			if p.Active == "active" || p.Active == "activating" || p.Active == "reloading" {
				it.State = "running"
			}
			if p.PID > 0 {
				it.Since, it.Listen = sc.startTime(p.PID), sc.listening(p.PID)
			}
			rep.Items = append(rep.Items, it)
			i = len(rep.Items) - 1
		}
		it := &rep.Items[i]
		it.Kind, it.Name, it.Manager, it.Instance, it.Dir = in.Kind, in.Title, "Ferry", in.Name, in.Dir
		if in.Version != "" {
			it.Version = in.Version
		}
		it.Hosting = in.Hosting
		if it.Name == "" {
			it.Name = in.Name
		}
	}

	// Docker 那一行不列容器的端口：那是 docker-proxy 替容器监听的，容器那一行已经有了
	published := map[string]bool{}
	for _, it := range rep.Items {
		if it.Via == "docker" {
			for _, l := range it.Listen {
				published[l] = true
			}
		}
	}
	for i := range rep.Items {
		if it := &rep.Items[i]; it.Kind == "docker" {
			var own []string
			for _, l := range it.Listen {
				if !published[l] {
					own = append(own, l)
				}
			}
			it.Listen = own
		}
	}

	order := func(it proto.Software) int {
		for i, r := range swRules {
			if r.kind == it.Kind {
				return i
			}
		}
		return len(swRules)
	}
	sort.SliceStable(rep.Items, func(i, j int) bool {
		a, b := rep.Items[i], rep.Items[j]
		if oa, ob := order(a), order(b); oa != ob {
			return oa < ob
		}
		if (a.State == "running") != (b.State == "running") {
			return a.State == "running"
		}
		return a.Name+a.Unit+a.Container < b.Name+b.Unit+b.Container
	})
	if os.Geteuid() != 0 && sc.proc == "/proc" {
		rep.Notes = append(rep.Notes, "agent 没有以 root 运行，其他用户的进程可能看不到")
	}
	return rep
}

// readProcs 读出全部进程，认出其中的软件。
func (sc *swScan) readProcs() {
	sc.procs = map[int]*swProc{}
	ents, _ := os.ReadDir(sc.proc)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(sc.proc, e.Name())
		cmd, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		if len(cmd) == 0 {
			continue // 内核线程
		}
		p := &swProc{pid: pid, args: strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00"), rule: -1}
		if stat, err := os.ReadFile(filepath.Join(dir, "stat")); err == nil {
			if f := statFields(string(stat)); len(f) >= 2 {
				p.ppid, _ = strconv.Atoi(f[1])
			}
		}
		if exe, err := sc.readlink(filepath.Join(dir, "exe")); err == nil {
			p.exe = strings.TrimSuffix(exe, " (deleted)")
		} else {
			p.exe = p.args[0]
		}
		if cg, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
			p.unit, p.container = cgroupOwner(string(cg))
		}
		p.rule = matchRule(p.exe, p.args)
		sc.procs[pid] = p
	}
}

// statFields /proc/<pid>/stat 里 (comm) 之后的字段：state ppid …（comm 里可能有空格和括号）。
func statFields(s string) []string {
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		s = s[i+1:]
	}
	return strings.Fields(s)
}

// matchRule 进程是哪种软件；-1 = 不认识。
func matchRule(exe string, args []string) int {
	base := path.Base(exe)
	if base == "." || base == "/" {
		return -1
	}
	line := exe + " " + strings.Join(args, " ")
	isJava := base == "java" || (len(args) > 0 && path.Base(args[0]) == "java")
	for i, r := range swRules {
		switch {
		case len(r.java) > 0:
			if isJava && containsAny(line, r.java) {
				return i
			}
		case len(r.exe) > 0:
			if contains(r.exe, base) && (len(r.has) == 0 || containsAny(line, r.has)) {
				return i
			}
		case len(r.has) > 0:
			if containsAny(line, r.has) {
				return i
			}
		}
	}
	return -1
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, v := range subs {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

var reContainerID = regexp.MustCompile(`^(?:docker-|libpod-|cri-containerd-)?([0-9a-f]{64})(?:\.scope)?$`)

// cgroupOwner 从 /proc/<pid>/cgroup 看进程由谁运行：systemd 单元，或 Docker 容器（取最里层的）。
func cgroupOwner(cg string) (unit, container string) {
	var p string
	for _, line := range strings.Split(cg, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "": // cgroup v2
			p = parts[2]
		case parts[1] == "name=systemd" && p == "": // cgroup v1
			p = parts[2]
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if m := reContainerID.FindStringSubmatch(seg); m != nil {
			unit, container = "", m[1]
			continue
		}
		if strings.HasSuffix(seg, ".service") && !strings.HasPrefix(seg, "user@") {
			unit, container = seg, ""
		}
	}
	return unit, container
}

// fromProc 一个认出的进程 → 软件。
func (sc *swScan) fromProc(p *swProc) proto.Software {
	r := swRules[p.rule]
	it := proto.Software{Kind: r.kind, Name: r.name, State: "running", PID: p.pid, Since: sc.startTime(p.pid)}
	switch {
	case p.container != "":
		it.Via, it.Container = "docker", shortID(p.container)
	case p.unit != "":
		it.Via, it.Unit = "systemd", p.unit
		if sc.ferryUnit(p.unit) {
			it.Manager = "Ferry"
		}
	default:
		it.Via = "process"
	}
	listen := sc.listening(p.pid)
	if p.container != "" && !sc.sameNet(p.pid) {
		it.Inner = listen // 容器自己的网络：主机端口看 docker 的端口映射
	} else {
		it.Listen = listen
	}
	if len(r.java) > 0 {
		it.Path, it.Version = sc.javaInfo(p)
		if it.Version == "" && p.container == "" && it.Path != p.exe {
			it.Version = sc.pkgVersion(it.Path)
		}
	} else {
		it.Path = p.exe
		if p.container == "" { // 容器里的程序在主机上运行不了，版本看镜像标签
			it.Version = sc.version(r, p.exe)
		}
	}
	if it.Kind == "mysql" && strings.Contains(sc.vers[p.exe+"\x00raw"], "MariaDB") {
		it.Kind, it.Name = "mariadb", "MariaDB"
	}
	if strings.HasPrefix(it.Path, "/www/server/") && it.Manager == "" {
		it.Manager = "宝塔面板"
	}
	return it
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// sameNet 进程是否在主机的网络里（容器用 host 网络时，它监听的就是主机端口）。
func (sc *swScan) sameNet(pid int) bool {
	ns, err := sc.readlink(filepath.Join(sc.proc, strconv.Itoa(pid), "ns", "net"))
	return err == nil && sc.hostNet != "" && ns == sc.hostNet
}

// startTime 进程的启动时间（RFC3339）：stat 第 22 个字段（开机后的时钟周期数，按每秒 100 个）+ 开机时间。
func (sc *swScan) startTime(pid int) string {
	stat, err := os.ReadFile(filepath.Join(sc.proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	f := statFields(string(stat))
	if len(f) < 20 {
		return ""
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return ""
	}
	st, err := os.ReadFile(filepath.Join(sc.proc, "stat"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(st), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			bt, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return ""
			}
			return time.Unix(bt+ticks/100, 0).Format(time.RFC3339)
		}
	}
	return ""
}

// ferryUnit 单元文件是不是 Ferry 生成的。
func (sc *swScan) ferryUnit(unit string) bool {
	b, err := os.ReadFile(filepath.Join(sc.unitDir, unit))
	return err == nil && strings.HasPrefix(string(b), ManagedMarker)
}

// version 主机上的程序的版本：运行 <程序> <verArg>；取不到时看它属于哪个软件包。
func (sc *swScan) version(r swRule, exe string) string {
	if !strings.HasPrefix(exe, "/") {
		return ""
	}
	if v, ok := sc.vers[exe]; ok {
		return v
	}
	v, out := "", ""
	if r.verArg != "" {
		bin := exe
		if path.Base(exe) == "1panel-agent" { // 没有 version 命令（运行它会再启动一个）
			bin = path.Join(path.Dir(exe), "1panel-core")
		}
		out, _ = sc.run(shellQuote(bin)+" "+r.verArg+" 2>&1", 5*time.Second)
		re := r.verRe
		if re == nil {
			re = reVerNum
		}
		if m := re.FindStringSubmatch(out); m != nil {
			v = m[1]
		}
	}
	if v == "" {
		v = sc.pkgVersion(exe)
	}
	sc.vers[exe], sc.vers[exe+"\x00raw"] = v, out
	return v
}

// pkgVersion 文件所属软件包的版本（rpm 或 dpkg）；不属于任何软件包为空。
func (sc *swScan) pkgVersion(file string) string {
	if !strings.HasPrefix(file, "/") {
		return ""
	}
	k := "\x00pkg" + file
	if v, ok := sc.vers[k]; ok {
		return v
	}
	q := shellQuote(file)
	cmd := `rpm -qf --qf '%{VERSION}\n' ` + q + ` 2>/dev/null || { p=$(dpkg -S ` + q + ` 2>/dev/null | head -n 1 | sed 's/: .*//'); ` +
		`[ -n "$p" ] && dpkg-query -W -f='${Version}\n' "$p"; }`
	out, _ := sc.run(cmd, 5*time.Second)
	v := ""
	// 不属于软件包时 rpm 输出 file … is not owned by any package
	if lines := splitLines(out); len(lines) > 0 && !strings.Contains(lines[0], " ") {
		if m := reVerNum.FindStringSubmatch(lines[0]); m != nil {
			v = m[1]
		}
	}
	sc.vers[k] = v
	return v
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var reJarVer = regexp.MustCompile(`-v?(\d+(?:\.\d+)+)(?:[-.][0-9A-Za-z]+)*\.[jw]ar$`)

// javaInfo Java 软件的位置（jar，或 -D….home 指的目录）与版本（jar 文件名或 MANIFEST 里的）。
func (sc *swScan) javaInfo(p *swProc) (where, ver string) {
	jar := ""
	for i, a := range p.args {
		if a == "-jar" && i+1 < len(p.args) {
			jar = p.args[i+1]
			break
		}
	}
	if jar == "" {
		for _, a := range p.args {
			if strings.HasPrefix(a, "-D") && strings.Contains(a, ".home=") {
				_, where, _ = strings.Cut(a, "=")
				return where, ""
			}
		}
		return p.exe, ""
	}
	if m := reJarVer.FindStringSubmatch(jar); m != nil {
		return jar, m[1]
	}
	// 从进程的根目录读 jar：容器里的进程也能读到
	full := jar
	if !strings.HasPrefix(full, "/") {
		if cwd, err := sc.readlink(filepath.Join(sc.proc, strconv.Itoa(p.pid), "cwd")); err == nil {
			full = path.Join(cwd, full)
		}
	}
	return jar, manifestVersion(filepath.Join(sc.proc, strconv.Itoa(p.pid), "root", filepath.FromSlash(full)))
}

// manifestVersion jar 里 META-INF/MANIFEST.MF 的 Implementation-Version。
func manifestVersion(jar string) string {
	z, err := zip.OpenReader(jar)
	if err != nil {
		return ""
	}
	defer z.Close()
	for _, f := range z.File {
		if f.Name != "META-INF/MANIFEST.MF" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ""
		}
		defer rc.Close()
		s := bufio.NewScanner(rc)
		for s.Scan() {
			if v, ok := strings.CutPrefix(s.Text(), "Implementation-Version:"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------- Docker

// dockerPS docker ps --format '{{json .}}' 的一行。
type dockerPS struct {
	ID, Names, Image, State, Status, Ports, Labels string
}

// docker 读容器列表：给进程认出的软件补上容器信息，再补上其他容器。返回 SoftwareReport.Docker。
func (sc *swScan) docker(rep *proto.SoftwareReport, byContainer map[string][]int) string {
	if _, err := sc.run("command -v docker", 5*time.Second); err != nil {
		if len(byContainer) > 0 {
			rep.Notes = append(rep.Notes, "有软件在容器里运行，但服务器上没有 docker 命令，读不到容器名与端口映射")
		}
		return ""
	}
	out, err := sc.run("docker ps -a --no-trunc --format '{{json .}}'", 15*time.Second)
	if err != nil {
		msg := strings.TrimSpace(out)
		if strings.Contains(msg, "Cannot connect to the Docker daemon") || strings.Contains(msg, "Is the docker daemon running") {
			return "Docker 没有运行"
		}
		if msg == "" {
			msg = err.Error()
		}
		return "读取容器列表失败：" + msg
	}
	// 容器的顶层进程：没认出软件的运行中容器也要 PID、端口、启动时间
	top := map[string]int{}
	for pid, p := range sc.procs {
		if p.container == "" {
			continue
		}
		if pp := sc.procs[p.ppid]; pp != nil && pp.container == p.container {
			continue
		}
		if cur, ok := top[p.container]; !ok || pid < cur {
			top[p.container] = pid
		}
	}
	for _, line := range splitLines(out) {
		var c dockerPS
		if json.Unmarshal([]byte(line), &c) != nil || c.ID == "" {
			continue
		}
		name := strings.TrimPrefix(strings.Split(c.Names, ",")[0], "/")
		repo, tag := splitImage(c.Image)
		manager := containerManager(name, c.Labels)
		running := c.State == "running" || (c.State == "" && strings.HasPrefix(c.Status, "Up"))
		ports := dockerPorts(c.Ports)
		fill := func(it *proto.Software) {
			it.Via, it.Container, it.Image, it.Status = "docker", name, c.Image, c.Status
			if manager != "" {
				it.Manager = manager
			}
			if it.Version == "" && tag != "" && tag != "latest" {
				it.Version = tag
			}
			if len(it.Inner) > 0 || len(it.Listen) == 0 {
				it.Listen = ports
			}
		}
		if idx, ok := byContainer[c.ID]; ok {
			for _, i := range idx {
				// 旧版 MariaDB 镜像里的程序也叫 mysqld
				if r := imageRule(repo); r >= 0 && swRules[r].kind == "mariadb" && rep.Items[i].Kind == "mysql" {
					rep.Items[i].Kind, rep.Items[i].Name = "mariadb", "MariaDB"
				}
				fill(&rep.Items[i])
			}
			continue
		}
		it := proto.Software{Name: repo, State: "stopped"}
		if r := imageRule(repo); r >= 0 {
			it.Kind, it.Name = swRules[r].kind, swRules[r].name
		}
		if it.Name == "" {
			it.Name = name
		}
		if running {
			it.State = "running"
			if pid := top[c.ID]; pid > 0 {
				it.PID, it.Since = pid, sc.startTime(pid)
				if l := sc.listening(pid); sc.sameNet(pid) {
					it.Listen = l
				} else {
					it.Inner = l
				}
			}
		}
		fill(&it)
		rep.Items = append(rep.Items, it)
	}
	return "ok"
}

// splitImage registry:5000/a/mysql:8.4@sha256:… → mysql、8.4。
func splitImage(img string) (repo, tag string) {
	img, _, _ = strings.Cut(img, "@")
	if strings.HasPrefix(img, "sha256:") {
		return "", ""
	}
	last := img[strings.LastIndexByte(img, '/')+1:]
	repo, tag, _ = strings.Cut(last, ":")
	return repo, tag
}

// imageRule 镜像名是哪种软件。
func imageRule(repo string) int {
	repo = strings.ToLower(repo)
	for i, r := range swRules {
		for _, p := range r.images {
			if repo == p || strings.HasPrefix(repo, p+"-") || strings.HasPrefix(repo, p+"_") {
				return i
			}
		}
	}
	return -1
}

// containerManager 容器是用什么装的：1Panel 的应用商店，或某个 docker compose 项目。
func containerManager(name, labels string) string {
	l := parseLabels(labels)
	if l["createdBy"] == "1Panel" || strings.HasPrefix(name, "1Panel-") {
		return "1Panel"
	}
	if p := l["com.docker.compose.project"]; p != "" {
		return "Docker Compose（" + p + "）"
	}
	return ""
}

// parseLabels a=b,c=d（值里可能有逗号：没有 = 的部分接到上一个值后面）。
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	last := ""
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			if last != "" {
				out[last] += "," + part
			}
			continue
		}
		out[k], last = v, k
	}
	return out
}

// dockerPorts 0.0.0.0:3306->3306/tcp, :::3306->3306/tcp, 33060/tcp → *:3306（只要映射到主机的）。
func dockerPorts(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		host, _, ok := strings.Cut(strings.TrimSpace(part), "->")
		if !ok {
			continue
		}
		suffix := "" // tcp 不写，其他（udp）写上
		if _, p, ok := strings.Cut(part, "/"); ok && strings.TrimSpace(p) != "tcp" {
			suffix = "/" + strings.TrimSpace(p)
		}
		i := strings.LastIndexByte(host, ':')
		if i < 0 {
			continue
		}
		addr, port := host[:i], host[i+1:]
		switch addr {
		case "0.0.0.0", "::", "[::]", "":
			addr = "*"
		}
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]"
		}
		a := addr + ":" + port + suffix
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// ---------------------------------------------------------------- systemd

// units 补上装了、但进程里没找到的系统服务（停止的，或进程没认出来的）。
func (sc *swScan) units(rep *proto.SoftwareReport) {
	out, err := sc.run("systemctl list-unit-files --type=service --no-legend --no-pager", 15*time.Second)
	if err != nil {
		return
	}
	have := map[string]bool{}
	pids := map[int]bool{}
	for _, it := range rep.Items {
		have[it.Unit] = true
		pids[it.PID] = true
	}
	for _, line := range splitLines(out) {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasSuffix(f[0], ".service") || strings.Contains(f[0], "@") || have[f[0]] {
			continue
		}
		if _, ok := sc.appUnits[f[0]]; ok {
			continue
		}
		if len(f) > 1 && (f[1] == "masked" || f[1] == "alias") {
			continue
		}
		r := unitRule(strings.TrimSuffix(f[0], ".service"))
		if r < 0 {
			continue
		}
		p := sc.unitProps(f[0])
		if p.Load == "not-found" || (p.PID > 0 && pids[p.PID]) {
			continue
		}
		it := proto.Software{Kind: swRules[r].kind, Name: swRules[r].name, State: "stopped", Via: "systemd", Unit: f[0]}
		if sc.ferryUnit(f[0]) {
			it.Manager = "Ferry"
		}
		if p.Active == "active" || p.Active == "activating" || p.Active == "reloading" {
			it.State = "running"
		}
		if p.PID > 0 {
			it.PID, it.Since = p.PID, sc.startTime(p.PID)
			it.Listen = sc.listening(p.PID)
			pids[p.PID] = true
		}
		if exe := sc.execStart(f[0]); exe != "" {
			it.Path = exe
			it.Version = sc.version(swRules[r], exe)
		}
		if strings.HasPrefix(it.Path, "/www/server/") && it.Manager == "" {
			it.Manager = "宝塔面板"
		}
		have[f[0]] = true
		rep.Items = append(rep.Items, it)
	}
}

// unitRule 单元名是哪种软件。
func unitRule(name string) int {
	for i, r := range swRules {
		for _, u := range r.units {
			if p, ok := strings.CutSuffix(u, "*"); ok {
				if strings.HasPrefix(name, p) {
					return i
				}
			} else if name == u {
				return i
			}
		}
	}
	return -1
}

var reExecPath = regexp.MustCompile(`path=([^ ;]+)`)

// execStart 单元 ExecStart 的程序。
func (sc *swScan) execStart(unit string) string {
	out, err := sc.run("systemctl show -p ExecStart --no-pager "+shellQuote(unit), 5*time.Second)
	if err != nil {
		return ""
	}
	if m := reExecPath.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// osInfo /etc/os-release：选模板的装法用。
func osInfo(file string) *proto.OSInfo {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	o := &proto.OSInfo{}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			o.ID = v
		case "ID_LIKE":
			o.Like = strings.Fields(v)
		case "VERSION_ID":
			o.Version = v
		case "PRETTY_NAME":
			o.Pretty = v
		}
	}
	return o
}
