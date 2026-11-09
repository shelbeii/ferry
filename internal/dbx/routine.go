package dbx

// 存储过程和函数：列表、看定义、改（保存）。删除走 OpSQL（drop-function、drop-procedure）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Routine 一个函数或存储过程。
type Routine struct {
	Schema   string `json:"schema,omitempty"` // PostgreSQL 的模式
	Name     string `json:"name"`
	Type     string `json:"type"`           // function | procedure
	Args     string `json:"args,omitempty"` // 参数：MySQL「IN a INT, OUT b VARCHAR(10)」；PostgreSQL 是区分重载用的参数表
	Returns  string `json:"returns,omitempty"`
	Language string `json:"language,omitempty"` // PostgreSQL：plpgsql、sql……
	Created  string `json:"created,omitempty"`
	Updated  string `json:"updated,omitempty"`
	Comment  string `json:"comment,omitempty"`
	OID      uint32 `json:"oid,omitempty"` // PostgreSQL：重载的函数同名，靠它找定义
}

// RoutineRef 指定一个函数或存储过程（看定义、改）。
type RoutineRef struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Args   string `json:"args,omitempty"`
	OID    uint32 `json:"oid,omitempty"`
}

// Routines 列出库（PostgreSQL 是全部用户模式）里的函数和存储过程。
func (cl *Client) Routines(ctx context.Context, dbName string) ([]Routine, error) {
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	out := []Routine{}
	if cl.Conn.Kind == MySQL {
		rows, err := db.QueryContext(ctx, `SELECT ROUTINE_NAME, ROUTINE_TYPE, IFNULL(DTD_IDENTIFIER, ''),
				IFNULL(DATE_FORMAT(CREATED, '%Y-%m-%d %H:%i:%s'), ''), IFNULL(DATE_FORMAT(LAST_ALTERED, '%Y-%m-%d %H:%i:%s'), ''), IFNULL(ROUTINE_COMMENT, '')
			FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? ORDER BY ROUTINE_NAME`, dbName)
		if err != nil {
			return nil, cl.explain(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r Routine
			if err := rows.Scan(&r.Name, &r.Type, &r.Returns, &r.Created, &r.Updated, &r.Comment); err != nil {
				return nil, err
			}
			r.Type = strings.ToLower(r.Type)
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
		// 参数
		prows, err := db.QueryContext(ctx, `SELECT SPECIFIC_NAME, ROUTINE_TYPE, IFNULL(PARAMETER_MODE, ''), IFNULL(PARAMETER_NAME, ''), IFNULL(DTD_IDENTIFIER, '')
			FROM information_schema.PARAMETERS WHERE SPECIFIC_SCHEMA = ? AND ORDINAL_POSITION > 0 ORDER BY SPECIFIC_NAME, ORDINAL_POSITION`, dbName)
		if err != nil {
			return out, nil // 参数只是显示用
		}
		defer prows.Close()
		args := map[string][]string{}
		for prows.Next() {
			var name, typ, mode, pname, dtd string
			if err := prows.Scan(&name, &typ, &mode, &pname, &dtd); err != nil {
				return out, nil
			}
			if strings.EqualFold(typ, "FUNCTION") {
				mode = "" // 函数的参数只能是输入的（MySQL 8 也写成 IN）
			}
			p := strings.TrimSpace(mode + " " + pname + " " + dtd)
			args[strings.ToLower(typ)+" "+name] = append(args[strings.ToLower(typ)+" "+name], p)
		}
		for i := range out {
			out[i].Args = strings.Join(args[out[i].Type+" "+out[i].Name], ", ")
		}
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT n.nspname, p.proname, CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END,
			pg_get_function_identity_arguments(p.oid), COALESCE(pg_get_function_result(p.oid), ''), l.lanname,
			COALESCE(obj_description(p.oid, 'pg_proc'), ''), p.oid::bigint
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace JOIN pg_language l ON l.oid = p.prolang
		WHERE p.prokind IN ('f', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
			AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'
			AND NOT EXISTS (SELECT 1 FROM pg_depend e WHERE e.objid = p.oid AND e.deptype = 'e')
		ORDER BY n.nspname <> 'public', n.nspname, p.proname, 4`)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Routine
		var oid int64
		if err := rows.Scan(&r.Schema, &r.Name, &r.Type, &r.Args, &r.Returns, &r.Language, &r.Comment, &oid); err != nil {
			return nil, err
		}
		r.OID = uint32(oid)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoutineDef 函数或存储过程的定义（CREATE 语句；MySQL 去掉了 DEFINER）。
func (cl *Client) RoutineDef(ctx context.Context, dbName string, r RoutineRef) (string, error) {
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return "", err
	}
	if cl.Conn.Kind == MySQL {
		def, err := mysqlRoutineDef(ctx, db, dbName, r)
		if err != nil {
			return "", err
		}
		return definer.ReplaceAllString(def, ""), nil
	}
	if r.OID == 0 {
		return "", errors.New("没有指定是哪个函数")
	}
	var def sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT pg_get_functiondef($1::oid)`, int64(r.OID)).Scan(&def); err != nil {
		return "", cl.explain(err)
	}
	if !def.Valid {
		return "", fmt.Errorf("没有找到 %s（可能已经删了，刷新一下）", r.Name)
	}
	return strings.TrimSpace(def.String), nil
}

// mysqlRoutineDef SHOW CREATE FUNCTION / PROCEDURE 里的定义（带 DEFINER）。
func mysqlRoutineDef(ctx context.Context, db *sql.DB, dbName string, r RoutineRef) (string, error) {
	word, col := "PROCEDURE", "Create Procedure"
	if r.Type == "function" {
		word, col = "FUNCTION", "Create Function"
	}
	rows, err := db.QueryContext(ctx, "SHOW CREATE "+word+" "+TableName(MySQL, dbName, "", r.Name))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("没有找到 %s（可能已经删了，刷新一下）", r.Name)
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	for i, c := range cols {
		if c == col {
			if !vals[i].Valid || vals[i].String == "" {
				return "", errors.New("看不到定义：要是这个函数的创建者，或者有这个库的 SELECT 权限（MySQL 8 要 SHOW_ROUTINE）")
			}
			return vals[i].String, nil
		}
	}
	return "", errors.New("看不到定义")
}

// routineHead 保存的语句只能是建函数、存储过程的（前面可以有注释）。
var routineHead = regexp.MustCompile(`(?is)^\s*(?:(?:--[^\n]*(?:\n|$)|/\*.*?\*/)\s*)*CREATE\s+(?:OR\s+REPLACE\s+)?(?:DEFINER\s*=\s*\S+\s+)?(FUNCTION|PROCEDURE)\s`)

// RoutineSQL 整理要保存的定义：去掉末尾的分号（MySQL 一次只能发一条），不能有 DELIMITER。
func RoutineSQL(text string) (string, error) {
	s := strings.TrimSpace(text)
	for strings.HasSuffix(s, ";") {
		s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	}
	if s == "" {
		return "", errors.New("定义是空的")
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "DELIMITER") {
			return "", errors.New("这里不用写 DELIMITER：整段作为一条语句发给数据库")
		}
	}
	if !routineHead.MatchString(s) {
		return "", errors.New("这里只能写 CREATE FUNCTION 或 CREATE PROCEDURE（建函数、存储过程的语句）")
	}
	return s, nil
}

// SaveRoutine 保存函数或存储过程：old 为空是新建。
// MySQL 不能直接替换：先删掉原来的再建，建不成时把原来的建回去。PostgreSQL 用 CREATE OR REPLACE（参数变了是另一个函数，原来的还在）。
func (cl *Client) SaveRoutine(ctx context.Context, dbName string, old *RoutineRef, text string) error {
	if cl.Conn.ReadOnly {
		return ErrReadOnly
	}
	s, err := RoutineSQL(text)
	if err != nil {
		return err
	}
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return cl.explain(err)
	}
	defer conn.Close()
	if cl.Conn.Kind != MySQL || old == nil {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			return cl.explain(err)
		}
		return nil
	}
	prev, err := mysqlRoutineDef(ctx, db, dbName, *old)
	if err != nil {
		return err
	}
	word := "PROCEDURE"
	if old.Type == "function" {
		word = "FUNCTION"
	}
	if _, err := conn.ExecContext(ctx, "DROP "+word+" IF EXISTS "+TableName(MySQL, dbName, "", old.Name)); err != nil {
		return cl.explain(err)
	}
	if _, err := conn.ExecContext(ctx, s); err != nil {
		err = cl.explain(err)
		// 建回原来的：先按原样（带 DEFINER），没权限时去掉 DEFINER（建成当前用户的）
		if _, e2 := conn.ExecContext(ctx, prev); e2 != nil {
			if _, e3 := conn.ExecContext(ctx, definer.ReplaceAllString(prev, "")); e3 != nil {
				return fmt.Errorf("%w\n\n原来的 %s 已经删了，也没能建回去（%v）。原来的定义：\n%s", err, old.Name, e3, prev)
			}
		}
		return fmt.Errorf("%w\n\n没有保存，原来的 %s 还在。", err, old.Name)
	}
	return nil
}
