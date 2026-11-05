package dbx

// 导出成 SQL 文件（像 mysqldump / pg_dump 的简化版），用 Ferry 的「执行 SQL 文件」、mysql、psql 都能导回去：
//   - 数据在同一个快照里读（一个可重复读的只读事务）：导出时有人在写，表之间也对得上；
//   - 多行一条 INSERT（导入快）；计算列不导出（导回去时数据库自己算）；
//   - MySQL：表、数据、视图（按依赖顺序）、存储过程和函数、触发器（数据之后，导入时不触发）；先关外键检查；
//   - PostgreSQL：枚举类型、序列、函数、表、数据、序列的当前值、外键（数据之后加，表的先后不要紧）、视图、触发器。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DumpOptions 导出成 SQL 文件：表结构，或表结构和数据（像 Navicat 的「转储 SQL 文件」）。
type DumpOptions struct {
	Out    string
	Schema string   // PostgreSQL：只导出这个模式里的（Tables 为空时）
	Tables []string // 为空时导出库（模式）里全部的表和视图，还有函数、存储过程、类型
	Data   bool     // 也导出数据（INSERT）
}

// definer MySQL 视图、触发器、存储过程定义里的 DEFINER=`用户`@`主机`：导到别的服务器上时那个用户不一定有
var definer = regexp.MustCompile("DEFINER=`[^`]*`@`[^`]*` ")

// 多行一条 INSERT：最多这么多行、这么长
const (
	insertRows  = 500
	insertBytes = 1 << 20
)

type dumper struct {
	cl       *Client
	ctx      context.Context
	tx       *sql.Tx
	w        io.Writer
	kind     string
	db       string
	whole    bool // 导出整个库（模式）：也导出函数、存储过程、类型
	rows     int64
	progress func(string)
	notes    []string // 导不了的（权限不够之类），写在文件末尾
}

func (d *dumper) printf(format string, a ...any) { fmt.Fprintf(d.w, format, a...) }

func (d *dumper) section(what, name string) {
	d.printf("-- ----------------------------\n-- %s %s\n-- ----------------------------\n", what, name)
}

// Dump 导出结构（和数据）。
func (cl *Client) Dump(ctx context.Context, dbName string, o DumpOptions, progress func(msg string)) (*ExportResult, error) {
	kind := cl.Conn.Kind
	start := time.Now()
	all, err := cl.Tables(ctx, dbName)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range o.Tables {
		want[t] = true
	}
	var tables, views []*TableInfo
	for _, t := range all {
		if kind == Postgres && o.Schema != "" && t.Schema != o.Schema {
			continue
		}
		if len(want) > 0 && !want[t.Name] {
			continue
		}
		ti, err := cl.TableInfo(ctx, dbName, t.Schema, t.Name)
		if err != nil {
			return nil, err
		}
		if t.Type == "view" {
			views = append(views, ti)
		} else {
			tables = append(tables, ti)
		}
	}
	if len(tables)+len(views) == 0 {
		return nil, errors.New("没有要导出的表")
	}
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer conn.Close()
	// 同一个快照：可重复读的只读事务，第一次读之后看到的都是那一刻的数据
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, cl.explain(err)
	}
	defer tx.Rollback()
	of, err := createOut(o.Out)
	if err != nil {
		return nil, err
	}
	d := &dumper{cl: cl, ctx: ctx, tx: tx, w: of.w, kind: kind, db: dbName, whole: len(want) == 0, progress: progress}
	what := "结构"
	if o.Data {
		what = "结构和数据"
	}
	d.printf("-- Ferry 导出的%s\n-- 连接：%s（%s）\n-- 库：%s\n-- 时间：%s\n-- 导入：Ferry「数据库」里右键库 →「执行 SQL 文件」，或者 mysql / psql\n\n",
		what, cl.Conn.Name, cl.Version, dbName, time.Now().Format("2006-01-02 15:04:05"))
	if kind == MySQL {
		err = d.mysql(tables, views, o.Data)
	} else {
		err = d.pg(tables, views, o.Data)
	}
	if err != nil {
		of.abort()
		return nil, cl.explain(err)
	}
	if len(d.notes) > 0 {
		d.printf("\n-- 没导出的：\n")
		for _, n := range d.notes {
			d.printf("--   %s\n", n)
		}
	}
	size, err := of.finish()
	if err != nil {
		return nil, err
	}
	return &ExportResult{Rows: d.rows, Tables: len(tables) + len(views), Bytes: size, Ms: time.Since(start).Milliseconds(), Out: o.Out}, nil
}

