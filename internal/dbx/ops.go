package dbx

import (
	"errors"
	"fmt"
	"strings"
)

// OpSQL 右键菜单里的操作 → 要执行的语句（先给用户看，确认后用 Exec 执行）。
//
//	create-db <名称> [charset]   drop-db <名称>
//	drop-table <表>  truncate <表>  drop-view <视图>  create-schema <名称>  rename <原名> <新名> [view]
func OpSQL(kind, db, schema, op string, args []string) ([]string, error) {
	need := func(n int) error {
		if len(args) < n || strings.TrimSpace(args[0]) == "" {
			return errors.New("缺少名称")
		}
		return nil
	}
	switch op {
	case "create-db":
		if err := need(1); err != nil {
			return nil, err
		}
		s := "CREATE DATABASE " + Quote(kind, args[0])
		if kind == MySQL {
			cs := "utf8mb4"
			if len(args) > 1 && args[1] != "" {
				cs = args[1]
			}
			if strings.ContainsAny(cs, " ;'\"`") {
				return nil, errors.New("字符集不合法")
			}
			s += " DEFAULT CHARACTER SET " + cs
		} else if len(args) > 1 && args[1] != "" {
			s += " ENCODING " + Literal(kind, args[1])
		}
		return []string{s}, nil
	case "drop-db":
		if err := need(1); err != nil {
			return nil, err
		}
		return []string{"DROP DATABASE " + Quote(kind, args[0])}, nil
	case "create-schema":
		if kind != Postgres {
			return nil, errors.New("MySQL 没有模式（schema），请新建库")
		}
		if err := need(1); err != nil {
			return nil, err
		}
		return []string{"CREATE SCHEMA " + Quote(kind, args[0])}, nil
	case "drop-table":
		if err := need(1); err != nil {
			return nil, err
		}
		return []string{"DROP TABLE " + TableName(kind, db, schema, args[0])}, nil
	case "drop-view":
		if err := need(1); err != nil {
			return nil, err
		}
		return []string{"DROP VIEW " + TableName(kind, db, schema, args[0])}, nil
	case "rename": // rename <原名> <新名> [view]
		if len(args) < 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" {
			return nil, errors.New("缺少原名或新名")
		}
		from, to := args[0], strings.TrimSpace(args[1])
		if from == to {
			return nil, errors.New("新名和原名一样")
		}
		view := len(args) > 2 && args[2] == "view"
		if kind == MySQL {
			return []string{"RENAME TABLE " + TableName(kind, db, schema, from) + " TO " + TableName(kind, db, schema, to)}, nil
		}
		word := "TABLE"
		if view {
			word = "VIEW"
		}
		return []string{"ALTER " + word + " " + TableName(kind, db, schema, from) + " RENAME TO " + Quote(kind, to)}, nil
	case "truncate":
		if err := need(1); err != nil {
			return nil, err
		}
		return []string{"TRUNCATE TABLE " + TableName(kind, db, schema, args[0])}, nil
	}
	return nil, fmt.Errorf("不认识的操作 %q", op)
}

// exportText 导出时一个值的写法；sqlLit 为 true 时写成 SQL 常量。
func exportText(v any, typ string, sqlLit bool, kind string) string {
	if v == nil {
		if sqlLit {
			return "NULL"
		}
		return ""
	}
	if b, ok := v.([]byte); ok && IsBinaryType(typ) {
		if !sqlLit {
			return fmt.Sprintf("%x", b)
		}
		if kind == MySQL {
			if len(b) == 0 {
				return "''"
			}
			return fmt.Sprintf("0x%x", b)
		}
		return fmt.Sprintf("'\\x%x'::bytea", b)
	}
	var s string
	switch x := cell(v, typ).(type) {
	case string:
		s = x
	case Cut:
		// cell 截断了长文本：导出要完整的
		if b, ok := v.([]byte); ok {
			s = string(b)
		} else {
			s = fmt.Sprint(v)
		}
	case Binary:
		if b, ok := v.([]byte); ok {
			s = string(b)
		}
	default:
		s = fmt.Sprint(x)
	}
	if !sqlLit {
		return s
	}
	switch x := v.(type) {
	case int64, float64, float32:
		return s
	case bool:
		if kind == MySQL {
			if x {
				return "1"
			}
			return "0"
		}
		return s
	}
	return Literal(kind, s)
}
