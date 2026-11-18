package dbx

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// 使用可新建临时库的专用测试实例。验证两种兼容引擎的 MySQL 客户端入口。
func TestMySQLCompatibleLive(t *testing.T) {
	for _, product := range []struct{ name, prefix string }{
		{"TiDB", "FERRY_TIDB"},
		{"OceanBase", "FERRY_OCEANBASE"},
	} {
		t.Run(product.name, func(t *testing.T) {
			ctx := t.Context()
			user := os.Getenv(product.prefix + "_USER")
			if user == "" {
				user = "root"
			}
			c := liveAdapterConn(t, MySQL, product.prefix+"_LIVE", user)
			pw := os.Getenv(product.prefix + "_PASSWORD")
			setup := openLiveAdapter(t, ctx, c, pw, false)
			db, err := setup.DB(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("ferry_compat_%d", time.Now().UnixNano())
			qname := Quote(MySQL, name)
			if _, err := db.ExecContext(ctx, "CREATE DATABASE "+qname); err != nil {
				t.Fatal(err)
			}
			defer db.ExecContext(ctx, "DROP DATABASE "+qname)
			const table = "items"
			qtable := TableName(MySQL, name, "", table)
			if _, err := db.ExecContext(ctx, "CREATE TABLE "+qtable+" (id BIGINT PRIMARY KEY, value VARCHAR(20) NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "INSERT INTO "+qtable+" VALUES (1, '兼容测试'), (2, 'second')"); err != nil {
				t.Fatal(err)
			}

			cl := setup
			t.Log("版本", cl.Version)
			tables, err := cl.Tables(ctx, name)
			if err != nil || len(tables) != 1 || tables[0].Name != table {
				t.Fatalf("表列表：%v, %v", tables, err)
			}
			info, err := cl.TableInfo(ctx, name, "", table)
			if err != nil || len(info.Columns) != 2 || !slices.Equal(info.PrimaryKey, []string{"id"}) {
				t.Fatalf("表结构：%+v, %v", info, err)
			}
			q := "SELECT value FROM " + qtable + " WHERE id = 1"
			results, err := cl.Query(ctx, name, q, QueryOptions{})
			if err != nil || len(results) != 1 || len(results[0].Rows) != 1 || results[0].Rows[0][0] != "兼容测试" {
				t.Fatalf("查询：%+v, %v", results, err)
			}
			c.AI = true
			c.ReadOnly = true
			readOnly := openLiveAdapter(t, ctx, c, pw, false)
			results, err = readOnly.Query(ctx, name, q, QueryOptions{})
			if err != nil || len(results) != 1 {
				t.Fatalf("只读查询：%+v, %v", results, err)
			}
			if results[0].Error != "" || len(results[0].Rows) != 1 || results[0].Rows[0][0] != "兼容测试" {
				t.Fatalf("只读查询：%+v", results)
			}
			if r, err := readOnly.AIQuery(ctx, name, q, 100); err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "兼容测试" {
				t.Fatalf("AI 只读查询：%+v, %v", r, err)
			}
			csv := filepath.Join(t.TempDir(), "compat.csv")
			if _, err := cl.Export(ctx, name, ExportOptions{Query: q, Format: "csv", Out: csv}, nil); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(csv)
			if err != nil || !bytes.Contains(data, []byte("兼容测试")) {
				t.Fatalf("CSV：%q, %v", data, err)
			}
			c.ReadOnly, c.AIWrite = false, true
			verifySQLAIWrite(t, openLiveAdapter(t, ctx, c, pw, false), name, qtable, "value")
		})
	}
}
