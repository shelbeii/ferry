package dbx

// 导入：执行 SQL 文件、把 CSV 导进一张表。

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// ---------------------------------------------------------------- SQL 文件

// ImportSQLOptions 执行 SQL 文件。
type ImportSQLOptions struct {
	File string
	// Continue 有语句出错时接着执行后面的（默认停下）
	Continue bool
	// BatchInserts 合并连续写入同一张表的单行 INSERT。仅在遇错停止时启用。
	BatchInserts bool
	// StartOffset 断点续传：跳过文件前 StartOffset 字节，从上次成功执行的位置继续。
	StartOffset int64
}

// ImportError 执行出错的一条。
type ImportError struct {
	N     int64  `json:"n"`             // 第几条语句
	End   int64  `json:"end,omitempty"` // 合并执行时的最后一条语句
	SQL   string `json:"sql"`
	Error string `json:"error"`
}

func (e ImportError) StatementLabel() string {
	if e.End > e.N {
		return fmt.Sprintf("第 %d–%d 条语句（合并执行）", e.N, e.End)
	}
	return fmt.Sprintf("第 %d 条语句", e.N)
}

// ImportResult 导入的结果。
type ImportResult struct {
	Stmts  int64         `json:"stmts,omitempty"` // 执行了多少条语句（SQL 文件）
	Rows   int64         `json:"rows,omitempty"`  // 导入了多少行（CSV）
	Failed int64         `json:"failed,omitempty"`
	Errors []ImportError `json:"errors,omitempty"` // 前 20 个错误
	Bytes  int64         `json:"bytes"`
	Ms     int64         `json:"ms"`
	// Stopped 出错后停下了（没执行完）
	Stopped                 bool  `json:"stopped,omitempty"`
	LastSuccessfulStatement int64 `json:"lastSuccessfulStatement,omitempty"`
	LastSuccessfulOffset    int64 `json:"lastSuccessfulOffset,omitempty"`
}

type ImportSQLStoppedError struct {
	Result ImportResult
	Cause  error
}

func (e *ImportSQLStoppedError) Error() string {
	return fmt.Sprintf("SQL 文件执行中止，已提交的改动可能仍然生效。最后成功返回第 %d 条语句（文件字节 %d）：%v",
		e.Result.LastSuccessfulStatement, e.Result.LastSuccessfulOffset, e.Cause)
}

func (e *ImportSQLStoppedError) Unwrap() error { return e.Cause }

func (e *ImportSQLStoppedError) JSONDetail() map[string]any {
	return map[string]any{"kind": "sql-import-stopped", "phase": "执行 SQL 文件", "attempted": e.Result.Stmts,
		"lastSuccessfulStatement": e.Result.LastSuccessfulStatement, "lastSuccessfulOffset": e.Result.LastSuccessfulOffset}
}

// ErrImportReadOnly 只读连接不能导入。
var ErrImportReadOnly = errors.New("这个连接设成了只读，不能导入")

