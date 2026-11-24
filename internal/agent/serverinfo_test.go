package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func TestServerInfoNativePrimedAndCached(t *testing.T) {
	proc := t.TempDir()
	osRelease := filepath.Join(t.TempDir(), "os-release")
	os.WriteFile(osRelease, []byte("NAME=\"openEuler\"\nPRETTY_NAME=\"openEuler 24.03 (LTS-SP4)\"\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "uptime"), []byte("12345.67 8000.00\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "cpuinfo"), []byte("processor : 0\nmodel name : Hygon C86 7163\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "loadavg"), []byte("0.25 1.50 2.00 1/200 300\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "meminfo"), []byte("MemTotal:       8192000 kB\nMemFree:        1024000 kB\nMemAvailable:   5120000 kB\nBuffers:         128000 kB\nCached:         2048000 kB\nSReclaimable:    256000 kB\nShmem:           128000 kB\nSwapTotal:      2048000 kB\nSwapFree:       1536000 kB\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "stat"), []byte("cpu 100 0 100 700 50 10 10 30 20 0\ncpu0 50 0 50 350 25 5 5 15 10 0\ncpu1 50 0 50 350 25 5 5 15 10 0\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "mounts"), []byte(
		"/dev/vda1 / ext4 rw 0 0\n"+
			"tmpfs /run tmpfs rw 0 0\n"+
			"/dev/vdb1 /data\\040with\\040spaces xfs rw 0 0\n"+
			"bad:/nfs /mnt/bad nfs4 rw 0 0\n",
	), 0o644)

	state := &serverInfoState{}
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	state.setCPU(readCPUStat(proc), t0)

	// 模拟 5 秒后采集：更新 /proc/stat，应直接利用预采基线，不调用 sleep
	os.WriteFile(filepath.Join(proc, "stat"), []byte("cpu 120 0 110 760 55 11 11 33 25 0\ncpu0 60 0 55 380 28 5 5 17 12 0\ncpu1 60 0 55 380 27 6 6 16 13 0\n"), 0o644)
	now := t0.Add(5 * time.Second)
	var slept time.Duration
	var virtCalls int
	p := &serverInfoProbe{
		proc:     proc,
		osFiles:  []string{osRelease},
		state:    state,
		now:      func() time.Time { return now },
		sleep:    func(d time.Duration) { slept += d },
		hostname: func() (string, error) { return "prod-a", nil },
		uname:    func() (string, string, string) { return "6.6.0-oe2403", "x86_64", "prod-a" },
		statMount: func(mp string) (mountStat, error) {
			switch mp {
			case "/":
				return mountStat{bsize: 4096, blocks: 1000, bfree: 500, bavail: 450, files: 1000, ffree: 700}, nil
			case "/run":
				return mountStat{bsize: 4096, blocks: 100, bfree: 99, bavail: 99, files: 100, ffree: 99}, nil
			case "/data with spaces":
				return mountStat{bsize: 4096, blocks: 200, bfree: 0, bavail: 0, files: 500, ffree: 5}, nil
			default:
				return mountStat{}, errors.New("stale nfs")
			}
		},
		addrs: func() []proto.ServerAddr {
			return []proto.ServerAddr{{Interface: "eth0", Family: "inet", Address: "10.0.0.10/24"}}
		},
		run: func(cmd string, _ time.Duration) (string, error) {
			virtCalls++
			return "kvm\n", nil
		},
	}

	r := p.collect()
	if slept != 0 {
		t.Fatalf("已有预采 CPU 基线时不应等待，实际等待 %s", slept)
	}
	if r.Hostname != "prod-a" || r.OS != "openEuler 24.03 (LTS-SP4)" || r.Kernel != "6.6.0-oe2403" || r.Arch != "x86_64" || r.Virtualization != "kvm" || r.Timezone != "CST +0800" {
		t.Fatalf("系统基础字段不对：%+v", r)
	}
	if r.CPU.Cores != 2 || r.CPU.Model != "Hygon C86 7163" || r.CPU.Usage == nil || *r.CPU.Usage != 35 || r.CPU.Window != 5 {
		t.Fatalf("CPU 采样不对：%+v", r.CPU)
	}
	if r.Memory.Total != 8192000*1024 || r.Memory.Used != 3072000*1024 || r.Memory.Cached != 2304000*1024 || r.Swap.Used != 512000*1024 {
		t.Fatalf("内存与 Swap 不对：%+v %+v", r.Memory, r.Swap)
	}
	if len(r.Mounts) != 3 || r.Mounts[0].Path != "/" || r.Mounts[1].Path != "/data with spaces" || !r.Mounts[2].Temporary {
		t.Fatalf("挂载点解析不对：%+v", r.Mounts)
	}
	if r.Mounts[0].Usage == nil || *r.Mounts[0].Usage != 53 || r.Mounts[1].InodesUsage == nil || *r.Mounts[1].InodesUsage != 99 {
		t.Fatalf("挂载点百分比计算不对：%+v", r.Mounts)
	}
	if len(r.Notes) != 1 {
		t.Fatalf("失败的 NFS 挂载应记一条提示：%v", r.Notes)
	}

	// 再次采集：静态虚拟化探测应复用缓存，不再起子进程
	now = now.Add(2 * time.Second)
	p.collect()
	if virtCalls != 1 {
		t.Fatalf("静态虚拟化探测应只执行 1 次，实际 %d 次", virtCalls)
	}
}

func TestUnescapeMountPath(t *testing.T) {
	if got := unescapeMountPath(`/data\040with\040spaces\134sub`); got != `/data with spaces\sub` {
		t.Fatalf("got %q", got)
	}
}
