package agent

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// serverInfoState 常驻 agent 跨请求共用的服务器信息状态：
// 1. 启动时立即记录一次 /proc/stat 基线，首次打开「服务器信息」页时无需再硬等 sleep 1；
// 2. 缓存不变的静态字段（虚拟化类型、lscpu 型号），避免每次刷新派生子进程。
type serverInfoState struct {
	mu      sync.Mutex
	cpuPrev []string
	cpuAt   time.Time

	staticOnce sync.Once
	virt       string
	lscpuModel string
}

// prime 在常驻 agent 启动时采一次 /proc/stat 基线（仅读一个 proc 文件，耗时不到 1 ms）。
func (s *serverInfoState) prime(proc string) {
	if s == nil {
		return
	}
	lines := readCPUStat(proc)
	if len(lines) == 0 {
		return
	}
	s.mu.Lock()
	if s.cpuPrev == nil {
		s.cpuPrev, s.cpuAt = lines, time.Now()
	}
	s.mu.Unlock()
}

func (s *serverInfoState) prevCPU() ([]string, time.Time) {
	if s == nil {
		return nil, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cpuPrev, s.cpuAt
}

func (s *serverInfoState) setCPU(lines []string, at time.Time) {
	if s == nil || len(lines) == 0 {
		return
	}
	s.mu.Lock()
	s.cpuPrev, s.cpuAt = lines, at
	s.mu.Unlock()
}

type mountStat struct {
	bsize  uint64
	blocks uint64
	bfree  uint64
	bavail uint64
	files  uint64
	ffree  uint64
}

type serverInfoProbe struct {
	proc      string
	osFiles   []string
	state     *serverInfoState
	now       func() time.Time
	sleep     func(time.Duration)
	hostname  func() (string, error)
	uname     func() (kernel, arch, nodename string)
	statMount func(string) (mountStat, error)
	addrs     func() []proto.ServerAddr
	run       func(cmd string, timeout time.Duration) (string, error)
}

func (a *Agent) serverInfo() int {
	p := &serverInfoProbe{
		proc:      "/proc",
		osFiles:   []string{"/etc/os-release", "/usr/lib/os-release"},
		state:     a.si,
		now:       a.Sys.Now,
		sleep:     a.Sys.Sleep,
		hostname:  os.Hostname,
		uname:     unameInfo,
		statMount: statMount,
		addrs:     readServerAddrs,
		run:       a.Sys.Shell,
	}
	rep := p.collect()
	return a.result(proto.CodeOK, "", rep)
}

const serverCPUPrevMax = 10 * time.Minute

func (p *serverInfoProbe) collect() *proto.ServerInfo {
	r := &proto.ServerInfo{Mounts: []proto.ServerMount{}, Addresses: []proto.ServerAddr{}}
	now := p.now()
	zone, offset := now.Zone()
	sign := "+"
	off := offset
	if off < 0 {
		sign = "-"
		off = -off
	}
	r.Timezone = fmt.Sprintf("%s %s%02d%02d", zone, sign, off/3600, (off%3600)/60)
	r.Now = now.Unix()

	kernel, arch, nodename := p.uname()
	r.Kernel, r.Arch = kernel, arch
	if h, err := p.hostname(); err == nil && strings.TrimSpace(h) != "" {
		r.Hostname = strings.TrimSpace(h)
	} else {
		r.Hostname = nodename
	}

	if b, err := os.ReadFile(filepath.Join(p.proc, "uptime")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			r.Uptime, _ = strconv.ParseFloat(f[0], 64)
		}
	}

	r.OS = readOSPretty(p.osFiles)
	r.CPU.Model = readCPUInfoModel(filepath.Join(p.proc, "cpuinfo"))

	// 虚拟化类型与 lscpu 型号：静态信息，常驻 agent 生命周期内只探测一次
	needLSCPU := r.CPU.Model == ""
	r.Virtualization, r.CPU.Model = p.staticFields(r.CPU.Model, needLSCPU)

	// CPU 使用率：常驻 agent 已提前采过基线，间隔 >= 1s 时直接算差值（0 等待）
	prev, prevAt := p.state.prevCPU()
	age := now.Sub(prevAt)
	var before, after []string
	var window float64
	if prev != nil && age >= time.Second && age <= serverCPUPrevMax {
		before = prev
		after = readCPUStat(p.proc)
		window = math.Round(age.Seconds())
	} else if prev != nil && age > 0 && age < time.Second {
		p.sleep(time.Second - age)
		before = prev
		after = readCPUStat(p.proc)
		window = 1
	} else {
		before = readCPUStat(p.proc)
		p.sleep(time.Second)
		after = readCPUStat(p.proc)
		window = 1
	}
	if len(after) > 0 {
		p.state.setCPU(after, p.now())
	}
	for _, line := range after {
		f := strings.Fields(line)
		if len(f) > 0 && len(f[0]) > 3 && strings.HasPrefix(f[0], "cpu") {
			r.CPU.Cores++
		}
	}
	if r.CPU.Usage = calcCPUUsage(before, after); r.CPU.Usage != nil {
		r.CPU.Window = window
	} else {
		r.Notes = append(r.Notes, "未能读取 CPU 使用率")
	}

	if b, err := os.ReadFile(filepath.Join(p.proc, "loadavg")); err == nil {
		for _, field := range strings.Fields(string(b)) {
			if len(r.CPU.Load) == 3 {
				break
			}
			v, err := strconv.ParseFloat(field, 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				break
			}
			r.CPU.Load = append(r.CPU.Load, v)
		}
	}

	readServerMemory(filepath.Join(p.proc, "meminfo"), &r.Memory, &r.Swap)
	if r.Memory.Total == 0 {
		r.Notes = append(r.Notes, "未能读取内存信息")
	}

	mounts, partial := p.readMounts()
	r.Mounts = mounts
	if partial {
		r.Notes = append(r.Notes, "部分挂载点的容量无法读取（可能未挂载、无权限或网络文件系统超时）")
	}
	if len(r.Mounts) == 0 {
		r.Notes = append(r.Notes, "未能读取挂载点容量")
	}

	if p.addrs != nil {
		r.Addresses = p.addrs()
	}
	if r.Addresses == nil {
		r.Addresses = []proto.ServerAddr{}
	}
	return r
}

