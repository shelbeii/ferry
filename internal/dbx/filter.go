package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Filter 表数据的一个筛选条件（像 Navicat 的筛选：列、运算、值）。
type Filter struct {
	Col    string `json:"col"`
	Op     string `json:"op"` // 见 filterOps
	Value  string `json:"value,omitempty"`
	Value2 string `json:"value2,omitempty"` // 介于：上限
	// Or 和前一个条件是「或」（默认「且」；第一个条件不看它）
	Or bool `json:"or,omitempty"`
	// Off 暂时不用这个条件（界面上取消勾选）
	Off bool `json:"off,omitempty"`
}

// Sort 按一列排序。
type Sort struct {
	Col  string `json:"col"`
	Desc bool   `json:"desc,omitempty"`
}

// 支持的运算。值都作为参数传给数据库（不拼进 SQL）。
var filterOps = map[string]bool{
	"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true,
	"contains": true, "not-contains": true, "starts": true, "ends": true,
	"null": true, "not-null": true, "empty": true, "not-empty": true,
	"in": true, "not-in": true, "between": true, "not-between": true,
}

// BrowseOptions 翻页、排序、筛选。
type BrowseOptions struct {
	Where   string   // 用户直接写的条件（不含 WHERE）；和 Filters 同时有时用 AND 连起来
	Filters []Filter // 筛选
	Sorts   []Sort   // 排序（先按第一列）
	Offset  int
	Limit   int
}

// placeholders 生成参数占位符：MySQL 是 ?，PostgreSQL 是 $1、$2……
type placeholders struct {
	kind string
	args []any
}

func (p *placeholders) add(v any) string {
	p.args = append(p.args, v)
	if p.kind == MySQL {
		return "?"
	}
	return fmt.Sprintf("$%d", len(p.args))
}

// likeEscape 「包含」等运算里，值里的 % _ 和转义符按原样匹配。
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// WhereSQL 筛选条件 → WHERE 后面的部分和参数。没有条件时返回空串。
func WhereSQL(kind, raw string, filters []Filter) (string, []any, error) {
	p := &placeholders{kind: kind}
	var b strings.Builder
	n := 0
	for _, f := range filters {
		if f.Off {
			continue
		}
		if strings.TrimSpace(f.Col) == "" {
			return "", nil, errors.New("筛选条件没有选列")
		}
		if !filterOps[f.Op] {
			return "", nil, fmt.Errorf("不认识的筛选运算 %q", f.Op)
		}
		col := Quote(kind, f.Col)
		// PostgreSQL 的 LIKE 只能用在文本上：其他类型的列先转成文本
		text := col
		if kind == Postgres {
			text = "CAST(" + col + " AS TEXT)"
		}
		esc := ""
		if kind == MySQL {
			esc = ` ESCAPE '\\'`
		} else {
			esc = ` ESCAPE '\'`
		}
		var cond string
		switch f.Op {
		case "=", "<>", "<", "<=", ">", ">=":
			cond = col + " " + f.Op + " " + p.add(f.Value)
		case "contains":
			cond = text + " LIKE " + p.add("%"+likeEscape(f.Value)+"%") + esc
		case "not-contains":
			cond = text + " NOT LIKE " + p.add("%"+likeEscape(f.Value)+"%") + esc
		case "starts":
			cond = text + " LIKE " + p.add(likeEscape(f.Value)+"%") + esc
		case "ends":
			cond = text + " LIKE " + p.add("%"+likeEscape(f.Value)) + esc
		case "null":
			cond = col + " IS NULL"
		case "not-null":
			cond = col + " IS NOT NULL"
		case "empty":
			cond = "(" + col + " IS NULL OR " + text + " = '')"
		case "not-empty":
			cond = "(" + col + " IS NOT NULL AND " + text + " <> '')"
		case "in", "not-in":
			var items []string
			for _, v := range strings.Split(f.Value, ",") {
				if v = strings.TrimSpace(v); v != "" {
					items = append(items, p.add(v))
				}
			}
			if len(items) == 0 {
				return "", nil, fmt.Errorf("列 %s 的「在列表中」没有填值（多个值用逗号隔开）", f.Col)
			}
			word := " IN ("
			if f.Op == "not-in" {
				word = " NOT IN ("
			}
			cond = col + word + strings.Join(items, ", ") + ")"
		case "between", "not-between":
			word := " BETWEEN "
			if f.Op == "not-between" {
				word = " NOT BETWEEN "
			}
			cond = col + word + p.add(f.Value) + " AND " + p.add(f.Value2)
		}
		if n > 0 {
			if f.Or {
				b.WriteString(" OR ")
			} else {
				b.WriteString(" AND ")
			}
		}
		b.WriteString(cond)
		n++
	}
	built := b.String()
	raw = strings.TrimSpace(raw)
	if raw != "" && strings.Contains(raw, ";") && len(Split(kind, raw)) > 1 {
		return "", nil, errors.New("筛选条件里不能有分号")
	}
	switch {
	case raw != "" && built != "":
		return "(" + raw + ") AND (" + built + ")", p.args, nil
	case raw != "":
		return raw, nil, nil
	}
	return built, p.args, nil
}

