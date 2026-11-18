package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	dm "gitee.com/chunanyong/dm"
)

var (
	dmRoutes  sync.Map
	dmRouteID atomic.Uint64
)

func init() {
	// DM 的多端点拨号会丢弃调用方的 context。用每个 Client 的地址键
	// 找到对应的 SSH 通道，避免驱动直接拨目标地址。
	dm.RegisterDialContext("ferry", func(ctx context.Context, addr string) (net.Conn, error) {
		client, ok := dmRoutes.Load(addr)
		if !ok {
			return nil, errors.New("达梦连接已关闭，请重试")
		}
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		return client.(*Client).dial(ctx)
	})
}

const dmDatabase = "DM"

func (cl *Client) damengPool(name string) (*sql.DB, error) {
	if name != dmDatabase {
		return nil, errors.New("达梦使用实例中的模式，请选择 DM")
	}
	if strings.Contains(cl.Conn.User, ":") {
		return nil, errors.New("达梦驱动的用户名不能包含冒号")
	}
	q := url.Values{
		"dialName":       {"ferry"},
		"connectTimeout": {strconv.FormatInt(dialTimeout.Milliseconds(), 10)},
		"autoCommit":     {"true"},
	}
	if cl.dmRoute == "" {
		cl.dmRoute = net.JoinHostPort(fmt.Sprintf("ferry-dm-%x", dmRouteID.Add(1)), strconv.Itoa(cl.Conn.Port))
		dmRoutes.Store(cl.dmRoute, cl)
	}
	// DM 驱动按最后一个 @ 和 ? 拆分 DSN，用户名和密码不做 URL 解码。
	dsn := fmt.Sprintf("dm://%s:%s@%s?%s", cl.Conn.User, cl.password, cl.dmRoute, q.Encode())
	cn, err := (&dm.DmDriver{}).OpenConnector(dsn)
	if err != nil {
		return nil, errors.New("达梦连接参数无效，请检查地址和端口")
	}
	return sql.OpenDB(cn), nil
}

func damengErrorCode(err error) string {
	var e *dm.DmError
	if errors.As(err, &e) {
		return strconv.FormatInt(int64(e.ErrCode), 10)
	}
	return ""
}

func damengTables(ctx context.Context, db *sql.DB) ([]Table, error) {
	rows, err := db.QueryContext(ctx, `SELECT OWNER, TABLE_NAME, 'table' FROM ALL_TABLES
		WHERE OWNER NOT IN ('SYS','SYSAUDITOR','SYSSSO','SYSJOB','CTISYS','SYSSTATS')
		OR OWNER = USER
		UNION ALL
		SELECT OWNER, VIEW_NAME, 'view' FROM ALL_VIEWS
		WHERE OWNER NOT IN ('SYS','SYSAUDITOR','SYSSSO','SYSJOB','CTISYS','SYSSTATS')
		OR OWNER = USER
		ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Table{}
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Schema, &t.Name, &t.Type); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func damengTable(ctx context.Context, db *sql.DB, schema, name string) (*TableInfo, error) {
	if schema == "" {
		if err := db.QueryRowContext(ctx, "SELECT USER FROM DUAL").Scan(&schema); err != nil {
			return nil, err
		}
	}
	ti := &TableInfo{Name: name, Schema: schema, Type: "table"}
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, NULLABLE
		FROM ALL_TAB_COLUMNS WHERE OWNER = ? AND TABLE_NAME = ? ORDER BY COLUMN_ID`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ColumnInfo
		var nullable string
		if err := rows.Scan(&c.Name, &c.Type, &nullable); err != nil {
			return nil, err
		}
		c.Nullable = nullable == "Y"
		ti.Columns = append(ti.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ti.Columns) == 0 {
		return nil, fmt.Errorf("找不到达梦表或视图 %s.%s", schema, name)
	}
	ti.PrimaryKey, err = strings1(ctx, db, `SELECT cc.COLUMN_NAME FROM ALL_CONSTRAINTS c
		JOIN ALL_CONS_COLUMNS cc ON c.OWNER = cc.OWNER AND c.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		WHERE c.CONSTRAINT_TYPE = 'P' AND c.OWNER = ? AND c.TABLE_NAME = ? ORDER BY cc.POSITION`, schema, name)
	if err != nil {
		return nil, err
	}
	var view int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ALL_VIEWS WHERE OWNER = ? AND VIEW_NAME = ?", schema, name).Scan(&view); err != nil {
		return nil, err
	}
	if view > 0 {
		ti.Type = "view"
	}
	return ti, nil
}
