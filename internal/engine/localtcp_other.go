//go:build !windows && !linux && !darwin

package engine

func lookupLocalTCP(string, int) (map[int]localTCPProc, bool) {
	return nil, false
}