// OrderSQL 排序 → ORDER BY 后面的部分。
func OrderSQL(kind string, sorts []Sort) string {
	var parts []string
	for _, s := range sorts {
		if strings.TrimSpace(s.Col) == "" {
			continue
		}
		p := Quote(kind, s.Col)
		if s.Desc {
			p += " DESC"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// selectSQL 读表数据的查询（带条件、排序）。
func selectSQL(kind, dbName, schema, table string, o BrowseOptions) (string, []any, error) {
	q := "SELECT * FROM " + TableName(kind, dbName, schema, table)
	w, args, err := WhereSQL(kind, o.Where, o.Filters)
	if err != nil {
		return "", nil, err
	}
	if w != "" {
		q += " WHERE " + w
	}
	if ob := OrderSQL(kind, o.Sorts); ob != "" {
		q += " ORDER BY " + ob
	}
	return q, args, nil
}

// Rows 读表数据的一页。
func (cl *Client) Rows(ctx context.Context, dbName, schema, table string, o BrowseOptions) (*Result, error) {
	q, args, err := selectSQL(cl.Conn.Kind, dbName, schema, table, o)
	if err != nil {
		return nil, err
	}
	if o.Limit <= 0 || o.Limit > 10000 {
		o.Limit = 200
	}
	page := fmt.Sprintf(" LIMIT %d", o.Limit+1)
	if o.Offset > 0 {
		page += fmt.Sprintf(" OFFSET %d", o.Offset)
	}
	r, err := cl.readQuery(ctx, dbName, q+page, args, o.Limit)
	if err != nil {
		return nil, err
	}
	// 给「在查询里打开」用：参数代入后的语句，LIMIT 是一页的行数
	r.SQL = Stmt{SQL: q, Args: args}.Preview(cl.Conn.Kind) + fmt.Sprintf(" LIMIT %d", o.Limit)
	return r, nil
}

// Count 表（按条件）的行数。
func (cl *Client) Count(ctx context.Context, dbName, schema, table string, o BrowseOptions) (int64, error) {
	kind := cl.Conn.Kind
	q := "SELECT COUNT(*) FROM " + TableName(kind, dbName, schema, table)
	w, args, err := WhereSQL(kind, o.Where, o.Filters)
	if err != nil {
		return 0, err
	}
	if w != "" {
		q += " WHERE " + w
	}
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, cl.explain(err)
	}
	defer tx.Rollback()
	var n int64
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, cl.explain(err)
	}
	return n, nil
}

// readQuery 在只读事务里执行一条查询（表数据、筛选：用户写的条件不应该能改数据）。
func (cl *Client) readQuery(ctx context.Context, dbName, q string, args []any, max int) (*Result, error) {
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
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer rows.Close()
	cols, data, more, err := readRows(rows, max)
	if err != nil {
		return nil, cl.explain(err)
	}
	return &Result{SQL: q, Columns: cols, Rows: data, More: more, Ms: time.Since(start).Milliseconds()}, nil
}