// ImportSQL 执行 SQL 文件：边读边按语句拆开执行（文件可以很大），用同一条连接（文件里的 SET、USE、关外键检查对后面的语句有效）。
// progress 报告已成功执行到的字节和尝试执行的语句数。
func (cl *Client) ImportSQL(ctx context.Context, dbName string, o ImportSQLOptions, progress func(done, total, stmts int64)) (*ImportResult, error) {
	if cl.Conn.ReadOnly {
		return nil, ErrImportReadOnly
	}
	f, err := os.Open(o.File)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, _ := f.Stat()
	total := int64(0)
	if st != nil {
		total = st.Size()
	}
	// 脚本可以 USE、SET 或打开事务，结束后关掉这条连接。
	db, err := cl.pool(dbName, roleImport)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer conn.Close()
	id := cl.connID(ctx, conn)
	start := time.Now()
	res := &ImportResult{}
	kind := cl.Conn.Kind
	rd, closeReader, read, err := openSQLReader(f, o.StartOffset)
	if err != nil {
		return nil, err
	}
	if closeReader != nil {
		defer closeReader()
	}
	last := time.Now()
	batchInserts := o.BatchInserts && !o.Continue && (kind == MySQL || kind == Postgres)
	batchBytes := maxImportBatchBytes
	if batchInserts && kind == MySQL {
		var packet int64
		if err := conn.QueryRowContext(ctx, "SELECT @@max_allowed_packet").Scan(&packet); err != nil {
			batchInserts = false
		} else {
			batchBytes = int(min(int64(batchBytes), packet-1)) // COM_QUERY 还要占一个字节
		}
	}
	run := func(stmts []string, offsets []int64) bool {
		for i := 0; i < len(stmts); {
			if ctx.Err() != nil {
				return false
			}
			s, n := stmts[i], 1
			if batchInserts {
				s, n = batchSQLInserts(kind, stmts[i:], batchBytes)
			}
			res.Stmts += int64(n)
			r := cl.runOne(ctx, conn, dbName, id, s, 1)
			if r.Error != "" {
				res.Failed += int64(n)
				if len(res.Errors) < 20 {
					e := ImportError{N: res.Stmts - int64(n) + 1, SQL: firstLine(stmts[i]), Error: r.Error}
					if n > 1 {
						e.End = res.Stmts
					}
					res.Errors = append(res.Errors, e)
				}
				if !o.Continue || ctx.Err() != nil {
					res.Stopped = true
					return false
				}
			} else {
				res.LastSuccessfulStatement = res.Stmts
				res.LastSuccessfulOffset = offsets[i+n-1]
			}
			i += n
			if progress != nil && time.Since(last) > 300*time.Millisecond {
				last = time.Now()
				progress(res.LastSuccessfulOffset, total, res.Stmts)
			}
		}
		return true
	}
	runCopy := func(stmt string, data io.Reader, endOffset func() int64) bool {
		if ctx.Err() != nil {
			return false
		}
		res.Stmts++
		err := runPGCopyFrom(ctx, conn, stmt, data)
		if err != nil {
			res.Failed++
			if len(res.Errors) < 20 {
				res.Errors = append(res.Errors, ImportError{N: res.Stmts, SQL: firstLine(stmt), Error: cl.explain(err).Error()})
			}
			if !o.Continue || ctx.Err() != nil {
				res.Stopped = true
				return false
			}
			return true
		}
		res.LastSuccessfulStatement = res.Stmts
		res.LastSuccessfulOffset = endOffset()
		if progress != nil && time.Since(last) > 300*time.Millisecond {
			last = time.Now()
			progress(res.LastSuccessfulOffset, total, res.Stmts)
		}
		return true
	}
	err = eachStatementWithCopy(ctx, kind, rd, 1<<20, &read, run, runCopy)
	res.Bytes = read
	res.Ms = time.Since(start).Milliseconds()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		res.Stopped = true
		return res, &ImportSQLStoppedError{Result: *res, Cause: err}
	}
	if progress != nil {
		done := read
		if res.Stopped {
			done = res.LastSuccessfulOffset
		}
		progress(done, total, res.Stmts)
	}
	return res, nil
}

// ---------------------------------------------------------------- CSV

// CSVOptions 读 CSV 的方式。
type CSVOptions struct {
	File string `json:"file"`
	// Delimiter 分隔符：, \t ; |；为空时按第一行猜
	Delimiter string `json:"delimiter,omitempty"`
	// Encoding utf-8 | gbk；为空时自动（不是 UTF-8 时按 GBK，Excel 中文版另存的 CSV 常是 GBK）
	Encoding string `json:"encoding,omitempty"`
	// Header 第一行是列名
	Header bool `json:"header"`
}

// CSVImport 把 CSV 导进一张表。
type CSVImport struct {
	CSVOptions
	Schema string `json:"schema,omitempty"`
	Table  string `json:"table"`
	// Columns 文件里第 i 列导进表的哪一列；空串 = 不导这一列
	Columns []string `json:"columns"`
	// EmptyNull 空的单元格写 NULL（否则写空字符串）
	EmptyNull bool `json:"emptyNull"`
	// Conflict 主键/唯一键冲突策略："" / "fail"（报错回滚）| "skip"（跳过冲突行）| "replace"（覆盖冲突行）
	Conflict string `json:"conflict,omitempty"`
	// Truncate 导入前先清空目标表
	Truncate bool `json:"truncate,omitempty"`
	// BatchCommit 分批提交（每 5000 行提交一次，适合超大 CSV，出错时已提交批次保留）
	BatchCommit bool `json:"batchCommit,omitempty"`
}