// ---------------------------------------------------------------- MySQL

func (d *dumper) mysql(tables, views []*TableInfo, data bool) error {
	d.printf("SET NAMES utf8mb4;\nSET FOREIGN_KEY_CHECKS = 0;\n\n")
	for _, ti := range tables {
		name := Quote(MySQL, ti.Name)
		d.section("表", ti.Name)
		d.printf("DROP TABLE IF EXISTS %s;\n%s;\n\n", name, strings.TrimRight(strings.TrimSpace(ti.DDL), ";"))
		if data {
			if err := d.data(ti, name, ""); err != nil {
				return err
			}
		}
	}
	for _, ti := range viewOrder(MySQL, views) {
		d.section("视图", ti.Name)
		ddl := definer.ReplaceAllString(strings.TrimRight(strings.TrimSpace(ti.DDL), ";"), "")
		d.printf("DROP VIEW IF EXISTS %s;\n%s;\n\n", Quote(MySQL, ti.Name), ddl)
	}
	if d.whole {
		if err := d.mysqlRoutines(); err != nil {
			return err
		}
	}
	if err := d.mysqlTriggers(tables); err != nil {
		return err
	}
	d.printf("SET FOREIGN_KEY_CHECKS = 1;\n")
	return nil
}

// mysqlRoutines 存储过程和函数。
func (d *dumper) mysqlRoutines() error {
	rows, err := d.tx.QueryContext(d.ctx, `SELECT ROUTINE_NAME, ROUTINE_TYPE FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ? ORDER BY ROUTINE_TYPE, ROUTINE_NAME`, d.db)
	if err != nil {
		return err
	}
	type routine struct{ name, typ string }
	var rs []routine
	for rows.Next() {
		var r routine
		if err := rows.Scan(&r.name, &r.typ); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	for _, r := range rs {
		col := "Create Procedure"
		if r.typ == "FUNCTION" {
			col = "Create Function"
		}
		def, err := d.showCreate("SHOW CREATE "+r.typ+" "+Quote(MySQL, r.name), col)
		if err != nil || def == "" {
			d.notes = append(d.notes, fmt.Sprintf("%s %s（看不到定义：要有这个库的权限）", r.typ, r.name))
			continue
		}
		d.section(map[string]string{"PROCEDURE": "存储过程", "FUNCTION": "函数"}[r.typ], r.name)
		d.printf("DROP %s IF EXISTS %s;\nDELIMITER ;;\n%s;;\nDELIMITER ;\n\n", r.typ, Quote(MySQL, r.name), definer.ReplaceAllString(def, ""))
	}
	return nil
}

// mysqlTriggers 导出的表上的触发器（放在数据之后：导入数据时不触发）。
func (d *dumper) mysqlTriggers(tables []*TableInfo) error {
	in := map[string]bool{}
	for _, ti := range tables {
		in[ti.Name] = true
	}
	rows, err := d.tx.QueryContext(d.ctx, `SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY EVENT_OBJECT_TABLE, ACTION_ORDER`, d.db)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			rows.Close()
			return err
		}
		if in[table] {
			names = append(names, name)
		}
	}
	rows.Close()
	for _, name := range names {
		def, err := d.showCreate("SHOW CREATE TRIGGER "+Quote(MySQL, name), "SQL Original Statement")
		if err != nil || def == "" {
			d.notes = append(d.notes, fmt.Sprintf("触发器 %s（看不到定义）", name))
			continue
		}
		d.section("触发器", name)
		d.printf("DROP TRIGGER IF EXISTS %s;\nDELIMITER ;;\n%s;;\nDELIMITER ;\n\n", Quote(MySQL, name), definer.ReplaceAllString(def, ""))
	}
	return nil
}

