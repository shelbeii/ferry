package dbx

import "testing"

// 查询窗口里执行的：只有改动的语句记审计
func TestWrites(t *testing.T) {
	got := Writes(MySQL, "SELECT 1; update t set a = 1 where id = 2; -- 注释\nSHOW TABLES; DELETE FROM t; ")
	if len(got) != 2 || got[0] != "update t set a = 1 where id = 2" || got[1] != "DELETE FROM t" {
		t.Fatalf("%q", got)
	}
	if got := Writes(Postgres, "select * from t; explain select 1"); len(got) != 0 {
		t.Fatalf("只读的不记：%q", got)
	}
	if got := Writes(MySQL, "/* x */ CREATE TABLE a (id int)"); len(got) != 1 {
		t.Fatalf("建表：%q", got)
	}
}