// CSVPreview 预览：前几行和猜出来的格式。
type CSVPreview struct {
	Delimiter string     `json:"delimiter"`
	Encoding  string     `json:"encoding"`
	Rows      [][]string `json:"rows"` // 前 20 行（含表头）
	Size      int64      `json:"size"`
}

// openCSV 打开 CSV：去掉 BOM，按编码解码，设好分隔符。
func openCSV(o CSVOptions) (*csv.Reader, *os.File, *countReader, string, string, error) {
	f, err := os.Open(o.File)
	if err != nil {
		return nil, nil, nil, "", "", err
	}
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, nil, "", "", err
	}
	cr := &countReader{r: f}
	var r io.Reader = bufio.NewReaderSize(cr, 1<<20)
	bom := bytes.HasPrefix(head, []byte("\xEF\xBB\xBF"))
	if bom {
		r.(*bufio.Reader).Discard(3)
		head = head[3:]
	}
	enc := strings.ToLower(o.Encoding)
	if enc == "" {
		enc = "utf-8"
		if !bom && !validPrefix(head) {
			enc = "gbk"
		}
	}
	if enc == "gbk" || enc == "gb18030" {
		r = transform.NewReader(r, simplifiedchinese.GB18030.NewDecoder())
		if h, err := simplifiedchinese.GB18030.NewDecoder().Bytes(head); err == nil {
			head = h
		}
	}
	delim := o.Delimiter
	if delim == "" {
		delim = guessDelimiter(head)
	}
	if delim == `\t` {
		delim = "\t"
	}
	rr := csv.NewReader(r)
	rr.Comma, _ = utf8.DecodeRuneInString(delim)
	rr.FieldsPerRecord = -1
	rr.LazyQuotes = true
	rr.ReuseRecord = false
	return rr, f, cr, delim, enc, nil
}

// validPrefix 开头一段是不是 UTF-8（最后可能截断在一个字的中间，不算错）。
func validPrefix(b []byte) bool {
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n <= 1 {
			return len(b) < utf8.UTFMax && !utf8.FullRune(b)
		}
		b = b[n:]
	}
	return true
}

// guessDelimiter 第一行里逗号、制表符、分号、竖线哪个多用哪个。
func guessDelimiter(head []byte) string {
	line := string(head)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	best, n := ",", 0
	for _, d := range []string{",", "\t", ";", "|"} {
		if c := strings.Count(line, d); c > n {
			best, n = d, c
		}
	}
	return best
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// PreviewCSV 读前 20 行给用户对列。
func PreviewCSV(o CSVOptions) (*CSVPreview, error) {
	rr, f, _, delim, enc, err := openCSV(o)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p := &CSVPreview{Delimiter: delim, Encoding: enc, Rows: [][]string{}}
	if st, err := f.Stat(); err == nil {
		p.Size = st.Size()
	}
	for len(p.Rows) < 20 {
		rec, err := rr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("第 %d 行格式不对：%w", len(p.Rows)+1, err)
		}
		p.Rows = append(p.Rows, rec)
	}
	return p, nil
}

