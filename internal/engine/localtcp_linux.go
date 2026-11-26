//go:build linux

package engine

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// lookupLocalTCP 通过读取 Linux /proc/net/tcp(6) 与 /proc/<pid>/fd 反查本机连接对应进程。
func lookupLocalTCP(remoteIP string, remotePort int) (map[int]localTCPProc, bool) {
	return lookupLocalTCPInProc("/proc", remoteIP, remotePort)
}

func lookupLocalTCPInProc(procRoot, remoteIP string, remotePort int) (map[int]localTCPProc, bool) {
	target, ok := normalizeIPAddr(remoteIP)
	if !ok || remotePort <= 0 {
		return nil, false
	}
	inodeToPort := map[string]int{}
	var anyOK bool

	if b, err := os.ReadFile(filepath.Join(procRoot, "net", "tcp")); err == nil {
		anyOK = true
		parseProcNetTCP(string(b), false, target, remotePort, inodeToPort)
	}
	if b, err := os.ReadFile(filepath.Join(procRoot, "net", "tcp6")); err == nil {
		anyOK = true
		parseProcNetTCP(string(b), true, target, remotePort, inodeToPort)
	}
	if !anyOK {
		return nil, false
	}
	out := map[int]localTCPProc{}
	if len(inodeToPort) == 0 {
		return out, true
	}
	// 先占位：即使无权读其他 OS 用户的 /proc/<pid>/fd，也已确定该源端口确实属于本机
	for _, port := range inodeToPort {
		out[port] = localTCPProc{}
	}

	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return out, true
	}
	remaining := len(inodeToPort)
	for _, ent := range ents {
		if remaining == 0 {
			break
		}
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdDir := filepath.Join(procRoot, ent.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
				continue
			}
			inode := link[len("socket:[") : len(link)-1]
			port, match := inodeToPort[inode]
			if !match {
				continue
			}
			name, cmdline := readLinuxProcMeta(filepath.Join(procRoot, ent.Name()))
			out[port] = localTCPProc{PID: pid, Name: name, CmdLine: cmdline}
			delete(inodeToPort, inode)
			remaining--
			if remaining == 0 {
				break
			}
		}
	}
	return out, true
}

func parseProcNetTCP(content string, ipv6 bool, target netip.Addr, remotePort int, inodeToPort map[string]int) {
	lines := strings.Split(content, "\n")
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != "01" { // 01 = TCP_ESTABLISHED
			continue
		}
		_, lport, okL := parseProcAddr(f[1], ipv6)
		rip, rport, okR := parseProcAddr(f[2], ipv6)
		if !okL || !okR || rport != remotePort || rip != target {
			continue
		}
		inode := f[9]
		if inode != "" && inode != "0" {
			inodeToPort[inode] = lport
		}
	}
}

func parseProcAddr(field string, ipv6 bool) (netip.Addr, int, bool) {
	ipHex, portHex, ok := strings.Cut(field, ":")
	if !ok {
		return netip.Addr{}, 0, false
	}
	p, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	if !ipv6 {
		if len(ipHex) != 8 {
			return netip.Addr{}, 0, false
		}
		v, err := strconv.ParseUint(ipHex, 16, 32)
		if err != nil {
			return netip.Addr{}, 0, false
		}
		var b [4]byte
		binary.NativeEndian.PutUint32(b[:], uint32(v))
		return netip.AddrFrom4(b), int(p), true
	}
	if len(ipHex) != 32 {
		return netip.Addr{}, 0, false
	}
	var b [16]byte
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseUint(ipHex[i*8:(i+1)*8], 16, 32)
		if err != nil {
			return netip.Addr{}, 0, false
		}
		binary.NativeEndian.PutUint32(b[i*4:(i+1)*4], uint32(v))
	}
	return netip.AddrFrom16(b).Unmap(), int(p), true
}

func readLinuxProcMeta(procDir string) (name, cmdline string) {
	if b, err := os.ReadFile(filepath.Join(procDir, "comm")); err == nil {
		name = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(procDir, "cmdline")); err == nil && len(b) > 0 {
		trimmed := bytes.TrimRight(b, "\x00")
		cmdline = strings.TrimSpace(string(bytes.ReplaceAll(trimmed, []byte{0}, []byte{' '})))
		if name == "" {
			if f := strings.Fields(cmdline); len(f) > 0 {
				name = filepath.Base(f[0])
			}
		}
	}
	if len(cmdline) > 160 {
		cmdline = cmdline[:160] + "…"
	}
	return name, cmdline
}
