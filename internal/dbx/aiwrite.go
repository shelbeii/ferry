package dbx

// AI 改数据（连接上勾选了「允许 AI 修改」）：插入、修改、删除数据，建表改表。
// 每次都要用户看过完整的 SQL 同意后才执行（编程软件的在 Ferry 桌面版里确认，Ferry 的 AI 助手在对话里确认），
// 这里只管检查和执行：不许动账号权限、服务器级的设置和对象，不许建删库，不许读写服务器上的文件、执行系统命令；
// 全部语句在一个事务里执行，有一条失败就全部撤回（MySQL 的建表改表语句会立即生效，撤不回）。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// AIMaxStmts 一次最多多少条语句
	AIMaxStmts = 100
	// aiMaxSQL 一次的 SQL 最多多长
	aiMaxSQL = 1 << 20
	// aiWriteRows 改动里的查询最多返回多少行
	aiWriteRows = 50
)

// AIWriteTimeout 一次改动最多执行多久（测试时改短）；等锁最多 aiLockWait。
var AIWriteTimeout = 10 * time.Minute

var aiLockWait = 10 * time.Second

// aiWriteWords AI 改数据能用的开头（查询的也行）。
var aiWriteWords = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true, "MERGE": true,
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true, "RENAME": true, "COMMENT": true}

// aiWriteBanned 改数据时不许用的写法（在 aiFlatten 后的大写文本上匹配）。
var aiWriteBanned = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`^(CREATE|ALTER|DROP)\s+(OR\s+REPLACE\s+)?(DEFINER\s*=\s*\S+(\s*@\s*\S+)?\s+)?(TRUSTED\s+|PROCEDURAL\s+)?` +
		`(USER|ROLE|GROUP|SERVER|EXTENSION|LANGUAGE|PUBLICATION|SUBSCRIPTION|TABLESPACE|EVENT|FOREIGN|SYSTEM|DEFAULT|RESOURCE|LOGFILE|INSTANCE|UNDO)\b`),
		"不能建、改、删数据库账号、权限和服务器级的对象（账号、角色、扩展、表空间、外部服务器、定时事件、系统设置……）"},
	{regexp.MustCompile(`^(CREATE|DROP)\s+(DATABASE|SCHEMA)\b`), "不能建库、删库：请用户在 Ferry 的「数据库」里做"},
	{regexp.MustCompile(`\bCONCURRENTLY\b`), "不能用 CONCURRENTLY（不能在事务里执行）"},
	{regexp.MustCompile(`\bINTO\s+(OUTFILE|DUMPFILE)\b|\b(DATA|INDEX)\s+DIRECTORY\b`), "不能写服务器上的文件"},
	{regexp.MustCompile(`\bSONAME\b|\bLANGUAGE\s+(C|INTERNAL|PLPYTHON\w*|PLPERLU|PLTCLU|PLSH|PLV8)\b`), "不能建能执行系统命令的函数（C、plpython 等语言，MySQL 的 SONAME）"},
	{regexp.MustCompile(`\b(SLEEP|BENCHMARK|GET_LOCK|RELEASE_LOCK|RELEASE_ALL_LOCKS|IS_FREE_LOCK|LOAD_FILE|` +
		`PG_SLEEP\w*|PG_TERMINATE_BACKEND|PG_CANCEL_BACKEND|PG_READ_FILE|PG_READ_BINARY_FILE|PG_LS_\w+|PG_STAT_FILE|PG_FILE_\w+|` +
		`LO_\w+|DBLINK\w*|PG_ADVISORY\w*|PG_TRY_ADVISORY\w*|SET_CONFIG|PG_RELOAD_CONF|PG_ROTATE_LOGFILE|PG_SWITCH_WAL|` +
		`PG_CREATE_\w+|PG_DROP_\w+|PG_LOGICAL_\w+|PG_REPLICATION_\w+|PG_PROMOTE|PG_WAL_REPLAY_\w+|PG_NOTIFY|PG_IMPORT_SYSTEM_COLLATIONS|` +
		`QUERY_TO_XML\w*|CURSOR_TO_XML\w*|TABLE_TO_XML\w*|SCHEMA_TO_XML\w*|DATABASE_TO_XML\w*)\s*\(`), "用到了不允许的函数"},
	{regexp.MustCompile(`\bMYSQL\s*\.\s*\w|\bPG_(AUTHID|SHADOW|USER_MAPPINGS?)\b|\bPG_CATALOG\s*\.\s*PG_`), "不能动数据库的系统表（账号、权限……）"},
}

// CheckAIWrite 检查 AI 要执行的改动，返回拆好的语句。dbName 是在哪个库里执行。
func CheckAIWrite(kind, dbName, src string) ([]string, error) {
	if len(src) > aiMaxSQL {
		return nil, fmt.Errorf("SQL 太长（%d KB）：一次最多 %d KB，分几次执行", len(src)>>10, aiMaxSQL>>10)
	}
	stmts := Split(kind, src)
	switch {
	case len(stmts) == 0:
		return nil, errors.New("没有要执行的语句")
	case len(stmts) > AIMaxStmts:
		return nil, fmt.Errorf("一次最多 %d 条语句（这次 %d 条）：分几次执行", AIMaxStmts, len(stmts))
	}
	if err := CheckAIWriteStmts(kind, dbName, stmts); err != nil {
		return nil, err
	}
	return stmts, nil
}

