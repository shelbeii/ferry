package dbx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Change 表格里改的一行。
type Change struct {
	Op string `json:"op"` // update | insert | delete
	// Key 定位这一行的列（TableInfo.Key）原来的值；nil 值为 NULL
	Key map[string]*string `json:"key,omitempty"`
	// Values 改后的值（update 只含改了的列，insert 只含填了的列，没填的用默认值）；nil 值为 NULL
	Values map[string]*string `json:"values,omitempty"`
}

// Stmt 一条带参数的语句。
type Stmt struct {
	SQL  string
	Args []any
	One  bool // 必须正好影响一行
}

// Preview 把参数代入后的语句（给用户看；执行时仍用参数）。
func (s Stmt) Preview(kind string) string {
	if len(s.Args) == 0 {
		return s.SQL
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(s.SQL); i++ {
		ch := s.SQL[i]
		if kind == MySQL && ch == '?' && n < len(s.Args) {
			b.WriteString(literalArg(kind, s.Args[n]))
			n++
			continue
		}
		if kind == Postgres && ch == '$' && i+1 < len(s.SQL) && s.SQL[i+1] >= '1' && s.SQL[i+1] <= '9' {
			j := i + 1
			for j < len(s.SQL) && s.SQL[j] >= '0' && s.SQL[j] <= '9' {
				j++
			}
			var k int
			fmt.Sscan(s.SQL[i+1:j], &k)
			if k >= 1 && k <= len(s.Args) {
				b.WriteString(literalArg(kind, s.Args[k-1]))
				i = j - 1
				continue
			}
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func literalArg(kind string, a any) string {
	if a == nil {
		return "NULL"
	}
	return Literal(kind, fmt.Sprint(a))
}

func arg(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// ChangeSQL 改动 → 语句。
func ChangeSQL(kind, db string, ti *TableInfo, changes []Change) ([]Stmt, error) {
	cols := map[string]ColumnInfo{}
	for _, c := range ti.Columns {
		cols[c.Name] = c
	}
	table := TableName(kind, db, ti.Schema, ti.Name)
	ph := func(n int) string {
		if kind == MySQL {
			return "?"
		}
		return fmt.Sprintf("$%d", n)
	}
	where := func(c Change, args []any) (string, []any, error) {
		if len(ti.Key) == 0 {
			return "", nil, fmt.Errorf("表 %s 没有主键，不能直接改数据（可以在查询里写 UPDATE）", ti.Name)
		}
		var conds []string
		for _, k := range ti.Key {
			v, ok := c.Key[k]
			if !ok {
				return "", nil, fmt.Errorf("缺少主键列 %s 的值", k)
			}
			if v == nil {
				conds = append(conds, Quote(kind, k)+" IS NULL")
				continue
			}
			args = append(args, *v)
			conds = append(conds, Quote(kind, k)+" = "+ph(len(args)))
		}
		return strings.Join(conds, " AND "), args, nil
	}
	sortedKeys := func(m map[string]*string) ([]string, error) {
		ks := make([]string, 0, len(m))
		for k := range m {
			if _, ok := cols[k]; !ok {
				return nil, fmt.Errorf("表 %s 没有列 %s", ti.Name, k)
			}
			if cols[k].Generated {
				return nil, fmt.Errorf("列 %s 是计算列，不能改", k)
			}
			ks = append(ks, k)
		}
		// 按表里的列顺序
		pos := map[string]int{}
		for i, c := range ti.Columns {
			pos[c.Name] = i
		}
		sort.Slice(ks, func(i, j int) bool { return pos[ks[i]] < pos[ks[j]] })
		return ks, nil
	}
	var out []Stmt
	for _, c := range changes {
		switch c.Op {
		case "update":
			ks, err := sortedKeys(c.Values)
			if err != nil {
				return nil, err
			}
			if len(ks) == 0 {
				continue
			}
			var sets []string
			var args []any
			for _, k := range ks {
				args = append(args, arg(c.Values[k]))
				sets = append(sets, Quote(kind, k)+" = "+ph(len(args)))
			}
			w, args, err := where(c, args)
			if err != nil {
				return nil, err
			}
			out = append(out, Stmt{SQL: "UPDATE " + table + " SET " + strings.Join(sets, ", ") + " WHERE " + w, Args: args, One: true})
		case "insert":
			ks, err := sortedKeys(c.Values)
			if err != nil {
				return nil, err
			}
			if len(ks) == 0 {
				if kind == MySQL {
					out = append(out, Stmt{SQL: "INSERT INTO " + table + " () VALUES ()", One: true})
				} else {
					out = append(out, Stmt{SQL: "INSERT INTO " + table + " DEFAULT VALUES", One: true})
				}
				continue
			}
			var names, vals []string
			var args []any
			for _, k := range ks {
				args = append(args, arg(c.Values[k]))
				names = append(names, Quote(kind, k))
				vals = append(vals, ph(len(args)))
			}
			out = append(out, Stmt{SQL: "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")", Args: args, One: true})
		case "delete":
			w, args, err := where(c, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, Stmt{SQL: "DELETE FROM " + table + " WHERE " + w, Args: args, One: true})
		default:
			return nil, fmt.Errorf("不认识的改动 %q", c.Op)
		}
	}
	return out, nil
}

// ApplyResult 保存改动的结果。
type ApplyResult struct {
	SQL      []string `json:"sql"`
	Affected int64    `json:"affected"`
	Ms       int64    `json:"ms"`
}

// Apply 在一个事务里执行改动；有一条没正好改到一行就全部撤销。
func (cl *Client) Apply(ctx context.Context, dbName string, stmts []Stmt) (*ApplyResult, error) {
	if cl.Conn.ReadOnly {
		return nil, ErrReadOnly
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
	res := &ApplyResult{SQL: []string{}}
	for i, s := range stmts {
		r, err := tx.ExecContext(ctx, s.SQL, s.Args...)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条没执行成功（已全部撤销）：%w\n%s", i+1, cl.explain(err), s.Preview(cl.Conn.Kind))
		}
		n, _ := r.RowsAffected()
		if s.One && n != 1 {
			return nil, fmt.Errorf("第 %d 条改到了 %d 行，应该正好 1 行：这一行可能已经被别人改了或删了（已全部撤销）\n%s", i+1, n, s.Preview(cl.Conn.Kind))
		}
		res.Affected += n
		res.SQL = append(res.SQL, s.Preview(cl.Conn.Kind))
	}
	if err := tx.Commit(); err != nil {
		return nil, cl.explain(err)
	}
	res.Ms = time.Since(start).Milliseconds()
	return res, nil
}

// ErrReadOnly 只读连接。
var ErrReadOnly = errors.New("这个连接设成了只读，不能改数据（可以在连接设置里关掉只读）")