func (p *serverInfoProbe) staticFields(cpuModel string, needLSCPU bool) (virt, model string) {
	model = cpuModel
	probe := func() (string, string) {
		var v, m string
		if p.run == nil {
			return "", ""
		}
		if out, _ := p.run("LC_ALL=C systemd-detect-virt 2>/dev/null || true", 5*time.Second); out != "" {
			v = strings.TrimSpace(out)
		}
		if needLSCPU {
			if out, err := p.run("LC_ALL=C lscpu 2>/dev/null", 5*time.Second); err == nil {
				for _, line := range strings.Split(out, "\n") {
					if key, val, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "Model name" {
						m = strings.TrimSpace(val)
						break
					}
				}
			}
		}
		return v, m
	}
	if p.state != nil {
		p.state.staticOnce.Do(func() {
			p.state.virt, p.state.lscpuModel = probe()
		})
		virt = p.state.virt
		if model == "" {
			model = p.state.lscpuModel
		}
		return virt, model
	}
	v, m := probe()
	if model == "" {
		model = m
	}
	return v, model
}

func readCPUStat(proc string) []string {
	b, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "cpu") {
			lines = append(lines, line)
		}
	}
	return lines
}

func calcCPUUsage(before, after []string) *float64 {
	read := func(lines []string) (total, idle uint64, ok bool) {
		for _, line := range lines {
			f := strings.Fields(line)
			if len(f) < 5 || f[0] != "cpu" {
				continue
			}
			for i := 1; i < min(len(f), 9); i++ {
				n, err := strconv.ParseUint(f[i], 10, 64)
				if err != nil {
					return 0, 0, false
				}
				total += n
				if i == 4 || i == 5 {
					idle += n
				}
			}
			return total, idle, true
		}
		return 0, 0, false
	}
	t1, i1, ok1 := read(before)
	t2, i2, ok2 := read(after)
	if !ok1 || !ok2 || t2 <= t1 || i2 < i1 || i2-i1 > t2-t1 {
		return nil
	}
	v := math.Round(1000*float64((t2-t1)-(i2-i1))/float64(t2-t1)) / 10
	return &v
}

func readOSPretty(files []string) string {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		kv := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			if unquoted, err := strconv.Unquote(val); err == nil {
				val = unquoted
			} else {
				val = strings.Trim(val, "\"'")
			}
			kv[key] = val
		}
		if kv["PRETTY_NAME"] != "" {
			return kv["PRETTY_NAME"]
		}
		if s := strings.TrimSpace(kv["NAME"] + " " + kv["VERSION"]); s != "" {
			return s
		}
	}
	return ""
}

func readCPUInfoModel(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name", "Hardware", "Processor":
			if v := strings.TrimSpace(val); v != "" {
				return v
			}
		}
	}
	return ""
}