// CheckAIWriteStmts 逐条检查已经拆好的语句（桌面版确认前再查一遍用）。
func CheckAIWriteStmts(kind, dbName string, stmts []string) error {
	if len(stmts) == 0 || len(stmts) > AIMaxStmts {
		return fmt.Errorf("语句的条数不对：%d", len(stmts))
	}
	if kind == MySQL && strings.EqualFold(dbName, "mysql") {
		return errors.New("不能改 mysql 系统库")
	}
	for i, s := range stmts {
		if len(Split(kind, s)) != 1 {
			return fmt.Errorf("第 %d 条不是一条语句", i+1)
		}
		if err := checkAIWriteStmt(kind, s); err != nil {
			if len(stmts) > 1 {
				return fmt.Errorf("第 %d 条（%s）%v", i+1, firstLine(s), err)
			}
			return err
		}
	}
	return nil
}

func checkAIWriteStmt(kind, s string) error {
	w := FirstWord(kind, s)
	if !aiWriteWords[w] && !aiStartWords[w] {
		return fmt.Errorf("不能执行 %s：只能插入、修改、删除数据（INSERT、UPDATE、DELETE），建表改表（CREATE、ALTER、DROP、TRUNCATE、RENAME）和查询；"+
			"事务由 Ferry 管（不要写 BEGIN、COMMIT），账号权限（GRANT）、SET、CALL、LOAD DATA、COPY 这类不行", w)
	}
	if kind == MySQL && strings.Contains(s, "/*!") {
		return errors.New("不能用 MySQL 的 /*! … */ 注释（里面的内容会被执行）")
	}
	if reUnicodeEscape.MatchString(s) {
		return errors.New("不能用 U&'…' 转义写法")
	}
	flat := strings.TrimSpace(strings.ToUpper(aiFlatten.Replace(stripComments(kind, s))))
	flat = strings.TrimLeft(flat, "( ")
	whole := strings.ToUpper(aiFlatten.Replace(s)) // 注释里的也查：MySQL 的 /*! */ 之外，宁可多拒绝
	for _, b := range aiWriteBanned {
		m := b.re.FindString(flat)
		if m == "" {
			m = b.re.FindString(whole)
		}
		if m != "" {
			return fmt.Errorf("%s：%s", b.why, strings.TrimSpace(strings.TrimSuffix(m, "(")))
		}
	}
	return nil
}

var (
	reDropAll  = regexp.MustCompile(`^(DROP\s+TABLE|TRUNCATE)\b`)
	reDropAny  = regexp.MustCompile(`^DROP\b`)
	reNoWhere  = regexp.MustCompile(`^(DELETE|UPDATE)\b`)
	reWhere    = regexp.MustCompile(`\bWHERE\b`)
	reAlterDel = regexp.MustCompile(`^ALTER\b.*\bDROP\b`)
	reDDL      = regexp.MustCompile(`^(CREATE|ALTER|DROP|TRUNCATE|RENAME)\b`)
)

// stmtFlat 去掉注释、字符串内容后的大写文本（判断危险用，不用来放行）。
func stmtFlat(kind, s string) string {
	t := stripComments(kind, s)
	var b strings.Builder
	in := byte(0)
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case in != 0:
			if c == '\\' && kind == MySQL && i+1 < len(t) {
				i++
			} else if c == in {
				in = 0
				b.WriteByte(c)
			}
		case c == '\'':
			in = c
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return strings.Join(strings.Fields(strings.ToUpper(b.String())), " ")
}

// AIWriteDanger 这些语句危险在哪（给确认框标红）；空 = 没发现特别危险的。
func AIWriteDanger(kind string, stmts []string) string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range stmts {
		f := strings.TrimLeft(stmtFlat(kind, s), "( ")
		switch {
		case reDropAll.MatchString(f):
			add("会删掉表或清空表里的全部数据")
		case reDropAny.MatchString(f):
			add("会删掉视图、索引等对象")
		case reNoWhere.MatchString(f) && !reWhere.MatchString(f):
			if strings.HasPrefix(f, "DELETE") {
				add("有 DELETE 没写 WHERE，会删掉整张表的数据")
			} else {
				add("有 UPDATE 没写 WHERE，会改整张表的数据")
			}
		case reAlterDel.MatchString(f):
			add("会删掉列、索引或约束")
		}
	}
	return strings.Join(out, "；")
}

// AIWriteNotes 执行前要让用户知道的（写进确认框）。
func AIWriteNotes(kind string, stmts []string) []string {
	if kind != MySQL {
		return []string{"全部语句在一个事务里执行：有一条失败就全部撤回（建表改表也能撤回）"}
	}
	for _, s := range stmts {
		if reDDL.MatchString(strings.TrimLeft(stmtFlat(kind, s), "( ")) {
			return []string{"全部语句在一个事务里执行，有一条失败就撤回改的数据；但 MySQL 的建表、改表、删表语句执行了就立即生效，撤不回"}
		}
	}
	return []string{"全部语句在一个事务里执行：有一条失败就全部撤回"}
}

