package dbx

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

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
	// stale 已经换成新的了（改了连接、通道断了）：没人用了就关掉
	stale   bool
	Version string
	// Encrypted 连接加密了（TLS；经 SSH 的不算）
	Encrypted bool

	// Redis：各个库的连接池、命令行窗口的连接、命令是否只读（COMMAND INFO 查过的）
	rdbs         map[int]*redis.Client
	rsess        map[string]*rsession
	rcmdRO       map[string]bool
	redisCluster bool
}

type session struct {
	db       string
	conn     *sql.Conn
	id       string // 这条连接在数据库里的编号（停止语句时用，见 connID）
	lastUsed time.Time
}

// Manager 打开着的连接。桌面版（ferry serve）里请求之间复用；命令行下用完就关。
type Manager struct {
	mu      sync.Mutex
	clients map[string]*Client // 连接 ID → 状态
	// opening 正在打开的连接：同一个连接同时来的请求等它打开完再复用（不然后一个会把前一个正在用的关掉）
	opening map[string]chan struct{}
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
			dead := c.users == 0 && c.tn != nil && !c.tn.Alive()
			// 查询窗口的会话单独按空闲时间关
			for sid, s := range c.sessions {
				if time.Since(s.lastUsed) > IdleClose {
					s.conn.Close()
					delete(c.sessions, sid)
				}
			}
			for sid, s := range c.rsess {
				if time.Since(s.lastUsed) > IdleClose {
					s.close()
					delete(c.rsess, sid)
				}
			}
			c.mu.Unlock()
			if idle || dead {
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
	for {
		m.mu.Lock()
		if ch := m.opening[c.ID]; ch != nil {
			m.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		cl := m.clients[c.ID]
		if cl != nil && (cl.key != key || (cl.tn != nil && !cl.tn.Alive())) {
			delete(m.clients, c.ID)
			cl.retire()
			cl = nil
		}
		if cl != nil {
			cl.mu.Lock()
			cl.users++
			cl.mu.Unlock()
			m.mu.Unlock()
			return cl, m.releaser(cl), nil
		}
		if !m.keep {
			m.mu.Unlock()
			cl, err := connect(ctx, c, key, password, open)
			if err != nil {
				return nil, nil, err
			}
			return cl, func() { cl.close() }, nil
		}
		ch := make(chan struct{})
		if m.opening == nil {
			m.opening = map[string]chan struct{}{}
		}
		m.opening[c.ID] = ch
		m.mu.Unlock()

		cl, err := connect(ctx, c, key, password, open)
		m.mu.Lock()
		delete(m.opening, c.ID)
		close(ch)
		if err == nil {
			if old := m.clients[c.ID]; old != nil {
				old.retire()
			}
			m.clients[c.ID] = cl
		}
		m.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		return cl, m.releaser(cl), nil
	}
}

// connect 新建连接：经服务器时先打开通道，再连一次数据库（密码不对、连不上时现在就报错）。
func connect(ctx context.Context, c *Conn, key, password string, open OpenTunnel) (*Client, error) {
	cl := &Client{Conn: *c, key: key, password: password, dbs: map[string]*sql.DB{}, sessions: map[string]*session{}, users: 1, lastUsed: time.Now()}
	cl.addr = c.Addr()
	if c.Via != nil {
		tn, err := open(c.Via)
		if err != nil {
			return nil, err
		}
		cl.tn = tn
		if c.Container != "" {
			addr, err := tn.ContainerAddr(ctx, c.Container, c.Port)
			if err != nil {
				tn.Close()
				return nil, err
			}
			cl.addr = addr
		}
	}
	var err error
	if c.Kind == Redis {
		err = cl.redisPing(ctx)
	} else {
		var db *sql.DB
		if db, err = cl.DB(ctx, ""); err == nil {
			err = pingVersion(ctx, cl, db)
		}
	}
	if err != nil {
		cl.close()
		return nil, err
	}
	return cl, nil
}

func pingVersion(ctx context.Context, cl *Client, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout+5*time.Second)
	defer cancel()
	var v string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return cl.explain(err)
	}
	cl.Version = v
	cl.checkEncrypted(ctx, db)
	return nil
}

func (m *Manager) releaser(cl *Client) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			cl.mu.Lock()
			cl.users--
			cl.lastUsed = time.Now()
			gone := cl.stale && cl.users == 0
			cl.mu.Unlock()
			if gone {
				cl.close()
			}
		})
	}
}

