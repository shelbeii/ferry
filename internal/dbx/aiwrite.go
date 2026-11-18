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

	"github.com/ClickHouse/clickhouse-go/v2"
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
	{regexp.MustCompile(`^(CREATE|ALTER|DROP|RENAME)\s+(OR\s+REPLACE\s+)?(DEFINER\s*=\s*\S+(\s*@\s*\S+)?\s+)?(TRUSTED\s+|PROCEDURAL\s+)?` +
		`(USER|ROLE|GROUP|SERVER|EXTENSION|LANGUAGE|PUBLICATION|SUBSCRIPTION|TABLESPACE|EVENT|FOREIGN|SYSTEM|DEFAULT|RESOURCE|LOGFILE|INSTANCE|UNDO)\b`),
		"不能建、改、删数据库账号、权限和服务器级的对象（账号、角色、扩展、表空间、外部服务器、定时事件、系统设置……）"},
	{regexp.MustCompile(`^(CREATE|DROP|ALTER)\s+(DATABASE|SCHEMA)\b`), "不能建库、删库、改库：请用户在 Ferry 的「数据库」里做"},
	{regexp.MustCompile(`\b(TO|FROM)\s+PROGRAM\b`), "不能执行系统命令（COPY … PROGRAM）"},
	{regexp.MustCompile(`\bCOPY\b.*\b(TO|FROM)\s+(/|[A-Z]:)`), "不能读写服务器上的文件（COPY … TO / FROM 文件）"},
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
	if kind == Redis {
		if _, err := ParseRedisDB(dbName); err != nil {
			return nil, err
		}
		return CheckAIRedisWrite(src)
	}
	if len(src) > aiMaxSQL {
		return nil, fmt.Errorf("SQL 太长（%d KB）：一次最多 %d KB，分几次执行", len(src)>>10, aiMaxSQL>>10)
	}
	var stmts []string
	switch kind {
	case MongoDB:
		stmts = []string{strings.TrimSpace(src)}
	case SQLServer:
		var err error
		stmts, err = scanSQLServer(src, true)
		if err != nil {
			return nil, err
		}
	default:
		stmts = Split(kind, src)
	}
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
	if kind == Redis {
		if _, err := ParseRedisDB(dbName); err != nil {
			return err
		}
		return CheckAIRedisWriteLines(stmts)
	}
	if len(stmts) == 0 || len(stmts) > AIMaxStmts {
		return fmt.Errorf("语句的条数不对：%d", len(stmts))
	}
	size := 0
	for _, s := range stmts {
		size += len(s)
		if size > aiMaxSQL {
			return errors.New("AI 改动的内容太长")
		}
	}
	if kind == MongoDB {
		if len(stmts) != 1 {
			return errors.New("MongoDB 每次确认一条改动")
		}
		_, err := parseAIMongoWrite(dbName, stmts[0])
		return err
	}
	if kind == ClickHouse && len(stmts) != 1 {
		return errors.New("ClickHouse 每次确认一条改动")
	}
	if kind == MySQL && strings.EqualFold(dbName, "mysql") {
		return errors.New("不能改 mysql 系统库")
	}
	for i, s := range stmts {
		parts := Split(kind, s)
		if kind == SQLServer {
			var err error
			parts, err = scanSQLServer(s, true)
			if err != nil {
				return err
			}
		}
		if len(parts) != 1 {
			return fmt.Errorf("第 %d 条不是一条语句", i+1)
		}
		if kind == Dameng && len(stmts) > 1 && reDDL.MatchString(stmtFlat(kind, s)) {
			return errors.New("达梦的建表改表需要单独确认")
		}
		if err := checkAdapterAISQL(kind, dbName, aiSQLText(kind, s)); err != nil {
			return err
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
	if kind == MySQL && mySQLDirective(s) {
		return errors.New("不能用 MySQL / TiDB 的 /*! 或 /*T! 指令注释")
	}
	if reUnicodeEscape.MatchString(s) {
		return errors.New("不能用 U&'…' 转义写法")
	}
	flat := strings.TrimSpace(aiSQLText(kind, stripComments(kind, s)))
	flat = strings.TrimLeft(flat, "( ")
	if adapterSQL(kind) && reDDL.MatchString(flat) && !aiAdapterDDL.MatchString(flat) {
		return errors.New("AI 只能创建、修改或删除表、视图和索引")
	}
	if kind == ClickHouse && (aiClickHouseEngine.MatchString(flat) || aiClickHouseClause.MatchString(flat)) {
		return errors.New("AI 只能使用本地 ClickHouse 表引擎和本地表改动")
	}
	whole := aiSQLText(kind, s) // 注释里的也查：MySQL 的 /*! */ 之外，宁可多拒绝
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
	aiAdapterDDL       = regexp.MustCompile(`^(?:(?:CREATE(?:\s+OR\s+REPLACE)?(?:\s+(?:UNIQUE|CLUSTERED|NONCLUSTERED))*|ALTER|DROP)\s+(?:TABLE|VIEW|INDEX)|TRUNCATE\s+TABLE)\b`)
	aiClickHouseEngine = regexp.MustCompile(`\bENGINE\s*=\s*(?:[^A-Z\s]|FILE\w*|URL\w*|S3\w*|AZURE\w*|HDFS\w*|REMOTE\w*|DISTRIBUTED\w*|MYSQL\w*|POSTGRESQL\w*|ODBC\w*|JDBC\w*|MONGODB\w*|REDIS\w*|EXECUTABLE\w*|ICEBERG\w*|DELTALAKE\w*)\b`)
	aiClickHouseClause = regexp.MustCompile(`\b(ATTACH|DETACH|FREEZE|UNFREEZE|FETCH\s+PARTITION|MOVE\s+PARTITION)\b`)
	reDropAll          = regexp.MustCompile(`^(DROP\s+TABLE|TRUNCATE)\b`)
	reDropAny          = regexp.MustCompile(`^DROP\b`)
	reNoWhere          = regexp.MustCompile(`^(DELETE|UPDATE)\b`)
	reAlterDel         = regexp.MustCompile(`^ALTER\b.*\bDROP\b`)
	reDDL              = regexp.MustCompile(`^(CREATE|ALTER|DROP|TRUNCATE|RENAME)\b`)
	reRoutine          = regexp.MustCompile(`^CREATE\s+(OR\s+REPLACE\s+)?(DEFINER\s*=\s*\S+\s+)?(CONSTRAINT\s+)?(TRIGGER|FUNCTION|PROCEDURE|RULE|AGGREGATE)\b`)
	reDefiner          = regexp.MustCompile(`\bSECURITY\s+DEFINER\b`)
	reOwner            = regexp.MustCompile(`\bOWNER\s+TO\b`)
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

// AIWriteDanger 这些语句危险在哪（给 AI 改动的确认框标红）；空 = 没发现特别危险的。
func AIWriteDanger(kind string, stmts []string) string {
	if kind == Redis {
		return AIRedisDanger(stmts)
	}
	if kind == MongoDB {
		return aiMongoDanger(stmts)
	}
	return strings.Join(Dangers(kind, stmts, true), "；")
}

// Dangers 这些语句危险在哪（去掉重复的）。ai 为 true 时（AI 写的）也提醒建触发器、函数，改所有者这类不容易看出来的；
// 用户自己在查询窗口写的只提醒删表、清空、不带 WHERE 的删改、删列。
func Dangers(kind string, stmts []string, ai bool) []string {
	out := []string{}
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
		case reNoWhere.MatchString(f) && !topWhere(f):
			if strings.HasPrefix(f, "DELETE") {
				add("有 DELETE 没写 WHERE，会删掉整张表的数据")
			} else {
				add("有 UPDATE 没写 WHERE，会改整张表的数据")
			}
		case reAlterDel.MatchString(f):
			add("会删掉列、索引或约束")
		}
		if !ai {
			continue
		}
		if reRoutine.MatchString(f) {
			add("会建触发器、函数或存储过程：以后会自动执行或被调用，看清楚里面做了什么")
		}
		if reDefiner.MatchString(f) {
			add("函数用建它的账号的权限执行（SECURITY DEFINER）")
		}
		if reOwner.MatchString(f) {
			add("会改对象的所有者（OWNER TO）")
		}
	}
	return out
}

