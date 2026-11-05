package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Quote 标识符加引号：MySQL 用反引号，PostgreSQL 用双引号。
func Quote(kind, id string) string {
	if kind == MySQL {
		return "`" + strings.ReplaceAll(id, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// Literal 字符串常量（只用于预览、DDL 里的注释和默认值；查询的值用参数）。
func Literal(kind, s string) string {
	s = strings.ReplaceAll(s, "'", "''")
	if kind == MySQL {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + s + "'"
}

// TableName 带库（MySQL）或模式（PostgreSQL）的表名。
func TableName(kind, db, schema, table string) string {
	if kind == MySQL {
		if db == "" {
			return Quote(kind, table)
		}
		return Quote(kind, db) + "." + Quote(kind, table)
	}
	if schema == "" {
		schema = "public"
	}
	return Quote(kind, schema) + "." + Quote(kind, table)
}

// Database 一个库。
type Database struct {
	Name   string `json:"name"`
	System bool   `json:"system,omitempty"` // 系统库（mysql、information_schema……）
	Keys   int64  `json:"keys,omitempty"`   // Redis：键的个数
}

var mysqlSystem = map[string]bool{"mysql": true, "information_schema": true, "performance_schema": true, "sys": true}

// Databases 列出库。
func (cl *Client) Databases(ctx context.Context) ([]Database, error) {
	if cl.Conn.Kind == Redis {
		return cl.redisDatabases(ctx)
	}
	db, err := cl.DB(ctx, "")
	if err != nil {
		return nil, err
	}
	q := "SHOW DATABASES"
	if cl.Conn.Kind == Postgres {
		q = "SELECT datname FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY 1"
	}
	names, err := strings1(ctx, db, q)
	if err != nil {
		return nil, cl.explain(err)
	}
	out := []Database{}
	for _, n := range names {
		out = append(out, Database{Name: n, System: cl.Conn.Kind == MySQL && mysqlSystem[strings.ToLower(n)]})
	}
	return out, nil
}

func strings1(ctx context.Context, db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

// Table 列表里的一张表或视图。
type Table struct {
	Schema  string `json:"schema,omitempty"` // PostgreSQL 的模式
	Name    string `json:"name"`
	Type    string `json:"type"` // table | view
	Rows    *int64 `json:"rows,omitempty"`
	Size    *int64 `json:"size,omitempty"` // 字节
	Comment string `json:"comment,omitempty"`
	Engine  string `json:"engine,omitempty"`
}

// Tables 列出库里的表和视图（PostgreSQL 是全部用户模式里的）。
func (cl *Client) Tables(ctx context.Context, dbName string) ([]Table, error) {
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	var rows *sql.Rows
	if cl.Conn.Kind == MySQL {
		rows, err = db.QueryContext(ctx, `SELECT '', TABLE_NAME, TABLE_TYPE, TABLE_ROWS, DATA_LENGTH + INDEX_LENGTH, IFNULL(TABLE_COMMENT, ''), IFNULL(ENGINE, '')
			FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`, dbName)
	} else {
		rows, err = db.QueryContext(ctx, `SELECT n.nspname, c.relname, c.relkind::text,
				CASE WHEN c.relkind IN ('r','p','m') THEN GREATEST(c.reltuples, 0)::bigint END,
				CASE WHEN c.relkind IN ('r','p','m') THEN pg_total_relation_size(c.oid) END,
				COALESCE(obj_description(c.oid, 'pg_class'), ''), ''
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r','p','v','m','f') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
				AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%' AND NOT c.relispartition
			ORDER BY n.nspname <> 'public', n.nspname, c.relname`)
	}
	if err != nil {
		return nil, cl.explain(err)
	}
	defer rows.Close()
	out := []Table{}
	for rows.Next() {
		var t Table
		var typ string
		var n, size sql.NullInt64
		if err := rows.Scan(&t.Schema, &t.Name, &typ, &n, &size, &t.Comment, &t.Engine); err != nil {
			return nil, err
		}
		t.Type = "table"
		switch typ {
		case "VIEW", "SYSTEM VIEW", "v", "m":
			t.Type = "view"
		}
		if n.Valid {
			t.Rows = &n.Int64
		}
		if size.Valid {
			t.Size = &size.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ColumnInfo 表结构里的一列。
type ColumnInfo struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"` // 完整类型：varchar(255)、int unsigned、numeric(10,2)
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default"`         // SQL 表达式：'abc'、0、CURRENT_TIMESTAMP；nil = 没有默认值
	Auto     bool    `json:"auto,omitempty"`  // 自增（MySQL AUTO_INCREMENT、PostgreSQL identity / serial）
	Extra    string  `json:"extra,omitempty"` // MySQL：on update CURRENT_TIMESTAMP 等
	// Generated 计算列：不能改值，结构设计里不改它
	Generated bool   `json:"generated,omitempty"`
	Collation string `json:"collation,omitempty"`
	Comment   string `json:"comment,omitempty"`
	Key       bool   `json:"key,omitempty"` // 主键的一部分
}

// IndexInfo 一个索引。
type IndexInfo struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	Primary bool     `json:"primary,omitempty"`
	Method  string   `json:"method,omitempty"` // BTREE、HASH、FULLTEXT、gin……
	// Def 不是普通列的索引（表达式、部分索引）的完整定义：结构设计里只能整个删掉或留着
	Def string `json:"def,omitempty"`
}

// ForeignKey 外键。
type ForeignKey struct {
	Name string `json:"name"`
	Def  string `json:"def"` // FOREIGN KEY (a) REFERENCES t(b) ON DELETE …
}

// TableInfo 表结构。
type TableInfo struct {
	Schema     string       `json:"schema,omitempty"`
	Name       string       `json:"name"`
	Type       string       `json:"type"` // table | view
	Columns    []ColumnInfo `json:"columns"`
	Indexes    []IndexInfo  `json:"indexes"`
	Foreign    []ForeignKey `json:"foreign"`
	PrimaryKey []string     `json:"primaryKey"`
	// Key 改数据时用来定位一行的列（主键，没有时用不可为空的唯一索引）；为空时只能看不能改
	Key       []string `json:"key"`
	Comment   string   `json:"comment,omitempty"`
	Engine    string   `json:"engine,omitempty"`
	Collation string   `json:"collation,omitempty"`
	Charset   string   `json:"charset,omitempty"`
	PKName    string   `json:"pkName,omitempty"` // PostgreSQL 主键约束名
	DDL       string   `json:"ddl"`
}

// TableInfo 读表结构。
func (cl *Client) TableInfo(ctx context.Context, dbName, schema, table string) (*TableInfo, error) {
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	var ti *TableInfo
	if cl.Conn.Kind == MySQL {
		ti, err = mysqlTable(ctx, db, dbName, table, cl.isMariaDB())
	} else {
		ti, err = pgTable(ctx, db, schema, table)
	}
	if err != nil {
		return nil, cl.explain(err)
	}
	ti.Key = rowKey(ti)
	return ti, nil
}

func (cl *Client) isMariaDB() bool { return strings.Contains(strings.ToLower(cl.Version), "mariadb") }

// rowKey 定位一行用的列。
func rowKey(ti *TableInfo) []string {
	if ti.Type != "table" {
		return []string{}
	}
	ok := func(cols []string) bool {
		for _, c := range cols {
			found := false
			for _, ci := range ti.Columns {
				if ci.Name == c {
					found = true
					if ci.Nullable || IsBinaryType(baseType(ci.Type)) {
						return false
					}
				}
			}
			if !found {
				return false
			}
		}
		return len(cols) > 0
	}
	if ok(ti.PrimaryKey) {
		return ti.PrimaryKey
	}
	for _, ix := range ti.Indexes {
		if ix.Unique && ix.Def == "" && ok(ix.Columns) {
			return ix.Columns
		}
	}
	return []string{}
}

func baseType(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	if i := strings.IndexAny(t, "( "); i > 0 {
		t = t[:i]
	}
	return t
}

func mysqlTable(ctx context.Context, db *sql.DB, dbName, table string, maria bool) (*TableInfo, error) {
	ti := &TableInfo{Name: table, Type: "table", Columns: []ColumnInfo{}, Indexes: []IndexInfo{}, Foreign: []ForeignKey{}, PrimaryKey: []string{}}
	var typ string
	err := db.QueryRowContext(ctx, `SELECT TABLE_TYPE, IFNULL(TABLE_COMMENT, ''), IFNULL(ENGINE, ''), IFNULL(TABLE_COLLATION, '')
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, dbName, table).Scan(&typ, &ti.Comment, &ti.Engine, &ti.Collation)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("库 %s 里没有表 %s", dbName, table)
	}
	if err != nil {
		return nil, err
	}
	if strings.Contains(typ, "VIEW") {
		ti.Type = "view"
		ti.Comment = ""
	}
	if i := strings.Index(ti.Collation, "_"); i > 0 {
		ti.Charset = ti.Collation[:i]
	}
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, COLUMN_TYPE, DATA_TYPE, IS_NULLABLE, COLUMN_DEFAULT, IFNULL(EXTRA, ''), IFNULL(COLUMN_KEY, ''),
			IFNULL(COLUMN_COMMENT, ''), IFNULL(COLLATION_NAME, '')
		FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, dbName, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ColumnInfo
		var dataType, nullable, key string
		var def sql.NullString
		if err := rows.Scan(&c.Name, &c.Type, &dataType, &nullable, &def, &c.Extra, &key, &c.Comment, &c.Collation); err != nil {
			return nil, err
		}
		c.Nullable = nullable == "YES"
		extra := strings.ToLower(c.Extra)
		if strings.Contains(extra, "auto_increment") {
			c.Auto = true
		}
		if strings.Contains(extra, "generated") && !strings.Contains(extra, "default_generated") {
			c.Generated = true
		}
		c.Default = mysqlDefault(def, dataType, extra, maria)
		// Extra 里留下建列时还要带上的部分（on update …）
		ex := []string{}
		for _, p := range []string{"on update current_timestamp"} {
			if i := strings.Index(extra, p); i >= 0 {
				rest := c.Extra[i:]
				if j := strings.Index(strings.ToLower(rest[len(p):]), " "); j >= 0 {
					rest = rest[:len(p)+j]
				}
				ex = append(ex, rest)
			}
		}
		c.Extra = strings.Join(ex, " ")
		if c.Collation == ti.Collation {
			c.Collation = ""
		}
		c.Key = key == "PRI"
		ti.Columns = append(ti.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// 索引
	irows, err := db.QueryContext(ctx, `SELECT INDEX_NAME, NON_UNIQUE, IFNULL(COLUMN_NAME, ''), IFNULL(SUB_PART, 0), IFNULL(INDEX_TYPE, '')
		FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY INDEX_NAME = 'PRIMARY' DESC, INDEX_NAME, SEQ_IN_INDEX`, dbName, table)
	if err != nil {
		return nil, err
	}
	defer irows.Close()
	byName := map[string]*IndexInfo{}
	order := []string{}
	for irows.Next() {
		var name, col, method string
		var nonUnique, sub int
		if err := irows.Scan(&name, &nonUnique, &col, &sub, &method); err != nil {
			return nil, err
		}
		ix := byName[name]
		if ix == nil {
			ix = &IndexInfo{Name: name, Unique: nonUnique == 0, Primary: name == "PRIMARY", Method: method, Columns: []string{}}
			byName[name] = ix
			order = append(order, name)
		}
		if col == "" {
			ix.Def = "（表达式）"
		} else if sub > 0 {
			ix.Columns = append(ix.Columns, fmt.Sprintf("%s(%d)", col, sub))
		} else {
			ix.Columns = append(ix.Columns, col)
		}
	}
	if err := irows.Err(); err != nil {
		return nil, err
	}
	for _, n := range order {
		ix := byName[n]
		if ix.Primary {
			ti.PrimaryKey = ix.Columns
		}
		ti.Indexes = append(ti.Indexes, *ix)
	}

	// 外键：从建表语句里取（information_schema 里拼起来很麻烦）
	var name, ddl string
	if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE "+TableName(MySQL, dbName, "", table)).Scan(&name, &ddl); err != nil {
		// 视图的 SHOW CREATE TABLE 有 4 列
		var a, b string
		if err2 := db.QueryRowContext(ctx, "SHOW CREATE VIEW "+TableName(MySQL, dbName, "", table)).Scan(&name, &ddl, &a, &b); err2 != nil {
			return nil, err
		}
	}
	ti.DDL = ddl
	for _, line := range strings.Split(ddl, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if strings.HasPrefix(line, "CONSTRAINT ") && strings.Contains(line, "FOREIGN KEY") {
			rest := strings.TrimPrefix(line, "CONSTRAINT ")
			if i := strings.Index(rest, " FOREIGN KEY"); i > 0 {
				ti.Foreign = append(ti.Foreign, ForeignKey{Name: strings.Trim(rest[:i], "`"), Def: strings.TrimSpace(rest[i:])})
			}
		}
	}
	return ti, nil
}

var mysqlNumeric = map[string]bool{"tinyint": true, "smallint": true, "mediumint": true, "int": true, "integer": true, "bigint": true,
	"decimal": true, "numeric": true, "float": true, "double": true, "real": true, "bit": true, "year": true}

// mysqlDefault information_schema 里的默认值 → SQL 表达式。
// MySQL：字符串不带引号，表达式默认值的 EXTRA 有 DEFAULT_GENERATED；MariaDB：已经是 SQL 写法，没有默认值时是 NULL，默认 NULL 时是字符串 'NULL'。
func mysqlDefault(def sql.NullString, dataType, extra string, maria bool) *string {
	if !def.Valid {
		return nil
	}
	v := def.String
	if maria {
		return &v
	}
	lv := strings.ToLower(v)
	switch {
	case strings.Contains(extra, "default_generated"):
		if !strings.HasPrefix(lv, "current_timestamp") && !strings.HasPrefix(lv, "now(") {
			v = "(" + v + ")"
		}
	case mysqlNumeric[strings.ToLower(dataType)]:
		if strings.HasPrefix(v, "b'") { // bit
			break
		}
	case (dataType == "timestamp" || dataType == "datetime") && strings.HasPrefix(lv, "current_timestamp"):
	default:
		v = Literal(MySQL, v)
	}
	return &v
}

func pgTable(ctx context.Context, db *sql.DB, schema, table string) (*TableInfo, error) {
	if schema == "" {
		schema = "public"
	}
	ti := &TableInfo{Schema: schema, Name: table, Type: "table", Columns: []ColumnInfo{}, Indexes: []IndexInfo{}, Foreign: []ForeignKey{}, PrimaryKey: []string{}}
	reg := TableName(Postgres, "", schema, table)
	var kind string
	err := db.QueryRowContext(ctx, `SELECT c.relkind::text, COALESCE(obj_description(c.oid, 'pg_class'), '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2`, schema, table).Scan(&kind, &ti.Comment)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("模式 %s 里没有表 %s", schema, table)
	}
	if err != nil {
		return nil, err
	}
	if kind == "v" || kind == "m" {
		ti.Type = "view"
	}
	rows, err := db.QueryContext(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
			pg_get_expr(d.adbin, d.adrelid), COALESCE(col_description(a.attrelid, a.attnum), ''),
			a.attidentity::text, a.attgenerated::text, COALESCE(co.collname, '')
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_collation co ON co.oid = a.attcollation AND a.attcollation <> (SELECT typcollation FROM pg_type WHERE oid = a.atttypid)
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, reg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ColumnInfo
		var def sql.NullString
		var ident, gen string
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &def, &c.Comment, &ident, &gen, &c.Collation); err != nil {
			return nil, err
		}
		if gen != "" {
			c.Generated = true
		}
		if ident != "" {
			c.Auto = true
			if ident == "a" {
				c.Extra = "GENERATED ALWAYS AS IDENTITY"
			}
		}
		if def.Valid && !c.Generated {
			v := def.String
			if strings.HasPrefix(v, "nextval(") { // serial
				c.Auto = true
			}
			c.Default = &v
		}
		ti.Columns = append(ti.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	irows, err := db.QueryContext(ctx, `SELECT i.relname, ix.indisunique, ix.indisprimary, am.amname, pg_get_indexdef(ix.indexrelid),
			array_to_string(ARRAY(SELECT pg_get_indexdef(ix.indexrelid, k + 1, true) FROM generate_subscripts(ix.indkey, 1) k ORDER BY k), chr(31)),
			ix.indexprs IS NOT NULL OR ix.indpred IS NOT NULL,
			COALESCE((SELECT conname FROM pg_constraint WHERE conindid = ix.indexrelid AND conrelid = ix.indrelid LIMIT 1), '')
		FROM pg_index ix JOIN pg_class i ON i.oid = ix.indexrelid JOIN pg_am am ON am.oid = i.relam
		WHERE ix.indrelid = $1::regclass ORDER BY ix.indisprimary DESC, i.relname`, reg)
	if err != nil {
		return nil, err
	}
	defer irows.Close()
	for irows.Next() {
		var ix IndexInfo
		var def, cols, con string
		var expr bool
		if err := irows.Scan(&ix.Name, &ix.Unique, &ix.Primary, &ix.Method, &def, &cols, &expr, &con); err != nil {
			return nil, err
		}
		ix.Columns = []string{}
		for _, c := range strings.Split(cols, "\x1f") {
			if c != "" {
				ix.Columns = append(ix.Columns, strings.Trim(c, `"`))
			}
		}
		if expr {
			ix.Def = def
		}
		if ix.Primary {
			ti.PrimaryKey = ix.Columns
			ti.PKName = con
		} else if con != "" {
			ix.Method += "（约束 " + con + "）"
		}
		ti.Indexes = append(ti.Indexes, ix)
	}
	if err := irows.Err(); err != nil {
		return nil, err
	}
	irows.Close()
	for i := range ti.Columns {
		for _, k := range ti.PrimaryKey {
			if ti.Columns[i].Name == k {
				ti.Columns[i].Key = true
			}
		}
	}

	crows, err := db.QueryContext(ctx, `SELECT conname, contype::text, pg_get_constraintdef(oid, true) FROM pg_constraint
		WHERE conrelid = $1::regclass ORDER BY contype, conname`, reg)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	var checks []string
	for crows.Next() {
		var name, typ, def string
		if err := crows.Scan(&name, &typ, &def); err != nil {
			return nil, err
		}
		switch typ {
		case "f":
			ti.Foreign = append(ti.Foreign, ForeignKey{Name: name, Def: def})
		case "c", "x":
			checks = append(checks, "CONSTRAINT "+Quote(Postgres, name)+" "+def)
		}
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}
	crows.Close()

	if ti.Type == "view" {
		var def string
		if err := db.QueryRowContext(ctx, `SELECT pg_get_viewdef($1::regclass, true)`, reg).Scan(&def); err != nil {
			return nil, err
		}
		word := "VIEW"
		if kind == "m" {
			word = "MATERIALIZED VIEW"
		}
		ti.DDL = "CREATE " + word + " " + reg + " AS\n" + def
		return ti, nil
	}
	ti.DDL = pgDDL(ti, checks)
	return ti, nil
}

// pgDDL 按读到的结构拼出建表语句（PostgreSQL 没有 SHOW CREATE TABLE）。
func pgDDL(ti *TableInfo, checks []string) string {
	reg := TableName(Postgres, "", ti.Schema, ti.Name)
	var lines []string
	for _, c := range ti.Columns {
		lines = append(lines, "  "+pgColumnDef(c))
	}
	if len(ti.PrimaryKey) > 0 {
		pk := "  "
		if ti.PKName != "" {
			pk += "CONSTRAINT " + Quote(Postgres, ti.PKName) + " "
		}
		lines = append(lines, pk+"PRIMARY KEY ("+quoteList(Postgres, ti.PrimaryKey)+")")
	}
	for _, f := range ti.Foreign {
		lines = append(lines, "  CONSTRAINT "+Quote(Postgres, f.Name)+" "+f.Def)
	}
	for _, c := range checks {
		lines = append(lines, "  "+c)
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE " + reg + " (\n" + strings.Join(lines, ",\n") + "\n);\n")
	idx := append([]IndexInfo(nil), ti.Indexes...)
	sort.SliceStable(idx, func(i, j int) bool { return idx[i].Name < idx[j].Name })
	for _, ix := range idx {
		if ix.Primary || strings.Contains(ix.Method, "约束") {
			continue
		}
		if ix.Def != "" {
			b.WriteString(ix.Def + ";\n")
			continue
		}
		b.WriteString(pgIndexSQL(ti.Schema, ti.Name, IndexDef{Name: ix.Name, Columns: ix.Columns, Unique: ix.Unique, Method: ix.Method}) + ";\n")
	}
	if ti.Comment != "" {
		b.WriteString("COMMENT ON TABLE " + reg + " IS " + Literal(Postgres, ti.Comment) + ";\n")
	}
	for _, c := range ti.Columns {
		if c.Comment != "" {
			b.WriteString("COMMENT ON COLUMN " + reg + "." + Quote(Postgres, c.Name) + " IS " + Literal(Postgres, c.Comment) + ";\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func quoteList(kind string, cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIndexCol(kind, c)
	}
	return strings.Join(q, ", ")
}

// quoteIndexCol 索引列：MySQL 的前缀索引写作 name(10)。
func quoteIndexCol(kind, c string) string {
	if kind == MySQL {
		if i := strings.LastIndex(c, "("); i > 0 && strings.HasSuffix(c, ")") {
			n := c[i+1 : len(c)-1]
			if n != "" && strings.Trim(n, "0123456789") == "" {
				return Quote(kind, c[:i]) + "(" + n + ")"
			}
		}
	}
	return Quote(kind, c)
}