// Close 关闭这个连接打开着的一切（改了、删了连接，或用户点了断开），包括 AI 用单独账号打开的（ID#ai）。id 为空时全部关闭。
func (m *Manager) Close(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.clients {
		if id == "" || k == id || strings.HasPrefix(k, id+"#") {
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
	if s := cl.rsess[sid]; s != nil {
		s.close()
		delete(cl.rsess, sid)
	}
}

// retire 已经换成新的了：没人在用就现在关，有人在用就等用完再关。
func (cl *Client) retire() {
	cl.mu.Lock()
	cl.stale = true
	idle := cl.users == 0
	cl.mu.Unlock()
	if idle {
		cl.close()
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
	for _, s := range cl.rsess {
		s.close()
	}
	for _, r := range cl.rdbs {
		r.Close()
	}
	if cl.tn != nil {
		cl.tn.Close()
	}
}

func (cl *Client) dial(ctx context.Context) (net.Conn, error) {
	if cl.tn != nil {
		c, err := cl.tn.Dial(ctx, cl.addr)
		if err != nil {
			return nil, err
		}
		return withDeadlines(c), nil
	}
	d := net.Dialer{Timeout: dialTimeout}
	return d.DialContext(ctx, "tcp", cl.addr)
}

// withDeadlines 经 SSH 转发的连接不支持读写超时（SetDeadline 报 ssh: tcpChan: deadline not supported），
// go-redis 设超时失败就不用这个连接。中间接一个 net.Pipe（支持超时）、两头对拷：超时照常生效，
// 超时后客户端关掉连接，对拷的两头也跟着关。
func withDeadlines(c net.Conn) net.Conn {
	if c.SetDeadline(time.Time{}) == nil {
		return c
	}
	a, b := net.Pipe()
	safe.Go("数据库连接对拷", func() {
		io.Copy(c, b)
		c.Close()
		b.Close()
	})
	safe.Go("数据库连接对拷", func() {
		io.Copy(b, c)
		b.Close()
		c.Close()
	})
	return &pipeConn{Conn: a, local: c.LocalAddr(), remote: c.RemoteAddr()}
}

// pipeConn 地址用原来连接的（net.Pipe 的地址是 pipe）。
type pipeConn struct {
	net.Conn
	local, remote net.Addr
}

func (p *pipeConn) LocalAddr() net.Addr  { return p.local }
func (p *pipeConn) RemoteAddr() net.Addr { return p.remote }

// 每个库分三个连接池，互不占用：
//   - 普通的（浏览表、读结构、AI 查询……）：用完就还，最多 4 条；
//   - 查询窗口的会话：一个窗口一直占一条（USE、SET、事务都留在上面），关掉窗口时这条连接也关掉，
//     不还给别人用（不然别的请求会拿到切过库、改过会话变量的连接）；
//   - 停止语句（KILL QUERY、pg_cancel_backend）：查询窗口、普通的池都占满时也要能马上停。
type poolRole int

const (
	rolePlain poolRole = iota
	roleSession
	roleControl
)

// maxSessionsPerDB 一个库最多同时开着几个查询窗口的会话。
const maxSessionsPerDB = 8

// DB 某个库的连接池；name 为空时用连接的默认库。
func (cl *Client) DB(ctx context.Context, name string) (*sql.DB, error) {
	return cl.pool(name, rolePlain)
}

// pool 某个库某种用途的连接池（见 poolRole）。
func (cl *Client) pool(name string, role poolRole) (*sql.DB, error) {
	if name == "" {
		name = cl.Conn.Database
	}
	key := name
	if role != rolePlain {
		key = name + "\x00" + strconv.Itoa(int(role))
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.closed {
		return nil, errors.New("连接已关闭，请重试")
	}
	if db := cl.dbs[key]; db != nil {
		return db, nil
	}
	if cl.Conn.Kind == Redis {
		return nil, errors.New("Redis 不能执行 SQL")
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
		switch cl.tlsMode() {
		case tlsPrefer:
			cfg.TLS, cfg.AllowFallbackToPlaintext = cl.tlsConfig(), true
		case TLSRequire, TLSVerify:
			cfg.TLS = cl.tlsConfig()
		}
		// 明文密码只在 SSH 通道里、一定加密时允许
		cfg.AllowCleartextPasswords = cl.tn != nil || cl.tlsMode() == TLSRequire || cl.tlsMode() == TLSVerify
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
		cfg.TLSConfig = nil
		cfg.Fallbacks = nil
		switch cl.tlsMode() {
		case tlsPrefer: // 先试加密，数据库不支持时不加密再连
			cfg.TLSConfig = cl.tlsConfig()
			cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: cfg.Host, Port: cfg.Port}}
		case TLSRequire, TLSVerify:
			cfg.TLSConfig = cl.tlsConfig()
		}
		cfg.RuntimeParams = map[string]string{"application_name": "Ferry"}
		cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
		cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return cl.dial(ctx) }
		cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) { return []string{host}, nil }
		db = stdlib.OpenDB(*cfg)
	default:
		return nil, fmt.Errorf("不支持的数据库种类 %q", cl.Conn.Kind)
	}
	switch role {
	case roleSession:
		db.SetMaxOpenConns(maxSessionsPerDB)
		db.SetMaxIdleConns(0) // 会话关了连接就关
	case roleControl:
		db.SetMaxOpenConns(2)
		db.SetMaxIdleConns(1)
		db.SetConnMaxIdleTime(time.Minute)
	default:
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		db.SetConnMaxIdleTime(5 * time.Minute)
	}
	cl.dbs[key] = db
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
			cl.mu.Lock()
			s.lastUsed = time.Now()
			cl.mu.Unlock()
			return s.conn, func() {}, nil
		}
		cl.mu.Lock()
		s.conn.Close()
		delete(cl.sessions, sid)
		cl.mu.Unlock()
	}
	cl.mu.Lock()
	open := 0
	for _, s := range cl.sessions {
		if s.db == dbName {
			open++
		}
	}
	cl.mu.Unlock()
	if open >= maxSessionsPerDB {
		return nil, nil, fmt.Errorf("这个库已经有 %d 个查询窗口连着，关掉不用的查询窗口再试", open)
	}
	db, err := cl.pool(dbName, roleSession)
	if err != nil {
		return nil, nil, err
	}
	// 等连接时可以取消（连接建好以后就和这次请求无关了，一直留给这个窗口）
	c, err := db.Conn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, cl.explain(err)
	}
	id := cl.connID(ctx, c)
	cl.mu.Lock()
	if old := cl.sessions[sid]; old != nil && old.db == dbName { // 同一个窗口同时来了两个请求：留先建好的
		cl.mu.Unlock()
		c.Close()
		return old.conn, func() {}, nil
	} else if old != nil {
		old.conn.Close()
	}
	cl.sessions[sid] = &session{db: dbName, conn: c, id: id, lastUsed: time.Now()}
	cl.mu.Unlock()
	return c, func() {}, nil
}