// ImportCSV 把 CSV 导进表：支持高速通道（MySQL LOAD DATA / PG COPY）、冲突策略、清空表和分批提交。
func (cl *Client) ImportCSV(ctx context.Context, dbName string, o CSVImport, progress func(done, total, rows int64)) (*ImportResult, error) {
	if cl.Conn.ReadOnly {
		return nil, ErrImportReadOnly
	}
	if !slices.Contains([]string{"", "fail", "skip", "replace"}, o.Conflict) {
		return nil, fmt.Errorf("不支持的冲突策略 %q", o.Conflict)
	}
	kind := cl.Conn.Kind
	ti, err := cl.TableInfo(ctx, dbName, o.Schema, o.Table)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, c := range ti.Columns {
		if !c.Generated {
			known[c.Name] = true
		}
	}
	var pick []int // 导入的是文件里的第几列
	var rawNames []string
	var names []string
	for i, c := range o.Columns {
		if c == "" {
			continue
		}
		if !known[c] {
			return nil, fmt.Errorf("表 %s 没有列 %s（或者是计算列）", o.Table, c)
		}
		pick = append(pick, i)
		rawNames = append(rawNames, c)
		names = append(names, Quote(kind, c))
	}
	if len(pick) == 0 {
		return nil, errors.New("没有选要导入的列")
	}
	if kind == Postgres && o.Conflict == "replace" && len(ti.PrimaryKey) == 0 {
		return nil, errors.New("PostgreSQL 覆盖冲突行需要目标表有主键")
	}
	rr, f, cr, _, _, err := openCSV(o.CSVOptions)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	total := int64(0)
	if st, err := f.Stat(); err == nil {
		total = st.Size()
	}
	start := time.Now()
	if !o.BatchCommit {
		if kind == Postgres && (o.Conflict == "" || o.Conflict == "fail") {
			return cl.importCSVPostgresCopy(ctx, dbName, o, rr, cr, total, pick, names, start, progress)
		}
		if kind == MySQL {
			if ok, res, err := cl.tryImportCSVMySQLLoad(ctx, dbName, o, rr, cr, total, pick, names, start, progress); ok {
				return res, err
			}
		}
	}

	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	tableSQL := TableName(kind, dbName, o.Schema, o.Table)
	if o.Truncate {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tableSQL); err != nil {
			return nil, fmt.Errorf("清空目标表失败：%w", cl.explain(err))
		}
	}
	// 一条最多 500 行，参数个数不超过 6 万（PostgreSQL 限制 65535 个）
	per := min(500, 60000/len(pick))
	head := "INSERT INTO " + tableSQL + " (" + strings.Join(names, ", ") + ") VALUES "
	suffix := ""
	if kind == MySQL {
		switch o.Conflict {
		case "skip":
			head = "INSERT IGNORE INTO " + tableSQL + " (" + strings.Join(names, ", ") + ") VALUES "
		case "replace":
			head = "REPLACE INTO " + tableSQL + " (" + strings.Join(names, ", ") + ") VALUES "
		}
	} else if kind == Postgres {
		switch o.Conflict {
		case "skip":
			suffix = " ON CONFLICT DO NOTHING"
		case "replace":
			pkSet := map[string]bool{}
			for _, k := range ti.PrimaryKey {
				pkSet[k] = true
			}
			var updates []string
			for _, col := range rawNames {
				if !pkSet[col] {
					q := Quote(Postgres, col)
					updates = append(updates, q+" = EXCLUDED."+q)
				}
			}
			if len(updates) == 0 {
				suffix = " ON CONFLICT (" + quoteList(Postgres, ti.PrimaryKey) + ") DO NOTHING"
			} else {
				suffix = " ON CONFLICT (" + quoteList(Postgres, ti.PrimaryKey) + ") DO UPDATE SET " + strings.Join(updates, ", ")
			}
		}
	}
	var args []any
	var tuples []string
	p := &placeholders{kind: kind}
	res := &ImportResult{}
	line := int64(0)
	firstLine := int64(1)
	uncommitted := int64(0)
	flush := func() error {
		if len(tuples) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, head+strings.Join(tuples, ", ")+suffix, args...); err != nil {
			if o.BatchCommit {
				return fmt.Errorf("第 %d–%d 行导入失败（已提交 %d 行）：%w", firstLine, line, res.Rows-uncommitted, cl.explain(err))
			}
			return fmt.Errorf("第 %d–%d 行导入失败（全部撤回了）：%w", firstLine, line, cl.explain(err))
		}
		batchN := int64(len(tuples))
		res.Rows += batchN
		uncommitted += batchN
		tuples, args = tuples[:0], args[:0]
		p = &placeholders{kind: kind}
		firstLine = line + 1
		if o.BatchCommit && uncommitted >= 5000 {
			if err := tx.Commit(); err != nil {
				return cl.explain(err)
			}
			uncommitted = 0
			tx, err = db.BeginTx(ctx, nil)
			if err != nil {
				return cl.explain(err)
			}
		}
		return nil
	}
	last := time.Now()
	for {
		rec, err := rr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("第 %d 行格式不对：%w", line, err)
		}
		if line == 1 && o.Header {
			firstLine = 2
			continue
		}
		if len(rec) == 1 && rec[0] == "" {
			continue // 空行
		}
		ph := make([]string, len(pick))
		for j, i := range pick {
			var v any
			if i < len(rec) {
				v = rec[i]
			}
			if s, _ := v.(string); s == "" && o.EmptyNull {
				v = nil
			}
			ph[j] = p.add(v)
		}
		args = p.args
		tuples = append(tuples, "("+strings.Join(ph, ", ")+")")
		if len(tuples) >= per {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		if progress != nil && time.Since(last) > 300*time.Millisecond {
			last = time.Now()
			progress(cr.n, total, res.Rows)
		}
		if ctx.Err() != nil {
			if o.BatchCommit {
				return nil, fmt.Errorf("已停止（已提交 %d 行）", res.Rows-uncommitted)
			}
			return nil, errors.New("已停止，导入的都撤回了")
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, cl.explain(err)
	}
	tx = nil
	if progress != nil {
		progress(total, total, res.Rows)
	}
	res.Bytes = total
	res.Ms = time.Since(start).Milliseconds()
	return res, nil
}

