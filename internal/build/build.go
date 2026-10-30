// Package build 执行构建命令：同一条构建只执行一次，同名构建锁跨进程串行，输出同时写日志文件。
package build

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/flock"
	"github.com/captain/ferry/internal/safe"
)

// Step 一次要执行的构建。
type Step struct {
	Name    string
	Command string
	Shell   string
	Workdir string
	Env     []string
	Lock    string
	Apps    []*config.App
}

// Key 构建的唯一标识：命令、目录、环境、shell 都相同视为同一次构建。
func (s *Step) Key() string {
	return strings.Join([]string{s.Shell, s.Workdir, s.Command, strings.Join(s.Env, "\x00")}, "\x01")
}

// Plan 把要部署的应用按构建分组，展开命令里的变量。没有 build 的应用不需要构建。
func Plan(f *config.File, t *config.Target, apps []*config.App) ([]*Step, error) {
	var steps []*Step
	byName := map[string]*Step{}
	builds := map[string]*config.Build{}
	for _, a := range apps {
		b := f.ResolveBuild(a)
		if b == nil {
			continue
		}
		s, ok := byName[b.Name]
		if !ok {
			s = &Step{Name: b.Name}
			byName[b.Name] = s
			builds[b.Name] = b
			steps = append(steps, s)
		}
		s.Apps = append(s.Apps, a)
	}
	for _, s := range steps {
		b := builds[s.Name]
		base := map[string]string{"project": f.Project, "configDir": f.Dir, "target": t.Name}
		sc := &config.Scope{
			Builtins: base,
			Layers:   []map[string]string{t.Vars, f.Vars},
			Each: func(name string) (string, error) {
				var vals []string
				seen := map[string]bool{}
				for _, a := range s.Apps {
					one := &config.Scope{Builtins: map[string]string{"app": a.Name, "project": f.Project, "target": t.Name},
						Layers: []map[string]string{a.Vars, t.Vars, f.Vars}}
					v, err := one.Expand("${" + name + "}")
					if err != nil {
						return "", fmt.Errorf("应用 %s：%w", a.Name, err)
					}
					if !seen[v] {
						seen[v] = true
						vals = append(vals, v)
					}
				}
				return strings.Join(vals, ","), nil
			},
		}
		if len(s.Apps) == 1 {
			// 单个应用的构建可以直接用应用变量
			sc.Layers = append([]map[string]string{s.Apps[0].Vars}, sc.Layers...)
			sc.Builtins["app"] = s.Apps[0].Name
		}
		cmd, err := sc.Expand(b.Command)
		if err != nil {
			return nil, fmt.Errorf("构建 %s：%w", s.Name, err)
		}
		s.Command = cmd
		s.Shell = b.Shell
		wd := b.Workdir
		if wd == "" {
			wd = "."
		}
		if wd, err = sc.Expand(wd); err != nil {
			return nil, fmt.Errorf("构建 %s：%w", s.Name, err)
		}
		s.Workdir = f.LocalPath(wd)
		// 相对路径按配置文件所在目录计算：配置文件挪了位置，相对路径常会指到磁盘根目录或不存在的目录
		if st, err := os.Stat(s.Workdir); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("构建 %s 的目录 %s 不存在（workdir 相对配置文件所在目录 %s；配置文件挪过位置？）", s.Name, s.Workdir, f.Dir)
		}
		if filepath.Dir(s.Workdir) == s.Workdir {
			return nil, fmt.Errorf("构建 %s 的目录是磁盘根目录 %s：检查 workdir 及其中的变量（相对路径按配置文件所在目录 %s 计算，配置文件挪过位置？）", s.Name, s.Workdir, f.Dir)
		}
		env, err := sc.ExpandMap(b.Env)
		if err != nil {
			return nil, fmt.Errorf("构建 %s：%w", s.Name, err)
		}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.Env = append(s.Env, k+"="+env[k])
		}
		s.Lock = b.Lock
		if s.Lock == "" {
			sum := sha256.Sum256([]byte(strings.ToLower(s.Workdir)))
			s.Lock = "dir-" + hex.EncodeToString(sum[:8])
		}
	}
	return steps, nil
}

