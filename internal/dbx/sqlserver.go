package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

type dbDialer struct{ cl *Client }

func (d dbDialer) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	return d.cl.dialAddr(ctx, addr)
}

// HostName 告诉 SQL Server 驱动由 DialContext 解析地址，SSH 通道使用服务器侧 DNS。
func (dbDialer) HostName() string { return "" }

func (cl *Client) sqlServerPool(dbName string) (*sql.DB, error) {
	mode := cl.tlsMode()
	q := url.Values{
		"database": {dbName}, "app name": {"Ferry"},
		"encrypt": {"true"}, "TrustServerCertificate": {"true"},
	}
	if mode == TLSOff {
		q.Set("encrypt", "false") // SQL Server 可以要求加密，客户端仍会配合
	}
	if mode == TLSVerify {
		q.Set("TrustServerCertificate", "false")
		if host := strings.Trim(cl.Conn.Host, "[]"); host != "" {
			q.Set("hostNameInCertificate", host)
		}
	}
	u := url.URL{Scheme: "sqlserver", Host: cl.addr, User: url.UserPassword(cl.Conn.User, cl.password), RawQuery: q.Encode()}
	cn, err := mssql.NewConnector(u.String())
	if err != nil {
		// 驱动的 DSN 解析错误可能回显连接串。
		return nil, errors.New("SQL Server 连接参数无效，请检查地址和端口")
	}
	cn.Dialer = dbDialer{cl}
	return sql.OpenDB(cn), nil
}

func sqlServerAuthFailed(err error) bool {
	var e mssql.Error
	return errors.As(err, &e) && (e.Number == 18456 || e.Number == 18452 || e.Number == 18486)
}

func sqlServerErrorCode(err error) string {
	var e mssql.Error
	if errors.As(err, &e) {
		return fmt.Sprint(e.Number)
	}
	return ""
}

func sqlServerUUID(raw []byte) (string, bool) {
	var id mssql.UniqueIdentifier
	if err := id.Scan(raw); err != nil {
		return "", false
	}
	return id.String(), true
}

func sqlServerDatabases(ctx context.Context, db *sql.DB) ([]Database, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, database_id FROM sys.databases WHERE state = 0 AND HAS_DBACCESS(name) = 1 ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Database{}
	for rows.Next() {
		var d Database
		var id int
		if err := rows.Scan(&d.Name, &id); err != nil {
			return nil, err
		}
		d.System = id <= 4
		out = append(out, d)
	}
	return out, rows.Err()
}

func sqlServerTables(ctx context.Context, db *sql.DB) ([]Table, error) {
	const q = `SELECT s.name, o.name, CASE o.type WHEN 'V' THEN 'view' ELSE 'table' END
FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id
WHERE o.type IN ('U', 'V') AND o.is_ms_shipped = 0 ORDER BY s.name, o.name`
	rows, err := db.QueryContext(ctx, q)
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

func sqlServerTable(ctx context.Context, db *sql.DB, schema, table string) (*TableInfo, error) {
	if schema == "" {
		schema = "dbo"
	}
	const q = `SELECT CASE o.type WHEN 'V' THEN 'view' ELSE 'table' END,
 c.name, t.name, t.is_user_defined, c.max_length, c.precision, c.scale, c.is_nullable,
 c.is_identity, c.is_computed, dc.definition, ISNULL(ic.key_ordinal, 0)
FROM sys.objects o
JOIN sys.schemas s ON s.schema_id = o.schema_id
JOIN sys.columns c ON c.object_id = o.object_id
JOIN sys.types t ON t.user_type_id = c.user_type_id
LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
LEFT JOIN sys.indexes ix ON ix.object_id = o.object_id AND ix.is_primary_key = 1
LEFT JOIN sys.index_columns ic ON ic.object_id = ix.object_id AND ic.index_id = ix.index_id AND ic.column_id = c.column_id
WHERE s.name = @p1 AND o.name = @p2 AND o.type IN ('U', 'V') AND o.is_ms_shipped = 0
ORDER BY c.column_id`
	rows, err := db.QueryContext(ctx, q, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ti := &TableInfo{Schema: schema, Name: table, Columns: []ColumnInfo{}, Indexes: []IndexInfo{}, Foreign: []ForeignKey{}, PrimaryKey: []string{}}
	keys := map[int]string{}
	for rows.Next() {
		var c ColumnInfo
		var length, precision, scale, key int
		var custom bool
		if err := rows.Scan(&ti.Type, &c.Name, &c.Type, &custom, &length, &precision, &scale,
			&c.Nullable, &c.Auto, &c.Generated, &c.Default, &key); err != nil {
			return nil, err
		}
		if !custom {
			c.Type = sqlServerColumnType(c.Type, length, precision, scale)
		}
		if key > 0 {
			c.Key = true
			keys[key] = c.Name
		}
		ti.Columns = append(ti.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ti.Columns) == 0 {
		return nil, fmt.Errorf("找不到表或视图 %s", TableName(SQLServer, "", schema, table))
	}
	for i := 1; i <= len(keys); i++ {
		ti.PrimaryKey = append(ti.PrimaryKey, keys[i])
	}
	return ti, nil
}

func sqlServerColumnType(name string, length, precision, scale int) string {
	switch name {
	case "nchar", "nvarchar":
		if length > 0 {
			length /= 2
		}
		fallthrough
	case "char", "varchar", "binary", "varbinary":
		if length == -1 {
			return name + "(max)"
		}
		return fmt.Sprintf("%s(%d)", name, length)
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", name, precision, scale)
	case "datetime2", "datetimeoffset", "time":
		return fmt.Sprintf("%s(%d)", name, scale)
	default:
		return name
	}
}
