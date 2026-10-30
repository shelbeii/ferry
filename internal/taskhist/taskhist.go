// Package taskhist 桌面版任务面板的任务记录：每个任务一个文件，重启桌面版后还能看到；
// 按保留天数、总大小、个数清理（和 AI 助手的对话记录一样）。
package taskhist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/captain/ferry/internal/ai"
)

// 任务的状态（和桌面版一致）
const (
	Running     = "running"
	OK          = "ok"
	Failed      = "failed"
	Interrupted = "interrupted" // 桌面版关闭（或异常退出）时还没结束，结果未知
)

// InterruptedMsg 中断的任务显示的说明。
const InterruptedMsg = "桌面版关闭时这个任务还没结束，结果未知：请到服务器上查看状态"

// Record 一个任务。行、结果、错误详情由桌面版原样存取，这里不关心格式。
type Record struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Status    string          `json:"status"`
	StartedAt int64           `json:"startedAt"` // 毫秒
	EndedAt   int64           `json:"endedAt,omitempty"`
	Error     string          `json:"error,omitempty"`
	Config    string          `json:"config,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Lines     json.RawMessage `json:"lines,omitempty"`
}

// Summary 任务列表用的概要（不含输出）。
type Summary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt,omitempty"`
	Error     string `json:"error,omitempty"`
	Config    string `json:"config,omitempty"`
	Bytes     int64  `json:"bytes"`
}

var reID = regexp.MustCompile(`^[0-9a-z-]{6,40}$`)

func path(dir, id string) (string, error) {
	if !reID.MatchString(id) {
		return "", fmt.Errorf("任务 ID 无效：%q", id)
	}
	return filepath.Join(dir, id+".json"), nil
}

// Save 保存一个任务（只有本人能读：里面有服务器上的输出）。
func Save(dir string, r *Record) error {
	p, err := path(dir, r.ID)
	if err != nil {
		return err
	}
	switch r.Status {
	case Running, OK, Failed, Interrupted:
	default:
		return fmt.Errorf("任务状态无效：%q", r.Status)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ai.MakePrivate(dir)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Load 读一个任务。
func Load(dir, id string) (*Record, error) {
	p, err := path(dir, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("没有这个任务记录（可能已经被清理）：%s", id)
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("任务记录损坏：%v", err)
	}
	return &r, nil
}

// List 列出任务（最近开始的在前）。读不了的文件跳过。
// interrupt 为 true 时（桌面版刚启动）：还标着进行中的任务改成中断——它们是上次桌面版关闭时没跑完的。
func List(dir string, interrupt bool) ([]Summary, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Summary{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !reID.MatchString(id) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		r, err := Load(dir, id)
		if err != nil || r.ID != id {
			continue
		}
		if interrupt && r.Status == Running {
			r.Status, r.EndedAt = Interrupted, fi.ModTime().UnixMilli() // 最后一次保存的时间
			if r.Error == "" {
				r.Error = InterruptedMsg
			}
			if Save(dir, r) == nil {
				if fi2, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
					fi = fi2
				}
			}
		}
		out = append(out, Summary{ID: id, Title: r.Title, Status: r.Status, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
			Error: r.Error, Config: r.Config, Bytes: fi.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out, nil
}

// Delete 删除几个任务，返回删掉几个（本来就没有的不算错）。
func Delete(dir string, ids ...string) (int, error) {
	n := 0
	for _, id := range ids {
		p, err := path(dir, id)
		if err != nil {
			return n, err
		}
		err = os.Remove(p)
		if err == nil {
			n++
		} else if !errors.Is(err, os.ErrNotExist) {
			return n, err
		}
	}
	return n, nil
}

// Usage 任务记录一共几个、占多少字节。
func Usage(dir string) (count int, bytes int64) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			count++
			bytes += fi.Size()
		}
	}
	return
}

// Clean 按设置清理：删掉 days 天前的，再从最旧的删起，直到总大小不超过 maxBytes、个数不超过 maxCount。
// 三个值为 0 时不按它清理；keep 是正在保存的任务，不删。按最后一次保存的时间算新旧。返回删掉的任务 ID。
func Clean(dir string, days int, maxBytes int64, maxCount int, keep string) []string {
	ents, _ := os.ReadDir(dir)
	type f struct {
		id   string
		mod  time.Time
		size int64
	}
	var files []f
	var total int64
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{id, fi.ModTime(), fi.Size()})
		total += fi.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) }) // 最旧的在前
	count := len(files)
	removed := []string{}
	for _, x := range files {
		if keep != "" && x.id == keep {
			continue
		}
		old := days > 0 && time.Since(x.mod) > time.Duration(days)*24*time.Hour
		big := maxBytes > 0 && total > maxBytes
		many := maxCount > 0 && count > maxCount
		if !old && !big && !many {
			continue
		}
		if os.Remove(filepath.Join(dir, x.id+".json")) == nil {
			removed = append(removed, x.id)
			total -= x.size
			count--
		}
	}
	return removed
}

// CleanAll 删除全部任务记录。
func CleanAll(dir string) int {
	ents, _ := os.ReadDir(dir)
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") && os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// RelinkConfig 配置文件移动了：把 from 的任务改记成 to。返回改了几个。
func RelinkConfig(dir, from, to string) int {
	items, err := List(dir, false)
	if err != nil {
		return 0
	}
	n := 0
	for _, s := range items {
		if !samePath(s.Config, from) {
			continue
		}
		r, err := Load(dir, s.ID)
		if err != nil {
			continue
		}
		r.Config = to
		if Save(dir, r) == nil {
			n++
		}
	}
	return n
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || (runtime.GOOS == "windows" && strings.EqualFold(a, b))
}
