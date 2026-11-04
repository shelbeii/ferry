package dbx

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/captain/ferry/internal/safe"
)

// Tunnel 经服务器转发（engine.Tunnel）。
type Tunnel interface {
	Dial(ctx context.Context, addr string) (net.Conn, error)
	Alive() bool
	Close()
	ContainerAddr(ctx context.Context, container string, port int) (string, error)
}

// OpenTunnel 连上 Via 指定的服务器。
type OpenTunnel func(v *Via) (Tunnel, error)

// IdleClose 连接空闲这么久后关闭（桌面版里；命令行每次用完就关）。
const IdleClose = 10 * time.Minute

const dialTimeout = 15 * time.Second

// Client 一个数据库连接打开着的状态：各个库的连接池、查询窗口的会话、经服务器时的通道。
type Client struct {
	Conn     Conn
	key      string
	password string
	tn       Tunnel
	addr     string // 实际连的地址（容器时是查到的容器 IP）

	mu       sync.Mutex
	dbs      map[string]*sql.DB
	sessions map[string]*session
	users    int
	lastUsed time.Time
	closed   bool
	Version  string
}

type session struct {
	db       string
	conn     *sql.Conn
	lastUsed time.Time
}

// Manager 打开着的连接。桌面版（ferry serve）里请求之间复用；命令行下用完就关。
type Manager struct {
	mu      sync.Mutex
	clients map[string]*Client // 连接 ID → 状态
	keep    bool
	once    sync.Once
}

// Default ferry serve 和命令行共用；serve 启动时调 Default.Keep()。
var Default = &Manager{clients: map[string]*Client{}}

// Keep 请求之间留着连接，空闲 IdleClose 后关闭。
func (m *Manager) Keep() {
	m.mu.Lock()
	m.keep = true
	m.mu.Unlock()
	m.once.Do(func() { safe.Go("关闭空闲的数据库连接", m.reap) })
}

func (m *Manager) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		for id, c := range m.clients {
			c.mu.Lock()
			idle := c.users == 0 && time.Since(c.lastUsed) > IdleClose
			// 查询窗口的会话单独按空闲时间关
			for sid, s := range c.sessions {
				if time.Since(s.lastUsed) > IdleClose {
					s.conn.Close()
					delete(c.sessions, sid)
				}
			}
			c.mu.Unlock()
			if idle || (c.tn != nil && !c.tn.Alive() && c.users == 0) {
				delete(m.clients, id)
				c.close()
			}
		}
		m.mu.Unlock()
	}
}

func clientKey(c *Conn, password string) string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(append(b, []byte("\x00"+password)...))
	return hex.EncodeToString(h[:])
}

// Open 取得打开着的连接（复用或新建）；用完调用 release。
func (m *Manager) Open(ctx context.Context, c *Conn, password string, open OpenTunnel) (*Client, func(), error) {
	key := clientKey(c, password)
	m.mu.Lock()
	cl := m.clients[c.ID]
	if cl != nil && (cl.key != key || (cl.tn != nil && !cl.tn.Alive())) {
		delete(m.clients, c.ID)
		cl.mu.Lock()
		busy := cl.users > 0
		cl.mu.Unlock()
		if !busy {
			cl.close()
		}
		cl = nil
	}
	if cl != nil {
		cl.mu.Lock()
		cl.users++
		cl.mu.Unlock()
		m.mu.Unlock()
		return cl, m.releaser(cl), nil
	}
	m.mu.Unlock()

	cl = &Client{Conn: *c, key: key, password: password, dbs: map[string]*sql.DB{}, sessions: map[string]*session{}, users: 1, lastUsed: time.Now()}
	cl.addr = c.Addr()
	if c.Via != nil {
		tn, err := open(c.Via)
		if err != nil {
			return nil, nil, err
		}
		cl.tn = tn
		if c.Container != "" {
			addr, err := tn.ContainerAddr(ctx, c.Container, c.Port)
			if err != nil {
				tn.Close()
				return nil, nil, err
			}
			cl.addr = addr
		}
	}
	// 先连一次：密码不对、连不上时现在就报错
	db, err := cl.DB(ctx, "")
	if err == nil {
		err = pingVersion(ctx, cl, db)
	}
	if err != nil {
		cl.close()
		return nil, nil, err
	}
	m.mu.Lock()
	if m.keep {
		if old := m.clients[c.ID]; old != nil {
			old.close()
		}
		m.clients[c.ID] = cl
	}
	keep := m.keep
	m.mu.Unlock()
	if !keep {
		return cl, func() { cl.close() }, nil
	}
	return cl, m.releaser(cl), nil
}

func pingVersion(ctx context.Context, cl *Client, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout+5*time.Second)
	defer cancel()
	var v string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return cl.explain(err)
	}
	cl.Version = v
	return nil
}

func (m *Manager) releaser(cl *Client) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			cl.mu.Lock()
			cl.users--
			cl.lastUsed = time.Now()
			cl.mu.Unlock()
		})
	}
}

// Close 关闭这个连接打开着的一切（改了、删了连接，或用户点了断开）。id 为空时全部关闭。
func (m *Manager) Close(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.clients {
		if id == "" || k == id {
			delete(m.clients, k)
			c.close()
		}
	}
}

// Opened 打开着的连接 ID。
func (m *Manager) Opened() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for k := range m.clients {
		out = append(out, k)
	}
	return out
}

