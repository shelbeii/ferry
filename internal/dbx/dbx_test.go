package dbx

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		kind, in string
		want     []string
	}{
		{MySQL, "select 1; select 2;", []string{"select 1", "select 2"}},
		{MySQL, "select ';' as a; -- x;y\nselect 2", []string{"select ';' as a", "-- x;y\nselect 2"}},
		{MySQL, "select 'it\\'s;'; select `a;b` from t", []string{"select 'it\\'s;'", "select `a;b` from t"}},
		{MySQL, "# c;\nselect 1 /* ; */;\n;  ;", []string{"# c;\nselect 1 /* ; */"}},
		{MySQL, "DELIMITER $$\nCREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END$$\nDELIMITER ;\nCALL p();", []string{"CREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END", "CALL p()"}},
		{MySQL, "select 1--2;select 3", []string{"select 1--2", "select 3"}},
		{Postgres, "CREATE FUNCTION f() RETURNS int AS $$ SELECT 1; $$ LANGUAGE sql; select 2", []string{"CREATE FUNCTION f() RETURNS int AS $$ SELECT 1; $$ LANGUAGE sql", "select 2"}},
		{Postgres, "select $tag$a;b$tag$, 'x''y;'; select $1", []string{"select $tag$a;b$tag$, 'x''y;'", "select $1"}},
		{Postgres, "-- only comment", nil},
	}
	for _, c := range cases {
		got := Split(c.kind, c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%s, %q) = %q, want %q", c.kind, c.in, got, c.want)
		}
	}
}

func TestReadOnly(t *testing.T) {
	for s, want := range map[string]bool{
		"select 1": true, " /* x */ (SELECT 1)": true, "show tables": true, "with a as (select 1) select * from a": true,
		"update t set a=1": false, "-- select\ndelete from t": false, "drop table t": false, "set global x=1": false,
	} {
		if got := IsReadOnly(MySQL, s); got != want {
			t.Errorf("IsReadOnly(%q) = %v", s, got)
		}
	}
}

func p(s string) *string { return &s }

