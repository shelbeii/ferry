package dbx

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func (cl *Client) clickHousePool(name string, readOnly bool) *sql.DB {
	o := &clickhouse.Options{
		Addr:        []string{cl.addr},
		Auth:        clickhouse.Auth{Database: name, Username: cl.Conn.User, Password: cl.password},
		DialTimeout: dialTimeout,
	}
	if readOnly {
		o.Settings = clickhouse.Settings{"readonly": 1}
	}
	if cl.tlsMode() != TLSOff {
		o.TLS = cl.tlsConfig()
	}
	if cl.tn != nil {
		// ClickHouse 的原生协议在自定义 DialContext 时由调用方握手。
		o.DialContext = func(ctx context.Context, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			c, err := cl.dialAddr(ctx, addr)
			if err != nil || o.TLS == nil {
				return c, err
			}
			cfg := o.TLS.Clone()
			if cfg.ServerName == "" {
				cfg.ServerName, _, _ = net.SplitHostPort(addr)
			}
			tc := tls.Client(c, cfg)
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				return nil, err
			}
			return tc, nil
		}
	}
	return clickhouse.OpenDB(o)
}

func clickHouseAuthFailed(err error) bool {
	var e *clickhouse.Exception
	return errors.As(err, &e) && e.Code == 516
}

func clickHouseErrorCode(err error) string {
	var e *clickhouse.Exception
	if errors.As(err, &e) {
		return fmt.Sprint(e.Code)
	}
	return ""
}

func clickHouseTables(ctx context.Context, db *sql.DB, name string) ([]Table, error) {
	const q = `SELECT name, engine, comment FROM system.tables
WHERE database = ? AND is_temporary = 0 ORDER BY name`
	rows, err := db.QueryContext(ctx, q, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Table{}
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Name, &t.Engine, &t.Comment); err != nil {
			return nil, err
		}
		t.Type = clickHouseObjectType(t.Engine)
		out = append(out, t)
	}
	return out, rows.Err()
}

func clickHouseObjectType(engine string) string {
	if strings.HasSuffix(engine, "View") {
		return "view"
	}
	return "table"
}

func clickHouseTable(ctx context.Context, db *sql.DB, name, table string) (*TableInfo, error) {
	ti := &TableInfo{Name: table, Columns: []ColumnInfo{}, PrimaryKey: []string{}}
	err := db.QueryRowContext(ctx, `SELECT engine, comment FROM system.tables
WHERE database = ? AND name = ? AND is_temporary = 0`, name, table).Scan(&ti.Engine, &ti.Comment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("找不到 ClickHouse 表或视图 %q", table)
	}
	if err != nil {
		return nil, err
	}
	ti.Type = clickHouseObjectType(ti.Engine)
	rows, err := db.QueryContext(ctx, `SELECT name, type, default_kind, default_expression, comment, is_in_primary_key
FROM system.columns WHERE database = ? AND table = ? ORDER BY position`, name, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ColumnInfo
		var defKind, def string
		var pk uint8
		if err := rows.Scan(&c.Name, &c.Type, &defKind, &def, &c.Comment, &pk); err != nil {
			return nil, err
		}
		c.Nullable = strings.HasPrefix(c.Type, "Nullable(") || strings.HasPrefix(c.Type, "LowCardinality(Nullable(")
		c.Generated = defKind == "ALIAS" || defKind == "MATERIALIZED" || defKind == "EPHEMERAL"
		if defKind != "" {
			c.Default = &def
		}
		if c.Key = pk != 0; c.Key {
			ti.PrimaryKey = append(ti.PrimaryKey, c.Name)
		}
		ti.Columns = append(ti.Columns, c)
	}
	return ti, rows.Err()
}