// sessionID 查询窗口会话的连接编号。
func (cl *Client) sessionID(sid string) string {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if s := cl.sessions[sid]; s != nil {
		return s.id
	}
	return ""
}

// connID 这条连接在数据库里的编号：MySQL 的 CONNECTION_ID()、PostgreSQL 的进程号。拿不到时为空。
func (cl *Client) connID(ctx context.Context, conn *sql.Conn) string {
	switch cl.Conn.Kind {
	case Postgres:
		var pid uint32
		conn.Raw(func(dc any) error {
			if c, ok := dc.(*stdlib.Conn); ok {
				pid = c.Conn().PgConn().PID()
			}
			return nil
		})
		if pid != 0 {
			return strconv.FormatUint(uint64(pid), 10)
		}
	case MySQL:
		var id uint64
		if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err == nil {
			return strconv.FormatUint(id, 10)
		}
	}
	return ""
}

// stopStmt 让数据库停下编号为 id 的连接正在执行的语句（用另一条连接：MySQL KILL QUERY，PostgreSQL pg_cancel_backend）。
// 只断开连接时 MySQL 会把语句执行完（UPDATE 照样生效），所以点停止、读够行数时都要这样停。
func (cl *Client) stopStmt(dbName, id string) {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := cl.pool(dbName, roleControl)
	if err != nil {
		return
	}
	q := "KILL QUERY " + id
	if cl.Conn.Kind == Postgres {
		q = "SELECT pg_cancel_backend(" + id + ")"
	}
	db.ExecContext(ctx, q)
}

// pgInTx PostgreSQL 的这条连接在用户自己开的事务里（停语句会让整个事务作废，要先设保存点）。
func pgInTx(conn *sql.Conn) bool {
	in := false
	conn.Raw(func(dc any) error {
		if c, ok := dc.(*stdlib.Conn); ok {
			in = c.Conn().PgConn().TxStatus() == 'T'
		}
		return nil
	})
	return in
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
	if s := err.Error(); strings.Contains(s, "without any password configured") || strings.Contains(s, "no password is set") {
		return errors.New("这个 Redis 没有设密码：在连接里把密码清空（修改连接，清掉记住的密码）")
	}
	if te := cl.tlsError(err); te != nil {
		return te
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
	var re redis.Error
	if errors.As(err, &re) {
		s := re.Error()
		return strings.HasPrefix(s, "NOAUTH") || strings.HasPrefix(s, "WRONGPASS") || strings.HasPrefix(s, "ERR invalid password") ||
			strings.HasPrefix(s, "ERR invalid username-password")
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
