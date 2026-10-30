package engine

import (
	"context"
	"math"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

const serverInfoFixture = `@@system
hostname	prod-a
kernel	5.14.0-503.el9.aarch64
arch	aarch64
timezone	CST +0800
now	1793152800
uptime	90061.25 8000.00
virtualization	kvm
@@os
NAME="Rocky Linux"
VERSION="9.5 (Blue Onyx)"
PRETTY_NAME="Rocky Linux 9.5 (Blue Onyx)"
@@cpuinfo
processor : 0
model name : ARM Neoverse N1
processor : 1
@@memory
MemTotal:       8192000 kB
MemFree:        1024000 kB
MemAvailable:   5120000 kB
Buffers:         128000 kB
Cached:         2048000 kB
SReclaimable:    256000 kB
Shmem:           128000 kB
SwapTotal:      2048000 kB
SwapFree:       1536000 kB
@@load
0.25 1.50 2.00 1/200 300
@@cpu1
cpu 100 0 100 700 50 10 10 30 20 0
cpu0 50 0 50 350 25 5 5 15 10 0
cpu1 50 0 50 350 25 5 5 15 10 0
@@cpu2
cpu 120 0 110 760 55 11 11 33 25 0
cpu0 60 0 55 380 28 5 5 17 12 0
cpu1 60 0 55 380 27 6 6 16 13 0
@@disks
Filesystem Type 1024-blocks Used Available Capacity Mounted on
/dev/vda1 xfs 104857600 52428800 47185920 53% /
tmpfs tmpfs 4096000 1024 4094976 1% /run
/dev/vdb1 ext4 209715200 199229440 0 100% /data with spaces
server:/share nfs4 314572800 104857600 209715200 34% /mnt/share
/dev/vda1 xfs 104857600 52428800 47185920 53% /srv/bind
@@inodes
Filesystem Inodes IUsed IFree IUse% Mounted on
/dev/vda1 1000 300 700 30% /
tmpfs 100 1 99 1% /run
/dev/vdb1 1000 990 10 99% /data with spaces
server:/share - - - - /mnt/share
/dev/vda1 1000 300 700 30% /srv/bind
@@addresses
1: lo inet 127.0.0.1/8 scope host lo
2: eth0 inet 10.0.0.10/24 brd 10.0.0.255 scope global eth0
2: eth0 inet6 fe80::1/64 scope link
@@end
`

func TestParseServerInfo(t *testing.T) {
	r, err := parseServerInfo(strings.Split(serverInfoFixture, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "prod-a" || r.OS != "Rocky Linux 9.5 (Blue Onyx)" || r.Arch != "aarch64" || r.Uptime != 90061.25 || r.Timezone != "CST +0800" {
		t.Fatalf("系统信息：%+v", r)
	}
	if r.CPU.Cores != 2 || r.CPU.Model != "ARM Neoverse N1" || r.CPU.Usage == nil || *r.CPU.Usage != 35 || len(r.CPU.Load) != 3 || r.CPU.Load[1] != 1.5 {
		t.Fatalf("CPU（guest 不应重复累计）：%+v", r.CPU)
	}
	if r.Memory.Total != 8192000*1024 || r.Memory.Used != 3072000*1024 || r.Memory.Cached != 2304000*1024 || r.Swap.Used != 512000*1024 {
		t.Fatalf("内存与 Swap：%+v %+v", r.Memory, r.Swap)
	}
	if len(r.Mounts) != 5 || r.Mounts[0].Path != "/" {
		t.Fatalf("各挂载点必须保留，不把 bind mount 的容量加总：%+v", r.Mounts)
	}
	space := r.Mounts[1]
	if space.Path != "/data with spaces" || space.Total != 209715200*1024 || space.Available != 0 || space.InodesUsage == nil || *space.InodesUsage != 99 {
		t.Fatalf("含空格的挂载点及 inode：%+v", space)
	}
	if r.Mounts[2].FSType != "nfs4" || r.Mounts[2].InodesUsage != nil || !r.Mounts[3].Temporary {
		t.Fatalf("网络盘与临时文件系统：%+v", r.Mounts)
	}
	if len(r.Addresses) != 3 || r.Addresses[2].Address != "fe80::1/64" || len(r.Notes) > 0 {
		t.Fatalf("网卡与提示：%+v", r)
	}
}

func TestServerInfoPartialAndOldKernel(t *testing.T) {
	raw := strings.ReplaceAll(serverInfoFixture, "MemAvailable:   5120000 kB\n", "")
	raw = strings.ReplaceAll(raw, "PRETTY_NAME=\"Rocky Linux 9.5 (Blue Onyx)\"\n", "")
	raw = strings.ReplaceAll(raw, "SwapTotal:      2048000 kB", "SwapTotal:      0 kB")
	raw = strings.ReplaceAll(raw, "SwapFree:       1536000 kB", "SwapFree:       0 kB")
	raw = strings.ReplaceAll(raw, "@@addresses", "@@note 部分挂载点无法读取\n@@addresses")
	r, err := parseServerInfo(strings.Split(raw, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Memory.Available != (1024000+2304000)*1024 || r.OS != "Rocky Linux 9.5 (Blue Onyx)" || r.Swap.Total != 0 || len(r.Notes) != 1 {
		t.Fatalf("老内核估算与部分缺失：%+v", r)
	}
	for _, raw := range []string{"@@end", "@@system\nkernel\t6.1\n@@memory"} {
		if _, err := parseServerInfo(strings.Split(raw, "\n")); err == nil {
			t.Fatal("不完整的采样必须报错")
		}
	}
	// 可选工具不可用仍显示系统概况；CPU / 内存缺失不能当作使用率 0%。
	r, err = parseServerInfo(strings.Split("@@system\nkernel\t6.1\n@@end", "\n"))
	if err != nil || r.CPU.Usage != nil || len(r.Mounts) != 0 || len(r.Notes) != 3 {
		t.Fatalf("可选信息缺失：%+v %v", r, err)
	}
}

func TestServerCPUSamplingAndMissingValues(t *testing.T) {
	for _, tc := range []struct {
		before, after string
		want          *float64
	}{
		{"cpu 10 0 0 90", "cpu 10 0 0 190", new(float64(0))},
		{"cpu 10 0 0 90", "cpu 110 0 0 90", new(float64(100))},
		{"cpu 10 0 0 90", "cpu 10 0 0 90", nil},
		{"cpu 10 0 0 90", "cpu 0 0 0 0", nil},
		{"cpu 10 0 0 90", "cpu 0 0 0 200", nil},
		{"cpu 10 x 0 90", "cpu 10 0 0 190", nil},
		{"", "", nil},
	} {
		got := serverCPUUsage([]string{tc.before}, []string{tc.after})
		if (got == nil) != (tc.want == nil) || got != nil && math.Abs(*got-*tc.want) > .01 {
			t.Errorf("%s → %s：got %v want %v", tc.before, tc.after, got, tc.want)
		}
	}
	for _, bad := range []string{"-", "", "NaN", "+Inf", "-1%"} {
		if serverPercent(bad) != nil {
			t.Errorf("未知的使用率不能显示成 0：%q", bad)
		}
	}
}

// Linux CI 上执行实际采集脚本，覆盖命令格式与解析的衔接；只读取当前测试环境。
func TestServerInfoLinuxScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("采集脚本需要 Linux")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-s")
	cmd.Stdin = strings.NewReader(serverInfoScript)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseServerInfo(strings.Split(string(out), "\n"))
	if err != nil || r.Memory.Total == 0 || r.CPU.Cores == 0 || r.CPU.Usage == nil || r.Now == 0 || len(r.Mounts) == 0 {
		t.Fatalf("实际采集：%+v %v", r, err)
	}
}
