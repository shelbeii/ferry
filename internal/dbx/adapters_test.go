package dbx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSQLServerStatements(t *testing.T) {
	got, err := sqlServerStatements(`/* outer /* nested ; DELETE */ comment */ SELECT [name;]]x], 'EXEC;''DROP',
  CASE WHEN 1 = 1 THEN 2 ELSE 3 END FROM [dbo].[test] ORDER BY 1 OFFSET 0 ROWS FETCH NEXT 3 ROWS ONLY;
WITH cte AS (SELECT N'中文') SELECT * FROM cte UNION ALL SELECT N'再见'; -- trailing`)
	if err != nil || len(got) != 2 {
		t.Fatalf("拆分查询：%v, %v", got, err)
	}
	for _, src := range []string{
		"SELECT 1; DELETE FROM dbo.t",
		"SELECT 1 UPDATE dbo.t SET n=1",
		"SELECT 1UPDATE dbo.t SET n=1",
		"SELECT 1\nEXEC sys.sp_help",
		"SELECT 1 SELECT 2",
		"SELECT * INTO dbo.t FROM dbo.source",
		"WITH cte AS (SELECT * FROM dbo.t) DELETE FROM cte",
		"SELECT NEXT VALUE FOR dbo.seq",
		"SELECT * FROM OPENROWSET('x', 'y', 'z')",
		"SELECT 1 /* unfinished",
		"SELECT [unfinished",
		"SELECT ('unbalanced'",
		"'not a query' SELECT 1",
		"/* comment */ ",
	} {
		if _, err := sqlServerStatements(src); err == nil {
			t.Errorf("应拒绝 %q", src)
		}
	}
}

func TestMongoFilter(t *testing.T) {
	const id = "62a4acdc4f418d06353e393c"
	f, err := mongoFilter(`{"_id":{"$oid":"` + id + `"},"n":{"$numberLong":"9007199254740993"}}`)
	if err != nil || len(f) != 2 || f[0].Value.(bson.ObjectID).Hex() != id || f[1].Value != int64(9007199254740993) {
		t.Fatalf("没有保留 BSON 类型：%v %v", f, err)
	}
	for _, src := range []string{"[]", "null", "{", "{}{}", `{"_id":{"$oid":"bad"}}`} {
		if _, err := mongoFilter(src); err == nil {
			t.Errorf("应拒绝 %q", src)
		}
	}
}

func TestBrowseOnlyConnections(t *testing.T) {
	for _, kind := range []string{MongoDB, SQLServer} {
		c := Conn{Kind: kind, Name: kind, Host: "localhost", Port: 1, User: "tester", AI: true, AIWrite: true, AIUser: "other"}
		if err := c.Check(); err != nil || !c.ReadOnly || c.AI || c.AIWrite || c.AIUser != "" {
			t.Fatalf("只读能力没生效：%+v, %v", c, err)
		}
	}
}

func liveAdapterConn(t *testing.T, kind, env, user string) Conn {
	t.Helper()
	addr := os.Getenv(env)
	if addr == "" {
		t.Skip("没有设置 " + env)
	}
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	c := Conn{ID: "live-" + kind, Name: kind, Kind: kind, Host: host, Port: port, User: user}
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
	return c
}

func openLiveAdapter(t *testing.T, ctx context.Context, c Conn, password string, via bool) *Client {
	t.Helper()
	var open OpenTunnel
	if via {
		c.Via = &Via{Config: "-", Target: "-"}
		open = func(*Via) (Tunnel, error) { return sshLikeTunnel{}, nil }
	}
	m := Manager{}
	cl, release, err := m.Open(ctx, &c, password, open)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return cl
}