// showCreate SHOW CREATE … 的结果里叫 col 的那一列。
func (d *dumper) showCreate(q, col string) (string, error) {
	rows, err := d.tx.QueryContext(d.ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, data, _, err := readRows(rows, 1)
	if err != nil || len(data) == 0 {
		return "", err
	}
	for i, c := range cols {
		if strings.EqualFold(c.Name, col) {
			if s, ok := data[0][i].(string); ok {
				return s, nil
			}
			if c, ok := data[0][i].(Cut); ok {
				return "", fmt.Errorf("定义太长（%d 字节）", c.Size)
			}
		}
	}
	return "", nil
}

// ---------------------------------------------------------------- PostgreSQL

type pgSeq struct {
	schema, name, typ             string
	start, min, max, inc, cache   string
	cycle                         bool
	dep, ownSchema, ownTable, col string // dep：a = serial / OWNED BY，i = 自增列（identity），空 = 单独的序列
}

func (s pgSeq) reg() string { return TableName(Postgres, "", s.schema, s.name) }

func (d *dumper) pg(tables, views []*TableInfo, data bool) error {
	d.printf("SET client_encoding = 'UTF8';\nSET standard_conforming_strings = on;\nSET check_function_bodies = false;\nSET client_min_messages = warning;\n\n")
	schemas := []string{}
	seen := map[string]bool{}
	for _, ti := range append(append([]*TableInfo{}, tables...), views...) {
		if !seen[ti.Schema] {
			seen[ti.Schema] = true
			schemas = append(schemas, ti.Schema)
		}
	}
	sort.Strings(schemas)
	for _, s := range schemas {
		if s != "public" {
			d.printf("CREATE SCHEMA IF NOT EXISTS %s;\n", Quote(Postgres, s))
		}
	}
	// 先删掉旧的（视图、表；CASCADE 连带依赖它们的）
	d.printf("\n")
	for _, ti := range views {
		d.printf("DROP %s IF EXISTS %s CASCADE;\n", pgViewWord(ti), TableName(Postgres, "", ti.Schema, ti.Name))
	}
	for _, ti := range tables {
		d.printf("DROP TABLE IF EXISTS %s CASCADE;\n", TableName(Postgres, "", ti.Schema, ti.Name))
	}
	d.printf("\n")

	if d.whole {
		for _, s := range schemas {
			if err := d.pgEnums(s); err != nil {
				return err
			}
		}
	}
	seqs, err := d.pgSequences(schemas, tables)
	if err != nil {
		return err
	}
	for _, s := range seqs {
		if s.dep == "i" {
			continue // 自增列的序列由建表语句建
		}
		cycle := "NO CYCLE"
		if s.cycle {
			cycle = "CYCLE"
		}
		d.printf("CREATE SEQUENCE IF NOT EXISTS %s AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s;\n",
			s.reg(), s.typ, s.inc, s.min, s.max, s.start, s.cache, cycle)
	}
	if len(seqs) > 0 {
		d.printf("\n")
	}
	if d.whole {
		for _, s := range schemas {
			if err := d.pgFunctions(s); err != nil {
				return err
			}
		}
	}

	for _, ti := range tables {
		reg := TableName(Postgres, "", ti.Schema, ti.Name)
		d.section("表", ti.Schema+"."+ti.Name)
		noFK := *ti
		noFK.Foreign = nil // 外键在全部数据导完后再加
		d.printf("%s;\n\n", strings.TrimRight(strings.TrimSpace(pgDDL(&noFK, ti.checks)), ";"))
		if data {
			extra := ""
			for _, c := range ti.Columns {
				if strings.Contains(strings.ToUpper(c.Extra), "ALWAYS AS IDENTITY") {
					extra = "OVERRIDING SYSTEM VALUE "
				}
			}
			if err := d.data(ti, reg, extra); err != nil {
				return err
			}
		}
	}
	// 序列：归属哪一列，现在的值（导入后新插入的行接着编号，不和导入的撞）
	for _, s := range seqs {
		if s.dep == "a" {
			d.printf("ALTER SEQUENCE %s OWNED BY %s.%s;\n", s.reg(), TableName(Postgres, "", s.ownSchema, s.ownTable), Quote(Postgres, s.col))
		}
		var last string
		var called bool
		if err := d.tx.QueryRowContext(d.ctx, "SELECT last_value::text, is_called FROM "+s.reg()).Scan(&last, &called); err != nil {
			d.notes = append(d.notes, fmt.Sprintf("序列 %s 的当前值（%v）", s.name, err))
			continue
		}
		target := Literal(Postgres, s.reg())
		if s.dep == "i" {
			// 自增列的序列由建表语句新建，名字不一定和原来的一样（表改过名）：按表和列找
			target = fmt.Sprintf("pg_get_serial_sequence(%s, %s)", Literal(Postgres, TableName(Postgres, "", s.ownSchema, s.ownTable)), Literal(Postgres, s.col))
		}
		d.printf("SELECT pg_catalog.setval(%s, %s, %v);\n", target, last, called)
	}
	if len(seqs) > 0 {
		d.printf("\n")
	}
	for _, ti := range tables {
		for _, f := range ti.Foreign {
			d.printf("ALTER TABLE %s ADD CONSTRAINT %s %s;\n", TableName(Postgres, "", ti.Schema, ti.Name), Quote(Postgres, f.Name), f.Def)
		}
	}
	d.printf("\n")
	for _, ti := range viewOrder(Postgres, views) {
		d.section("视图", ti.Schema+"."+ti.Name)
		d.printf("%s;\n\n", strings.TrimRight(strings.TrimSpace(ti.DDL), ";"))
	}
	return d.pgTriggers(append(append([]*TableInfo{}, tables...), views...))
}

func pgViewWord(ti *TableInfo) string {
	if strings.HasPrefix(ti.DDL, "CREATE MATERIALIZED VIEW") {
		return "MATERIALIZED VIEW"
	}
	return "VIEW"
}

// pgEnums 枚举类型（已经有了就不建）。
func (d *dumper) pgEnums(schema string) error {
	rows, err := d.tx.QueryContext(d.ctx, `SELECT t.typname, string_agg(quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder)
		FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1 GROUP BY t.typname ORDER BY t.typname`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, labels string
		if err := rows.Scan(&name, &labels); err != nil {
			return err
		}
		d.printf("DO $$ BEGIN CREATE TYPE %s AS ENUM (%s); EXCEPTION WHEN duplicate_object THEN NULL; END $$;\n",
			TableName(Postgres, "", schema, name), labels)
	}
	return rows.Err()
}

// pgSequences 要导出的序列：单独的（导出整个模式时，或者导出的表的默认值用到的）、导出的表的 serial 列和自增列的。
func (d *dumper) pgSequences(schemas []string, tables []*TableInfo) ([]pgSeq, error) {
	in := map[string]bool{}
	defaults := ""
	for _, ti := range tables {
		in[ti.Schema+"."+ti.Name] = true
		for _, c := range ti.Columns {
			if c.Default != nil {
				defaults += *c.Default + "\n"
			}
		}
	}
	var out []pgSeq
	for _, schema := range schemas {
		rows, err := d.tx.QueryContext(d.ctx, `SELECT c.relname, format_type(s.seqtypid, NULL), s.seqstart::text, s.seqmin::text, s.seqmax::text,
				s.seqincrement::text, s.seqcache::text, s.seqcycle, COALESCE(dep.deptype::text, ''), COALESCE(tn.nspname, ''), COALESCE(t.relname, ''), COALESCE(a.attname, '')
			FROM pg_sequence s JOIN pg_class c ON c.oid = s.seqrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			LEFT JOIN pg_depend dep ON dep.objid = c.oid AND dep.classid = 'pg_class'::regclass AND dep.refclassid = 'pg_class'::regclass AND dep.deptype IN ('a', 'i')
			LEFT JOIN pg_class t ON t.oid = dep.refobjid LEFT JOIN pg_namespace tn ON tn.oid = t.relnamespace
			LEFT JOIN pg_attribute a ON a.attrelid = dep.refobjid AND a.attnum = dep.refobjsubid
			WHERE n.nspname = $1 ORDER BY c.relname`, schema)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			s := pgSeq{schema: schema}
			if err := rows.Scan(&s.name, &s.typ, &s.start, &s.min, &s.max, &s.inc, &s.cache, &s.cycle, &s.dep, &s.ownSchema, &s.ownTable, &s.col); err != nil {
				rows.Close()
				return nil, err
			}
			switch {
			case s.dep != "":
				if !in[s.ownSchema+"."+s.ownTable] {
					continue
				}
			case !d.whole && !strings.Contains(defaults, s.name):
				continue
			}
			out = append(out, s)
		}
		rows.Close()
	}
	return out, nil
}