func (cl *Client) importCSVPostgresCopy(ctx context.Context, dbName string, o CSVImport, rr *csv.Reader, cr *countReader, total int64, pick []int, names []string, start time.Time, progress func(done, total, rows int64)) (*ImportResult, error) {
	conn, err := migrationConn(ctx, cl, dbName)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer tx.Rollback()
	tableSQL := TableName(Postgres, dbName, o.Schema, o.Table)
	if o.Truncate {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tableSQL); err != nil {
			return nil, fmt.Errorf("清空目标表失败：%w", cl.explain(err))
		}
	}
	reader, writer := io.Pipe()
	stopPipe := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()) })
	defer stopPipe()
	type writeRes struct {
		rows int64
		err  error
	}
	doneCh := make(chan writeRes, 1)
	go func() {
		n, werr := streamCSVAsTSV(ctx, rr, cr, total, pick, o.Header, o.EmptyNull, false, writer, progress)
		_ = writer.CloseWithError(werr)
		doneCh <- writeRes{n, werr}
	}()
	copySQL := "COPY " + tableSQL + " (" + strings.Join(names, ", ") + ") FROM STDIN WITH (FORMAT text, NULL '\\N', ENCODING 'UTF8')"
	err = runPGCopyFrom(ctx, conn, copySQL, reader)
	_ = reader.Close()
	wr := <-doneCh
	if err != nil {
		return nil, cl.explain(err)
	}
	if wr.err != nil {
		return nil, wr.err
	}
	if err := tx.Commit(); err != nil {
		return nil, cl.explain(err)
	}
	if progress != nil {
		progress(total, total, wr.rows)
	}
	return &ImportResult{Rows: wr.rows, Bytes: total, Ms: time.Since(start).Milliseconds()}, nil
}

