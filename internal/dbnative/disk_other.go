//go:build !linux

package dbnative

func diskFree(path string) (uint64, error) {
	return 0, nil
}
