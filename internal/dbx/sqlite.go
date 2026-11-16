package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	sqlite3 "github.com/ncruces/go-sqlite3"
	sqlitedriver "github.com/ncruces/go-sqlite3/driver"
)

func sqliteURI(path, mode string) string {
	u := url.URL{Path: filepath.ToSlash(path)}
	return "file:" + u.EscapedPath() + "?mode=" + mode
}

func openSQLite(path, database string) (*sql.DB, error) {
	if database != "main" {
		return nil, errors.New("SQLite 只打开所选文件的 main 数据库")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("请选择本机 SQLite 数据库文件（需要绝对路径）")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取 SQLite 文件：%w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("请选择常规 SQLite 数据库文件")
	}
	return sqlitedriver.Open(sqliteURI(path, "ro"), func(c *sqlite3.Conn) error {
		if _, err := c.Config(sqlite3.DBCONFIG_DEFENSIVE, true); err != nil {
			return err
		}
		if err := c.Exec("PRAGMA trusted_schema=OFF; PRAGMA query_only=ON"); err != nil {
			return err
		}
		return c.SetAuthorizer(sqliteReadOnly)
	})
}

func sqliteExportDestination(database, output string) error {
	out, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(out)); err == nil {
		out = filepath.Join(parent, filepath.Base(out))
	}
	outInfo, _ := os.Stat(out)
	paths := []string{filepath.Clean(database)}
	if resolved, err := filepath.EvalSymlinks(database); err == nil && resolved != paths[0] {
		paths = append(paths, resolved)
	}
	for _, path := range paths {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			p := path + suffix
			if out == p || (runtime.GOOS == "windows" && strings.EqualFold(out, p)) {
				return errors.New("导出文件不能覆盖当前 SQLite 数据库或其日志")
			}
			if srcInfo, err := os.Stat(p); err == nil && outInfo != nil && os.SameFile(srcInfo, outInfo) {
				return errors.New("导出文件不能覆盖当前 SQLite 数据库或其日志")
			}
		}
	}
	return nil
}

// mode=ro 只约束主库文件，authorizer 同时拦住 ATTACH、临时表和更改设置的 PRAGMA。
func sqliteReadOnly(action sqlite3.AuthorizerActionCode, a, b, _, _ string) sqlite3.AuthorizerReturnCode {
	switch action {
	case sqlite3.AUTH_SELECT, sqlite3.AUTH_READ, sqlite3.AUTH_RECURSIVE:
		return sqlite3.AUTH_OK
	case sqlite3.AUTH_FUNCTION:
		switch strings.ToLower(b) {
		case "load_extension", "writefile", "readfile", "edit", "fts3_tokenizer", "zipfile":
		default:
			return sqlite3.AUTH_OK
		}
	case sqlite3.AUTH_PRAGMA:
		switch strings.ToLower(a) {
		case "table_info", "table_xinfo", "index_list", "index_info", "index_xinfo", "foreign_key_list", "table_list":
			return sqlite3.AUTH_OK
		case "database_list", "schema_version", "user_version", "application_id", "query_only":
			if b == "" {
				return sqlite3.AUTH_OK
			}
		}
	}
	return sqlite3.AUTH_DENY
}

func sqliteTables(ctx context.Context, db *sql.DB) ([]Table, error) {
	const q = `SELECT name, type FROM main.sqlite_schema
WHERE type IN ('table', 'view') AND substr(name, 1, 7) <> 'sqlite_' ORDER BY name`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Table{}
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Name, &t.Type); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func sqliteTable(ctx context.Context, db *sql.DB, table string) (*TableInfo, error) {
	ti := &TableInfo{Name: table, Schema: "main", Columns: []ColumnInfo{}, PrimaryKey: []string{}}
	err := db.QueryRowContext(ctx, `SELECT type, coalesce(sql, '') FROM main.sqlite_schema
WHERE name = ? COLLATE NOCASE AND type IN ('table', 'view')`, table).Scan(&ti.Type, &ti.DDL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("找不到 SQLite 表或视图 %q", table)
	}
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "PRAGMA main.table_xinfo("+Literal(SQLite, table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[int]string{}
	for rows.Next() {
		var c ColumnInfo
		var cid, notNull, pk, hidden int
		if err := rows.Scan(&cid, &c.Name, &c.Type, &notNull, &c.Default, &pk, &hidden); err != nil {
			return nil, err
		}
		c.Nullable, c.Generated, c.Key = notNull == 0, hidden >= 2, pk > 0
		if c.Key {
			keys[pk] = c.Name
		}
		if hidden != 1 {
			ti.Columns = append(ti.Columns, c)
		}
	}
	for i := 1; i <= len(keys); i++ {
		ti.PrimaryKey = append(ti.PrimaryKey, keys[i])
	}
	return ti, rows.Err()
}
