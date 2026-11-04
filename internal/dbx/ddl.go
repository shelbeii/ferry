package dbx

import (
	"errors"
	"fmt"
	"strings"
)

// Design 结构设计里编辑好的表。
type Design struct {
	Schema     string      `json:"schema,omitempty"`
	Name       string      `json:"name"`
	Orig       string      `json:"orig,omitempty"` // 原来的表名；为空表示新建
	Columns    []ColumnDef `json:"columns"`
	PrimaryKey []string    `json:"primaryKey"`
	Indexes    []IndexDef  `json:"indexes"`
	Comment    string      `json:"comment,omitempty"`
	Engine     string      `json:"engine,omitempty"`  // MySQL
	Charset    string      `json:"charset,omitempty"` // MySQL
	Collation  string      `json:"collation,omitempty"`
}

// ColumnDef 设计里的一列。
type ColumnDef struct {
	Name      string  `json:"name"`
	Orig      string  `json:"orig,omitempty"` // 原来的列名；为空表示新加的列
	Type      string  `json:"type"`
	Nullable  bool    `json:"nullable"`
	Default   *string `json:"default"` // SQL 表达式；nil = 没有默认值
	Auto      bool    `json:"auto,omitempty"`
	Extra     string  `json:"extra,omitempty"`
	Collation string  `json:"collation,omitempty"`
	Comment   string  `json:"comment,omitempty"`
}