// 专用测试库：需要可建库的账号。FERRY_SQLSERVER_LIVE=host:port、FERRY_SQLSERVER_PASSWORD。
func TestSQLServerLive(t *testing.T) {
	c := liveAdapterConn(t, SQLServer, "FERRY_SQLSERVER_LIVE", "sa")
	pw := os.Getenv("FERRY_SQLSERVER_PASSWORD")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m := &Manager{}
	var passwordError *NeedPasswordError
	if _, release, err := m.Open(ctx, &c, "invalid-test-password", nil); !errors.As(err, &passwordError) {
		if release != nil {
			release()
		}
		t.Fatalf("错误密码：%v", err)
	}
	client := openLiveAdapter(t, ctx, c, pw, false)
	if !client.Encrypted || !strings.HasPrefix(client.Version, "SQL Server ") {
		t.Fatalf("连接信息：%q, TLS=%v", client.Version, client.Encrypted)
	}
	master, err := client.DB(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ferry_live_%d", time.Now().UnixNano())
	qname := Quote(SQLServer, name)
	if _, err := master.ExecContext(ctx, "CREATE DATABASE "+qname); err != nil {
		t.Fatal(err)
	}
	reader := name + "_reader"
	qreader := Quote(SQLServer, reader)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := master.ExecContext(cleanup, "ALTER DATABASE "+qname+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE "+qname); err != nil {
			t.Error(err)
		}
	})
	const readerPassword = "FerryLive#75@;:=/[]"
	if _, err := master.ExecContext(ctx, "CREATE LOGIN "+qreader+" WITH PASSWORD = '"+readerPassword+"'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := master.ExecContext(cleanup, "DROP LOGIN "+qreader); err != nil {
			t.Error(err)
		}
	})
	db, err := client.DB(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE SCHEMA [test schema]",
		"CREATE TABLE [test schema].[a]]b] (id int PRIMARY KEY, name nvarchar(20) NULL, n decimal(19,4) DEFAULT 0)",
		"INSERT INTO [test schema].[a]]b] (id, name) VALUES (2, N'中文'), (1, N'one'), (3, NULL)",
		"CREATE VIEW [test schema].[read view] AS SELECT id, name FROM [test schema].[a]]b]",
		"CREATE USER " + qreader + " FOR LOGIN " + qreader,
		"ALTER ROLE db_datareader ADD MEMBER " + qreader,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	c.Database, c.User = name, reader
	via := openLiveAdapter(t, ctx, c, readerPassword, true)
	databases, err := via.Databases(ctx)
	if err != nil || !slices.ContainsFunc(databases, func(d Database) bool { return d.Name == name && !d.System }) {
		t.Fatalf("数据库列表：%v, %v", databases, err)
	}
	tables, err := via.Tables(ctx, name)
	if err != nil || len(tables) != 2 || tables[0].Name != "a]b" || tables[1].Type != "view" {
		t.Fatalf("表与视图：%v, %v", tables, err)
	}
	ti, err := via.TableInfo(ctx, name, "test schema", "a]b")
	if err != nil || len(ti.Columns) != 3 || ti.Columns[1].Type != "nvarchar(20)" || !slices.Equal(ti.PrimaryKey, []string{"id"}) {
		t.Fatalf("列结构：%+v, %v", ti, err)
	}
	r, err := via.Rows(ctx, name, "test schema", "a]b", BrowseOptions{Limit: 1, Offset: 1})
	if err != nil || r.Error != "" || !r.More || len(r.Rows) != 1 || r.Rows[0][1] != "中文" {
		t.Fatalf("分页：%+v, %v", r, err)
	}
	got, err := via.Query(ctx, name, r.SQL+"; SELECT COUNT(*) FROM [test schema].[a]]b]", QueryOptions{})
	if err != nil || len(got) != 2 || len(got[0].Rows) != 1 || got[1].Error != "" || got[1].Rows[0][0] != "3" {
		t.Fatalf("运行查询：%+v, %v", got, err)
	}
	if _, err := via.Query(ctx, name, "SELECT 1; DROP TABLE [test schema].[a]]b]", QueryOptions{}); err == nil {
		t.Fatal("只读批次允许了 DROP")
	}
	got, err = via.Query(ctx, name, `SELECT CAST('01234567-89ab-cdef-0123-456789abcdef' AS uniqueidentifier),
CAST('2026-10-08 08:30:00.1234567 +08:00' AS datetimeoffset)`, QueryOptions{})
	if err != nil || len(got) != 1 || got[0].Error != "" || !slices.Equal(got[0].Rows[0],
		[]any{"01234567-89AB-CDEF-0123-456789ABCDEF", "2026-10-08 08:30:00.1234567+08:00"}) {
		t.Fatalf("GUID 与带时区日期：%+v, %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "out.csv")
	if _, err := via.Export(ctx, name, ExportOptions{Out: path, Query: "SELECT name FROM [test schema].[a]]b] ORDER BY id"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := via.Export(ctx, name, ExportOptions{Out: path, Query: "SELECT * FROM [bad table]"}, nil); err == nil {
		t.Fatal("无效查询导出成功")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("中文")) {
		t.Fatalf("CSV：%q, %v", data, err)
	}
}

// 专用测试实例：需要能写入、删除临时数据库。FERRY_MONGO_LIVE、FERRY_MONGO_USER、FERRY_MONGO_PASSWORD。
func TestMongoDBLive(t *testing.T) {
	c := liveAdapterConn(t, MongoDB, "FERRY_MONGO_LIVE", os.Getenv("FERRY_MONGO_USER"))
	pw := os.Getenv("FERRY_MONGO_PASSWORD")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if c.User != "" {
		var passwordError *NeedPasswordError
		m := &Manager{}
		if _, release, err := m.Open(ctx, &c, "invalid-test-password", nil); !errors.As(err, &passwordError) {
			if release != nil {
				release()
			}
			t.Fatalf("错误密码：%v", err)
		}
	}
	direct := openLiveAdapter(t, ctx, c, pw, false)
	name := fmt.Sprintf("ferry_live_%d", time.Now().UnixNano())
	db := direct.mongo.Database(name)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.Drop(cleanup); err != nil {
			t.Error(err)
		}
	})
	id := bson.NewObjectID()
	const collection = "文档"
	_, err := db.Collection(collection).InsertMany(ctx, []bson.M{
		{"_id": id, "n": int64(9007199254740993), "tag": "keep"},
		{"_id": bson.NewObjectID(), "tag": "keep"},
		{"_id": bson.NewObjectID(), "tag": "skip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Database = name
	via := openLiveAdapter(t, ctx, c, pw, true)
	databases, err := via.Databases(ctx)
	if err != nil || !slices.ContainsFunc(databases, func(d Database) bool { return d.Name == name }) {
		t.Fatalf("数据库列表：%v, %v", databases, err)
	}
	tables, err := via.Tables(ctx, "")
	if err != nil || len(tables) != 1 || tables[0].Name != collection {
		t.Fatalf("集合列表：%v, %v", tables, err)
	}
	filter := `{"tag":"keep"}`
	r, err := via.MongoDocuments(ctx, "", collection, MongoFindOptions{Filter: filter, Limit: 1})
	if err != nil || len(r.Documents) != 1 || !r.More || !strings.Contains(r.Documents[0].JSON, id.Hex()) {
		t.Fatalf("第一页：%+v, %v", r, err)
	}
	r, err = via.MongoDocuments(ctx, "", collection, MongoFindOptions{Filter: filter, Limit: 1, Skip: 1})
	if err != nil || len(r.Documents) != 1 || r.More {
		t.Fatalf("第二页：%+v, %v", r, err)
	}
	path := filepath.Join(t.TempDir(), "docs.json")
	out, err := via.MongoExport(ctx, "", collection, filter, path, nil)
	if err != nil || out.Rows != 2 {
		t.Fatalf("导出：%+v, %v", out, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) || !bytes.Contains(data, []byte(`"$numberLong":"9007199254740993"`)) ||
		!bytes.Contains(data, []byte(`"$oid":"`+id.Hex()+`"`)) {
		t.Fatalf("Extended JSON：%q, %v", data, err)
	}
}
