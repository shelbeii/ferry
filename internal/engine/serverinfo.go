package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ServerInfo 是一次只读采样，不要求在服务器上安装监控程序，也用于「新建连接」。
type ServerInfo struct {
	Hostname       string        `json:"hostname"`
	OS             string        `json:"os"`
	Kernel         string        `json:"kernel"`
	Arch           string        `json:"arch"`
	Virtualization string        `json:"virtualization"`
	Timezone       string        `json:"timezone"`
	Now            int64         `json:"now"`
	Uptime         float64       `json:"uptime"`
	CPU            ServerCPU     `json:"cpu"`
	Memory         ServerMemory  `json:"memory"`
	Swap           ServerSwap    `json:"swap"`
	Mounts         []ServerMount `json:"mounts"`
	Addresses      []ServerAddr  `json:"addresses"`
	Notes          []string      `json:"notes,omitempty"`
}

type ServerCPU struct {
	Model string `json:"model"`
	Cores int    `json:"cores"` // 逻辑 CPU 数
	// Usage 为两次 /proc/stat（间隔约一秒）的差值；nil 表示采样不可用。
	Usage *float64  `json:"usage"`
	Load  []float64 `json:"load"` // 1 / 5 / 15 分钟平均负载，不是百分比
}

type ServerMemory struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"` // total - available，缓存不算实际占用
	Available uint64 `json:"available"`
	Free      uint64 `json:"free"`
	Cached    uint64 `json:"cached"` // buffers + cached + SReclaimable - Shmem
}

type ServerSwap struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
}

type ServerMount struct {
	Device      string   `json:"device"`
	Path        string   `json:"path"`
	FSType      string   `json:"fsType"`
	Total       uint64   `json:"total"`
	Used        uint64   `json:"used"`
	Available   uint64   `json:"available"`
	Usage       *float64 `json:"usage"` // df 的使用率，包含文件系统保留空间的影响
	Inodes      uint64   `json:"inodes"`
	InodesUsed  uint64   `json:"inodesUsed"`
	InodesUsage *float64 `json:"inodesUsage"`
	Temporary   bool     `json:"temporary"`
}

type ServerAddr struct {
	Interface string `json:"interface"`
	Family    string `json:"family"`
	Address   string `json:"address"`
}

// 只读 Linux 自带的 procfs 和 coreutils。df 用 POSIX 的单行格式；含空格的挂载点由解析端保留。
// 临时文件系统也采集，由界面决定是否显示；不加总多个挂载点，避免 bind mount 重复计算容量。
const serverInfoScript = `export LC_ALL=C
[ "$(uname -s)" = Linux ] || { echo '仅支持 Linux 服务器' >&2; exit 2; }
echo @@system
printf 'hostname\t'; hostname 2>/dev/null
printf 'kernel\t'; uname -r
printf 'arch\t'; uname -m
printf 'timezone\t'; date '+%Z %z'
printf 'now\t'; date +%s
printf 'uptime\t'; cat /proc/uptime 2>/dev/null
printf 'virtualization\t'; systemd-detect-virt 2>/dev/null || true
echo @@os
cat /etc/os-release 2>/dev/null || cat /usr/lib/os-release 2>/dev/null
echo @@cpuinfo
cat /proc/cpuinfo 2>/dev/null
echo @@cpumodel
lscpu 2>/dev/null
echo @@memory
cat /proc/meminfo 2>/dev/null
echo @@load
cat /proc/loadavg 2>/dev/null
echo @@cpu1
grep '^cpu' /proc/stat 2>/dev/null
sleep 1
echo @@cpu2
grep '^cpu' /proc/stat 2>/dev/null
echo @@disks
if command -v timeout >/dev/null 2>&1; then timeout 8 df -a -P -T -k 2>/dev/null; else df -a -P -T -k 2>/dev/null; fi
[ $? = 0 ] || echo '@@note 部分挂载点的容量无法读取（可能未挂载、无权限或网络文件系统超时）'
echo @@inodes
if command -v timeout >/dev/null 2>&1; then timeout 8 df -a -P -i 2>/dev/null; else df -a -P -i 2>/dev/null; fi
echo @@addresses
ip -o address show 2>/dev/null
echo @@end
`

func (e *Engine) ServerInfo(target string) (*ServerInfo, error) {
	var out *ServerInfo
	err := e.withConn(target, func(s *session) error {
		ctx, cancel := context.WithTimeout(e.ctx(), 25*time.Second)
		defer cancel()
		var lines []string
		res, err := s.c.RunContext(ctx, "sh -s", []byte(serverInfoScript), func(line string) { lines = append(lines, line) })
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errors.New("读取服务器信息超时，请稍后刷新")
			}
			return err
		}
		if res.Code != 0 {
			return fmt.Errorf("读取服务器信息失败：%s", strings.TrimSpace(res.Stderr))
		}
		out, err = parseServerInfo(lines)
		return err
	})
	return out, err
}