func (cl *Client) tryImportCSVMySQLLoad(ctx context.Context, dbName string, o CSVImport, rr *csv.Reader, cr *countReader, total int64, pick []int, names []string, start time.Time, progress func(done, total, rows int64)) (bool, *ImportResult, error) {
	conn, err := migrationConn(ctx, cl, dbName)
	if err != nil {
		return false, nil, nil
	}
	defer conn.Close()
	var local bool
	if err := conn.QueryRowContext(ctx, "SELECT @@global.local_infile").Scan(&local); err != nil || !local {
		return false, nil, nil
	}
	if o.Conflict == "" || o.Conflict == "fail" {
		if _, err := conn.ExecContext(ctx, `SET SESSION sql_mode = CONCAT_WS(',', NULLIF(@@session.sql_mode, ''), 'STRICT_ALL_TABLES')`); err != nil {
			return false, nil, nil
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return true, nil, cl.explain(err)
	}
	defer tx.Rollback()
	tableSQL := TableName(MySQL, dbName, o.Schema, o.Table)
	if o.Truncate {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+tableSQL); err != nil {
			return true, nil, fmt.Errorf("清空目标表失败：%w", cl.explain(err))
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return true, nil, err
	}
	reader, writer := io.Pipe()
	stopPipe := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()) })
	defer stopPipe()
	handler := fmt.Sprintf("ferry_csv_%x", nonce)
	mysql.RegisterReaderHandler(handler, func() io.Reader { return reader })
	defer mysql.DeregisterReaderHandler(handler)
	type writeRes struct {
		rows int64
		err  error
	}
	doneCh := make(chan writeRes, 1)
	go func() {
		n, werr := streamCSVAsTSV(ctx, rr, cr, total, pick, o.Header, o.EmptyNull, true, writer, progress)
		_ = writer.CloseWithError(werr)
		doneCh <- writeRes{n, werr}
	}()
	modifier := ""
	if o.Conflict == "skip" {
		modifier = " IGNORE"
	} else if o.Conflict == "replace" {
		modifier = " REPLACE"
	}
	loadSQL := "LOAD DATA LOCAL INFILE 'Reader::" + handler + "'" + modifier + " INTO TABLE " + tableSQL +
		" CHARACTER SET utf8mb4 FIELDS TERMINATED BY 0x09 ESCAPED BY 0x5c LINES TERMINATED BY 0x0a (" + strings.Join(names, ", ") + ")"
	_, err = tx.ExecContext(ctx, loadSQL)
	_ = reader.Close()
	wr := <-doneCh
	if err != nil {
		return true, nil, cl.explain(err)
	}
	if wr.err != nil {
		return true, nil, wr.err
	}
	if o.Conflict == "" || o.Conflict == "fail" {
		if err := mySQLMigrationCheckWarnings(ctx, tx); err != nil {
			return true, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return true, nil, cl.explain(err)
	}
	if progress != nil {
		progress(total, total, wr.rows)
	}
	return true, &ImportResult{Rows: wr.rows, Bytes: total, Ms: time.Since(start).Milliseconds()}, nil
}

func streamCSVAsTSV(ctx context.Context, rr *csv.Reader, cr *countReader, total int64, pick []int, header, emptyNull, mysqlMode bool, w io.Writer, progress func(done, total, rows int64)) (int64, error) {
	bw := bufio.NewWriterSize(w, 128<<10)
	var lineBuf []byte
	var line, rows int64
	last := time.Now()
	for {
		if ctx.Err() != nil {
			return rows, errors.New("已停止，导入的都撤回了")
		}
		rec, err := rr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return rows, fmt.Errorf("第 %d 行格式不对：%w", line, err)
		}
		if line == 1 && header {
			continue
		}
		if len(rec) == 1 && rec[0] == "" {
			continue
		}
		lineBuf = lineBuf[:0]
		for j, i := range pick {
			if j > 0 {
				lineBuf = append(lineBuf, '\t')
			}
			val := ""
			present := i < len(rec)
			if present {
				val = rec[i]
			}
			if !present || (val == "" && emptyNull) {
				lineBuf = append(lineBuf, '\\', 'N')
				continue
			}
			if mysqlMode {
				lineBuf = mysqlMigrationEscape(lineBuf, []byte(val))
			} else {
				lineBuf = pgCopyTextEscape(lineBuf, val)
			}
		}
		lineBuf = append(lineBuf, '\n')
		if _, err := bw.Write(lineBuf); err != nil {
			return rows, err
		}
		rows++
		if progress != nil && time.Since(last) > 300*time.Millisecond {
			last = time.Now()
			progress(cr.n, total, rows)
		}
	}
	return rows, bw.Flush()
}

func pgCopyTextEscape(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		default:
			dst = append(dst, s[i])
		}
	}
	return dst
}

