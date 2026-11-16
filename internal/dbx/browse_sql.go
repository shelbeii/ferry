package dbx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func readOnlySQLStatements(kind, src string) ([]string, error) {
	if kind == SQLServer {
		return sqlServerStatements(src)
	}
	stmts := Split(kind, src)
	if len(stmts) == 0 {
		return nil, errors.New("没有要执行的查询")
	}
	for _, stmt := range stmts {
		switch FirstWord(kind, stmt) {
		case "SELECT", "WITH", "EXPLAIN":
			continue
		case "PRAGMA", "VALUES":
			if kind == SQLite {
				continue
			}
		case "SHOW", "DESCRIBE", "DESC", "EXISTS":
			if kind == ClickHouse {
				continue
			}
		}
		return nil, fmt.Errorf("这个连接只接受查询：%s", firstLine(stmt))
	}
	return stmts, nil
}

func (cl *Client) readOnlySQLRows(ctx context.Context, db, schema, table string, o BrowseOptions) (*Result, error) {
	if o.Where != "" || len(o.Filters) > 0 || len(o.Sorts) > 0 {
		return nil, errors.New("请在查询窗口里指定筛选和排序")
	}
	if o.Limit <= 0 || o.Limit > 1000 {
		o.Limit = 200
	}
	kind := cl.Conn.Kind
	ti, err := cl.TableInfo(ctx, db, schema, table)
	if err != nil {
		return nil, err
	}
	base := "SELECT * FROM " + TableName(kind, cl.databaseName(db), schema, table)
	order := quoteList(kind, ti.PrimaryKey)
	if kind == SQLServer && order == "" {
		order = "(SELECT NULL)"
	}
	if order != "" {
		base += " ORDER BY " + order
	}
	page := func(limit int) string {
		offset := max(0, o.Offset)
		if kind == SQLServer {
			return fmt.Sprintf("%s OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", base, offset, limit)
		}
		return fmt.Sprintf("%s LIMIT %d OFFSET %d", base, limit, offset)
	}
	r := cl.readOnlySQL(ctx, db, page(o.Limit+1), o.Limit)
	r.SQL = page(o.Limit)
	return &r, nil
}

func (cl *Client) readOnlySQL(ctx context.Context, database, query string, limit int) Result {
	start := time.Now()
	r := Result{SQL: query}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	err := func() error {
		db, err := cl.DB(ctx, database)
		if err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		r.Columns, r.Rows, r.More, err = readRows(rows, limit)
		if r.More {
			cancel()
		}
		return err
	}()
	r.Ms = time.Since(start).Milliseconds()
	if err != nil {
		r.Error, r.Code = cl.explain(err).Error(), ErrorCode(err)
	}
	return r
}

func (cl *Client) readOnlySQLQuery(ctx context.Context, db, src string, o QueryOptions) ([]Result, error) {
	stmts, err := readOnlySQLStatements(cl.Conn.Kind, src)
	if err != nil {
		return nil, err
	}
	if o.Max <= 0 || o.Max > 1000 {
		o.Max = 1000
	}
	out := make([]Result, 0, len(stmts))
	for _, stmt := range stmts {
		r := cl.readOnlySQL(ctx, db, stmt, o.Max)
		out = append(out, r)
		if ctx.Err() != nil || (r.Error != "" && o.Stop) {
			break
		}
	}
	return out, nil
}

func (cl *Client) readOnlySQLExport(ctx context.Context, dbName string, o ExportOptions, progress func(int64)) (*ExportResult, error) {
	if o.Format != "" && o.Format != "csv" {
		return nil, errors.New("这个连接支持导出为 CSV")
	}
	kind := cl.Conn.Kind
	if kind == SQLite {
		if err := sqliteExportDestination(cl.Conn.File, o.Out); err != nil {
			return nil, err
		}
	}
	var q string
	if strings.TrimSpace(o.Query) == "" && o.Table != "" {
		if o.Browse.Where != "" || len(o.Browse.Filters) > 0 || len(o.Browse.Sorts) > 0 {
			return nil, errors.New("请在查询窗口里指定筛选和排序，再导出结果")
		}
		q = "SELECT * FROM " + TableName(kind, cl.databaseName(dbName), o.Schema, o.Table)
	} else {
		stmts, err := readOnlySQLStatements(kind, o.Query)
		if err != nil {
			return nil, err
		}
		if len(stmts) != 1 {
			return nil, errors.New("导出只能是一条查询")
		}
		q = stmts[0]
	}
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	f, err := createOut(o.Out)
	if err != nil {
		return nil, err
	}
	defer f.abort()
	n, err := writeCSV(ctx, db, f.w, q, nil, false, progress)
	if err != nil {
		return nil, cl.explain(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size, err := f.finish()
	if err != nil {
		return nil, err
	}
	return &ExportResult{Rows: n, Bytes: size, Ms: time.Since(start).Milliseconds(), Out: o.Out}, nil
}