func readServerMemory(path string, m *proto.ServerMemory, sw *proto.ServerSwap) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	mem := map[string]uint64{}
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if f := strings.Fields(val); len(f) > 0 {
			n, _ := strconv.ParseUint(f[0], 10, 64)
			mem[key] = n * 1024
		}
	}
	m.Total, m.Free = mem["MemTotal"], mem["MemFree"]
	sum := mem["Buffers"] + mem["Cached"] + mem["SReclaimable"]
	if sum > mem["Shmem"] {
		m.Cached = sum - mem["Shmem"]
	}
	available, ok := mem["MemAvailable"]
	if !ok {
		available = m.Free + m.Cached
	}
	m.Available = min(available, m.Total)
	m.Used = m.Total - m.Available
	sw.Total, sw.Free = mem["SwapTotal"], min(mem["SwapFree"], mem["SwapTotal"])
	sw.Used = sw.Total - sw.Free
}

func (p *serverInfoProbe) readMounts() ([]proto.ServerMount, bool) {
	b, err := os.ReadFile(filepath.Join(p.proc, "mounts"))
	if err != nil {
		return []proto.ServerMount{}, false
	}
	type rawMount struct {
		device, path, fsType string
	}
	var raw []rawMount
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		mp := unescapeMountPath(f[1])
		if !strings.HasPrefix(mp, "/") {
			continue
		}
		raw = append(raw, rawMount{device: unescapeMountPath(f[0]), path: mp, fsType: f[2]})
	}
	if len(raw) == 0 {
		return []proto.ServerMount{}, false
	}
	type statRes struct {
		st  mountStat
		err error
	}
	results := make([]statRes, len(raw))
	done := make([]bool, len(raw))
	var mu sync.Mutex

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		eachParallel(len(raw), 8, func(i int) {
			if ctx.Err() != nil {
				return
			}
			st, err := p.statMount(raw[i].path)
			mu.Lock()
			results[i] = statRes{st: st, err: err}
			done[i] = true
			mu.Unlock()
		})
	}()
	select {
	case <-finished:
	case <-ctx.Done():
	}

	partial := false
	mounts := make([]proto.ServerMount, 0, len(raw))
	mu.Lock()
	for i, rm := range raw {
		if !done[i] || results[i].err != nil {
			partial = true
			continue
		}
		st := results[i].st
		m := proto.ServerMount{
			Device:    rm.device,
			Path:      rm.path,
			FSType:    rm.fsType,
			Temporary: isTemporaryFS(rm.fsType),
		}
		if st.blocks > 0 && st.bsize > 0 {
			m.Total = st.blocks * st.bsize
			usedBlocks := st.blocks - min(st.bfree, st.blocks)
			m.Used = usedBlocks * st.bsize
			m.Available = st.bavail * st.bsize
			if nonroot := usedBlocks + st.bavail; nonroot > 0 {
				pct := math.Ceil(float64(usedBlocks) * 100 / float64(nonroot))
				if pct > 100 {
					pct = 100
				}
				m.Usage = &pct
			}
		}
		if st.files > 0 {
			m.Inodes = st.files
			m.InodesUsed = st.files - min(st.ffree, st.files)
			pct := math.Ceil(float64(m.InodesUsed) * 100 / float64(st.files))
			if pct > 100 {
				pct = 100
			}
			m.InodesUsage = &pct
		}
		mounts = append(mounts, m)
	}
	mu.Unlock()
	sort.SliceStable(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
	return mounts, partial
}

func isTemporaryFS(fsType string) bool {
	switch fsType {
	case "tmpfs", "devtmpfs", "ramfs", "proc", "sysfs", "devpts", "cgroup", "cgroup2", "mqueue", "hugetlbfs",
		"securityfs", "debugfs", "tracefs", "pstore", "configfs", "fusectl", "autofs", "binfmt_misc", "bpf", "rpc_pipefs", "nsfs", "efivarfs":
		return true
	}
	return false
}

// unescapeMountPath /proc/mounts 里把空格、制表符、换行、反斜杠写成八进制 \040 等，还原成原路径。
func unescapeMountPath(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func readServerAddrs() []proto.ServerAddr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []proto.ServerAddr{}
	}
	out := []proto.ServerAddr{}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ones, _ := ipNet.Mask.Size()
			if ip4 := ipNet.IP.To4(); ip4 != nil {
				out = append(out, proto.ServerAddr{Interface: ifc.Name, Family: "inet", Address: fmt.Sprintf("%s/%d", ip4.String(), ones)})
			} else {
				out = append(out, proto.ServerAddr{Interface: ifc.Name, Family: "inet6", Address: fmt.Sprintf("%s/%d", ipNet.IP.String(), ones)})
			}
		}
	}
	return out
}