// CloseSession 关闭查询窗口的会话。
func (m *Manager) CloseSession(id, sid string) {
	m.mu.Lock()
	cl := m.clients[id]
	m.mu.Unlock()
	if cl == nil {
		return
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if s := cl.sessions[sid]; s != nil {
		s.conn.Close()
		delete(cl.sessions, sid)
	}
}

func (cl *Client) close() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.closed {
		return
	}
	cl.closed = true
	for _, s := range cl.sessions {
		s.conn.Close()
	}
	for _, db := range cl.dbs {
		db.Close()
	}
	if cl.tn != nil {
		cl.tn.Close()
	}
}

func (cl *Client) dial(ctx context.Context) (net.Conn, error) {
	if cl.tn != nil {
		return cl.tn.Dial(ctx, cl.addr)
	}
	d := net.Dialer{Timeout: dialTimeout}
	return d.DialContext(ctx, "tcp", cl.addr)
}

// DB 某个库的连接池；name 为空时用连接的默认库。
func (cl *Client) DB(ctx context.Context, name string) (*sql.DB, error) {
	if name == "" {
		name = cl.Conn.Database
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.closed {
		return nil, errors.New("连接已关闭，请重试")
	}
	if db := cl.dbs[name]; db != nil {
		return db, nil
	}
	var db *sql.DB
	switch cl.Conn.Kind {
	case MySQL:
		cfg := mysql.NewConfig()
		cfg.User = cl.Conn.User
		cfg.Passwd = cl.password
		cfg.Net = "tcp"
		cfg.Addr = cl.addr
		cfg.DBName = name
		cfg.Timeout = dialTimeout
		cfg.InterpolateParams = true
		cfg.ClientFoundRows = true // UPDATE 的影响行数按匹配到的行算（值没变也算 1 行）
		cfg.AllowNativePasswords = true
		cfg.AllowCleartextPasswords = cl.tn != nil // 只在 SSH 通道里允许明文密码
		if err := cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
			return nil, err
		}
		cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return cl.dial(ctx) }
		cn, err := mysql.NewConnector(cfg)
		if err != nil {
			return nil, err
		}
		db = sql.OpenDB(cn)
	case Postgres:
		if name == "" {
			name = "postgres"
		}
		host, port, _ := net.SplitHostPort(cl.addr)
		cfg, err := pgx.ParseConfig("")
		if err != nil {
			return nil, err
		}
		cfg.Host = host
		fmt.Sscan(port, &cfg.Port)
		cfg.User = cl.Conn.User
		cfg.Password = cl.password
		cfg.Database = name
		cfg.ConnectTimeout = dialTimeout
		cfg.TLSConfig = nil // 经 SSH 通道或内网；不协商 TLS
		cfg.Fallbacks = nil
		cfg.RuntimeParams = map[string]string{"application_name": "Ferry"}
		cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
		cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return cl.dial(ctx) }
		cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) { return []string{host}, nil }
		db = stdlib.OpenDB(*cfg)
	default:
		return nil, fmt.Errorf("不支持的数据库种类 %q", cl.Conn.Kind)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	cl.dbs[name] = db
	return db, nil
}

// Session 查询窗口的会话（同一个窗口里的语句用同一条连接：USE、SET、事务都保留）。sid 为空时临时取一条。
func (cl *Client) Session(ctx context.Context, sid, dbName string) (*sql.Conn, func(), error) {
	if sid == "" {
		db, err := cl.DB(ctx, dbName)
		if err != nil {
			return nil, nil, err
		}
		c, err := db.Conn(ctx)
		if err != nil {
			return nil, nil, cl.explain(err)
		}
		return c, func() { c.Close() }, nil
	}
	cl.mu.Lock()
	s := cl.sessions[sid]
	if s != nil && s.db != dbName {
		s.conn.Close()
		delete(cl.sessions, sid)
		s = nil
	}
	cl.mu.Unlock()
	if s != nil {
		if err := s.conn.PingContext(ctx); err == nil {
			s.lastUsed = time.Now()
			return s.conn, func() {}, nil
		}
		cl.mu.Lock()
		s.conn.Close()
		delete(cl.sessions, sid)
		cl.mu.Unlock()
	}
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, nil, err
	}
	c, err := db.Conn(context.WithoutCancel(ctx))
	if err != nil {
		return nil, nil, cl.explain(err)
	}
	cl.mu.Lock()
	cl.sessions[sid] = &session{db: dbName, conn: c, lastUsed: time.Now()}
	cl.mu.Unlock()
	return c, func() {}, nil
}

// DropSession 会话出错断开了：下次重新建。
func (cl *Client) DropSession(sid string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if s := cl.sessions[sid]; s != nil {
		s.conn.Close()
		delete(cl.sessions, sid)
	}
}

// explain 把驱动的错误说成中文，密码不对时返回 NeedPasswordError。
func (cl *Client) explain(err error) error {
	if err == nil {
		return nil
	}
	if authFailed(err) {
		return &NeedPasswordError{ID: cl.Conn.ID, Name: cl.Conn.Name, Env: PasswordEnv(cl.Conn.ID), Rejected: true, Msg: err.Error()}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("连接 %s 超时：%w", cl.addr, err)
	}
	if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "connect: ") {
		where := "这台电脑"
		if cl.tn != nil {
			where = "服务器上"
		}
		return fmt.Errorf("连不上 %s（%s看，数据库没在运行或端口不对）：%w", cl.addr, where, err)
	}
	return err
}

func authFailed(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1045 || me.Number == 1698
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "28P01" || pe.Code == "28000"
	}
	return false
}

// ErrorCode 数据库返回的错误码（给界面显示）。
func ErrorCode(err error) string {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return fmt.Sprint(me.Number)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}
