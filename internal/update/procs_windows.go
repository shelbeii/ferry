package update

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// RunningState 本机 ferry 进程与旧版残留二进制的占用情况。
type RunningState struct {
	OtherProcesses int // 除当前进程外，还有多少个进程占着当前 ferry.exe
	StaleBinaries  int // 尝试清理后仍被旧进程占用的 ferry.exe.old-* 数量
}

// InspectRunning 检查安装目录中是否有其他进程占用 ferry.exe，以及清理后是否仍有旧版 ferry.exe.old-* 被占用。
// 非安装目录（开发/测试二进制）直接返回零值，避免影响测试和性能。
func InspectRunning() RunningState {
	exe, err := os.Executable()
	if err != nil {
		return RunningState{}
	}
	dir := resolveInstallDir(exe)
	if dir == "" {
		return RunningState{}
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return inspectRunningIn(dir, exe, os.Getpid())
}

func inspectRunningIn(dir, exePath string, selfPID int) RunningState {
	var state RunningState
	state.StaleBinaries = cleanupOldBinaries(dir)
	if exePath != "" {
		pids := fileHolderPIDs(exePath)
		seen := make(map[int]struct{}, len(pids))
		for _, pid := range pids {
			if pid <= 0 || pid == selfPID {
				continue
			}
			if _, ok := seen[pid]; ok {
				continue
			}
			seen[pid] = struct{}{}
			state.OtherProcesses++
		}
	}
	return state
}

// cleanupOldBinaries 尝试删除安装目录下残留的 ferry.exe.old-*；返回仍被占用、删不掉的数量。
func cleanupOldBinaries(dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	locked := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), "ferry.exe.old-") {
			continue
		}
		full := filepath.Join(dir, name)
		if err := os.Remove(full); err != nil {
			if _, statErr := os.Stat(full); statErr == nil {
				locked++
			}
		}
	}
	return locked
}

var (
	rmDLL           = syscall.NewLazyDLL("rstrtmgr.dll")
	rmStartSession  = rmDLL.NewProc("RmStartSession")
	rmRegisterFiles = rmDLL.NewProc("RmRegisterResources")
	rmGetList       = rmDLL.NewProc("RmGetList")
	rmEndSession    = rmDLL.NewProc("RmEndSession")
	rmErrMoreData   = syscall.Errno(234)
)

type rmProcessInfo struct {
	PID         uint32
	StartTime   syscall.Filetime
	AppName     [256]uint16
	ServiceName [64]uint16
	AppType     uint32
	AppStatus   uint32
	TSSessionID uint32
	Restartable int32
}

// fileHolderPIDs 查询当前占用 path 的进程 PID（通过 Windows Restart Manager）。
func fileHolderPIDs(path string) []int {
	if rmStartSession.Find() != nil {
		return nil
	}
	var session uint32
	key := make([]uint16, 33)
	if r, _, _ := rmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return nil
	}
	defer rmEndSession.Call(uintptr(session))
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	files := []*uint16{p}
	if r, _, _ := rmRegisterFiles.Call(uintptr(session), 1, uintptr(unsafe.Pointer(&files[0])), 0, 0, 0, 0); r != 0 {
		return nil
	}
	n := uint32(4)
	for tries := 0; tries < 3; tries++ {
		infos := make([]rmProcessInfo, n)
		var needed, reasons uint32
		count := n
		r, _, _ := rmGetList.Call(
			uintptr(session),
			uintptr(unsafe.Pointer(&needed)),
			uintptr(unsafe.Pointer(&count)),
			uintptr(unsafe.Pointer(&infos[0])),
			uintptr(unsafe.Pointer(&reasons)),
		)
		if syscall.Errno(r) == rmErrMoreData {
			n = needed + 2
			continue
		}
		if r != 0 {
			return nil
		}
		out := make([]int, 0, count)
		for _, in := range infos[:count] {
			out = append(out, int(in.PID))
		}
		return out
	}
	return nil
}
