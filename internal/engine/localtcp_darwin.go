//go:build darwin

package engine

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// lookupLocalTCP 在 macOS 上通过 lsof 反查本机连向 remoteIP:remotePort 的进程。
func lookupLocalTCP(remoteIP string, remotePort int) (map[int]localTCPProc, bool) {
	target, ok := normalizeIPAddr(remoteIP)
	if !ok || remotePort <= 0 {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addr := target.String()
	if target.Is6() {
		addr = "[" + addr + "]"
	}
	cmd := exec.CommandContext(ctx, "lsof", "-nP", fmt.Sprintf("-iTCP@%s:%d", addr, remotePort), "-sTCP:ESTABLISHED", "-F", "pcn")
	out, err := cmd.Output()
	if err != nil {
		// lsof 找不到匹配连接时通常退出码为 1 且输出为空
		if len(out) == 0 {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return map[int]localTCPProc{}, true
			}
			return nil, false
		}
	}
	return parseDarwinLsof(string(out)), true
}

func parseDarwinLsof(raw string) map[int]localTCPProc {
	res := map[int]localTCPProc{}
	var curPID int
	var curCmd string
	for _, line := range strings.Split(raw, "\n") {
		if len(line) < 2 {
			continue
		}
		val := strings.TrimSpace(line[1:])
		switch line[0] {
		case 'p':
			curPID, _ = strconv.Atoi(val)
			curCmd = ""
		case 'c':
			curCmd = val
		case 'n':
			localSide, _, ok := strings.Cut(val, "->")
			if !ok {
				continue
			}
			_, portStr, err := net.SplitHostPort(localSide)
			if err != nil {
				continue
			}
			if lp, err := strconv.Atoi(portStr); err == nil && lp > 0 {
				res[lp] = localTCPProc{PID: curPID, Name: curCmd}
			}
		}
	}
	return res
}
