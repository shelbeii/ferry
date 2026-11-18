package dbx

// 导入：执行 SQL 文件、把 CSV 导进一张表。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

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
	rd := bufio.NewReader(f)
	var read int64
	if b, _ := rd.Peek(3); bytes.Equal(b, []byte("\xEF\xBB\xBF")) {
		rd.Discard(3)
		read = 3
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
	err = eachStatement(ctx, kind, rd, 1<<20, &read, run)
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

// ImportCSV 把 CSV 导进表：在一个事务里多行一条 INSERT，有一行失败就全部撤回。
func (cl *Client) ImportCSV(ctx context.Context, dbName string, o CSVImport, progress func(done, total, rows int64)) (*ImportResult, error) {
	if cl.Conn.ReadOnly {
		return nil, ErrImportReadOnly
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
	var names []string
	for i, c := range o.Columns {
		if c == "" {
			continue
		}
		if !known[c] {
			return nil, fmt.Errorf("表 %s 没有列 %s（或者是计算列）", o.Table, c)
		}
		pick = append(pick, i)
		names = append(names, Quote(kind, c))
	}
	if len(pick) == 0 {
		return nil, errors.New("没有选要导入的列")
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
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer tx.Rollback()
	// 一条最多 500 行，参数个数不超过 6 万（PostgreSQL 限制 65535 个）
	per := min(500, 60000/len(pick))
	head := "INSERT INTO " + TableName(kind, dbName, o.Schema, o.Table) + " (" + strings.Join(names, ", ") + ") VALUES "
	var args []any
	var tuples []string
	p := &placeholders{kind: kind}
	res := &ImportResult{}
	line := int64(0)
	firstLine := int64(1)
	flush := func() error {
		if len(tuples) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, head+strings.Join(tuples, ", "), args...); err != nil {
			return fmt.Errorf("第 %d–%d 行导入失败（全部撤回了）：%w", firstLine, line, cl.explain(err))
		}
		res.Rows += int64(len(tuples))
		tuples, args = tuples[:0], args[:0]
		p = &placeholders{kind: kind}
		firstLine = line + 1
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
			return nil, errors.New("已停止，导入的都撤回了")
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, cl.explain(err)
	}
	if progress != nil {
		progress(total, total, res.Rows)
	}
	res.Bytes = total
	res.Ms = time.Since(start).Milliseconds()
	return res, nil
}

// eachStatement 从 rd 边读边拆语句（每次读 chunk 字节），拆出来的交给 fn；fn 返回 false 时停下。read 记读了多少字节。
func eachStatement(ctx context.Context, kind string, rd io.Reader, chunk int, read *int64, fn func([]string, []int64) bool) error {
	var buf strings.Builder
	delim := ";"
	b := make([]byte, chunk)
	var pending []byte // 上次读到的最后不完整的一行
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := io.ReadFull(rd, b)
		*read += int64(n)
		eof := rerr == io.EOF || rerr == io.ErrUnexpectedEOF
		if rerr != nil && !eof {
			return rerr
		}
		base := *read - int64(n+len(pending)+buf.Len())
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
		}
		buf.Write(data)
		var offsets []int64
		stmts, used, d := splitScan(kind, buf.String(), delim, eof, true, func(end int) {
			offsets = append(offsets, base+int64(end))
		})
		delim = d
		if !eof && used > 0 {
			rem := buf.String()[used:]
			buf.Reset()
			buf.WriteString(rem)
		}
		if !fn(stmts, offsets) || eof {
			return nil
		}
	}
}