// openSQLReader 打开 SQL 文件：支持 .sql.gz 透明解压、剥离 UTF-8 BOM、GB18030(GBK) 自动解码以及按字节偏移断点续传。
func openSQLReader(f *os.File, startOffset int64) (io.Reader, func(), int64, error) {
	if startOffset < 0 {
		return nil, nil, 0, errors.New("续传偏移量不能为负数")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, 0, err
	}
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, 0, err
	}
	if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		if startOffset > 0 {
			return nil, nil, 0, errors.New("gzip 压缩的 .sql.gz 文件不支持按字节偏移续传，请先解压后再续传")
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("打开 gzip SQL 文件失败：%w", err)
		}
		br := bufio.NewReaderSize(gz, 1<<20)
		peek, _ := br.Peek(64 << 10)
		var read int64
		bom := bytes.HasPrefix(peek, []byte("\xEF\xBB\xBF"))
		if bom {
			_, _ = br.Discard(3)
			peek = peek[3:]
			read = 3
		}
		if !bom && !validPrefix(peek) {
			return bufio.NewReaderSize(transform.NewReader(br, simplifiedchinese.GB18030.NewDecoder()), 1<<20), func() { _ = gz.Close() }, read, nil
		}
		return br, func() { _ = gz.Close() }, read, nil
	}

	bom := bytes.HasPrefix(head, []byte("\xEF\xBB\xBF"))
	headCheck := head
	if bom {
		headCheck = head[3:]
	}
	isGBK := !bom && !validPrefix(headCheck)
	if startOffset > 0 {
		if st, err := f.Stat(); err == nil && startOffset > st.Size() {
			return nil, nil, 0, fmt.Errorf("续传偏移量 %d 超过文件大小 %d", startOffset, st.Size())
		}
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return nil, nil, 0, err
		}
		br := bufio.NewReaderSize(f, 1<<20)
		if isGBK {
			return bufio.NewReaderSize(transform.NewReader(br, simplifiedchinese.GB18030.NewDecoder()), 1<<20), nil, startOffset, nil
		}
		return br, nil, startOffset, nil
	}

	br := bufio.NewReaderSize(f, 1<<20)
	var read int64
	if bom {
		_, _ = br.Discard(3)
		read = 3
	}
	if isGBK {
		return bufio.NewReaderSize(transform.NewReader(br, simplifiedchinese.GB18030.NewDecoder()), 1<<20), nil, read, nil
	}
	return br, nil, read, nil
}

func runPGCopyFrom(ctx context.Context, conn *sql.Conn, stmt string, data io.Reader) error {
	return conn.Raw(func(driver any) error {
		c, ok := driver.(*stdlib.Conn)
		if !ok {
			return errors.New("当前连接不支持 PostgreSQL COPY FROM STDIN")
		}
		_, err := c.Conn().PgConn().CopyFrom(ctx, data, stmt)
		return err
	})
}

// eachStatement 从 rd 边读边拆语句（每次读 chunk 字节），拆出来的交给 fn；fn 返回 false 时停下。read 记读了多少字节。
func eachStatement(ctx context.Context, kind string, rd io.Reader, chunk int, read *int64, fn func([]string, []int64) bool) error {
	return eachStatementWithCopy(ctx, kind, rd, chunk, read, fn, nil)
}

