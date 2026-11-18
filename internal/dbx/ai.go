package dbx

// 给 AI 用的只读查询（编程软件经 ferry mcp、Ferry 自己的 AI 助手）：比只读连接更严——
// 只能是一条查询；不许写文件、读服务器上的文件、踢别人的连接、加锁、睡眠、读账号密码表；
// 执行时按引擎使用只读事务、只读连接或查询校验，返回的行数、单元格长度、总长度都有上限。
//
// 能不能给 AI 用由连接的「允许 AI 查询」（Conn.AI）决定，默认关着：查出来的数据会发给 AI 的模型。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	AIDefaultRows = 100
	AIMaxRows     = 500
	// aiMaxCell 一个单元格最多给多少个字符
	aiMaxCell = 1000
	// aiMaxBytes 结果最多多少字节（再多就少给几行）
	aiMaxBytes = 48 << 10
)

// AITimeout 一条查询在数据库里最多执行多久（测试时改短）。
var AITimeout = 15 * time.Second

// aiStartWords AI 的查询能用的开头。
var aiStartWords = map[string]bool{"SELECT": true, "WITH": true, "SHOW": true, "EXPLAIN": true, "DESC": true,
	"DESCRIBE": true, "VALUES": true, "TABLE": true}

// aiBanned 不许用的写法。在去掉引号、注释符号的大写文本上匹配（字符串、注释里的也算：宁可多拒绝，不能漏）。
var aiBanned = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`\bINTO\b`), "不能用 SELECT … INTO（会写文件、变量或建表）"},
	{regexp.MustCompile(`\bFOR\s+(UPDATE|SHARE|NO\s+KEY\s+UPDATE|KEY\s+SHARE)\b|\bLOCK\s+IN\s+SHARE\s+MODE\b`), "不能加锁（FOR UPDATE、FOR SHARE）"},
	{regexp.MustCompile(`\b(SLEEP|BENCHMARK|GET_LOCK|RELEASE_LOCK|RELEASE_ALL_LOCKS|IS_FREE_LOCK|LOAD_FILE|` +
		`PG_SLEEP\w*|PG_TERMINATE_BACKEND|PG_CANCEL_BACKEND|PG_READ_FILE|PG_READ_BINARY_FILE|PG_LS_\w+|PG_STAT_FILE|PG_FILE_\w+|` +
		`LO_\w+|DBLINK\w*|PG_ADVISORY\w*|PG_TRY_ADVISORY\w*|SET_CONFIG|PG_RELOAD_CONF|PG_ROTATE_LOGFILE|PG_SWITCH_WAL|` +
		`PG_CREATE_\w+|PG_DROP_\w+|PG_LOGICAL_\w+|PG_REPLICATION_\w+|PG_PROMOTE|PG_WAL_REPLAY_\w+|PG_NOTIFY|PG_IMPORT_SYSTEM_COLLATIONS|` +
		`QUERY_TO_XML\w*|CURSOR_TO_XML\w*|TABLE_TO_XML\w*|SCHEMA_TO_XML\w*|DATABASE_TO_XML\w*|NEXTVAL|SETVAL)\s*\(`), "用到了不允许的函数"},
	{regexp.MustCompile(`\bMYSQL\s*\.\s*(USER|GLOBAL_PRIV)\b|\bPG_(AUTHID|SHADOW|USER_MAPPINGS?)\b|\bSHOW\s+CREATE\s+USER\b`), "不能查账号密码表"},
}

// reUnicodeEscape PostgreSQL 的 U&"…" 能把函数名写成转义（U&"pg\005fsleep"），绕过上面的匹配：在原文上查。
var reUnicodeEscape = regexp.MustCompile(`(?i)\bU&\s*['"]`)

// aiFlatten 去掉引号、反引号和注释符号，换成空格，再转大写：`pg_sleep`(1)、SLEEP/**/(1) 这类写法也能认出来。
var aiFlatten = strings.NewReplacer("'", " ", `"`, " ", "`", " ", "/*", " ", "*/", " ", "--", " ", "#", " ", "\n", " ", "\r", " ", "\t", " ")

var aiKingbasePrefix = regexp.MustCompile(`\bSYS_`)

func aiSQLText(kind, stmt string) string {
	text := strings.ToUpper(aiFlatten.Replace(stmt))
	if kind == Kingbase {
		// 金仓同时提供 sys_ 名称和 pg_ 兼容名称，两者使用同样的拦截规则。
		return aiKingbasePrefix.ReplaceAllString(text, "PG_")
	}
	return text
}

var aiExternalSQL = regexp.MustCompile(`\b(OPENQUERY|OPENROWSET|OPENDATASOURCE|BULK|XP_\w+|SP_OA\w+|UTL_\w+|DBMS_\w+|NEXTVAL|ATTACH|LOAD_EXTENSION)\b`)

