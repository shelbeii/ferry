package engine

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
)

// 审计记录：agent 管不到的改动（终端、文件页、数据库客户端）由这里发给服务器上的 agent 记下（audit 命令，写
// <ferryDir>/audit.jsonl 和 syslog）；这台电脑上也留一份（<数据目录>/audit.jsonl）。改动服务器的 agent 命令
// （部署、软件、防火墙……）agent 自己记，这里只记一份到本机。
// 写不上不影响操作本身（只提醒一次）：记录很要紧，但不能因为它让人没法救火。

// processSession 这次运行的 Ferry 的会话号（桌面版开着期间不变），终端在后面加序号。
var processSession = func() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}()

var termSeq atomic.Int32

// auditEntry 填好操作人、项目、目标的一条。
func (s *session) auditEntry(kind, object, detail string, err error) proto.AuditEntry {
	e := proto.AuditEntry{
		Time: time.Now(), Actor: s.e.actor(), Project: s.e.File.Project, Target: s.t.Name, Host: s.t.Host,
		Session: processSession, Kind: kind, Object: object, Detail: detail, Result: "ok",
	}
	if err != nil {
		e.Result, e.Error = "failed", err.Error()
	}
	return e
}

// audit 记一条：服务器上（有 agent 时写 audit.jsonl + syslog，只登录的「新建连接」只写 syslog）和这台电脑上。
// 用单独的 15 秒超时（用户取消了操作也要记上）。
func (s *session) audit(e proto.AuditEntry) {
	saveLocalAudit(e)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var err error
	if s.agent == "" {
		msg := fmt.Sprintf("%s %s %s", e.Actor, proto.AuditKindText(e.Kind), e.Object)
		_, err = s.c.RunContext(ctx, "logger -t ferry-audit -- "+shQuote(strings.TrimSpace(msg)), nil, nil)
	} else {
		eng := *s.e
		eng.Ctx = ctx
		eng.Rep = quietReporter{}
		sc := *s
		sc.e = &eng
		remoteEntry := e
		remoteEntry.Host = ""
		err = sc.callData("audit", &proto.Request{Audit: &remoteEntry}, nil)
	}
	if err != nil {
		s.e.warnAuditOnce(err)
	}
}

var auditWarned sync.Map

// warnAuditOnce 每台服务器只提醒一次。
func (e *Engine) warnAuditOnce(err error) {
	if e.Rep == nil {
		return
	}
	if _, dup := auditWarned.LoadOrStore(err.Error(), true); dup {
		return
	}
	e.Rep.Warn("审计记录没写到服务器上（只记在了这台电脑上）：%v", err)
}

// quietReporter 写审计记录时不输出东西。
type quietReporter struct{}

func (quietReporter) Step(string, ...any)           {}
func (quietReporter) Info(string, ...any)           {}
func (quietReporter) Warn(string, ...any)           {}
func (quietReporter) Error(string, ...any)          {}
func (quietReporter) Event(string, proto.Event)     {}
func (quietReporter) Progress(string, int64, int64) {}
func (quietReporter) BuildOutput() io.Writer        { return io.Discard }
func (quietReporter) Confirm(string) bool           { return false }
func (quietReporter) Secret(string) (string, bool)  { return "", false }

// withChange 像 withClient，做完再记一条审计（kind、object、detail）。要传的文件已经存在（还没改动）时不记。
func (e *Engine) withChange(target, kind, object, detail string, fn func(c *remote.Client) error) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	s, err := e.connect(t)
	if err != nil {
		return err
	}
	defer s.close()
	err = fn(s.c)
	if err != nil && !s.c.Alive() {
		s.broken.Store(true)
	}
	var exists *ExistsError
	if !errors.As(err, &exists) {
		s.audit(s.auditEntry(kind, object, detail, err))
	}
	return err
}

// Audit 记一条审计：连上目标（数据库客户端经这台服务器连时用）。target 为空时只记在这台电脑上。
func (e *Engine) Audit(target, kind, object, detail string, err error) {
	if target == "" || e.File == nil {
		AuditLocal(e.actor(), kind, object, detail, err)
		return
	}
	t, terr := e.File.Target(target)
	if terr != nil {
		return
	}
	s, cerr := e.connect(t)
	if cerr != nil {
		entry := proto.AuditEntry{Time: time.Now(), Actor: e.actor(), Project: e.File.Project, Target: t.Name, Host: t.Host, Session: processSession,
			Kind: kind, Object: object, Detail: detail, Result: "ok"}
		if err != nil {
			entry.Result, entry.Error = "failed", err.Error()
		}
		saveLocalAudit(entry)
		e.warnAuditOnce(cerr)
		return
	}
	defer s.close()
	s.audit(s.auditEntry(kind, object, detail, err))
}