func TestChangeSQL(t *testing.T) {
	ti := &TableInfo{Name: "user", Type: "table", Key: []string{"id"}, Columns: []ColumnInfo{{Name: "id", Type: "int"}, {Name: "name", Type: "varchar(20)"}, {Name: "age", Type: "int"}}}
	st, err := ChangeSQL(MySQL, "app", ti, []Change{
		{Op: "update", Key: map[string]*string{"id": p("1")}, Values: map[string]*string{"age": nil, "name": p("O'Neil")}},
		{Op: "insert", Values: map[string]*string{"name": p("a")}},
		{Op: "delete", Key: map[string]*string{"id": p("3")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"UPDATE `app`.`user` SET `name` = 'O''Neil', `age` = NULL WHERE `id` = '1'",
		"INSERT INTO `app`.`user` (`name`) VALUES ('a')",
		"DELETE FROM `app`.`user` WHERE `id` = '3'",
	}
	for i, s := range st {
		if got := s.Preview(MySQL); got != want[i] {
			t.Errorf("%d: %s\nwant %s", i, got, want[i])
		}
	}
	pg, err := ChangeSQL(Postgres, "", &TableInfo{Schema: "public", Name: "t", Type: "table", Key: []string{"a", "b"}, Columns: []ColumnInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}}},
		[]Change{{Op: "update", Key: map[string]*string{"a": p("1"), "b": nil}, Values: map[string]*string{"c": p("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	if pg[0].SQL != `UPDATE "public"."t" SET "c" = $1 WHERE "a" = $2 AND "b" IS NULL` || len(pg[0].Args) != 2 {
		t.Errorf("pg: %s %v", pg[0].SQL, pg[0].Args)
	}
	if _, err := ChangeSQL(MySQL, "app", &TableInfo{Name: "x", Columns: ti.Columns}, []Change{{Op: "delete"}}); err == nil {
		t.Error("没有主键时应该报错")
	}
}

func TestMySQLDefault(t *testing.T) {
	ns := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	for _, c := range []struct {
		def           sql.NullString
		typ, extra    string
		maria         bool
		want          string
		wantNoDefault bool
	}{
		{ns("abc"), "varchar", "", false, "'abc'", false},
		{ns("0"), "int", "", false, "0", false},
		{ns("CURRENT_TIMESTAMP"), "timestamp", "default_generated", false, "CURRENT_TIMESTAMP", false},
		{ns("uuid()"), "varchar", "default_generated", false, "(uuid())", false},
		{sql.NullString{}, "varchar", "", false, "", true},
		{ns("'abc'"), "varchar", "", true, "'abc'", false},
		{ns("NULL"), "varchar", "", true, "NULL", false},
	} {
		got := mysqlDefault(c.def, c.typ, c.extra, c.maria)
		if c.wantNoDefault {
			if got != nil {
				t.Errorf("%v: want nil, got %s", c.def, *got)
			}
			continue
		}
		if got == nil || *got != c.want {
			t.Errorf("%v %s: got %v want %s", c.def, c.typ, got, c.want)
		}
	}
}

func TestDesignMySQL(t *testing.T) {
	d := Design{Name: "user", Columns: []ColumnDef{
		{Name: "id", Type: "bigint unsigned", Auto: true},
		{Name: "name", Type: "varchar(64)", Default: p("''"), Comment: "名字"},
	}, PrimaryKey: []string{"id"}, Indexes: []IndexDef{{Name: "idx_name", Columns: []string{"name"}, Unique: true}}, Engine: "InnoDB", Charset: "utf8mb4", Comment: "用户"}
	st, err := DesignSQL(MySQL, "app", d, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE TABLE `app`.`user` (\n  `id` bigint unsigned NOT NULL AUTO_INCREMENT,\n  `name` varchar(64) NOT NULL DEFAULT '' COMMENT '名字',\n  PRIMARY KEY (`id`),\n  UNIQUE INDEX `idx_name` (`name`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户'"
	if st[0] != want {
		t.Errorf("create:\n%s\nwant\n%s", st[0], want)
	}

	orig := &TableInfo{Name: "user", Type: "table", Engine: "InnoDB", Comment: "用户", PrimaryKey: []string{"id"},
		Columns: []ColumnInfo{{Name: "id", Type: "bigint unsigned", Auto: true, Key: true}, {Name: "name", Type: "varchar(64)", Default: p("''")}, {Name: "old", Type: "int", Nullable: true}},
		Indexes: []IndexInfo{{Name: "PRIMARY", Primary: true, Unique: true, Columns: []string{"id"}}, {Name: "idx_name", Unique: true, Columns: []string{"name"}, Method: "BTREE"}}}
	// 改名 name → nick，加一列 age 放在最前面之后，删 old，索引跟着列名走
	d2 := Design{Name: "users", Comment: "用户", Engine: "InnoDB", PrimaryKey: []string{"id"}, Columns: []ColumnDef{
		{Name: "id", Orig: "id", Type: "bigint unsigned", Auto: true},
		{Name: "age", Type: "int", Nullable: true},
		{Name: "nick", Orig: "name", Type: "varchar(64)", Default: p("''")},
	}, Indexes: []IndexDef{{Name: "idx_name", Orig: "idx_name", Columns: []string{"nick"}, Unique: true, Method: "BTREE"}}}
	st, err = DesignSQL(MySQL, "app", d2, orig)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(st, ";\n")
	want = "ALTER TABLE `app`.`user`\n  DROP COLUMN `old`,\n  ADD COLUMN `age` int NULL AFTER `id`,\n  CHANGE COLUMN `name` `nick` varchar(64) NOT NULL DEFAULT '' AFTER `age`;\nRENAME TABLE `app`.`user` TO `app`.`users`"
	if got != want {
		t.Errorf("alter:\n%s\nwant\n%s", got, want)
	}
	// 什么都没改
	same := Design{Name: "user", Comment: "用户", Engine: "InnoDB", PrimaryKey: []string{"id"}, Columns: []ColumnDef{
		{Name: "id", Orig: "id", Type: "bigint unsigned", Auto: true}, {Name: "name", Orig: "name", Type: "varchar(64)", Default: p("''")}, {Name: "old", Orig: "old", Type: "int", Nullable: true},
	}, Indexes: []IndexDef{{Name: "idx_name", Orig: "idx_name", Columns: []string{"name"}, Unique: true, Method: "BTREE"}}}
	if st, _ := DesignSQL(MySQL, "app", same, orig); len(st) != 0 {
		t.Errorf("没改时不应有语句：%q", st)
	}
}

func TestDesignPostgres(t *testing.T) {
	orig := &TableInfo{Schema: "public", Name: "t", Type: "table", PrimaryKey: []string{"id"}, PKName: "t_pkey",
		Columns: []ColumnInfo{{Name: "id", Type: "integer", Auto: true, Key: true}, {Name: "a", Type: "text", Nullable: true}},
		Indexes: []IndexInfo{{Name: "t_pkey", Primary: true, Unique: true, Columns: []string{"id"}, Method: "btree"}, {Name: "t_a", Columns: []string{"a"}, Method: "btree"}}}
	d := Design{Schema: "public", Name: "t", PrimaryKey: []string{"id"}, Columns: []ColumnDef{
		{Name: "id", Orig: "id", Type: "integer", Auto: true},
		{Name: "b", Orig: "a", Type: "varchar(10)", Nullable: false, Default: p("'x'"), Comment: "说明"},
		{Name: "c", Type: "int", Nullable: true},
	}, Indexes: []IndexDef{{Name: "t_b", Orig: "t_a", Columns: []string{"b"}, Method: "btree"}}}
	st, err := DesignSQL(Postgres, "", d, orig)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`ALTER TABLE "public"."t" RENAME COLUMN "a" TO "b"`,
		`ALTER TABLE "public"."t" ALTER COLUMN "b" TYPE varchar(10) USING "b"::varchar(10)`,
		`ALTER TABLE "public"."t" ALTER COLUMN "b" SET DEFAULT 'x'`,
		`ALTER TABLE "public"."t" ALTER COLUMN "b" SET NOT NULL`,
		`COMMENT ON COLUMN "public"."t"."b" IS '说明'`,
		`ALTER TABLE "public"."t" ADD COLUMN "c" int`,
		`ALTER INDEX "public"."t_a" RENAME TO "t_b"`,
	}
	if !reflect.DeepEqual(st, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(st, "\n"), strings.Join(want, "\n"))
	}
}

func TestWhereSQL(t *testing.T) {
	w, args, err := WhereSQL(MySQL, "", []Filter{
		{Col: "name", Op: "contains", Value: "50%"},
		{Col: "age", Op: "between", Value: "1", Value2: "9", Or: true},
		{Col: "x", Op: "=", Value: "skip", Off: true},
		{Col: "id", Op: "in", Value: "1, 2,,3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "`name` LIKE ? ESCAPE '\\\\' OR `age` BETWEEN ? AND ? AND `id` IN (?, ?, ?)"; w != want {
		t.Errorf("got  %s\nwant %s", w, want)
	}
	if !reflect.DeepEqual(args, []any{`%50\%%`, "1", "9", "1", "2", "3"}) {
		t.Errorf("args %q", args)
	}
	w, args, _ = WhereSQL(Postgres, "a > 1", []Filter{{Col: "n", Op: "ends", Value: "x_"}, {Col: "m", Op: "null"}})
	if w != `(a > 1) AND (CAST("n" AS TEXT) LIKE $1 ESCAPE '\' AND "m" IS NULL)` || args[0] != `%x\_` {
		t.Errorf("pg: %s %q", w, args)
	}
	if _, _, err := WhereSQL(MySQL, "1=1; drop table t", nil); err == nil {
		t.Error("分号应该被拒绝")
	}
	if _, _, err := WhereSQL(MySQL, "", []Filter{{Col: "a", Op: "x"}}); err == nil {
		t.Error("不认识的运算应该被拒绝")
	}
	if got := OrderSQL(Postgres, []Sort{{Col: "a", Desc: true}, {Col: "b"}}); got != `"a" DESC, "b"` {
		t.Errorf("order %s", got)
	}
}