func parseServerInfo(lines []string) (*ServerInfo, error) {
	sections := map[string][]string{}
	var section string
	r := &ServerInfo{Mounts: []ServerMount{}, Addresses: []ServerAddr{}}
	for _, line := range lines {
		if note, ok := strings.CutPrefix(line, "@@note "); ok {
			r.Notes = append(r.Notes, note)
			continue
		}
		if strings.HasPrefix(line, "@@") {
			section = strings.TrimSpace(strings.TrimPrefix(line, "@@"))
			continue
		}
		sections[section] = append(sections[section], line)
	}
	for _, line := range sections["system"] {
		key, val, _ := strings.Cut(line, "\t")
		val = strings.TrimSpace(val)
		switch key {
		case "hostname":
			r.Hostname = val
		case "kernel":
			r.Kernel = val
		case "arch":
			r.Arch = val
		case "timezone":
			r.Timezone = val
		case "virtualization":
			r.Virtualization = val
		case "now":
			r.Now, _ = strconv.ParseInt(val, 10, 64)
		case "uptime":
			if f := strings.Fields(val); len(f) > 0 {
				r.Uptime, _ = strconv.ParseFloat(f[0], 64)
			}
		}
	}
	if r.Kernel == "" || section != "end" {
		return nil, errors.New("服务器信息不完整，请确认服务器是 Linux 并重新刷新")
	}
	os := map[string]string{}
	for _, line := range sections["os"] {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, "\"'")
		}
		os[key] = value
	}
	r.OS = os["PRETTY_NAME"]
	if r.OS == "" {
		r.OS = strings.TrimSpace(os["NAME"] + " " + os["VERSION"])
	}
	for _, line := range sections["cpuinfo"] {
		key, value, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(key) {
		case "model name", "Hardware", "Processor":
			if r.CPU.Model == "" {
				r.CPU.Model = strings.TrimSpace(value)
			}
		}
	}
	if r.CPU.Model == "" {
		for _, line := range sections["cpumodel"] {
			if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "Model name" {
				r.CPU.Model = strings.TrimSpace(value)
				break
			}
		}
	}
	for _, line := range sections["cpu2"] {
		f := strings.Fields(line)
		if len(f) > 0 && len(f[0]) > 3 && strings.HasPrefix(f[0], "cpu") {
			r.CPU.Cores++
		}
	}
	r.CPU.Usage = serverCPUUsage(sections["cpu1"], sections["cpu2"])
	if r.CPU.Usage == nil {
		r.Notes = append(r.Notes, "未能读取 CPU 使用率")
	}
	for _, field := range strings.Fields(strings.Join(sections["load"], " ")) {
		if len(r.CPU.Load) == 3 {
			break
		}
		v, err := strconv.ParseFloat(field, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			break
		}
		r.CPU.Load = append(r.CPU.Load, v)
	}
	mem := map[string]uint64{}
	for _, line := range sections["memory"] {
		key, value, _ := strings.Cut(line, ":")
		if f := strings.Fields(value); len(f) > 0 {
			mem[key] = serverUint(f[0]) * 1024
		}
	}
	m := &r.Memory
	m.Total, m.Free = mem["MemTotal"], mem["MemFree"]
	m.Cached = serverSub(mem["Buffers"]+mem["Cached"]+mem["SReclaimable"], mem["Shmem"])
	available, ok := mem["MemAvailable"]
	if !ok { // 老内核没有 MemAvailable
		available = m.Free + m.Cached
	}
	m.Available = min(available, m.Total)
	m.Used = m.Total - m.Available
	r.Swap.Total, r.Swap.Free = mem["SwapTotal"], min(mem["SwapFree"], mem["SwapTotal"])
	r.Swap.Used = r.Swap.Total - r.Swap.Free
	if m.Total == 0 {
		r.Notes = append(r.Notes, "未能读取内存信息")
	}
	inodes := map[string][]string{}
	for _, line := range sections["inodes"] {
		if f := strings.Fields(line); len(f) >= 6 {
			inodes[strings.Join(f[5:], " ")] = f
		}
	}
	for _, line := range sections["disks"] {
		f := strings.Fields(line)
		if len(f) < 7 || !strings.HasPrefix(f[6], "/") {
			continue
		}
		mount := ServerMount{Device: f[0], FSType: f[1], Path: strings.Join(f[6:], " "),
			Total: serverUint(f[2]) * 1024, Used: serverUint(f[3]) * 1024, Available: serverUint(f[4]) * 1024,
			Usage: serverPercent(f[5])}
		switch mount.FSType {
		case "tmpfs", "devtmpfs", "ramfs", "proc", "sysfs", "devpts", "cgroup", "cgroup2", "mqueue", "hugetlbfs",
			"securityfs", "debugfs", "tracefs", "pstore", "configfs", "fusectl", "autofs", "binfmt_misc", "bpf", "rpc_pipefs", "nsfs", "efivarfs":
			mount.Temporary = true
		}
		if inode := inodes[mount.Path]; inode != nil {
			mount.Inodes, mount.InodesUsed = serverUint(inode[1]), serverUint(inode[2])
			mount.InodesUsage = serverPercent(inode[4])
		}
		r.Mounts = append(r.Mounts, mount)
	}
	sort.SliceStable(r.Mounts, func(i, j int) bool { return r.Mounts[i].Path < r.Mounts[j].Path })
	if len(r.Mounts) == 0 {
		r.Notes = append(r.Notes, "未能读取挂载点容量")
	}
	for _, line := range sections["addresses"] {
		f := strings.Fields(line)
		if len(f) >= 4 && (f[2] == "inet" || f[2] == "inet6") {
			r.Addresses = append(r.Addresses, ServerAddr{Interface: strings.TrimSuffix(f[1], ":"), Family: f[2], Address: f[3]})
		}
	}
	return r, nil
}

func serverCPUUsage(before, after []string) *float64 {
	read := func(lines []string) (total, idle uint64, ok bool) {
		for _, line := range lines {
			f := strings.Fields(line)
			if len(f) < 5 || f[0] != "cpu" {
				continue
			}
			// guest、guest_nice 已包含在 user、nice 中，不能重复相加。
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

func serverUint(s string) uint64   { n, _ := strconv.ParseUint(s, 10, 64); return n }
func serverSub(a, b uint64) uint64 { return a - min(a, b) }
func serverPercent(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
