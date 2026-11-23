package dbx

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

func migrateMySQL(ctx context.Context, sc, dc *sql.Conn, src *Client, srcDB string, dst *Client, o MigrationOptions, p *MigrationPlan, progress migrationProgress) (*MigrationResult, error) {
	result := &MigrationResult{Phase: "准备"}
	fail := func(err error) (*MigrationResult, error) { return result, &migrationError{*result, err} }
	for _, c := range []*sql.Conn{sc, dc} {
		if _, err := c.ExecContext(ctx, "SET SESSION time_zone = '+00:00'"); err != nil {
			return fail(err)
		}
	}
	_, err := dc.ExecContext(ctx, `SET SESSION sql_mode = CONCAT_WS(',', NULLIF(@@session.sql_mode, ''), 'NO_AUTO_VALUE_ON_ZERO')`)
	if err != nil {
		return fail(err)
	}
	sourceID, targetID := src.connID(ctx, sc), dst.connID(ctx, dc)
	stop := context.AfterFunc(ctx, func() {
		dst.stopStmt(o.TargetDB, targetID)
		src.stopStmt(srcDB, sourceID)
	})
	defer stop()
	source, err := sc.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fail(err)
	}
	defer source.Rollback()
	if o.Whole {
		if err := checkMigrationEmpty(ctx, dc, MySQL, o.TargetDB); err != nil {
			return fail(err)
		}
	}
	objects, err := mySQLMigrationObjects(ctx, source, srcDB, o, p)
	if err != nil {
		return fail(err)
	}
	var foreign []string
	result.Phase = "建表"
	progress.step("准备目标表")
	for _, t := range p.checks {
		if !t.Create {
			continue
		}
		result.Table = &t.MigrationTable
		ddl, constraints := mySQLMigrationTableDDL(t.source)
		if _, err := dc.ExecContext(ctx, mysqlMigrationSchemaName(ddl, srcDB, o.TargetDB)); err != nil {
			return fail(fmt.Errorf("%s：%w", t.String(), err))
		}
		result.Created = append(result.Created, t.MigrationTable)
		foreign = append(foreign, constraints...)
	}
	count := progress.counter()
	for _, t := range p.checks {
		result.Phase, result.Table = "复制数据", &t.MigrationTable
		progress.step("复制 " + t.String())
		n, err := mySQLMigrationTable(ctx, source, dc, t.source, o.TargetDB, p.Method, count)
		if err != nil {
			return fail(fmt.Errorf("%s：%w", t.String(), err))
		}
		result.Completed = append(result.Completed, t.MigrationTable)
		result.Rows += n
	}
	count.flush()
	result.Phase, result.Table = "恢复外键和其他对象", nil
	progress.step("恢复外键和其他对象")
	for _, stmt := range append(foreign, objects...) {
		if _, err := dc.ExecContext(ctx, mysqlMigrationSchemaName(stmt, srcDB, o.TargetDB)); err != nil {
			return fail(err)
		}
	}
	result.Phase = "完成"
	return result, nil
}

