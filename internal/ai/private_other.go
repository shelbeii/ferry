//go:build !windows

package ai

// makePrivate 其他系统上目录已经是 0700、文件 0600，不用再做什么。
func makePrivate(string) {}