// AIStmtResult 一条语句的结果。
type AIStmtResult struct {
	SQL      string   `json:"sql"`
	Affected *int64   `json:"affected,omitempty"`
	Columns  []Column `json:"columns,omitempty"`
	Rows     [][]any  `json:"rows,omitempty"`
	More     bool     `json:"more,omitempty"`
	Error    string   `json:"error,omitempty"`
	Ms       int64    `json:"ms"`
}

// AIExecResult 一次改动的结果。
type AIExecResult struct {
	OK bool `json:"ok"`
	// Committed 改动已经提交（OK 时为 true；失败时撤回了，MySQL 已经执行的建表改表除外）
	Committed bool           `json:"committed"`
	Results   []AIStmtResult `json:"results"`
	Note      string         `json:"note,omitempty"`
	Ms        int64          `json:"ms"`
}

// ErrAIWriteNotAllowed 连接没有允许 AI 修改。
var ErrAIWriteNotAllowed = errors.New("这个数据库连接没有允许 AI 修改")

// AIExec 执行用户同意了的改动：在一个事务里逐条执行，有一条失败就撤回。
func (cl *Client) AIExec(ctx context.Context, dbName string, stmts []string) (*AIExecResult, error) {
	kind := cl.Conn.Kind
	switch {
	case cl.Conn.ReadOnly:
		return nil, ErrReadOnly
	case !cl.Conn.AI || !cl.Conn.AIWrite:
		return nil, ErrAIWriteNotAllowed
	}
	if err := CheckAIWriteStmts(kind, dbName, stmts); err != nil {
		return nil, err
	}
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, AIWriteTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer conn.Close()
	start := time.Now()
	lock := int(aiLockWait.Seconds())
	if kind == MySQL {
		// 等锁的时间：别的程序锁着表时不一直等（改完改回默认值，改不回去就不再用这个连接）
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d, SESSION lock_wait_timeout = %d", lock, lock)); err == nil {
			defer func() {
				if _, err := conn.ExecContext(context.Background(), "SET SESSION innodb_lock_wait_timeout = DEFAULT, SESSION lock_wait_timeout = DEFAULT"); err != nil {
					conn.Raw(func(any) error { return driver.ErrBadConn })
				}
			}()
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer tx.Rollback()
	if kind == Postgres {
		tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", AIWriteTimeout.Milliseconds()))
		tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", aiLockWait.Milliseconds()))
	}
	r := &AIExecResult{Results: []AIStmtResult{}}
	ddl := false
	for _, s := range stmts {
		one := cl.aiExecOne(ctx, tx, s)
		r.Results = append(r.Results, one)
		if one.Error != "" {
			tx.Rollback()
			r.Note = "执行到这一条失败了，前面的改动已经撤回"
			if ddl {
				r.Note = "执行到这一条失败了：前面改的数据已经撤回，但 MySQL 已经执行的建表、改表、删表语句撤不回"
			}
			r.Ms = time.Since(start).Milliseconds()
			return r, nil
		}
		if kind == MySQL && reDDL.MatchString(strings.TrimLeft(stmtFlat(kind, s), "( ")) {
			ddl = true
		}
	}
	if err := tx.Commit(); err != nil {
		r.Note = "提交失败，改动已经撤回：" + cl.explain(err).Error()
		r.Ms = time.Since(start).Milliseconds()
		return r, nil
	}
	r.OK, r.Committed = true, true
	r.Ms = time.Since(start).Milliseconds()
	return r, nil
}

func (cl *Client) aiExecOne(ctx context.Context, tx *sql.Tx, s string) AIStmtResult {
	start := time.Now()
	r := AIStmtResult{SQL: s}
	fail := func(err error) AIStmtResult {
		if ctx.Err() != nil {
			err = fmt.Errorf("超过 %d 分钟没执行完，已停止", int(AIWriteTimeout.Minutes()))
		} else if ErrorCode(err) == "1205" || ErrorCode(err) == "55P03" {
			err = fmt.Errorf("等了 %d 秒还是锁不到（别的程序正在用这张表）：%w", int(aiLockWait.Seconds()), err)
		}
		r.Error = cl.explain(err).Error()
		r.Ms = time.Since(start).Milliseconds()
		return r
	}
	if returnsRows(cl.Conn.Kind, s) {
		rows, err := tx.QueryContext(ctx, s)
		if err != nil {
			return fail(err)
		}
		cols, data, more, err := readRows(rows, aiWriteRows)
		rows.Close()
		if err != nil {
			return fail(err)
		}
		if len(cols) > 0 {
			for _, row := range data {
				for i, v := range row {
					row[i] = aiCell(v)
				}
			}
			r.Columns, r.Rows, r.More = cols, data, more
		}
	} else {
		res, err := tx.ExecContext(ctx, s)
		if err != nil {
			return fail(err)
		}
		if n, err := res.RowsAffected(); err == nil {
			r.Affected = &n
		}
	}
	r.Ms = time.Since(start).Milliseconds()
	return r
}
