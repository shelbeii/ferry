package agent

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/captain/ferry/internal/proto"
)

// 审计记录（ferry audit）：谁、什么时候、在这台服务器上做了什么改动。
//
//	<ferryDir>/audit.jsonl                   一行一条（proto.AuditEntry），只追加，只有 root 能读写
//	<ferryDir>/audit-<时间>.jsonl.gz          超过 20 MB 时轮转出去的，留最近 12 个
//
// 每条同时写一份到 syslog（logger -t ferry-audit）：进 journald，转发出去的日志 root 改了 audit.jsonl 也删不掉。
// 两种来源：改动服务器的 agent 命令（部署、软件、防火墙、告警、备份……）在 Run 里自动记；
// agent 管不到的（终端、文件页、数据库客户端）由本机发 audit 命令来记。

const (
	auditFile     = "audit.jsonl"
	auditRotateAt = 20 << 20
	auditKeep     = 12
)

func (a *Agent) auditPath() string { return filepath.Join(a.ferryDir(), auditFile) }

// auditCmd audit：req.Audit 不为空时追加，否则按 req.AuditQuery 读。
func (a *Agent) auditCmd() int {
	if e := a.req.Audit; e != nil {
		if !reAuditKind.MatchString(e.Kind) {
			return a.result(proto.CodeBadInput, "审计记录的类型无效："+e.Kind, nil)
		}
		if err := a.appendAudit(*e); err != nil {
			return a.result(proto.CodeFailed, "写审计记录失败："+err.Error(), nil)
		}
		return a.result(proto.CodeOK, "", nil)
	}
	q := proto.AuditQuery{}
	if a.req.AuditQuery != nil {
		q = *a.req.AuditQuery
	}
	out, err := readAudit(a.ferryDir(), q)
	if err != nil {
		return a.result(proto.CodeFailed, "读审计记录失败："+err.Error(), nil)
	}
	return a.result(proto.CodeOK, "", out)
}