// pgFunctions 模式里的函数和存储过程（不含扩展带来的）。
func (d *dumper) pgFunctions(schema string) error {
	q := `SELECT p.proname, pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.prokind IN ('f', 'p')
		AND NOT EXISTS (SELECT 1 FROM pg_depend e WHERE e.objid = p.oid AND e.deptype = 'e') ORDER BY p.proname`
	rows, err := d.tx.QueryContext(d.ctx, q, schema)
	if err != nil {
		d.notes = append(d.notes, fmt.Sprintf("模式 %s 的函数（%v）", schema, err))
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return err
		}
		d.section("函数", schema+"."+name)
		d.printf("%s;\n\n", strings.TrimRight(strings.TrimSpace(def), ";"))
	}
	return rows.Err()
}

// pgTriggers 表和视图上的触发器（数据之后：导入时不触发）。
func (d *dumper) pgTriggers(objs []*TableInfo) error {
	for _, ti := range objs {
		rows, err := d.tx.QueryContext(d.ctx, `SELECT pg_get_triggerdef(oid, true) FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal ORDER BY tgname`,
			TableName(Postgres, "", ti.Schema, ti.Name))
		if err != nil {
			return err
		}
		for rows.Next() {
			var def string
			if err := rows.Scan(&def); err != nil {
				rows.Close()
				return err
			}
			d.printf("%s;\n", def)
		}
		rows.Close()
	}
	return nil
}

