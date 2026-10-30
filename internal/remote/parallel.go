package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"

	"github.com/captain/ferry/internal/safe"
)

// UploadItem 一个要上传的文件。
type UploadItem struct {
	Local  string
	Remote string
	Mode   os.FileMode
	Size   int64
}

// 切片大小与每次写入的大小：切片决定连接间怎样分活，写入粒度决定进度刷新的频率
// （经 VPN 单条连接可能只有 0.2 MB/s，4 MB 一片要写 20 秒，进度不能按片报）。
const (
	chunkSize = 4 << 20
	writeSize = 256 << 10
)

// stallTimeout 一次写入（256 KB）这么久都没写完：连接卡死了（隧道、VPN 断了却没有报错），关掉它，重新连接后接着传。
var stallTimeout = 90 * time.Second

// 端到端测试用：传到这么多字节时断开主连接一次（FERRY_TEST_DROP_UPLOAD_AFTER），看能不能重新连接后接着传
var (
	testDropAfter, _ = strconv.ParseInt(os.Getenv("FERRY_TEST_DROP_UPLOAD_AFTER"), 10, 64)
	testDropped      atomic.Bool
)

// ErrStalled 上传卡住了。
var ErrStalled = errors.New("上传卡住了：长时间没有进展")

// UploadState 一组文件的上传进度：哪些片已经写完。连接断了重新连上后，用同一个 UploadState 再调用 UploadParallel，
// 只传还没写完的片。
type UploadState struct {
	mu      sync.Mutex
	created map[int]bool            // 远程的 .part 已建好
	renamed map[int]bool            // 已经传完、改好名
	done    map[int]map[int64]int64 // 文件 → 写完的片：起点 → 长度
	bytes   int64
}

// Done 已经写完的字节数。
func (st *UploadState) Done() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.bytes
}

func (st *UploadState) init() {
	if st.created == nil {
		st.created, st.renamed, st.done = map[int]bool{}, map[int]bool{}, map[int]map[int64]int64{}
	}
}

func (st *UploadState) finished(item int, off int64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.done[item][off]
	return ok
}

func (st *UploadState) mark(item int, off, n int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done[item] == nil {
		st.done[item] = map[int64]int64{}
	}
	if _, ok := st.done[item][off]; !ok {
		st.done[item][off] = n
		st.bytes += n
	}
}

// reset 远程的 .part 没了（或长度不对）：这个文件从头传。
func (st *UploadState) reset(item int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, n := range st.done[item] {
		st.bytes -= n
	}
	delete(st.done, item)
	delete(st.created, item)
}