// topWhere 语句本身（不算括号里的子查询）有没有 WHERE。f 是 stmtFlat 后的文本（字符串的内容已经去掉）。
func topWhere(f string) bool {
	depth := 0
	for i := 0; i < len(f); i++ {
		switch c := f[i]; {
		case c == '(':
			depth++
		case c == ')':
			depth = max(depth-1, 0)
		case depth == 0 && strings.HasPrefix(f[i:], "WHERE") && (i == 0 || !isWordByte(f[i-1])) && (i+5 == len(f) || !isWordByte(f[i+5])):
			return true
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c >= 0x80
}

// AIWriteNotes 执行前要让用户知道的（写进确认框）。
func AIWriteNotes(kind string, stmts []string) []string {
	if kind == Redis {
		return AIRedisNotes()
	}
	if kind == MongoDB {
		return []string{"每次执行一条 MongoDB 操作。副本集和分片集群用事务执行批量更新、删除；单机实例修改多个文档时，失败可能部分生效。"}
	}
	if kind == ClickHouse {
		return []string{"每次执行一条 ClickHouse 语句。ClickHouse 的写入和建表改表不提供整批回滚，取消等待后后台改动也可能继续。"}
	}
	if kind == Dameng && len(stmts) == 1 && reDDL.MatchString(stmtFlat(kind, stmts[0])) {
		return []string{"达梦的建表、改表和删表单独执行，生效后可能无法回滚。"}
	}
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

func AIWriteText(kind string, stmts []string) string {
	switch kind {
	case Redis, MongoDB:
		return strings.Join(stmts, "\n")
	default:
		return strings.Join(stmts, ";\n\n") + ";"
	}
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
	// Redis 命令的回复（SQL 是那条命令）；给 AI 的是 redis-cli 那样的文字
	Reply     *RedisReply `json:"-"`
	ReplyText string      `json:"reply,omitempty"`
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
	if cl.Conn.Kind == Redis {
		return cl.AIRedisExec(ctx, dbName, stmts)
	}
	kind := cl.Conn.Kind
	switch {
	case cl.Conn.ReadOnly:
		return nil, ErrReadOnly
	case !cl.Conn.AI || !cl.Conn.AIWrite:
		return nil, ErrAIWriteNotAllowed
	}
	dbName = cl.databaseName(dbName)
	if err := CheckAIWriteStmts(kind, dbName, stmts); err != nil {
		return nil, err
	}
	if kind == MongoDB {
		return cl.aiMongoExec(ctx, dbName, stmts[0])
	}
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, AIWriteTimeout)
	defer cancel()
	if kind == ClickHouse || kind == Dameng && reDDL.MatchString(stmtFlat(kind, stmts[0])) {
		if kind == ClickHouse {
			ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": 1}))
		}
		start := time.Now()
		one := cl.aiExecOne(ctx, db, stmts[0])
		ok := one.Error == ""
		r := &AIExecResult{OK: ok, Committed: ok, Results: []AIStmtResult{one}, Ms: time.Since(start).Milliseconds()}
		if !ok {
			r.Note = "执行失败，已生效的改动可能无法撤回"
		}
		return r, nil
	}
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
	if postgresProtocol(kind) {
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

type sqlExecutor interface {
	querier
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (cl *Client) aiExecOne(ctx context.Context, tx sqlExecutor, s string) AIStmtResult {
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
