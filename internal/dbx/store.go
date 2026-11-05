// Package dbx 数据库客户端（桌面版左下角的「数据库」）：保存的数据库连接，直接连或经一台服务器的 SSH 转发连；
// 浏览库、表，查询，改数据，建表改表，导出。支持 MySQL / MariaDB、PostgreSQL 和 Redis（键的浏览、改值、命令行，见 redis.go）。
package dbx

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/secret"
)

// 数据库种类。
const (
	MySQL    = "mysql" // MySQL、MariaDB
	Postgres = "postgres"
	Redis    = "redis"
)

// Conn 一个保存的数据库连接。
type Conn struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // mysql | postgres | redis
	// Host、Port 数据库的地址；经服务器连时是从服务器上看的地址（通常 127.0.0.1）
	Host string `json:"host"`
	Port int    `json:"port"`
	// User Redis 是 ACL 的用户名（一般不填）
	User     string `json:"user"`
	Database string `json:"database,omitempty"` // 默认库；PostgreSQL 没填时连 postgres；Redis 是库的编号（没填是 0）
	// Via 经这台服务器连（SSH 端口转发）；为空时这台电脑直接连
	Via *Via `json:"via,omitempty"`
	// Container 数据库在这个 Docker 容器里、端口没映射到服务器上：连接时查容器的 IP 再连（Host 不用）
	Container string `json:"container,omitempty"`
	// Software 是 Ferry 在这台服务器上装的软件（实例名）：没单独保存密码时用安装时保存的密码
	Software string `json:"software,omitempty"`
	// TLS 加密：空 = 自动（这台电脑直接连 MySQL、PostgreSQL 时数据库支持就加密；经服务器连和 Redis 不加密）|
	// off 不加密 | require 加密，不验证证书 | verify 加密并验证证书（见 tls.go）
	TLS string `json:"tls,omitempty"`
	// ReadOnly 只许查，不许改（生产服务器上的数据库新建时默认打开）
	ReadOnly bool `json:"readOnly,omitempty"`
	// SavePassword 密码保存在系统凭据库里（否则每次打开 Ferry 后第一次连接时问）
	SavePassword bool `json:"savePassword,omitempty"`
	// AI 允许 AI 查询（编程软件经 MCP、Ferry 的 AI 助手）：只能看结构、执行只读查询（见 ai.go）；查出来的数据会发给 AI 的模型
	AI bool `json:"ai,omitempty"`
	// AIWrite 也允许 AI 修改：插入、修改、删除数据，建表改表（见 aiwrite.go），每次都要用户看过 SQL 同意。要先允许 AI 查询，只读的连接不行
	AIWrite bool `json:"aiWrite,omitempty"`
	// AIUser AI 用这个账号连（建议是只有查询权限的账号）；为空时和用户一样用 User。密码另存在凭据库里（AIPasswordAccount）
	AIUser  string `json:"aiUser,omitempty"`
	Color   string `json:"color,omitempty"`
	Created string `json:"created,omitempty"`
}

// Via 经哪台服务器连。
type Via struct {
	Config string `json:"config"` // ferry.yaml（或「新建连接」的 connections.yaml）的路径
	Target string `json:"target"` // 目标名
	Title  string `json:"title,omitempty"`
}

// Addr 数据库地址 host:port。
func (c *Conn) Addr() string {
	h := c.Host
	if h == "" {
		h = "127.0.0.1"
	}
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		h = "[" + h + "]"
	}
	return fmt.Sprintf("%s:%d", h, c.Port)
}

// Check 检查字段。
func (c *Conn) Check() error {
	c.Name = strings.TrimSpace(c.Name)
	c.Host = strings.TrimSpace(c.Host)
	c.User = strings.TrimSpace(c.User)
	switch c.Kind {
	case MySQL, Postgres, Redis:
	default:
		return fmt.Errorf("不支持的数据库种类 %q（支持 mysql、postgres、redis）", c.Kind)
	}
	if c.Name == "" {
		return errors.New("请填连接名")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("端口应在 1–65535 之间")
	}
	if c.User == "" && c.Kind != Redis {
		return errors.New("请填用户名")
	}
	if c.Kind == Redis {
		c.Database = strings.TrimSpace(c.Database)
		if _, err := ParseRedisDB(c.Database); err != nil {
			return err
		}
	}
	if c.Container == "" && c.Host == "" {
		return errors.New("请填地址")
	}
	if c.Via != nil && (c.Via.Config == "" || c.Via.Target == "") {
		return errors.New("经服务器连时要选服务器")
	}
	if !c.AI || c.ReadOnly {
		c.AIWrite = false
	}
	c.AIUser = strings.TrimSpace(c.AIUser)
	if !c.AI {
		c.AIUser = ""
	}
	if c.Container != "" && c.Via == nil {
		return errors.New("连容器里的数据库要经服务器连")
	}
	return checkTLS(c)
}

// PasswordAccount 密码在凭据库里的账户名。
func PasswordAccount(id string) string { return secret.KindDB + ":" + id }