// appendAudit 补上时间、操作人、项目、目标，追加一行（O_APPEND、一次写完，几个 agent 同时写也不会交错），再写 syslog。
func (a *Agent) appendAudit(e proto.AuditEntry) error {
	if e.Time.IsZero() {
		e.Time = a.Sys.Now()
	}
	if e.Actor == "" {
		e.Actor = a.req.Actor
	}
	if e.Project == "" {
		e.Project = a.req.Project
	}
	if e.Target == "" {
		e.Target = a.req.Target
	}
	if e.Result == "" {
		e.Result = "ok"
	}
	e.Detail = clipUTF8(e.Detail, proto.AuditMaxDetail)
	e.Object = clipUTF8(e.Object, 1024)
	e.Error = clipUTF8(e.Error, 1024)
	e.Host = ""
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	dir := a.ferryDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// 轮转和追加互斥（拿不到锁也照样追加：记录比轮转要紧）
	if unlock, lerr := a.Sys.Lock(filepath.Join(dir, "audit.lock"), 5*time.Second); lerr == nil {
		defer unlock()
		rotateAudit(dir, a.Sys.Now())
	}
	f, err := os.OpenFile(filepath.Join(dir, auditFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	a.Sys.Shell("logger -t ferry-audit -- "+shellQuote(auditText(e)), 10*time.Second)
	if werr != nil {
		return werr
	}
	return cerr
}

// auditText 写进 syslog 的一行。
func auditText(e proto.AuditEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", e.Actor, proto.AuditKindText(e.Kind))
	if e.Object != "" {
		b.WriteString(" " + e.Object)
	}
	if e.Detail != "" {
		d := strings.Join(strings.Fields(e.Detail), " ")
		if r := []rune(d); len(r) > 200 {
			d = string(r[:200]) + "…"
		}
		b.WriteString("（" + d + "）")
	}
	if e.Result == "failed" {
		b.WriteString("：失败")
		if e.Error != "" {
			b.WriteString("，" + strings.Join(strings.Fields(e.Error), " "))
		}
	}
	if e.Project != "" || e.Target != "" {
		fmt.Fprintf(&b, " [%s/%s]", e.Project, e.Target)
	}
	return b.String()
}

// rotateAudit 超过 20 MB 时压缩成 audit-<时间>.jsonl.gz，只留最近 12 个。
func rotateAudit(dir string, now time.Time) {
	p := filepath.Join(dir, auditFile)
	st, err := os.Stat(p)
	if err != nil || st.Size() < auditRotateAt {
		return
	}
	dst := filepath.Join(dir, "audit-"+now.Format("20060102-150405")+".jsonl.gz")
	if err := gzipFile(p, dst); err != nil {
		os.Remove(dst)
		return
	}
	os.Remove(p)
	old := rotatedAudit(dir)
	for len(old) > auditKeep {
		os.Remove(old[len(old)-1])
		old = old[:len(old)-1]
	}
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		out.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// rotatedAudit 轮转出去的文件，新的在前。
func rotatedAudit(dir string) []string {
	ms, _ := filepath.Glob(filepath.Join(dir, "audit-*.jsonl.gz"))
	sort.Sort(sort.Reverse(sort.StringSlice(ms)))
	return ms
}

// readAudit 按条件读，新的在前：先读 audit.jsonl，不够再往前读轮转出去的。
func readAudit(dir string, q proto.AuditQuery) ([]proto.AuditEntry, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	out := []proto.AuditEntry{}
	files := append([]string{filepath.Join(dir, auditFile)}, rotatedAudit(dir)...)
	for i, p := range files {
		got, older, err := readAuditFile(p, q)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if i == 0 {
				return nil, err
			}
			continue
		}
		for j := len(got) - 1; j >= 0 && len(out) < limit; j-- {
			out = append(out, got[j])
		}
		if len(out) >= limit || older {
			break
		}
	}
	return out, nil
}

// readAuditFile 一个文件里符合条件的（按时间先后）；older 为 true 表示这个文件里已经有早于 Since 的，不用再往前读了。
func readAuditFile(p string, q proto.AuditQuery) (list []proto.AuditEntry, older bool, err error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(p, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, false, err
		}
		defer zr.Close()
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e proto.AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if !q.Since.IsZero() && e.Time.Before(q.Since) {
			older = true
			continue
		}
		if !auditMatch(e, q) {
			continue
		}
		list = append(list, e)
	}
	return list, older, sc.Err()
}

// auditMatch 类型（以 - 结尾按前缀）、操作人（含这几个字）对得上。
func auditMatch(e proto.AuditEntry, q proto.AuditQuery) bool {
	if k := q.Kind; k != "" {
		if strings.HasSuffix(k, "-") {
			if !strings.HasPrefix(e.Kind, k) {
				return false
			}
		} else if e.Kind != k {
			return false
		}
	}
	if q.Actor != "" && !strings.Contains(strings.ToLower(e.Actor), strings.ToLower(q.Actor)) {
		return false
	}
	return true
}

// clipUTF8 截到 n 字节以内（不截断半个字），截了的加「…」。
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

var reAuditKind = regexp.MustCompile(`^[a-z]+(-[a-z]+)*$`)

// auditRun 改动服务器的命令做完后记一条（成功、失败都记）。没有操作人的（定时器自己运行的备份、告警）不记。
func (a *Agent) auditRun(cmd string, code int) {
	if a.req == nil || a.req.Actor == "" {
		return
	}
	kind, object, detail, ok := proto.AuditOf(cmd, a.req)
	if !ok {
		return
	}
	e := proto.AuditEntry{Kind: kind, Object: object, Detail: detail, Result: "ok"}
	if code != proto.CodeOK {
		e.Result, e.Error = "failed", a.lastMsg
	}
	if err := a.appendAudit(e); err != nil {
		fmt.Fprintf(os.Stderr, "ferry-agent：写审计记录失败：%v\n", err)
	}
}