// IndexDef 设计里的一个索引（不含主键）。
type IndexDef struct {
	Name    string   `json:"name"`
	Orig    string   `json:"orig,omitempty"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	Method  string   `json:"method,omitempty"`
	Def     string   `json:"def,omitempty"` // 表达式索引：原样保留
}

func (d *Design) check() error {
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" {
		return errors.New("请填表名")
	}
	if len(d.Columns) == 0 {
		return errors.New("至少要有一列")
	}
	seen := map[string]bool{}
	for i := range d.Columns {
		c := &d.Columns[i]
		c.Name = strings.TrimSpace(c.Name)
		c.Type = strings.TrimSpace(c.Type)
		if c.Name == "" {
			return fmt.Errorf("第 %d 列没有名字", i+1)
		}
		if c.Type == "" {
			return fmt.Errorf("列 %s 没有类型", c.Name)
		}
		if strings.ContainsAny(c.Type, ";") {
			return fmt.Errorf("列 %s 的类型不合法", c.Name)
		}
		if seen[strings.ToLower(c.Name)] {
			return fmt.Errorf("列名 %s 重复了", c.Name)
		}
		seen[strings.ToLower(c.Name)] = true
	}
	for _, k := range d.PrimaryKey {
		if !seen[strings.ToLower(k)] {
			return fmt.Errorf("主键里的列 %s 不存在", k)
		}
	}
	names := map[string]bool{}
	for i := range d.Indexes {
		ix := &d.Indexes[i]
		ix.Name = strings.TrimSpace(ix.Name)
		if ix.Name == "" {
			return fmt.Errorf("第 %d 个索引没有名字", i+1)
		}
		if names[strings.ToLower(ix.Name)] {
			return fmt.Errorf("索引名 %s 重复了", ix.Name)
		}
		names[strings.ToLower(ix.Name)] = true
		if ix.Def == "" && len(ix.Columns) == 0 {
			return fmt.Errorf("索引 %s 没有选列", ix.Name)
		}
		for _, c := range ix.Columns {
			n := c
			if i := strings.LastIndex(n, "("); i > 0 {
				n = n[:i]
			}
			if ix.Def == "" && !seen[strings.ToLower(n)] {
				return fmt.Errorf("索引 %s 里的列 %s 不存在", ix.Name, c)
			}
		}
	}
	return nil
}

// DesignSQL 设计 → 要执行的语句。orig 为原来的表结构（新建时为 nil）。
func DesignSQL(kind, db string, d Design, orig *TableInfo) ([]string, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	if kind == MySQL {
		if orig == nil {
			return []string{mysqlCreate(db, d)}, nil
		}
		return mysqlAlter(db, d, orig), nil
	}
	if orig == nil {
		return pgCreate(d), nil
	}
	return pgAlter(d, orig), nil
}

// ---------------------------------------------------------------- MySQL

func mysqlColumnDef(c ColumnDef) string {
	s := Quote(MySQL, c.Name) + " " + c.Type
	if c.Collation != "" {
		s += " COLLATE " + c.Collation
	}
	if c.Nullable {
		s += " NULL"
	} else {
		s += " NOT NULL"
	}
	if c.Default != nil && !c.Auto {
		s += " DEFAULT " + *c.Default
	}
	if c.Auto {
		s += " AUTO_INCREMENT"
	}
	if c.Extra != "" {
		s += " " + c.Extra
	}
	if c.Comment != "" {
		s += " COMMENT " + Literal(MySQL, c.Comment)
	}
	return s
}

func mysqlIndexDef(ix IndexDef) string {
	s := "INDEX "
	switch {
	case ix.Unique:
		s = "UNIQUE INDEX "
	case strings.EqualFold(ix.Method, "FULLTEXT"):
		s = "FULLTEXT INDEX "
	case strings.EqualFold(ix.Method, "SPATIAL"):
		s = "SPATIAL INDEX "
	}
	s += Quote(MySQL, ix.Name) + " (" + quoteList(MySQL, ix.Columns) + ")"
	if strings.EqualFold(ix.Method, "HASH") {
		s += " USING HASH"
	}
	return s
}

func mysqlTableOptions(d Design) []string {
	var opts []string
	if d.Engine != "" {
		opts = append(opts, "ENGINE="+d.Engine)
	}
	if d.Charset != "" {
		opts = append(opts, "DEFAULT CHARSET="+d.Charset)
	}
	if d.Collation != "" {
		opts = append(opts, "COLLATE="+d.Collation)
	}
	return opts
}

func mysqlCreate(db string, d Design) string {
	var lines []string
	for _, c := range d.Columns {
		lines = append(lines, "  "+mysqlColumnDef(c))
	}
	if len(d.PrimaryKey) > 0 {
		lines = append(lines, "  PRIMARY KEY ("+quoteList(MySQL, d.PrimaryKey)+")")
	}
	for _, ix := range d.Indexes {
		lines = append(lines, "  "+mysqlIndexDef(ix))
	}
	s := "CREATE TABLE " + TableName(MySQL, db, "", d.Name) + " (\n" + strings.Join(lines, ",\n") + "\n)"
	opts := mysqlTableOptions(d)
	if d.Comment != "" {
		opts = append(opts, "COMMENT="+Literal(MySQL, d.Comment))
	}
	if len(opts) > 0 {
		s += " " + strings.Join(opts, " ")
	}
	return s
}

func mysqlAlter(db string, d Design, orig *TableInfo) []string {
	origCols := map[string]ColumnInfo{}
	for _, c := range orig.Columns {
		origCols[c.Name] = c
	}
	var parts []string
	// 删掉的列
	kept := map[string]bool{}
	for _, c := range d.Columns {
		if c.Orig != "" {
			kept[c.Orig] = true
		}
	}
	var origOrder []string
	for _, c := range orig.Columns {
		if !kept[c.Name] {
			parts = append(parts, "DROP COLUMN "+Quote(MySQL, c.Name))
		} else {
			origOrder = append(origOrder, c.Name)
		}
	}
	// 主键、索引先删（改列时可能依赖它们），最后再加
	pkChanged := !sameList(d.PrimaryKey, renamedList(orig.PrimaryKey, d.Columns))
	origIdx := map[string]IndexInfo{}
	for _, ix := range orig.Indexes {
		if !ix.Primary {
			origIdx[ix.Name] = ix
		}
	}
	keepIdx := map[string]bool{}
	var addIdx []IndexDef
	for _, ix := range d.Indexes {
		if o, ok := origIdx[ix.Orig]; ok && ix.Orig == ix.Name && (ix.Def != "" || (sameList(ix.Columns, renamedList(o.Columns, d.Columns)) && ix.Unique == o.Unique && strings.EqualFold(ix.Method, o.Method))) {
			keepIdx[ix.Orig] = true
			continue
		}
		if ix.Def != "" {
			if _, ok := origIdx[ix.Orig]; ok {
				keepIdx[ix.Orig] = true
			}
			continue
		}
		addIdx = append(addIdx, ix)
	}
	for _, ix := range orig.Indexes {
		if !ix.Primary && !keepIdx[ix.Name] {
			parts = append(parts, "DROP INDEX "+Quote(MySQL, ix.Name))
		}
	}
	if pkChanged && len(orig.PrimaryKey) > 0 {
		parts = append(parts, "DROP PRIMARY KEY")
	}
	// 列：新加的、改了的、挪了位置的
	prevOrig := map[string]string{} // 原来的列 → 原来排在它前面的（保留下来的）列
	for i, n := range origOrder {
		if i > 0 {
			prevOrig[n] = origOrder[i-1]
		} else {
			prevOrig[n] = ""
		}
	}
	prevDesign := "" // 设计里前一列的原名（新列为空串 + 标记）
	prevIsNew := false
	for i, c := range d.Columns {
		pos := " FIRST"
		if i > 0 {
			pos = " AFTER " + Quote(MySQL, d.Columns[i-1].Name)
		}
		if c.Orig == "" {
			parts = append(parts, "ADD COLUMN "+mysqlColumnDef(c)+pos)
		} else {
			o := origCols[c.Orig]
			if o.Generated {
				prevDesign, prevIsNew = c.Orig, false
				continue
			}
			moved := prevIsNew || prevOrig[c.Orig] != prevDesign
			if moved || c.Name != c.Orig || columnChanged(c, o) {
				p := ""
				if moved {
					p = pos
				}
				parts = append(parts, "CHANGE COLUMN "+Quote(MySQL, c.Orig)+" "+mysqlColumnDef(c)+p)
			}
		}
		prevDesign, prevIsNew = c.Orig, c.Orig == ""
	}
	if pkChanged && len(d.PrimaryKey) > 0 {
		parts = append(parts, "ADD PRIMARY KEY ("+quoteList(MySQL, d.PrimaryKey)+")")
	}
	for _, ix := range addIdx {
		parts = append(parts, "ADD "+mysqlIndexDef(ix))
	}
	if d.Comment != orig.Comment {
		parts = append(parts, "COMMENT = "+Literal(MySQL, d.Comment))
	}
	if d.Engine != "" && !strings.EqualFold(d.Engine, orig.Engine) {
		parts = append(parts, "ENGINE = "+d.Engine)
	}
	if d.Collation != "" && !strings.EqualFold(d.Collation, orig.Collation) {
		cs := d.Charset
		if cs == "" {
			cs = strings.SplitN(d.Collation, "_", 2)[0]
		}
		parts = append(parts, "DEFAULT CHARSET = "+cs+" COLLATE = "+d.Collation)
	} else if d.Charset != "" && !strings.EqualFold(d.Charset, orig.Charset) {
		parts = append(parts, "DEFAULT CHARSET = "+d.Charset)
	}
	var out []string
	if len(parts) > 0 {
		out = append(out, "ALTER TABLE "+TableName(MySQL, db, "", orig.Name)+"\n  "+strings.Join(parts, ",\n  "))
	}
	if d.Name != orig.Name {
		out = append(out, "RENAME TABLE "+TableName(MySQL, db, "", orig.Name)+" TO "+TableName(MySQL, db, "", d.Name))
	}
	return out
}

func columnChanged(c ColumnDef, o ColumnInfo) bool {
	return !strings.EqualFold(normType(c.Type), normType(o.Type)) || c.Nullable != o.Nullable || !sameDefault(c.Default, o.Default) ||
		c.Auto != o.Auto || c.Comment != o.Comment || !strings.EqualFold(c.Extra, o.Extra) || !strings.EqualFold(c.Collation, o.Collation)
}

func normType(t string) string { return strings.Join(strings.Fields(strings.ToLower(t)), " ") }

func sameDefault(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return strings.TrimSpace(*a) == strings.TrimSpace(*b)
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// renamedList 原来的列名换成设计里的新名字（列改了名时，索引、主键跟着改，不算变化）。
func renamedList(cols []string, design []ColumnDef) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c
		base, suffix := c, ""
		if j := strings.LastIndex(c, "("); j > 0 && strings.HasSuffix(c, ")") {
			base, suffix = c[:j], c[j:]
		}
		for _, d := range design {
			if d.Orig == base {
				out[i] = d.Name + suffix
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- PostgreSQL

func pgColumnDef(c ColumnInfo) string {
	return pgColDef(ColumnDef{Name: c.Name, Type: c.Type, Nullable: c.Nullable, Default: c.Default, Auto: c.Auto && !isSerialDefault(c.Default), Extra: c.Extra, Collation: c.Collation})
}

func isSerialDefault(d *string) bool { return d != nil && strings.HasPrefix(*d, "nextval(") }

func pgColDef(c ColumnDef) string {
	s := Quote(Postgres, c.Name) + " " + c.Type
	if c.Collation != "" {
		s += " COLLATE " + Quote(Postgres, c.Collation)
	}
	if c.Default != nil && !(c.Auto && !isSerialDefault(c.Default)) {
		s += " DEFAULT " + *c.Default
	}
	if c.Auto && !isSerialDefault(c.Default) {
		if strings.Contains(strings.ToUpper(c.Extra), "ALWAYS") {
			s += " GENERATED ALWAYS AS IDENTITY"
		} else {
			s += " GENERATED BY DEFAULT AS IDENTITY"
		}
	}
	if !c.Nullable {
		s += " NOT NULL"
	}
	return s
}

func pgIndexSQL(schema, table string, ix IndexDef) string {
	s := "CREATE "
	if ix.Unique {
		s += "UNIQUE "
	}
	s += "INDEX " + Quote(Postgres, ix.Name) + " ON " + TableName(Postgres, "", schema, table)
	if m := strings.ToLower(strings.TrimSpace(ix.Method)); m != "" && m != "btree" && !strings.Contains(m, "约束") {
		s += " USING " + m
	}
	return s + " (" + quoteList(Postgres, ix.Columns) + ")"
}

func pgCreate(d Design) []string {
	reg := TableName(Postgres, "", d.Schema, d.Name)
	var lines []string
	for _, c := range d.Columns {
		lines = append(lines, "  "+pgColDef(c))
	}
	if len(d.PrimaryKey) > 0 {
		lines = append(lines, "  PRIMARY KEY ("+quoteList(Postgres, d.PrimaryKey)+")")
	}
	out := []string{"CREATE TABLE " + reg + " (\n" + strings.Join(lines, ",\n") + "\n)"}
	for _, ix := range d.Indexes {
		out = append(out, pgIndexSQL(d.Schema, d.Name, ix))
	}
	if d.Comment != "" {
		out = append(out, "COMMENT ON TABLE "+reg+" IS "+Literal(Postgres, d.Comment))
	}
	for _, c := range d.Columns {
		if c.Comment != "" {
			out = append(out, "COMMENT ON COLUMN "+reg+"."+Quote(Postgres, c.Name)+" IS "+Literal(Postgres, c.Comment))
		}
	}
	return out
}

func pgAlter(d Design, orig *TableInfo) []string {
	reg := TableName(Postgres, "", orig.Schema, orig.Name)
	var out []string
	alter := func(s string) { out = append(out, "ALTER TABLE "+reg+" "+s) }
	origCols := map[string]ColumnInfo{}
	for _, c := range orig.Columns {
		origCols[c.Name] = c
	}
	kept := map[string]bool{}
	for _, c := range d.Columns {
		if c.Orig != "" {
			kept[c.Orig] = true
		}
	}
	// 先删索引和主键
	pkChanged := !sameList(d.PrimaryKey, renamedList(orig.PrimaryKey, d.Columns))
	origIdx := map[string]IndexInfo{}
	for _, ix := range orig.Indexes {
		if !ix.Primary {
			origIdx[ix.Name] = ix
		}
	}
	keepIdx := map[string]bool{}
	var addIdx []IndexDef
	var renameIdx [][2]string
	for _, ix := range d.Indexes {
		o, ok := origIdx[ix.Orig]
		if ok && (ix.Def != "" || (sameList(ix.Columns, renamedList(o.Columns, d.Columns)) && ix.Unique == o.Unique && strings.EqualFold(ix.Method, o.Method))) {
			keepIdx[ix.Orig] = true
			if ix.Name != ix.Orig {
				renameIdx = append(renameIdx, [2]string{ix.Orig, ix.Name})
			}
			continue
		}
		if ix.Def != "" {
			continue
		}
		addIdx = append(addIdx, ix)
	}
	for _, ix := range orig.Indexes {
		if ix.Primary || keepIdx[ix.Name] {
			continue
		}
		if i := strings.Index(ix.Method, "（约束 "); i >= 0 {
			con := strings.TrimSuffix(ix.Method[i+len("（约束 "):], "）")
			alter("DROP CONSTRAINT " + Quote(Postgres, con))
		} else {
			out = append(out, "DROP INDEX "+TableName(Postgres, "", orig.Schema, ix.Name))
		}
	}
	if pkChanged && len(orig.PrimaryKey) > 0 {
		name := orig.PKName
		if name == "" {
			name = orig.Name + "_pkey"
		}
		alter("DROP CONSTRAINT " + Quote(Postgres, name))
	}
	for _, c := range orig.Columns {
		if !kept[c.Name] {
			alter("DROP COLUMN " + Quote(Postgres, c.Name))
		}
	}
	for _, c := range d.Columns {
		if c.Orig == "" {
			alter("ADD COLUMN " + pgColDef(c))
			if c.Comment != "" {
				out = append(out, "COMMENT ON COLUMN "+reg+"."+Quote(Postgres, c.Name)+" IS "+Literal(Postgres, c.Comment))
			}
			continue
		}
		o := origCols[c.Orig]
		if o.Generated {
			continue
		}
		name := c.Orig
		if c.Name != c.Orig {
			alter("RENAME COLUMN " + Quote(Postgres, c.Orig) + " TO " + Quote(Postgres, c.Name))
			name = c.Name
		}
		col := Quote(Postgres, name)
		if !strings.EqualFold(normType(c.Type), normType(o.Type)) || !strings.EqualFold(c.Collation, o.Collation) {
			s := "ALTER COLUMN " + col + " TYPE " + c.Type
			if c.Collation != "" {
				s += " COLLATE " + Quote(Postgres, c.Collation)
			}
			alter(s + " USING " + col + "::" + c.Type)
		}
		oAuto := o.Auto && !isSerialDefault(o.Default)
		cAuto := c.Auto && !isSerialDefault(c.Default)
		if oAuto && !cAuto {
			alter("ALTER COLUMN " + col + " DROP IDENTITY")
		}
		if !sameDefault(c.Default, o.Default) && !cAuto {
			if c.Default == nil {
				alter("ALTER COLUMN " + col + " DROP DEFAULT")
			} else {
				alter("ALTER COLUMN " + col + " SET DEFAULT " + *c.Default)
			}
		}
		if cAuto && !oAuto {
			if o.Default != nil {
				alter("ALTER COLUMN " + col + " DROP DEFAULT")
			}
			alter("ALTER COLUMN " + col + " ADD GENERATED BY DEFAULT AS IDENTITY")
		}
		if c.Nullable != o.Nullable {
			if c.Nullable {
				alter("ALTER COLUMN " + col + " DROP NOT NULL")
			} else {
				alter("ALTER COLUMN " + col + " SET NOT NULL")
			}
		}
		if c.Comment != o.Comment {
			v := "NULL"
			if c.Comment != "" {
				v = Literal(Postgres, c.Comment)
			}
			out = append(out, "COMMENT ON COLUMN "+reg+"."+col+" IS "+v)
		}
	}
	if pkChanged && len(d.PrimaryKey) > 0 {
		alter("ADD PRIMARY KEY (" + quoteList(Postgres, d.PrimaryKey) + ")")
	}
	for _, r := range renameIdx {
		out = append(out, "ALTER INDEX "+TableName(Postgres, "", orig.Schema, r[0])+" RENAME TO "+Quote(Postgres, r[1]))
	}
	for _, ix := range addIdx {
		out = append(out, pgIndexSQL(orig.Schema, orig.Name, ix))
	}
	if d.Comment != orig.Comment {
		v := "NULL"
		if d.Comment != "" {
			v = Literal(Postgres, d.Comment)
		}
		out = append(out, "COMMENT ON TABLE "+reg+" IS "+v)
	}
	if d.Name != orig.Name {
		alter("RENAME TO " + Quote(Postgres, d.Name))
	}
	return out
}
