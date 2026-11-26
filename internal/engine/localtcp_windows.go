//go:build windows

package engine

import (
	"encoding/binary"
	"net/netip"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modIPHlpAPI             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modIPHlpAPI.NewProc("GetExtendedTcpTable")
)

const (
	afINET                      = 2
	afINET6                     = 23
	tcpTableOwnerPIDConnections = 4
	mibTCPStateEstab            = 5
)

// lookupLocalTCP 通过 Windows IP Helper API（GetExtendedTcpTable）反查本机连向 remoteIP:remotePort 的进程。
func lookupLocalTCP(remoteIP string, remotePort int) (map[int]localTCPProc, bool) {
	target, ok := normalizeIPAddr(remoteIP)
	if !ok || remotePort <= 0 {
		return nil, false
	}
	out := map[int]localTCPProc{}
	var anyOK bool

	if target.Is4() {
		if rows, ok4 := readWinTCP4Table(); ok4 {
			anyOK = true
			for _, r := range rows {
				if r.state != mibTCPStateEstab || r.remotePort != remotePort || r.remoteIP != target {
					continue
				}
				out[r.localPort] = localTCPProc{PID: r.pid}
			}
		}
	}
	// IPv6 表同时覆盖纯 IPv6 与双栈 IPv4-mapped 连接
	if rows6, ok6 := readWinTCP6Table(); ok6 {
		anyOK = true
		for _, r := range rows6 {
			if r.state != mibTCPStateEstab || r.remotePort != remotePort || r.remoteIP != target {
				continue
			}
			if _, exists := out[r.localPort]; !exists {
				out[r.localPort] = localTCPProc{PID: r.pid}
			}
		}
	}
	if !anyOK {
		return nil, false
	}
	if len(out) == 0 {
		return out, true
	}

	// 只解析命中的少量 PID 名称；OpenProcess 失败时用 Toolhelp32 快照兜底
	var snapNames map[uint32]string
	for port, p := range out {
		if p.PID <= 0 {
			continue
		}
		name := winProcessName(uint32(p.PID))
		if name == "" {
			if snapNames == nil {
				snapNames = winSnapshotProcessNames()
			}
			name = snapNames[uint32(p.PID)]
		}
		p.Name = name
		out[port] = p
	}
	return out, true
}

type winTCPRow struct {
	state      uint32
	localPort  int
	remoteIP   netip.Addr
	remotePort int
	pid        int
}

func readWinTCPTableBuf(family uintptr) ([]byte, bool) {
	if err := procGetExtendedTcpTable.Find(); err != nil {
		return nil, false
	}
	var size uint32
	for attempt := 0; attempt < 3; attempt++ {
		var buf []byte
		var ptr uintptr
		if size > 0 {
			buf = make([]byte, size)
			ptr = uintptr(unsafe.Pointer(&buf[0]))
		}
		r1, _, _ := procGetExtendedTcpTable.Call(
			ptr,
			uintptr(unsafe.Pointer(&size)),
			0, // bOrder = FALSE
			family,
			tcpTableOwnerPIDConnections,
			0,
		)
		if r1 == 0 && len(buf) >= 4 {
			return buf[:size], true
		}
		if syscall.Errno(r1) != windows.ERROR_INSUFFICIENT_BUFFER || size == 0 {
			return nil, false
		}
		// 稍微多留一点余量，避免并发新增连接再次触发 INSUFFICIENT_BUFFER
		size += 1024
	}
	return nil, false
}

func readWinTCP4Table() ([]winTCPRow, bool) {
	buf, ok := readWinTCPTableBuf(afINET)
	if !ok {
		return nil, false
	}
	return parseWinTCP4Table(buf), true
}

func parseWinTCP4Table(buf []byte) []winTCPRow {
	if len(buf) < 4 {
		return nil
	}
	const rowSize = 24
	count := int(binary.LittleEndian.Uint32(buf[:4]))
	maxRows := (len(buf) - 4) / rowSize
	if count > maxRows {
		count = maxRows
	}
	out := make([]winTCPRow, 0, count)
	for i := 0; i < count; i++ {
		off := 4 + i*rowSize
		state := binary.LittleEndian.Uint32(buf[off:])
		localPort := ntohs32(binary.LittleEndian.Uint32(buf[off+8:]))
		var rip [4]byte
		copy(rip[:], buf[off+12:off+16])
		remotePort := ntohs32(binary.LittleEndian.Uint32(buf[off+16:]))
		pid := int(binary.LittleEndian.Uint32(buf[off+20:]))
		out = append(out, winTCPRow{
			state:      state,
			localPort:  localPort,
			remoteIP:   netip.AddrFrom4(rip),
			remotePort: remotePort,
			pid:        pid,
		})
	}
	return out
}

func readWinTCP6Table() ([]winTCPRow, bool) {
	buf, ok := readWinTCPTableBuf(afINET6)
	if !ok {
		return nil, false
	}
	return parseWinTCP6Table(buf), true
}

func parseWinTCP6Table(buf []byte) []winTCPRow {
	if len(buf) < 4 {
		return nil
	}
	const rowSize = 56
	count := int(binary.LittleEndian.Uint32(buf[:4]))
	maxRows := (len(buf) - 4) / rowSize
	if count > maxRows {
		count = maxRows
	}
	out := make([]winTCPRow, 0, count)
	for i := 0; i < count; i++ {
		off := 4 + i*rowSize
		localPort := ntohs32(binary.LittleEndian.Uint32(buf[off+20:]))
		var rip [16]byte
		copy(rip[:], buf[off+24:off+40])
		remotePort := ntohs32(binary.LittleEndian.Uint32(buf[off+44:]))
		state := binary.LittleEndian.Uint32(buf[off+48:])
		pid := int(binary.LittleEndian.Uint32(buf[off+52:]))
		out = append(out, winTCPRow{
			state:      state,
			localPort:  localPort,
			remoteIP:   netip.AddrFrom16(rip).Unmap(),
			remotePort: remotePort,
			pid:        pid,
		})
	}
	return out
}

func ntohs32(v uint32) int {
	return int((v&0xff)<<8 | (v>>8)&0xff)
}

func winProcessName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil || n == 0 {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}

func winSnapshotProcessNames() map[uint32]string {
	out := map[uint32]string{}
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil {
		return out
	}
	for {
		out[pe.ProcessID] = windows.UTF16ToString(pe.ExeFile[:])
		if err := windows.Process32Next(h, &pe); err != nil {
			break
		}
	}
	return out
}