func mySQLMigrationTableDDL(t *TableInfo) (string, []string) {
	lines := strings.Split(t.DDL, "\n")
	kept := make([]string, 0, len(lines))
	var foreign []string
	for _, line := range lines {
		d := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if strings.HasPrefix(d, "CONSTRAINT ") && strings.Contains(d, " FOREIGN KEY (") {
			foreign = append(foreign, "ALTER TABLE "+Quote(MySQL, t.Name)+" ADD "+d)
			continue
		}
		if strings.HasPrefix(line, ")") && len(kept) > 0 {
			kept[len(kept)-1] = strings.TrimSuffix(kept[len(kept)-1], ",")
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), foreign
}

func mySQLMigrationObjects(ctx context.Context, source *sql.Tx, db string, o MigrationOptions, p *MigrationPlan) ([]string, error) {
	if !o.Whole {
		return nil, nil
	}
	d, script := migrationDump(ctx, source, db, MySQL, true)
	d.createOnly = true
	if err := d.mysqlRoutines(); err != nil {
		return nil, err
	}
	for _, view := range viewOrder(MySQL, p.views) {
		d.printf("%s;\n", d.mysqlDefinition(strings.TrimRight(strings.TrimSpace(view.DDL), ";")))
	}
	if err := d.mysqlTriggers(migrationSourceTables(p)); err != nil {
		return nil, err
	}
	return Split(MySQL, script.String()), migrationNotes(d)
}

// SHOW CREATE may qualify references with the old database. Rewrite identifier
// tokens only, leaving quoted values and comments in program bodies alone.
func mysqlMigrationSchemaName(stmt, from, to string) string {
	if from == to {
		return stmt
	}
	var b strings.Builder
	var previous, clause string
	trigger := false
	for i := 0; i < len(stmt); {
		start := i
		var identifier, token string
		switch {
		case stmt[i] == '`':
			i = skipQuoted(MySQL, stmt, i)
			if i > start+1 {
				identifier = strings.ReplaceAll(stmt[start+1:i-1], "``", "`")
			}
			token = "identifier"
		case stmt[i] == '\'' || stmt[i] == '"':
			i = skipQuoted(MySQL, stmt, i)
			token = "literal"
		case stmt[i] == '#' || strings.HasPrefix(stmt[i:], "-- "):
			i += strings.IndexByte(stmt[i:]+"\n", '\n')
		case strings.HasPrefix(stmt[i:], "/*"):
			end := strings.Index(stmt[i+2:]+"*/", "*/")
			i = min(len(stmt), i+end+4)
		case mysqlMigrationIdent(stmt[i]):
			for i < len(stmt) && mysqlMigrationIdent(stmt[i]) {
				i++
			}
			identifier = stmt[start:i]
			token = strings.ToUpper(identifier)
		default:
			i++
			if !strings.ContainsRune(" \t\r\n", rune(stmt[start])) {
				token = stmt[start:i]
			}
		}
		if identifier == from && mysqlMigrationQualifier(stmt[i:], previous, clause, trigger) {
			b.WriteString(Quote(MySQL, to))
		} else {
			b.WriteString(stmt[start:i])
		}
		switch token {
		case "FROM", "JOIN", "UPDATE", "WHERE", "ON", "GROUP", "ORDER", "SET", "SELECT", ";":
			clause = token
		case "TRIGGER":
			trigger = previous == "CREATE"
		}
		if token != "" {
			previous = token
		}
	}
	return b.String()
}

func mysqlMigrationQualifier(rest, previous, clause string, trigger bool) bool {
	rest = strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(rest, ".") {
		return false
	}
	switch previous {
	case "FROM", "JOIN", "INTO", "UPDATE", "TABLE", "REFERENCES", "CALL", "VIEW", "PROCEDURE", "FUNCTION", "TRIGGER":
		return true
	case "ON":
		return trigger
	case ",", "(":
		if clause == "FROM" || clause == "JOIN" || clause == "UPDATE" {
			return true
		}
	}
	rest = strings.TrimLeft(rest[1:], " \t\r\n")
	i := 0
	if strings.HasPrefix(rest, "`") {
		i = skipQuoted(MySQL, rest, 0)
	} else {
		for i < len(rest) && mysqlMigrationIdent(rest[i]) {
			i++
		}
	}
	rest = strings.TrimLeft(rest[i:], " \t\r\n")
	return strings.HasPrefix(rest, ".") || strings.HasPrefix(rest, "(")
}

func mysqlMigrationIdent(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$' || c >= 128
}

func mysqlMigrationType(columnType string) string {
	before, _, _ := strings.Cut(columnType, "(")
	before, _, _ = strings.Cut(before, " ")
	return strings.ToLower(before)
}

type mysqlMigrationColumn struct {
	name, read, insert string
	load, assign       string
}

func mySQLMigrationColumns(t *TableInfo) []mysqlMigrationColumn {
	cols := make([]mysqlMigrationColumn, 0, len(t.Columns))
	for _, c := range t.Columns {
		if c.Generated {
			continue
		}
		name := Quote(MySQL, c.Name)
		col := mysqlMigrationColumn{name: name, read: name, insert: "?", load: name}
		variable := fmt.Sprintf("@ferry_col_%d", len(cols))
		switch mysqlMigrationType(c.Type) {
		case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
			col.read, col.insert = "HEX("+name+")", "UNHEX(?)"
			col.load, col.assign = variable, name+"=UNHEX("+variable+")"
		case "bit", "enum", "set":
			// Use numeric representations to preserve BIT and even the empty invalid ENUM value.
			col.read, col.insert = "CAST("+name+" AS UNSIGNED)", "CAST(? AS UNSIGNED)"
			col.load, col.assign = variable, name+"=CAST("+variable+" AS UNSIGNED)"
		}
		cols = append(cols, col)
	}
	return cols
}

func mySQLMigrationTable(ctx context.Context, src *sql.Tx, dc *sql.Conn, table *TableInfo, targetDB, method string, count *migrationCounter) (int64, error) {
	key := migrationTableID(table)
	cols := mySQLMigrationColumns(table)
	if len(cols) == 0 {
		return 0, fmt.Errorf("表 %s 没有可写入的列", key.String())
	}
	read := make([]string, len(cols))
	for i, c := range cols {
		read[i] = c.read
	}
	query := "SELECT " + strings.Join(read, ", ") + " FROM " + key.sqlName(MySQL)
	if len(table.PrimaryKey) > 0 {
		query += " ORDER BY " + quoteList(MySQL, table.PrimaryKey)
	}
	target, err := dc.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return 0, err
	}
	defer target.Rollback()
	if err := migrationTableEmpty(ctx, target, key, MySQL, true); err != nil {
		return 0, err
	}
	if err := migrationExistingTriggers(ctx, target, MySQL, targetDB, key); err != nil {
		return 0, err
	}
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	rows, err := src.QueryContext(readCtx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	if method == "INSERT" {
		n, err = mySQLMigrationInsert(ctx, target, key, cols, rows, count)
	} else {
		n, err = mySQLMigrationLoad(ctx, target, key, cols, rows, stopRead, count)
	}
	if err != nil {
		return 0, err
	}
	if err := target.Commit(); err != nil {
		return 0, fmt.Errorf("提交失败，请确认目标表状态后再重试：%w", err)
	}
	return n, nil
}

func mySQLMigrationLoad(ctx context.Context, target *sql.Tx, table MigrationTable, cols []mysqlMigrationColumn, rows *sql.Rows, stopRead context.CancelFunc, count *migrationCounter) (int64, error) {
	var load, assign []string
	for _, c := range cols {
		load = append(load, c.load)
		if c.assign != "" {
			assign = append(assign, c.assign)
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, err
	}
	reader, writer := io.Pipe()
	stopPipe := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()) })
	defer stopPipe()
	name := fmt.Sprintf("ferry_migrate_%x", nonce)
	mysql.RegisterReaderHandler(name, func() io.Reader { return reader })
	defer mysql.DeregisterReaderHandler(name)
	source := make(chan mysqlMigrationRead, 1)
	go func() {
		n, err := writeMySQLMigrationRows(rows, writer, len(cols), count)
		_ = writer.CloseWithError(err)
		source <- mysqlMigrationRead{n, err}
	}()
	query := "LOAD DATA LOCAL INFILE 'Reader::" + name + "' INTO TABLE " + table.sqlName(MySQL) +
		" CHARACTER SET utf8mb4 FIELDS TERMINATED BY 0x09 ESCAPED BY 0x5c LINES TERMINATED BY 0x0a (" + strings.Join(load, ", ") + ")"
	if len(assign) > 0 {
		query += " SET " + strings.Join(assign, ", ")
	}
	result, err := target.ExecContext(ctx, query)
	_ = reader.Close()
	if err != nil {
		stopRead()
	}
	read := <-source
	if err != nil {
		return 0, err
	}
	if read.err != nil {
		return 0, read.err
	}
	return read.rows, mySQLMigrationCheckLoad(ctx, target, result, read.rows)
}