// ---------------------------------------------------------------- 共用

// data 一张表的数据：多行一条 INSERT，按主键排序（导回去时顺序稳定）；计算列不导出。
func (d *dumper) data(ti *TableInfo, name, extra string) error {
	var cols []string
	for _, c := range ti.Columns {
		if !c.Generated {
			cols = append(cols, Quote(d.kind, c.Name))
		}
	}
	if len(cols) == 0 {
		return nil
	}
	if d.progress != nil {
		d.progress("正在导出 " + ti.Name + " 的数据")
	}
	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + TableName(d.kind, d.db, ti.Schema, ti.Name)
	if len(ti.PrimaryKey) > 0 {
		var ks []Sort
		for _, k := range ti.PrimaryKey {
			ks = append(ks, Sort{Col: k})
		}
		q += " ORDER BY " + OrderSQL(d.kind, ks)
	}
	n, err := d.cl.writeInserts(d.ctx, d.tx, d.w, q, nil, name, extra, func(n int64) {
		if d.progress != nil && n%100000 == 0 {
			d.progress(fmt.Sprintf("正在导出 %s 的数据：%d 行", ti.Name, n))
		}
	})
	if err != nil {
		return fmt.Errorf("导出 %s 的数据：%w", ti.Name, err)
	}
	if n > 0 {
		d.printf("\n")
	}
	d.rows += n
	return nil
}

// writeInserts 把查询结果写成多行一条的 INSERT；table 是已经加好引号的表名，extra 加在 VALUES 前（OVERRIDING SYSTEM VALUE）。
func (cl *Client) writeInserts(ctx context.Context, q querier, w io.Writer, query string, args []any, table, extra string, progress func(int64)) (int64, error) {
	kind := cl.Conn.Kind
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	names, types, vals, ptrs, err := scanAll(rows)
	if err != nil {
		return 0, err
	}
	cols := make([]string, len(names))
	for i, c := range names {
		cols[i] = Quote(kind, c)
	}
	head := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") " + extra + "VALUES\n"
	lits := make([]string, len(names))
	var batch []string
	size := 0
	flush := func() {
		if len(batch) > 0 {
			io.WriteString(w, head+strings.Join(batch, ",\n")+";\n")
			batch, size = batch[:0], 0
		}
	}
	var n int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, v := range vals {
			lits[i] = exportText(v, types[i], true, kind)
		}
		row := "(" + strings.Join(lits, ", ") + ")"
		batch = append(batch, row)
		size += len(row)
		if len(batch) >= insertRows || size >= insertBytes {
			flush()
		}
		n++
		if progress != nil && n%10000 == 0 {
			progress(n)
		}
	}
	flush()
	return n, rows.Err()
}

// mentions 定义里有没有这个名字（整个词：前后不是字母、数字、下划线）。
func mentions(body, name string) bool {
	for i := 0; ; {
		j := strings.Index(body[i:], name)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(name)
		if (j == 0 || !isWordByte(body[j-1])) && (end == len(body) || !isWordByte(body[end])) {
			return true
		}
		i = j + 1
	}
}

// viewOrder 视图按依赖排：用到别的视图的放在后面（看定义里有没有那个视图的名字，宁可多排也不漏）。
func viewOrder(kind string, views []*TableInfo) []*TableInfo {
	var out []*TableInfo
	done := map[*TableInfo]bool{}
	visiting := map[*TableInfo]bool{}
	var visit func(v *TableInfo)
	visit = func(v *TableInfo) {
		if done[v] || visiting[v] {
			return
		}
		visiting[v] = true
		body := v.DDL
		if i := strings.Index(strings.ToUpper(body), " AS"); i >= 0 {
			body = body[i:]
		}
		for _, o := range views {
			if o != v && mentions(body, o.Name) {
				visit(o)
			}
		}
		visiting[v] = false
		done[v] = true
		out = append(out, v)
	}
	for _, v := range views {
		visit(v)
	}
	return out
}