func eachStatementWithCopy(ctx context.Context, kind string, rd io.Reader, chunk int, read *int64, fn func([]string, []int64) bool, copyFn func(stmt string, data io.Reader, endOffset func() int64) bool) error {
	var buf strings.Builder
	delim := ";"
	b := make([]byte, chunk)
	var pending []byte // 上次读到的最后不完整的一行
	eof := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !eof {
			n, rerr := io.ReadFull(rd, b)
			*read += int64(n)
			eof = rerr == io.EOF || rerr == io.ErrUnexpectedEOF
			if rerr != nil && !eof {
				return rerr
			}
			data := b[:n]
			if len(pending) > 0 {
				data = append(pending, data...)
			}
			// 只交整行给拆语句的（DELIMITER、注释要按行看）
			if !eof {
				i := bytes.LastIndexByte(b[:n], '\n')
				if i < 0 {
					if len(pending) == 0 {
						data = bytes.Clone(data) // b 会被下一次读取覆盖
					}
					pending = data
					continue
				}
				i += len(pending)
				pending = bytes.Clone(data[i+1:])
				data = data[:i+1]
			} else {
				pending = nil
			}
			buf.Write(data)
		}
		for {
			base := *read - int64(len(pending)+buf.Len())
			var offsets []int64
			stmts, used, d := splitScan(kind, buf.String(), delim, eof, true, func(end int) {
				offsets = append(offsets, base+int64(end))
			})
			delim = d
			rem := ""
			if used > 0 {
				rem = buf.String()[used:]
				buf.Reset()
				if !eof || len(rem) > 0 {
					buf.WriteString(rem)
				}
			}
			if copyFn != nil && postgresProtocol(kind) && len(stmts) > 0 && isPGCopyFromStdin(stmts[len(stmts)-1]) {
				pre := len(stmts) - 1
				if pre > 0 && !fn(stmts[:pre], offsets[:pre]) {
					return nil
				}
				copyStmt := stmts[pre]
				copyStart := offsets[pre]
				tail := buf.String() + string(pending)
				buf.Reset()
				pending = nil
				cr := newPGCopyStream(tail, rd, read)
				if err := cr.skipStatementLine(); err != nil {
					return err
				}
				ok := copyFn(copyStmt, cr, func() int64 { return copyStart + cr.consumed })
				if !cr.done {
					_, _ = io.Copy(io.Discard, cr)
				}
				if cr.err != nil && !errors.Is(cr.err, io.EOF) {
					return cr.err
				}
				if rest := cr.remaining(); len(rest) > 0 {
					if !eof {
						if i := strings.LastIndexByte(rest, '\n'); i >= 0 {
							buf.WriteString(rest[:i+1])
							pending = []byte(rest[i+1:])
						} else {
							pending = []byte(rest)
						}
					} else {
						buf.WriteString(rest)
					}
				}
				if !ok {
					return nil
				}
				if buf.Len() > 0 {
					continue
				}
				break
			}
			if len(stmts) > 0 && !fn(stmts, offsets) {
				return nil
			}
			if eof && buf.Len() == 0 {
				return nil
			}
			break
		}
		if eof {
			return nil
		}
	}
}

type pgCopyStream struct {
	tail     string
	tailPos  int
	rd       io.Reader
	read     *int64
	br       *bufio.Reader
	buf      []byte
	consumed int64
	done     bool
	err      error
}

func newPGCopyStream(tail string, rd io.Reader, read *int64) *pgCopyStream {
	return &pgCopyStream{tail: tail, rd: rd, read: read}
}

func (c *pgCopyStream) readLine() (string, error) {
	if c.tailPos < len(c.tail) {
		rest := c.tail[c.tailPos:]
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line := rest[:i+1]
			c.tailPos += i + 1
			c.consumed += int64(len(line))
			return line, nil
		}
		prefix := rest
		c.tailPos = len(c.tail)
		c.consumed += int64(len(prefix))
		more, err := c.readLineFromRD()
		return prefix + more, err
	}
	return c.readLineFromRD()
}

func (c *pgCopyStream) readLineFromRD() (string, error) {
	if c.br == nil {
		c.br = bufio.NewReaderSize(c.rd, 64<<10)
	}
	line, err := c.br.ReadString('\n')
	if len(line) > 0 {
		*c.read += int64(len(line))
		c.consumed += int64(len(line))
	}
	return line, err
}

func (c *pgCopyStream) skipStatementLine() error {
	line, err := c.readLine()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "--") {
		// 极少数情况 COPY ... FROM stdin; 后面同一行直接跟了数据或空标记
		c.buf = append(c.buf, line...)
	}
	return nil
}

func (c *pgCopyStream) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.done {
			return 0, io.EOF
		}
		line, err := c.readLine()
		if err != nil && !errors.Is(err, io.EOF) {
			c.err = err
			return 0, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == `\.` {
			c.done = true
			return 0, io.EOF
		}
		if errors.Is(err, io.EOF) && line == "" {
			c.err = errors.New(`COPY 数据缺少结束标记 \.`)
			return 0, c.err
		}
		c.buf = append(c.buf, line...)
		if errors.Is(err, io.EOF) {
			c.err = errors.New(`COPY 数据缺少结束标记 \.`)
			return 0, c.err
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *pgCopyStream) remaining() string {
	var b strings.Builder
	if c.tailPos < len(c.tail) {
		b.WriteString(c.tail[c.tailPos:])
	}
	if c.br != nil && c.br.Buffered() > 0 {
		if peek, err := c.br.Peek(c.br.Buffered()); err == nil && len(peek) > 0 {
			_, _ = c.br.Discard(len(peek))
			*c.read += int64(len(peek))
			b.Write(peek)
		}
	}
	return b.String()
}