type mysqlMigrationRead struct {
	rows int64
	err  error
}

func writeMySQLMigrationRows(rows *sql.Rows, w io.Writer, columns int, count *migrationCounter) (int64, error) {
	buf := bufio.NewWriterSize(migrationCopyWriter{w, count}, 128<<10)
	values, ptrs := migrationScan(columns)
	var n int64
	var line []byte
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, value := range values {
			if i != 0 {
				line = append(line, '\t')
			}
			if value == nil {
				line = append(line, '\\', 'N')
				continue
			}
			line = mysqlMigrationEscape(line, mysqlMigrationValue(value))
		}
		line = append(line, '\n')
		if _, err := buf.Write(line); err != nil {
			return n, err
		}
		line = line[:0]
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	return n, buf.Flush()
}

func migrationScan(n int) ([]any, []any) {
	values, ptrs := make([]any, n), make([]any, n)
	for i := range values {
		ptrs[i] = &values[i]
	}
	return values, ptrs
}

func mysqlMigrationValue(v any) []byte {
	switch value := v.(type) {
	case []byte:
		return value
	case string:
		return []byte(value)
	case int64:
		return strconv.AppendInt(nil, value, 10)
	case uint64:
		return strconv.AppendUint(nil, value, 10)
	case float32:
		return strconv.AppendFloat(nil, float64(value), 'g', -1, 32)
	case float64:
		return strconv.AppendFloat(nil, value, 'g', -1, 64)
	default:
		return []byte(fmt.Sprint(value))
	}
}