// AuditLocal 只记在这台电脑上（直接连的数据库，没有经过服务器）。
func AuditLocal(actor, kind, object, detail string, err error) {
	e := proto.AuditEntry{Time: time.Now(), Actor: actor, Session: processSession, Kind: kind, Object: object, Detail: detail, Result: "ok"}
	if err != nil {
		e.Result, e.Error = "failed", err.Error()
	}
	saveLocalAudit(e)
}

// Actor 操作人（记审计、部署历史用）。
func (e *Engine) Actor() string { return e.actor() }

// AuditLog 读服务器上的审计记录（新的在前）。
func (e *Engine) AuditLog(target string, q proto.AuditQuery) ([]proto.AuditEntry, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	if t.Plain {
		return nil, errors.New("「新建连接」的服务器没有装 Ferry 的组件，审计记录只在 syslog 里（journalctl -t ferry-audit）")
	}
	s, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	var out []proto.AuditEntry
	if err := s.callData("audit", &proto.Request{AuditQuery: &q}, &out); err != nil {
		var ae *AgentError
		if errors.As(err, &ae) && strings.Contains(ae.Msg, "未知命令") {
			return nil, errors.New("服务器上的 Ferry 组件太旧，没有审计记录：重新连接一次就会更新")
		}
		return nil, err
	}
	if out == nil {
		out = []proto.AuditEntry{}
	}
	return out, nil
}

// ---------------------------------------------------------------- 这台电脑上的那份

const localAuditRotateAt = 20 << 20

var localAuditMu sync.Mutex

func localAuditPath() string { return filepath.Join(appdir.Dir(), "audit.jsonl") }

// saveLocalAudit 追加到 <数据目录>/audit.jsonl；超过 20 MB 改名成 audit-<时间>.jsonl，留 5 个。
func saveLocalAudit(e proto.AuditEntry) {
	// 跑测试时只写到 FERRY_HOME 指的测试目录，不碰真的数据目录
	if testing.Testing() && os.Getenv("FERRY_HOME") == "" {
		return
	}
	if appdir.Dir() == "" {
		return
	}
	e.Detail = clipAudit(e.Detail, proto.AuditMaxDetail)
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	localAuditMu.Lock()
	defer localAuditMu.Unlock()
	p := localAuditPath()
	if st, err := os.Stat(p); err == nil && st.Size() >= localAuditRotateAt {
		os.Rename(p, filepath.Join(filepath.Dir(p), "audit-"+time.Now().Format("20060102-150405")+".jsonl"))
		old, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "audit-*.jsonl"))
		sort.Strings(old)
		for len(old) > 5 {
			os.Remove(old[0])
			old = old[1:]
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

// LocalAuditLog 这台电脑上做过的（新的在前）；target 不为空时只看这台服务器的。
func LocalAuditLog(q proto.AuditQuery, project, target string) ([]proto.AuditEntry, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	localAuditMu.Lock()
	defer localAuditMu.Unlock()
	f, err := os.Open(localAuditPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []proto.AuditEntry{}, nil
		}
		return nil, err
	}
	defer f.Close()
	var all []proto.AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e proto.AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if !q.Since.IsZero() && e.Time.Before(q.Since) {
			continue
		}
		if !AuditMatch(e, q) || (project != "" && e.Project != project) || (target != "" && e.Target != target) {
			continue
		}
		all = append(all, e)
	}
	out := []proto.AuditEntry{}
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out, sc.Err()
}

// AuditMatch 类型（以 - 结尾按前缀）、操作人（含这几个字）对得上。
func AuditMatch(e proto.AuditEntry, q proto.AuditQuery) bool {
	if k := q.Kind; k != "" {
		if strings.HasSuffix(k, "-") {
			if !strings.HasPrefix(e.Kind, k) {
				return false
			}
		} else if e.Kind != k {
			return false
		}
	}
	return q.Actor == "" || strings.Contains(strings.ToLower(e.Actor), strings.ToLower(q.Actor))
}

func clipAudit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s[:n])
	if len(r) > 0 && r[len(r)-1] == 0xFFFD {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