// AIPasswordAccount AI 用的账号（Conn.AIUser）的密码在凭据库里的账户名。
func AIPasswordAccount(id string) string { return secret.KindDB + ":" + id + ":ai" }

// ForAI AI 连数据库用的连接：设了 AIUser 时换成那个账号（ID 后面加 #ai，和用户自己用的连接分开）。
func (c *Conn) ForAI() *Conn {
	if c.AIUser == "" {
		return c
	}
	a := *c
	a.ID = c.ID + "#ai"
	a.User = c.AIUser
	return &a
}

// PasswordEnv 桌面版把用户刚输入、不要求记住的密码这样传给 ferry。
func PasswordEnv(id string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(id) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "FERRY_DB_PASSWORD_" + b.String()
}

var storeMu sync.Mutex

func storePath() string { return appdir.Path("db", "connections.json") }

type storeFile struct {
	Connections []Conn `json:"connections"`
}

// List 保存的连接（按名称排序）。
func List() ([]Conn, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	return load()
}

func load() ([]Conn, error) {
	b, err := os.ReadFile(storePath())
	if errors.Is(err, os.ErrNotExist) {
		return []Conn{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", storePath(), err)
	}
	if f.Connections == nil {
		f.Connections = []Conn{}
	}
	sort.SliceStable(f.Connections, func(i, j int) bool {
		return strings.ToLower(f.Connections[i].Name) < strings.ToLower(f.Connections[j].Name)
	})
	return f.Connections, nil
}

func write(cs []Conn) error {
	if err := os.MkdirAll(appdir.SubDir("db"), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(storeFile{Connections: cs}, "", "  ")
	if err != nil {
		return err
	}
	return appdir.WriteFileAtomic(storePath(), append(b, '\n'), 0o600)
}

// Get 按 ID 找连接。
func Get(id string) (*Conn, error) {
	cs, err := List()
	if err != nil {
		return nil, err
	}
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i], nil
		}
	}
	return nil, fmt.Errorf("没有这个数据库连接：%s", id)
}

// Save 新建（ID 为空）或修改连接。password 不为 nil 时：SavePassword 打开则存进凭据库，否则从凭据库删掉。
// aiPassword 是 AI 用的账号的密码（AIUser 不为空时总是存进凭据库：AI 不会问密码）；为 nil 时不改。
func Save(c Conn, password, aiPassword *string) (*Conn, error) {
	if err := c.Check(); err != nil {
		return nil, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	cs, err := load()
	if err != nil {
		return nil, err
	}
	for _, o := range cs {
		if o.ID != c.ID && strings.EqualFold(o.Name, c.Name) {
			return nil, fmt.Errorf("已经有叫「%s」的连接了", c.Name)
		}
	}
	found := false
	if c.ID != "" {
		for i := range cs {
			if cs[i].ID == c.ID {
				c.Created = cs[i].Created
				cs[i] = c
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("没有这个数据库连接：%s", c.ID)
		}
	} else {
		c.ID = newID()
		c.Created = time.Now().Format(time.RFC3339)
		cs = append(cs, c)
	}
	switch {
	case !c.SavePassword:
		if err := secret.Delete(PasswordAccount(c.ID)); err != nil {
			return nil, err
		}
	case password != nil && *password != "":
		if err := secret.SetLabeled(PasswordAccount(c.ID), *password, c.Name); err != nil {
			return nil, err
		}
	}
	switch {
	case c.AIUser == "":
		if err := secret.Delete(AIPasswordAccount(c.ID)); err != nil {
			return nil, err
		}
	case aiPassword != nil:
		if err := secret.SetLabeled(AIPasswordAccount(c.ID), *aiPassword, c.Name+"（AI）"); err != nil {
			return nil, err
		}
	}
	if err := write(cs); err != nil {
		return nil, err
	}
	return &c, nil
}

// Remove 删除连接和保存的密码。
func Remove(id string) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	cs, err := load()
	if err != nil {
		return err
	}
	out := cs[:0]
	found := false
	for _, c := range cs {
		if c.ID == id {
			found = true
			continue
		}
		out = append(out, c)
	}
	if !found {
		return fmt.Errorf("没有这个数据库连接：%s", id)
	}
	if err := write(out); err != nil {
		return err
	}
	secret.Delete(AIPasswordAccount(id))
	return secret.Delete(PasswordAccount(id))
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NeedPasswordError 没有密码：桌面版据此弹框，填好后用 Env 环境变量重试。
type NeedPasswordError struct {
	ID, Name, Env string
	Rejected      bool // 填的密码不对
	Msg           string
}

func (e *NeedPasswordError) Error() string {
	if e.Rejected {
		return "数据库 " + e.Name + " 的密码不对：" + e.Msg
	}
	return "请填数据库 " + e.Name + " 的密码"
}

func (e *NeedPasswordError) JSONDetail() map[string]any {
	kind := "db-password-required"
	if e.Rejected {
		kind = "db-password-rejected"
	}
	return map[string]any{"kind": kind, "id": e.ID, "name": e.Name, "env": e.Env}
}