func mysqlMigrationEscape(dst, value []byte) []byte {
	for _, b := range value {
		switch b {
		case 0:
			dst = append(dst, '\\', '0')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case 26:
			dst = append(dst, '\\', 'Z')
		case '\\':
			dst = append(dst, '\\', '\\')
		default:
			dst = append(dst, b)
		}
	}
	return dst
}

func mySQLMigrationInsert(ctx context.Context, target *sql.Tx, table MigrationTable, cols []mysqlMigrationColumn, rows *sql.Rows, count *migrationCounter) (int64, error) {
	var names, slots []string
	for _, c := range cols {
		names, slots = append(names, c.name), append(slots, c.insert)
	}
	var packet int
	if err := target.QueryRowContext(ctx, "SELECT @@max_allowed_packet").Scan(&packet); err != nil {
		return 0, err
	}
	limit := min(packet/2, 1<<20)
	head := "INSERT INTO " + table.sqlName(MySQL) + " (" + strings.Join(names, ", ") + ") VALUES "
	tuple := "(" + strings.Join(slots, ", ") + "),"
	var n int64
	var args []any
	batchBytes := 0
	flush := func() error {
		batchRows := len(args) / len(cols)
		if batchRows == 0 {
			return nil
		}
		r, err := target.ExecContext(ctx, head+strings.TrimSuffix(strings.Repeat(tuple, batchRows), ","), args...)
		if err != nil {
			return err
		}
		if err := mySQLMigrationCheckLoad(ctx, target, r, int64(batchRows)); err != nil {
			return err
		}
		n += int64(batchRows)
		count.add(batchBytes)
		clear(args)
		args, batchBytes = args[:0], 0
		return nil
	}
	values, ptrs := migrationScan(len(cols))
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		rowBytes := len(tuple)
		for _, value := range values {
			rowBytes += 2*len(mysqlMigrationValue(value)) + 12
		}
		if len(args)/len(cols) >= 500 || batchBytes+rowBytes > limit {
			if err := flush(); err != nil {
				return n, err
			}
		}
		args = append(args, values...)
		batchBytes += rowBytes
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	err := flush()
	return n, err
}

func mySQLMigrationCheckLoad(ctx context.Context, target *sql.Tx, result sql.Result, read int64) error {
	var warnings int64
	if err := target.QueryRowContext(ctx, "SHOW COUNT(*) WARNINGS").Scan(&warnings); err != nil {
		return err
	}
	if warnings > 0 {
		var level, message string
		var code int
		if err := target.QueryRowContext(ctx, "SHOW WARNINGS LIMIT 1").Scan(&level, &code, &message); err != nil {
			return fmt.Errorf("目标产生 %d 条警告：%w", warnings, err)
		}
		return fmt.Errorf("目标产生 %d 条警告（%s %d）：%s", warnings, level, code, message)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if written != read {
		return fmt.Errorf("源端读取 %d 行，目标导入 %d 行", read, written)
	}
	return nil
}