var aiClickHouseExternal = regexp.MustCompile(`\b(FILE|URL|S3\w*|AZUREBLOBSTORAGE\w*|HDFS\w*|REMOTE\w*|CLUSTER\w*|MYSQL|POSTGRESQL|ODBC|JDBC|MONGODB|REDIS|EXECUTABLE\w*|INPUT|DICTIONARY\w*|ICEBERG\w*|DELTALAKE\w*)\s*\(|\b(SYSTEM|INFORMATION_SCHEMA)\s*\.|` +
	`\b(ON\s+CLUSTER|SETTINGS|OUTFILE|NAMED\s+COLLECTION|DICTGET\w*\s*\()`)

var aiPrivateCatalog = regexp.MustCompile(`\b(SQL_LOGINS|SERVER_PRINCIPALS|SYSLOGINS|SYSXLOGINS|SYSAUTH|SYSUSER\w*|DBA_USERS)\b`)

// TiDB 的 READ ONLY 事务语法不限制写入，复用 AI 查询规则并额外禁止写入关键字、会话变量。
var tiDBReadWords = regexp.MustCompile(`\b(INSERT|UPDATE|DELETE|REPLACE|MERGE|CREATE|ALTER|DROP|TRUNCATE|RENAME|IMPORT|LOAD|ADMIN|DO|CALL|SET|GRANT|REVOKE|LOCK|UNLOCK)\b|@`)

func checkTiDBRead(dbName, stmt string) error {
	if err := CheckAIQuery(MySQL, dbName, stmt); err != nil {
		return err
	}
	if tiDBReadWords.MatchString(strings.ToUpper(aiFlatten.Replace(stmt))) {
		return errors.New("TiDB 的只读查询不允许写语句、锁表或会话变量")
	}
	return nil
}

// CheckAIQuery 检查 AI 发来的查询，不行时说明原因。dbName 是要在哪个库里查。
func CheckAIQuery(kind, dbName, src string) error {
	if kind == MongoDB {
		_, err := parseAIMongoFind(dbName, src)
		return err
	}
	_, err := aiQueryStatement(kind, dbName, src)
	return err
}

func aiQueryStatement(kind, dbName, src string) (string, error) {
	stmts := Split(kind, src)
	if kind == SQLServer {
		var err error
		stmts, err = sqlServerStatements(src)
		if err != nil {
			return "", err
		}
	}
	if len(stmts) == 0 {
		return "", errors.New("没有要执行的查询")
	}
	if len(stmts) > 1 {
		return "", errors.New("一次只能执行一条查询")
	}
	s := stmts[0]
	switch w := FirstWord(kind, s); {
	case w == "":
		return "", errors.New("没有要执行的查询")
	case !aiStartWords[w]:
		return "", fmt.Errorf("只能执行查询（SELECT、WITH、SHOW、EXPLAIN 等），不能执行 %s", w)
	}
	if kind == MySQL && mySQLDirective(s) {
		return "", errors.New("不能用 MySQL 的 /*! … */ 或 TiDB 的 /*T! … */ 执行指令注释")
	}
	if reUnicodeEscape.MatchString(s) {
		return "", errors.New("不能用 U&'…' 转义写法")
	}
	if kind == MySQL && strings.EqualFold(dbName, "mysql") {
		return "", errors.New("不能在 mysql 系统库里查（里面有账号的密码）")
	}
	flat := aiSQLText(kind, s)
	if err := checkAdapterAISQL(kind, dbName, flat); err != nil {
		return "", err
	}
	if !postgresProtocol(kind) && FirstWord(kind, s) == "EXPLAIN" && strings.Contains(flat, "ANALYZE") {
		return "", errors.New("不能用 EXPLAIN ANALYZE（会执行语句）")
	}
	for _, b := range aiBanned {
		if m := b.re.FindString(flat); m != "" {
			return "", fmt.Errorf("%s：%s", b.why, strings.TrimSpace(strings.TrimSuffix(m, "(")))
		}
	}
	return s, nil
}

func mySQLDirective(s string) bool {
	return strings.Contains(s, "/*!") || strings.Contains(strings.ToUpper(s), "/*T!")
}

func checkAdapterAISQL(kind, dbName, text string) error {
	if adapterSQL(kind) && (aiExternalSQL.MatchString(text) || aiPrivateCatalog.MatchString(text)) {
		return errors.New("AI 不能执行外部访问函数或访问账号密码目录")
	}
	if kind == ClickHouse && (aiClickHouseExternal.MatchString(text) || strings.EqualFold(dbName, "system") || strings.EqualFold(dbName, "information_schema")) {
		return errors.New("AI 不能访问 ClickHouse 外部数据源、系统库或修改查询设置")
	}
	return nil
}

