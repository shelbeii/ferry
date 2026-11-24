//go:build !windows

package update

// RunningState 本机 ferry 进程与旧版残留二进制的占用情况（非 Windows 上不产生 .old-* 文件）。
type RunningState struct {
	OtherProcesses int
	StaleBinaries  int
}

// InspectRunning 在非 Windows 上返回零值。
func InspectRunning() RunningState {
	return RunningState{}
}
