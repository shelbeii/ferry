package dbx

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ExportOptions 导出数据（CSV 或 INSERT 语句）。
type ExportOptions struct {
	Format string // csv | tsv | sql
	Out    string // 文件路径
	// Table 导出的表（SQL 格式的 INSERT 写这个表名）；Query 为空时导出这张表（按 Browse 的条件、排序）
	Schema, Table string
	Query         string
	Browse        BrowseOptions
}

// ExportResult 导出的结果。
type ExportResult struct {
	Rows   int64  `json:"rows"`
	Tables int    `json:"tables,omitempty"`
	Bytes  int64  `json:"bytes"`
	Ms     int64  `json:"ms"`
	Out    string `json:"out"`
}

// outFile 先写 <文件>.part，写完再改名：中途失败不会留下半个文件。
type outFile struct {
	f   *os.File
	w   *bufio.Writer
	out string
}

func createOut(out string) (*outFile, error) {
	f, err := os.Create(out + ".part")
	if err != nil {
		return nil, err
	}
	return &outFile{f: f, w: bufio.NewWriterSize(f, 256<<10), out: out}, nil
}

func (o *outFile) abort() {
	o.f.Close()
	os.Remove(o.out + ".part")
}

func (o *outFile) finish() (int64, error) {
	if err := o.w.Flush(); err != nil {
		o.abort()
		return 0, err
	}
	st, _ := o.f.Stat()
	if err := o.f.Close(); err != nil {
		os.Remove(o.out + ".part")
		return 0, err
	}
	os.Remove(o.out)
	if err := os.Rename(o.out+".part", o.out); err != nil {
		return 0, err
	}
	if st == nil {
		return 0, nil
	}
	return st.Size(), nil
}

// Export 把查询结果全部写进文件（不限行数，边读边写）。只读事务里执行。
func (cl *Client) Export(ctx context.Context, dbName string, o ExportOptions, progress func(rows int64)) (*ExportResult, error) {
	kind := cl.Conn.Kind
	q := strings.TrimSpace(o.Query)
	var args []any
	if q == "" {
		if o.Table == "" {
			return nil, errors.New("没有要导出的内容")
		}
		var err error
		if q, args, err = selectSQL(kind, dbName, o.Schema, o.Table, o.Browse); err != nil {
			return nil, err
		}
	} else {
		stmts := Split(kind, q)
		if len(stmts) != 1 {
			return nil, errors.New("导出只能是一条查询")
		}
		q = stmts[0]
		if !returnsRows(kind, q) {
			return nil, errors.New("导出只能是查询（SELECT）")
		}
	}
	switch o.Format {
	case "", "csv", "tsv", "sql":
	default:
		return nil, fmt.Errorf("不支持的格式 %q", o.Format)
	}
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, cl.explain(err)
	}
	defer tx.Rollback()
	of, err := createOut(o.Out)
	if err != nil {
		return nil, err
	}
	var n int64
	if o.Format == "sql" {
		table := o.Table
		if table == "" {
			table = "导出"
		}
		n, err = cl.writeInserts(ctx, tx, of.w, q, args, TableName(kind, "", o.Schema, table), "", progress)
	} else {
		n, err = writeCSV(ctx, tx, of.w, q, args, o.Format == "tsv", progress)
	}
	if err != nil {
		of.abort()
		return nil, cl.explain(err)
	}
	size, err := of.finish()
	if err != nil {
		return nil, err
	}
	return &ExportResult{Rows: n, Bytes: size, Ms: time.Since(start).Milliseconds(), Out: o.Out}, nil
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func scanAll(rows *sql.Rows) ([]string, []string, []any, []any, error) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	names := make([]string, len(cts))
	types := make([]string, len(cts))
	for i, ct := range cts {
		names[i] = ct.Name()
		types[i] = strings.ToUpper(ct.DatabaseTypeName())
	}
	vals := make([]any, len(cts))
	ptrs := make([]any, len(cts))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	return names, types, vals, ptrs, nil
}

func writeCSV(ctx context.Context, q querier, w io.Writer, query string, args []any, tsv bool, progress func(int64)) (int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	names, types, vals, ptrs, err := scanAll(rows)
	if err != nil {
		return 0, err
	}
	io.WriteString(w, "\xEF\xBB\xBF") // Excel 按 UTF-8 打开
	cw := csv.NewWriter(w)
	if tsv {
		cw.Comma = '\t'
	}
	cw.Write(names)
	rec := make([]string, len(names))
	var n int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, v := range vals {
			rec[i] = exportText(v, types[i], false, "")
		}
		cw.Write(rec)
		n++
		if progress != nil && n%10000 == 0 {
			progress(n)
		}
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	cw.Flush()
	return n, cw.Error()
}
