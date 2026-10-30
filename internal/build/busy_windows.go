package build

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// FileBusy 文件被别的程序占着、删不掉（Windows 上正在运行的 jar 就是这样）。
func FileBusy(path string) bool {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	const access = 0x00010000                                                                    // DELETE
	const share = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE // 自己不挡别人
	h, err := syscall.CreateFile(p, access, share, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return errors.Is(err, syscall.Errno(32)) // ERROR_SHARING_VIOLATION
	}
	syscall.CloseHandle(h)
	return false
}

// Holder 占着文件的进程。
type Holder struct {
	PID  int
	Name string // 程序名（如 OpenJDK Platform binary）
}

var (
	rstrtmgr           = syscall.NewLazyDLL("rstrtmgr.dll")
	procRmStartSession = rstrtmgr.NewProc("RmStartSession")
	procRmRegister     = rstrtmgr.NewProc("RmRegisterResources")
	procRmGetList      = rstrtmgr.NewProc("RmGetList")
	procRmEndSession   = rstrtmgr.NewProc("RmEndSession")
	errMoreData        = syscall.Errno(234) // ERROR_MORE_DATA
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

// FileHolders 哪些进程占着文件（用 Windows 的重启管理器查，和资源管理器提示「文件正在使用」用的是同一个）。
func FileHolders(path string) []Holder {
	if procRmStartSession.Find() != nil {
		return nil
	}
	var session uint32
	key := make([]uint16, 33)
	if r, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return nil
	}
	defer procRmEndSession.Call(uintptr(session))
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	files := []*uint16{p}
	if r, _, _ := procRmRegister.Call(uintptr(session), 1, uintptr(unsafe.Pointer(&files[0])), 0, 0, 0, 0); r != 0 {
		return nil
	}
	n := uint32(4)
	for tries := 0; tries < 3; tries++ {
		infos := make([]rmProcessInfo, n)
		var needed, reasons uint32
		count := n
		r, _, _ := procRmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)),
			uintptr(unsafe.Pointer(&infos[0])), uintptr(unsafe.Pointer(&reasons)))
		if syscall.Errno(r) == errMoreData {
			n = needed + 2
			continue
		}
		if r != 0 {
			return nil
		}
		out := make([]Holder, 0, count)
		for _, in := range infos[:count] {
			out = append(out, Holder{PID: int(in.PID), Name: syscall.UTF16ToString(in.AppName[:])})
		}
		return out
	}
	return nil
}

// ProcessCommands 进程的命令行（看得出是哪个服务，如 java -jar …\ShopMall-auth.jar）。查不到的不在结果里。
func ProcessCommands(pids []int) map[int]string {
	out := map[int]string{}
	if len(pids) == 0 {
		return out
	}
	ids := make([]string, len(pids))
	for i, p := range pids {
		ids[i] = strconv.Itoa(p)
	}
	// 输出用 UTF-8、一行一个：进程号<Tab>命令行
	script := "[Console]::OutputEncoding=[Text.Encoding]::UTF8; Get-CimInstance Win32_Process -Filter \"" +
		"ProcessId=" + strings.Join(ids, " OR ProcessId=") + "\" | ForEach-Object { \"$($_.ProcessId)`t$($_.CommandLine)\" }"
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	b, err := c.Output()
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		id, cmd, ok := strings.Cut(strings.TrimRight(l, "\r"), "\t")
		if n, err := strconv.Atoi(strings.TrimSpace(id)); ok && err == nil {
			out[n] = strings.TrimSpace(cmd)
		}
	}
	return out
}

// KillProcess 结束进程（相当于任务管理器里的「结束任务」）。
func KillProcess(pid int) error {
	h, err := syscall.OpenProcess(0x0001|0x00100000, false, uint32(pid)) // PROCESS_TERMINATE | SYNCHRONIZE
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	if err := syscall.TerminateProcess(h, 1); err != nil {
		return err
	}
	syscall.WaitForSingleObject(h, 5000)
	return nil
}