// AIResult AI 查询的结果。
type AIResult struct {
	Columns []Column `json:"columns"`
	// Rows 单元格是 null 或文本；二进制写成「（二进制 N 字节）」，太长的截断并注明长度
	Rows [][]any `json:"rows"`
	// More 还有更多行没给（超过行数上限或结果太长）
	More bool   `json:"more,omitempty"`
	Note string `json:"note,omitempty"`
	Ms   int64  `json:"ms"`
}

// AIQuery 执行 AI 的查询（先用 CheckAIQuery 检查）。max 是最多返回多少行。
func (cl *Client) AIQuery(ctx context.Context, dbName, src string, max int) (*AIResult, error) {
	kind := cl.Conn.Kind
	if kind == MongoDB {
		return cl.aiMongoQuery(ctx, dbName, src, max)
	}
	stmt, err := aiQueryStatement(kind, cl.databaseName(dbName), src)
	if err != nil {
		return nil, err
	}
	if cl.isTiDB() {
		if err := checkTiDBRead(cl.databaseName(dbName), stmt); err != nil {
			return nil, err
		}
	}
	if max <= 0 {
		max = AIDefaultRows
	}
	max = min(max, AIMaxRows)
	db, err := cl.pool(dbName, roleRead)
	if err != nil {
		return nil, err
	}
	// 取连接、执行和读取结果共用同一个超时。
	ctx, cancel := context.WithTimeout(ctx, AITimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer conn.Close()
	start := time.Now()
	r, err := cl.aiRun(ctx, conn, dbName, stmt, max)
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return nil, fmt.Errorf("查询超过 %d 秒没执行完，已停止：缩小范围（加条件、LIMIT）再查", int(AITimeout.Seconds()))
		}
		return nil, cl.explain(err)
	}
	r.Ms = time.Since(start).Milliseconds()
	return r, nil
}

func (cl *Client) aiRun(ctx context.Context, conn *sql.Conn, dbName, stmt string, max int) (*AIResult, error) {
	ms := AITimeout.Milliseconds()
	id := cl.connID(ctx, conn)
	if cl.Conn.Kind == MySQL {
		// 会话级的超时：用完改回默认值，改不回去就不再用这个连接
		set, reset := fmt.Sprintf("SET SESSION max_execution_time = %d", ms), "SET SESSION max_execution_time = DEFAULT"
		if strings.Contains(strings.ToLower(cl.Version), "mariadb") {
			set, reset = fmt.Sprintf("SET SESSION max_statement_time = %d", ms/1000), "SET SESSION max_statement_time = DEFAULT"
		}
		if _, err := conn.ExecContext(ctx, set); err == nil {
			defer func() {
				if _, err := conn.ExecContext(context.Background(), reset); err != nil {
					conn.Raw(func(any) error { return driver.ErrBadConn })
				}
			}()
		}
	}
	q, release, err := cl.beginReadQuery(ctx, conn)
	if err != nil {
		return nil, err
	}
	defer release()
	if tx, ok := q.(*sql.Tx); ok && postgresProtocol(cl.Conn.Kind) {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
			return nil, err
		}
		tx.ExecContext(ctx, "SET LOCAL lock_timeout = 5000")
	}
	rows, err := q.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	cols, out, more, err := readRows(rows, max)
	if more && id != "" {
		cl.stopStmt(dbName, id) // 够数了：让数据库停下，不把剩下的读完
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	r := &AIResult{Columns: cols, Rows: make([][]any, 0, len(out)), More: more}
	size := 0
	for _, row := range out {
		for i, v := range row {
			row[i] = aiCell(v)
			if s, ok := row[i].(string); ok {
				size += len(s) + 4
			}
		}
		if size > aiMaxBytes && len(r.Rows) > 0 {
			r.More = true
			r.Note = fmt.Sprintf("结果太长，只给了前 %d 行：少选几列或加条件再查", len(r.Rows))
			break
		}
		r.Rows = append(r.Rows, row)
	}
	if r.More && r.Note == "" {
		r.Note = fmt.Sprintf("只给了前 %d 行，还有更多：要看更多就加条件或用 LIMIT / OFFSET 分页", len(r.Rows))
	}
	return r, nil
}

// aiCell 单元格 → AI 看的文本。
func aiCell(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case Binary:
		return fmt.Sprintf("（二进制 %d 字节）", x.Size)
	case Cut:
		return clipRunes(x.Text, aiMaxCell) + fmt.Sprintf("…（共 %d 字节，已截断）", x.Size)
	case string:
		if utf8.RuneCountInString(x) > aiMaxCell {
			return clipRunes(x, aiMaxCell) + fmt.Sprintf("…（共 %d 字节，已截断）", len(x))
		}
		return x
	default:
		return fmt.Sprint(x)
	}
}

func clipRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// isTimeout 数据库报的执行超时：MySQL 3024（max_execution_time）、MariaDB 1969（max_statement_time）、PostgreSQL 57014（statement_timeout）。
func isTimeout(err error) bool {
	switch ErrorCode(err) {
	case "3024", "1969", "57014":
		return true
	}
	return false
}
