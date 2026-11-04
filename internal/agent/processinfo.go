package agent

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type ProcessSnapshot struct {
	PID, Parent, Threads int
	Exe, Cwd, User, UID  string
	Args                 []string
	Memory               int64
	Libraries            []string
	Notes                []string
}

func (realSystem) Processes(pid int) []ProcessSnapshot {
	if pid <= 0 {
		return nil
	}
	var ids []int
	for id := range procTree("/proc", pid) {
		if id != pid {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	ids = append([]int{pid}, ids...)
	var out []ProcessSnapshot
	for _, id := range ids[:min(len(ids), 128)] {
		p, err := processSnapshot("/proc", id)
		if err == nil {
			out = append(out, p)
		}
	}
	return out
}

func infoReadFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

func processSnapshot(proc string, pid int) (ProcessSnapshot, error) {
	dir := filepath.Join(proc, strconv.Itoa(pid))
	p := ProcessSnapshot{PID: pid}
	before, err := infoReadFile(filepath.Join(dir, "stat"), 8192)
	if err != nil {
		return p, err
	}
	p.Exe, err = os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		return p, err
	}
	p.Cwd, err = os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		p.Notes = append(p.Notes, "无法读取进程工作目录："+err.Error())
	}
	args, err := infoReadFile(filepath.Join(dir, "cmdline"), 128*1024)
	if err == nil && len(args) > 0 {
		p.Args = strings.Split(strings.TrimRight(string(args), "\x00"), "\x00")
	} else if err != nil {
		p.Notes = append(p.Notes, "无法读取进程命令行："+err.Error())
	}
	status, err := infoReadFile(filepath.Join(dir, "status"), 64*1024)
	if err == nil {
		parseProcessStatus(string(status), &p)
		p.User = p.UID
		if u, err := user.LookupId(p.UID); err == nil {
			p.User = u.Username
		}
	} else {
		p.Notes = append(p.Notes, "无法读取进程用户与内存信息："+err.Error())
	}
	maps, err := infoReadFile(filepath.Join(dir, "maps"), 4*1024*1024)
	if err == nil {
		p.Libraries = runtimeLibraries(string(maps))
	} else {
		p.Notes = append(p.Notes, "无法读取进程加载的运行时动态库："+err.Error())
	}
	after, err := infoReadFile(filepath.Join(dir, "stat"), 8192)
	if err != nil || processStart(before) != processStart(after) {
		return p, fmt.Errorf("采集时进程 %d 已退出或被替换", pid)
	}
	return p, nil
}

func processStart(stat []byte) string {
	text := string(stat)
	if i := strings.LastIndexByte(text, ')'); i >= 0 {
		fields := strings.Fields(text[i+1:])
		if len(fields) > 19 {
			return fields[19] // /proc/<pid>/stat 的第 22 项 starttime。
		}
	}
	return ""
}

func parseProcessStatus(text string, p *ProcessSnapshot) {
	for _, line := range strings.Split(text, "\n") {
		key, value, _ := strings.Cut(line, ":")
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		switch key {
		case "Uid":
			p.UID = fields[min(1, len(fields)-1)] // effective uid
		case "PPid":
			p.Parent, _ = strconv.Atoi(fields[0])
		case "Threads":
			p.Threads, _ = strconv.Atoi(fields[0])
		case "VmRSS":
			p.Memory, _ = strconv.ParseInt(fields[0], 10, 64)
			p.Memory *= 1024
		}
	}
}

func runtimeLibraries(maps string) []string {
	seen := map[string]bool{}
	var paths []string
	for _, line := range strings.Split(maps, "\n") {
		i := strings.Index(line, "/")
		if i < 0 {
			continue
		}
		path := strings.TrimSuffix(line[i:], " (deleted)")
		base := filepath.Base(path)
		if base != "libcoreclr.so" && base != "libjvm.so" && !strings.HasPrefix(base, "libpython") {
			continue
		}
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}
