package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Split 把一段 SQL 按分号拆成语句：跳过引号、注释里的分号，PostgreSQL 的 $$ 函数体；
// MySQL 支持 DELIMITER（写存储过程时用）。去掉空语句和只有注释的语句。
func Split(kind, src string) []string {
	out, _, _ := splitScan(kind, src, ";", true)
	return out
}

// splitScan 拆语句的核心。final 为 false 时（文件读到一半）只返回已经结束的语句（后面跟着分隔符的）：
// used 是用掉了多少字节（剩下的接上后面读到的再拆），delim 是读到这里时的分隔符（MySQL 的 DELIMITER 改过的）。
func splitScan(kind, src, delim string, final bool) (out []string, used int, endDelim string) {
	var cur strings.Builder
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s != "" && strings.TrimSpace(stripComments(kind, s)) != "" {
			out = append(out, s)
		}
	}
	n := len(src)
	atLineStart := true
	for i := 0; i < n; {
		c := src[i]
		// MySQL：行首的 DELIMITER xx
		if kind == MySQL && atLineStart {
			j := i
			for j < n && (src[j] == ' ' || src[j] == '\t') {
				j++
			}
			if j+10 <= n && strings.EqualFold(src[j:j+9], "DELIMITER") && (src[j+9] == ' ' || src[j+9] == '\t') {
				end := strings.IndexByte(src[j:], '\n')
				line := src[j:]
				if end >= 0 {
					line = src[j : j+end]
				}
				if end < 0 && !final {
					break // DELIMITER 这一行还没读完
				}
				if d := strings.TrimSpace(line[9:]); d != "" {
					flush()
					delim = d
				}
				if end < 0 {
					i = n
				} else {
					i = j + end + 1
				}
				used = i
				continue
			}
		}
		atLineStart = false
		switch {
		case c == '\n':
			atLineStart = true
			cur.WriteByte(c)
			i++
		case c == '\'' || c == '"' || (c == '`' && kind == MySQL):
			j := skipQuoted(kind, src, i)
			cur.WriteString(src[i:j])
			i = j
		case c == '-' && i+1 < n && src[i+1] == '-' && (kind != MySQL || i+2 >= n || src[i+2] == ' ' || src[i+2] == '\t' || src[i+2] == '\n' || src[i+2] == '\r'):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = n - i
			}
			cur.WriteString(src[i : i+j])
			i += j
		case c == '#' && kind == MySQL:
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = n - i
			}
			cur.WriteString(src[i : i+j])
			i += j
		case c == '/' && i+1 < n && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				j = n
			} else {
				j = i + 2 + j + 2
			}
			cur.WriteString(src[i:j])
			i = j
		case c == '$' && kind == Postgres:
			if tag, ok := dollarTag(src[i:]); ok {
				end := strings.Index(src[i+len(tag):], tag)
				j := n
				if end >= 0 {
					j = i + len(tag) + end + len(tag)
				}
				cur.WriteString(src[i:j])
				i = j
				continue
			}
			cur.WriteByte(c)
			i++
		case strings.HasPrefix(src[i:], delim):
			flush()
			i += len(delim)
			used = i
		default:
			cur.WriteByte(c)
			i++
		}
	}
	if final {
		flush()
		used = n
	}
	return out, used, delim
}

func skipQuoted(kind, s string, i int) int {
	q := s[i]
	j := i + 1
	for j < len(s) {
		c := s[j]
		if c == '\\' && kind == MySQL && q != '`' {
			j += 2
			continue
		}
		if c == q {
			if j+1 < len(s) && s[j+1] == q { // '' 转义
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(s)
}

// dollarTag $$ 或 $tag$。
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (j > 1 && c >= '0' && c <= '9') || c >= 0x80) {
			return "", false
		}
	}
	return "", false
}