// ConnLost 错误是连接断了、卡住了（重新连接后可以接着传），而不是没权限、磁盘满这类换个连接也没用的问题。
func ConnLost(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrStalled) || errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"connection lost", "connection reset", "broken pipe", "closed network connection", "timed out", "timeout", "eof", "连接已断开"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// UploadParallel 把文件切成 4 MB 的片，用多条连接并行写入同一个远程文件的不同位置。
// 有的网络（如 VPN）对单条 TCP 连接限速，多条连接能成倍提速。
// primary 负责创建临时文件与最后改名；extra 是额外的连接（可以为空）。progress 报告累计上传的字节数。
// 写入目标是 <Remote>.part，全部完成后改名；完整性由服务器端的 sha256 校验保证。
// st 不为空时记下写完的片：连接断了（ConnLost）时 .part 留着，重新连接后用同一个 st 再调用，只传剩下的。
func UploadParallel(ctx context.Context, primary *Client, extra []*Client, items []UploadItem, st *UploadState, progress func(done int64, label string)) error {
	if st == nil {
		st = &UploadState{}
	}
	st.mu.Lock()
	st.init()
	st.mu.Unlock()
	ps, err := primary.sftpClient()
	if err != nil {
		return err
	}
	// 1. 建好临时文件并预设长度；接着传时确认上次的还在
	for i, it := range items {
		st.mu.Lock()
		created, renamed := st.created[i], st.renamed[i]
		st.mu.Unlock()
		if renamed {
			continue
		}
		if created {
			if fi, err := ps.Lstat(it.Remote + ".part"); err == nil && fi.Size() == it.Size {
				continue
			} else if err != nil && ConnLost(err) {
				return err
			}
			st.reset(i)
		}
		f, err := ps.OpenFile(it.Remote+".part", os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			return fmt.Errorf("创建远程文件 %s：%w", it.Remote, err)
		}
		if it.Size > 0 {
			if err := f.Truncate(it.Size); err != nil {
				f.Close()
				return fmt.Errorf("预分配 %s：%w", it.Remote, err)
			}
		}
		f.Close()
		st.mu.Lock()
		st.created[i] = true
		st.mu.Unlock()
	}
	// 出了换个连接也没用的错：删掉临时文件（连接断了的留着，接着传）
	cleanup := func(err error) {
		if ConnLost(err) || errors.Is(err, ErrCanceled) {
			return
		}
		for i, it := range items {
			ps.Remove(it.Remote + ".part")
			st.reset(i)
		}
	}

	// 2. 切片（已经写完的跳过）
	type task struct {
		item   int
		offset int64
		length int64
	}
	var tasks []task
	for i, it := range items {
		for off := int64(0); off < it.Size; off += chunkSize {
			n := int64(chunkSize)
			if off+n > it.Size {
				n = it.Size - off
			}
			if !st.finished(i, off) {
				tasks = append(tasks, task{i, off, n})
			}
		}
	}
	queue := make(chan task, len(tasks))
	for _, t := range tasks {
		queue <- t
	}
	close(queue)

	locals := make([]*os.File, len(items))
	for i, it := range items {
		f, err := os.Open(it.Local)
		if err != nil {
			cleanup(err)
			return err
		}
		defer f.Close()
		locals[i] = f
	}

	// 3. 每条连接一个工作者，从队列里取片写入
	clients := append([]*Client{primary}, extra...)
	var (
		done    atomic.Int64
		failed  atomic.Bool
		errOnce sync.Once
		first   error
		wg      sync.WaitGroup
	)
	done.Store(st.Done())
	fail := func(err error) {
		errOnce.Do(func() { first = err })
		failed.Store(true)
	}
	for _, c := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			var perr error
			defer func() {
				if perr != nil {
					fail(perr)
				}
			}()
			defer safe.Recover("并行上传", &perr)
			s, err := c.sftpClient()
			if err != nil {
				fail(err)
				return
			}
			handles := map[int]*sftp.File{}
			defer func() {
				for _, h := range handles {
					h.Close()
				}
			}()
			buf := make([]byte, writeSize)
			for t := range queue {
				if failed.Load() {
					return
				}
				if ctx.Err() != nil {
					fail(ErrCanceled)
					return
				}
				h := handles[t.item]
				if h == nil {
					if h, err = s.OpenFile(items[t.item].Remote+".part", os.O_WRONLY); err != nil {
						fail(fmt.Errorf("打开远程文件 %s：%w", items[t.item].Remote, err))
						return
					}
					handles[t.item] = h
				}
				var wrote int64
				for off := t.offset; off < t.offset+t.length; {
					n := int64(len(buf))
					if rest := t.offset + t.length - off; rest < n {
						n = rest
					}
					if _, err := locals[t.item].ReadAt(buf[:n], off); err != nil && !errors.Is(err, io.EOF) {
						fail(err)
						return
					}
					// 卡住（连接没断，但数据过不去）时关掉这条连接，让写入报错，重新连接后接着传
					var stalled atomic.Bool
					timer := time.AfterFunc(stallTimeout, func() { stalled.Store(true); c.Close() })
					_, err := h.WriteAt(buf[:n], off)
					timer.Stop()
					if err != nil {
						if stalled.Load() {
							err = fmt.Errorf("%w（%s 内 %s 没写完）", ErrStalled, stallTimeout, items[t.item].Remote)
						}
						// 这一片没写完：进度退回去，下次重传整片
						if progress != nil {
							progress(done.Add(-wrote), items[t.item].Remote)
						}
						fail(fmt.Errorf("上传 %s：%w", items[t.item].Remote, err))
						return
					}
					off += n
					wrote += n
					total := done.Add(n)
					if progress != nil {
						progress(total, items[t.item].Remote)
					}
					if testDropAfter > 0 && total >= testDropAfter && testDropped.CompareAndSwap(false, true) {
						primary.Close()
					}
				}
				st.mark(t.item, t.offset, t.length)
			}
		}(c)
	}
	wg.Wait()
	if first != nil {
		cleanup(first)
		return first
	}

	// 4. 全部写完：设权限、改名
	for i, it := range items {
		st.mu.Lock()
		renamed := st.renamed[i]
		st.mu.Unlock()
		if renamed {
			continue
		}
		if err := ps.Chmod(it.Remote+".part", it.Mode); err != nil {
			cleanup(err)
			return err
		}
		if err := ps.PosixRename(it.Remote+".part", it.Remote); err != nil {
			cleanup(err)
			return fmt.Errorf("改名 %s：%w", it.Remote, err)
		}
		st.mu.Lock()
		st.renamed[i] = true
		st.mu.Unlock()
	}
	return nil
}