// Runner 执行构建。
type Runner struct {
	LogDir  string            // 构建日志目录
	LockDir string            // 构建锁目录（所有项目共用，在数据目录下）
	Env     []string          // 设置里的构建环境（在构建自身的 env 之前）
	Out     io.Writer         // 实时输出（nil = 不输出）
	OnWait  func(lock string) // 等构建锁时回调
	LockMax time.Duration     // 最多等锁多久，默认 1 小时
	Ctx     context.Context   // 取消时结束构建（含子进程）；nil = 不能取消
}

// Run 执行一步构建，返回开始时间（产物修改时间不能早于它）。
func (r *Runner) Run(s *Step) (time.Time, error) {
	max := r.LockMax
	if max == 0 {
		max = time.Hour
	}
	lockPath := filepath.Join(r.LockDir, s.Lock+".lock")
	unlock, err := flock.Lock(lockPath, max, func() {
		if r.OnWait != nil {
			r.OnWait(s.Lock)
		}
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("构建锁 %s：%w", s.Lock, err)
	}
	defer unlock()

	start := time.Now()
	if err := os.MkdirAll(r.LogDir, 0o755); err != nil {
		return start, err
	}
	logPath := filepath.Join(r.LogDir, fmt.Sprintf("build-%s-%s.log", start.Format("20060102-150405"), safeName(s.Name)))
	logFile, err := os.Create(logPath)
	if err != nil {
		return start, err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "# %s\n# 目录：%s\n# 命令：%s\n\n", start.Format(time.RFC3339), s.Workdir, s.Command)

	cmd := shellCommand(s.Shell, s.Command)
	cmd.Dir = s.Workdir
	cmd.Env = append(append(os.Environ(), r.Env...), s.Env...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	tail := &tailBuffer{max: 40}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer safe.Recover("构建输出", nil)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			line := outputLine(sc.Text())
			fmt.Fprintln(logFile, line)
			tail.add(line)
			if r.Out != nil {
				fmt.Fprintln(r.Out, line)
			}
		}
		io.Copy(io.Discard, pr)
	}()
	ownGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Start()
	if runErr == nil {
		var stopWatch func() bool
		if r.Ctx != nil {
			stopWatch = context.AfterFunc(r.Ctx, func() { killTree(cmd) })
		}
		runErr = cmd.Wait()
		if stopWatch != nil {
			stopWatch()
		}
	}
	pw.Close()
	wg.Wait()
	if r.Ctx != nil && r.Ctx.Err() != nil {
		return start, ErrCanceled
	}
	if runErr != nil {
		return start, &Error{Step: s, Err: runErr, Tail: tail.lines, Log: logPath}
	}
	return start, nil
}

// ErrCanceled 构建被取消。
var ErrCanceled = errors.New("已取消")

// Error 构建失败（带最后几行输出与日志路径）。
type Error struct {
	Step *Step
	Err  error
	Tail []string
	Log  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("构建 %s 失败：%v（日志：%s）", e.Step.Name, e.Err, e.Log)
}

func shellCommand(shell, command string) *exec.Cmd {
	switch strings.ToLower(shell) {
	case "bash", "sh", "zsh":
		return exec.Command(shell, "-c", command)
	case "powershell", "pwsh":
		return exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", command)
	case "cmd":
		return cmdExe(command)
	case "":
		return defaultShell(command)
	}
	return exec.Command(shell, "-c", command)
}

// outputLine 构建输出的一行：Windows 上不是 UTF-8 的按 GBK 转换
// （中文 Windows 的默认编码，Maven 转述的系统报错、javac 的中文提示都是它，不转就是乱码）。
func outputLine(s string) string {
	if runtime.GOOS != "windows" || utf8.ValidString(s) {
		return s
	}
	if d, err := simplifiedchinese.GBK.NewDecoder().String(s); err == nil {
		return d
	}
	return s
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

type tailBuffer struct {
	max   int
	lines []string
}

func (t *tailBuffer) add(l string) {
	t.lines = append(t.lines, l)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}