func stripComments(kind, s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || (c == '`' && kind == MySQL):
			j := skipQuoted(kind, s, i)
			b.WriteString(s[i:j])
			i = j
		case c == '-' && i+1 < len(s) && s[i+1] == '-', c == '#' && kind == MySQL:
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return b.String()
			}
			i += j
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			b.WriteByte(' ')
			i += j + 4
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// FirstWord 语句的第一个关键字（大写，跳过注释和左括号）。
func FirstWord(kind, stmt string) string {
	s := strings.TrimLeft(stripComments(kind, stmt), " \t\r\n(")
	end := strings.IndexFunc(s, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	if end < 0 {
		end = len(s)
	}
	return strings.ToUpper(s[:end])
}

var readOnlyWords = map[string]bool{"SELECT": true, "SHOW": true, "DESC": true, "DESCRIBE": true, "EXPLAIN": true,
	"WITH": true, "VALUES": true, "TABLE": true, "USE": true, "HELP": true}

// IsReadOnly 只读连接能执行的语句（另外还放在只读事务里执行，WITH … DELETE 这类会被数据库拒绝）。
func IsReadOnly(kind, stmt string) bool { return readOnlyWords[FirstWord(kind, stmt)] }

// returnsRows 这种语句会返回结果集。
func returnsRows(kind, stmt string) bool {
	switch FirstWord(kind, stmt) {
	case "SELECT", "SHOW", "DESC", "DESCRIBE", "EXPLAIN", "WITH", "VALUES", "TABLE", "HELP", "CALL", "CHECK", "ANALYZE", "OPTIMIZE", "REPAIR", "CHECKSUM":
		return true
	}
	// PostgreSQL：INSERT … RETURNING
	return kind == Postgres && strings.Contains(strings.ToUpper(stripComments(kind, stmt)), "RETURNING")
}

// reModify 会改数据的关键字（判断 WITH 里有没有 INSERT、UPDATE……用）
var reModify = regexp.MustCompile(`\b(INSERT|UPDATE|DELETE|MERGE|REPLACE)\b`)

// stoppable 读够行数后可以让数据库停下的语句：只读的查询。INSERT … RETURNING、带 DELETE 的 WITH 这类
// 停下来会少改数据，要等它执行完。
func stoppable(kind, stmt string) bool {
	switch FirstWord(kind, stmt) {
	case "SELECT", "SHOW", "TABLE", "VALUES", "DESC", "DESCRIBE":
		return true
	case "WITH":
		return !reModify.MatchString(stmtFlat(kind, stmt))
	}
	return false
}

// QueryOptions 执行查询的选项。
type QueryOptions struct {
	Session string // 查询窗口的会话 ID
	Max     int    // 每条语句最多返回的行数
	// Stop 出错后不再执行后面的语句
	Stop bool
}

// Query 执行一段 SQL（可以有多条语句），每条一个结果。
func (cl *Client) Query(ctx context.Context, dbName, src string, o QueryOptions) ([]Result, error) {
	kind := cl.Conn.Kind
	stmts := Split(kind, src)
	if len(stmts) == 0 {
		return nil, errors.New("没有要执行的语句")
	}
	if cl.Conn.ReadOnly {
		for _, s := range stmts {
			if !IsReadOnly(kind, s) {
				return nil, fmt.Errorf("这个连接设成了只读，只能执行 SELECT、SHOW 这类查询：%s", firstLine(s))
			}
		}
	}
	if o.Max <= 0 {
		o.Max = 1000
	}
	conn, release, err := cl.Session(ctx, o.Session, dbName)
	if err != nil {
		return nil, err
	}
	defer release()
	id := cl.sessionID(o.Session)
	if o.Session == "" {
		id = cl.connID(ctx, conn)
	}
	out := []Result{}
	for _, s := range stmts {
		r := cl.runOne(ctx, conn, dbName, id, s, o.Max)
		out = append(out, r)
		if ctx.Err() != nil {
			// 取消时驱动会关掉这条连接：会话作废，下次重建
			if o.Session != "" {
				cl.DropSession(o.Session)
			}
			break
		}
		if r.Error != "" && o.Stop {
			break
		}
	}
	return out, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i] + " …"
	}
	if len(s) > 120 {
		s = s[:120] + " …"
	}
	return s
}

// runOne 执行一条语句。id 是这条连接在数据库里的编号：点了停止时让数据库停下这条语句；
// 查询读够 max 行时也让数据库停下（不然驱动会把剩下的结果全部读完再丢掉，表大时要传很久）。
func (cl *Client) runOne(ctx context.Context, conn *sql.Conn, dbName, id, stmt string, max int) Result {
	kind := cl.Conn.Kind
	start := time.Now()
	if id != "" {
		stop := context.AfterFunc(ctx, func() { cl.stopStmt(dbName, id) })
		defer stop()
	}
	r := Result{SQL: stmt}
	fail := func(err error) Result {
		err = cl.explain(err)
		r.Error = err.Error()
		r.Code = ErrorCode(err)
		r.Ms = time.Since(start).Milliseconds()
		return r
	}
	type querier interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}
	var q querier = conn
	var tx *sql.Tx
	if cl.Conn.ReadOnly {
		var err error
		tx, err = conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return fail(err)
		}
		defer tx.Rollback()
		q = tx
	}
	if returnsRows(kind, stmt) {
		canStop := id != "" && stoppable(kind, stmt)
		// PostgreSQL：停下用户事务里的语句会让整个事务作废，先设保存点，停了以后退回到这里
		savepoint := canStop && tx == nil && kind == Postgres && pgInTx(conn)
		if savepoint {
			if _, err := conn.ExecContext(ctx, "SAVEPOINT ferry_more"); err != nil {
				savepoint, canStop = false, false
			}
		}
		rows, err := q.QueryContext(ctx, stmt)
		if err != nil {
			return fail(err)
		}
		cols, data, more, err := readRows(rows, max)
		stopped := more && canStop
		if stopped {
			cl.stopStmt(dbName, id)
		}
		rows.Close()
		if savepoint {
			if stopped {
				conn.ExecContext(ctx, "ROLLBACK TO SAVEPOINT ferry_more")
			}
			conn.ExecContext(ctx, "RELEASE SAVEPOINT ferry_more")
		}
		if err != nil {
			return fail(err)
		}
		r.Columns, r.Rows, r.More = cols, data, more
		if len(cols) == 0 {
			r.Rows = nil
			zero := int64(0)
			r.Affected = &zero
		}
	} else {
		res, err := q.ExecContext(ctx, stmt)
		if err != nil {
			return fail(err)
		}
		if n, err := res.RowsAffected(); err == nil {
			r.Affected = &n
		}
		if kind == MySQL {
			if id, err := res.LastInsertId(); err == nil && id > 0 {
				r.LastID = &id
			}
		}
	}
	r.Ms = time.Since(start).Milliseconds()
	return r
}

// Exec 依次执行几条语句（结构设计、删表等确认过的改动），出错就停。
func (cl *Client) Exec(ctx context.Context, dbName string, stmts []string) ([]Result, error) {
	if cl.Conn.ReadOnly {
		return nil, ErrReadOnly
	}
	conn, release, err := cl.Session(ctx, "", dbName)
	if err != nil {
		return nil, err
	}
	defer release()
	id := cl.connID(ctx, conn)
	out := []Result{}
	for _, s := range stmts {
		r := cl.runOne(ctx, conn, dbName, id, s, 100)
		out = append(out, r)
		if r.Error != "" || ctx.Err() != nil {
			break
		}
	}
	return out, nil
}
