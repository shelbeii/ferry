package dbx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// poolDriver 假的数据库驱动：连接什么都能做；查询返回 rows 行、每行一列 width 字节的文本。
type poolDriver struct{ rows, width int }
type poolConn struct{ d *poolDriver }
type poolRows struct {
	d *poolDriver
	n int
}

func (d *poolDriver) Open(string) (driver.Conn, error)  { return &poolConn{d}, nil }
func (c *poolConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *poolConn) Close() error                        { return nil }
func (c *poolConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (c *poolConn) Ping(context.Context) error          { return nil }
func (c *poolConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &poolRows{d: c.d}, nil
}
func (r *poolRows) Columns() []string { return []string{"v"} }
func (r *poolRows) Close() error      { return nil }
func (r *poolRows) Next(dest []driver.Value) error {
	if r.n >= r.d.rows {
		return io.EOF
	}
	r.n++
	dest[0] = strings.Repeat("x", r.d.width)
	return nil
}

var registerPoolDriver sync.Once
var thePoolDriver = &poolDriver{rows: 1000, width: 1 << 20}

// testClient 三个池都用假驱动（键和 pool() 里的一致）。
func testClient(t *testing.T) *Client {
	registerPoolDriver.Do(func() { sql.Register("ferry-pools", thePoolDriver) })
	cl := &Client{Conn: Conn{Kind: Postgres, Database: "d"}, dbs: map[string]*sql.DB{}, sessions: map[string]*session{}}
	for role, n := range map[poolRole]int{rolePlain: 4, roleSession: maxSessionsPerDB, roleControl: 2} {
		db, _ := sql.Open("ferry-pools", "")
		db.SetMaxOpenConns(n)
		key := "d"
		if role != rolePlain {
			key = fmt.Sprintf("d\x00%d", role)
		}
		cl.dbs[key] = db
	}
	t.Cleanup(cl.close)
	return cl
}

// 查询窗口占着的连接不影响浏览表、读结构（普通的池），也不影响停止语句。
func TestSessionsDoNotStarvePlainPool(t *testing.T) {
	cl := testClient(t)
	for i := range maxSessionsPerDB {
		if _, _, err := cl.Session(context.Background(), fmt.Sprint(i), "d"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	for _, role := range []poolRole{rolePlain, roleControl} {
		db, err := cl.pool("d", role)
		if err != nil {
			t.Fatal(err)
		}
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("查询窗口开满后，%d 号池拿不到连接：%v", role, err)
		}
		c.Close()
	}
	// 再开一个窗口：马上说清楚，不一直等
	start := time.Now()
	_, _, err := cl.Session(context.Background(), "more", "d")
	if err == nil || !strings.Contains(err.Error(), "查询窗口") {
		t.Fatalf("窗口太多时应该报错：%v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("等了 %v", d)
	}
	// 关掉一个窗口后能再开
	cl.DropSession("0")
	if _, _, err := cl.Session(context.Background(), "more", "d"); err != nil {
		t.Fatal(err)
	}
}

// 等查询窗口的连接时可以取消。
func TestSessionWaitHonorsContext(t *testing.T) {
	cl := testClient(t)
	db, _ := cl.pool("d", roleSession)
	db.SetMaxOpenConns(1)
	busy, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := cl.Session(ctx, "w", "d"); err == nil {
		t.Fatal("应该失败")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("取消后还等了 %v", d)
	}
}

// 结果内容合计超过上限时像行数到了上限一样停下。
func TestReadRowsByteBudget(t *testing.T) {
	cl := testClient(t)
	old := maxResultBytes
	maxResultBytes = 8 << 20
	defer func() { maxResultBytes = old }()
	db, _ := cl.DB(context.Background(), "d")
	rows, err := db.Query("select")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	_, data, more, err := readRows(rows, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !more || len(data) > 40 {
		t.Fatalf("应该在约 8 MB 处停下：%d 行，more=%v", len(data), more)
	}
	if c, ok := data[0][0].(Cut); !ok || len(c.Text) != MaxCell {
		t.Fatalf("大单元格应该截断：%T", data[0][0])
	}
}
