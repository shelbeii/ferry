//go:build !linux

package agent

// fileIdentity 只在 Linux 上认文件身份（agent 只跑在 Linux 上）；其他系统上为空，不缓存。
func fileIdentity(string) string { return "" }
