package dbx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/appdir"
)

func savedHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("FERRY_HOME", home)
	appdir.Reset()
	t.Cleanup(appdir.Reset)
	return home
}

func TestSavedQueries(t *testing.T) {
	home := savedHome(t)
	if items, err := ListQueries("c1", "shop"); err != nil || len(items) != 0 {
		t.Fatalf("空目录：%v %v", items, err)
	}
	if _, err := SaveQuery("c1", "shop", "统计 订单", "", "SELECT 1;\n", false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(QueriesDir("c1", "shop"), home) {
		t.Fatalf("应该在数据目录里：%s", QueriesDir("c1", "shop"))
	}
	if _, err := SaveQuery("c1", "shop", "统计 订单", "", "SELECT 2;", false); err == nil {
		t.Fatal("同名的不该直接覆盖")
	}
	if _, err := SaveQuery("c1", "shop", "统计 订单", "统计 订单", "SELECT 2;", false); err != nil {
		t.Fatalf("保存自己：%v", err)
	}
	if s, _ := ReadQuery("c1", "shop", "统计 订单"); s != "SELECT 2;" {
		t.Fatalf("内容：%q", s)
	}
	for _, bad := range []string{"", "  ", "a/b", `a\b`, "..", "x:y", "a.", strings.Repeat("长", 101)} {
		if _, err := SaveQuery("c1", "shop", bad, "", "x", false); err == nil {
			t.Fatalf("名称 %q 应该不行", bad)
		}
	}
	// 改名
	if n, err := RenameQuery("c1", "shop", "统计 订单", "订单统计"); err != nil || n != "订单统计" {
		t.Fatalf("改名：%s %v", n, err)
	}
	items, _ := ListQueries("c1", "shop")
	if len(items) != 1 || items[0].Name != "订单统计" || items[0].Preview != "SELECT 2;" {
		t.Fatalf("改名后：%+v", items)
	}
	// 只改大小写
	SaveQuery("c1", "shop", "abc", "", "x", false)
	if n, err := RenameQuery("c1", "shop", "abc", "ABC"); err != nil || n != "ABC" {
		t.Fatalf("改大小写：%s %v", n, err)
	}
	items, _ = ListQueries("c1", "shop")
	if len(items) != 2 || items[0].Name != "ABC" {
		t.Fatalf("改大小写后：%+v", items)
	}
	if err := DeleteQuery("c1", "shop", "ABC"); err != nil {
		t.Fatal(err)
	}
	// 奇怪的库名不会跑到别的目录
	if d := QueriesDir("c1", "../x"); filepath.Dir(d) != filepath.Join(home, "db", "queries", "c1") {
		t.Fatalf("库名当目录：%s", d)
	}
}

func TestBackups(t *testing.T) {
	savedHome(t)
	now := time.Date(2026, 11, 9, 15, 30, 0, 0, time.Local)
	p1, err := NewBackupPath("c1", "shop", now)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p1, []byte("-- a"), 0o600)
	p2, _ := NewBackupPath("c1", "shop", now)
	if p1 == p2 || !strings.HasSuffix(p2, "2026-11-09_153000_2.sql") {
		t.Fatalf("同一秒的第二个：%s", p2)
	}
	os.WriteFile(p2, []byte("-- b"), 0o600)
	os.WriteFile(filepath.Join(filepath.Dir(p1), "readme.txt"), []byte("x"), 0o600)
	items, err := ListBackups("c1", "shop")
	if err != nil || len(items) != 2 || items[0].Name != "2026-11-09_153000_2" {
		t.Fatalf("列表：%+v %v", items, err)
	}
	if _, err := BackupPath("c1", "shop", "../../connections"); err == nil {
		t.Fatal("名称里有路径应该不行")
	}
	if err := DeleteBackup("c1", "shop", items[0].Name); err != nil {
		t.Fatal(err)
	}
	if items, _ := ListBackups("c1", "shop"); len(items) != 1 {
		t.Fatalf("删了一个：%+v", items)
	}
}

func TestRoutineSQL(t *testing.T) {
	ok := []string{
		"CREATE PROCEDURE p() BEGIN SELECT 1; END;",
		"-- 说明\nCREATE DEFINER=`root`@`%` FUNCTION f() RETURNS int RETURN 1",
		"/* x */ create or replace function public.f() returns int language sql as $$ select 1 $$;",
	}
	for _, s := range ok {
		got, err := RoutineSQL(s)
		if err != nil {
			t.Fatalf("%q：%v", s, err)
		}
		if strings.HasSuffix(got, ";") {
			t.Fatalf("末尾的分号要去掉：%q", got)
		}
	}
	for _, s := range []string{"", "DROP TABLE x", "DELIMITER ;;\nCREATE PROCEDURE p() BEGIN END;;\nDELIMITER ;", "CREATE TABLE t (a int)"} {
		if _, err := RoutineSQL(s); err == nil {
			t.Fatalf("%q 应该不行", s)
		}
	}
}

func TestDropRoutineOp(t *testing.T) {
	s, err := OpSQL(MySQL, "shop", "", "drop-procedure", []string{"p1"})
	if err != nil || s[0] != "DROP PROCEDURE `shop`.`p1`" {
		t.Fatalf("%v %v", s, err)
	}
	s, err = OpSQL(Postgres, "db", "report", "drop-function", []string{"f", "a integer, b text"})
	if err != nil || s[0] != `DROP FUNCTION "report"."f"(a integer, b text)` {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := OpSQL(Postgres, "db", "", "drop-function", []string{"f", "int); DROP TABLE x; --"}); err == nil {
		t.Fatal("参数里有分号应该不行")
	}
}
