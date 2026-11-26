package mcpserver

// 数据库：看保存的数据库连接、库表结构，执行只读查询（engine/dbai.go）；Redis 用 redis_query、redis_execute。
// 只能用用户在 Ferry「数据库」里勾选了「允许 AI 查询」的连接。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/dbx"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type dbConnsIn struct {
	Project string `json:"project,omitempty" jsonschema:"只看经这个项目的服务器连的（项目名或 ferry.yaml 的路径）；不填 = 全部"`
}

type dbSchemaIn struct {
	Connection string `json:"connection" jsonschema:"数据库连接（db_connections 里的 id 或 name）"`
	Database   string `json:"database,omitempty" jsonschema:"库名；不填 = 列出全部库（给了 table 时用连接的默认库）"`
	Schema     string `json:"schema,omitempty" jsonschema:"表所属的模式，使用 db_schema 返回的名称"`
	Table      string `json:"table,omitempty" jsonschema:"表名、视图名或 MongoDB 集合名"`
}

type dbQueryIn struct {
	Connection string `json:"connection" jsonschema:"数据库连接（db_connections 里的 id 或 name）"`
	Database   string `json:"database,omitempty" jsonschema:"在哪个库里查；不填 = 连接的默认库"`
	SQL        string `json:"sql" jsonschema:"一条只读查询，MongoDB 填 Extended JSON"`
	Limit      int    `json:"limit,omitempty" jsonschema:"最多返回多少行，默认 100，最多 500"`
}

type redisQueryIn struct {
	Connection string `json:"connection" jsonschema:"Redis 连接（db_connections 里 kind 为 redis 的 id 或 name）"`
	Database   string `json:"database,omitempty" jsonschema:"库的编号（0、1……）；不填 = 连接的默认库"`
	Command    string `json:"command" jsonschema:"一条只读的 Redis 命令，如 HGETALL user:1、SCAN 0 MATCH order:* COUNT 200"`
}

type redisExecIn struct {
	Connection string `json:"connection" jsonschema:"Redis 连接（db_connections 里 write 为 true 的 id 或 name）"`
	Database   string `json:"database,omitempty" jsonschema:"库的编号（0、1……）；不填 = 连接的默认库"`
	Commands   string `json:"commands" jsonschema:"要执行的 Redis 命令，一行一条（最多 100 条）"`
	Reason     string `json:"reason" jsonschema:"为什么这样改（显示在 Ferry 的确认框里）"`
}

type dbExecIn struct {
	Connection string `json:"connection" jsonschema:"数据库连接（db_connections 里 write 为 true 的 id 或 name）"`
	Database   string `json:"database,omitempty" jsonschema:"在哪个库里执行；不填 = 连接的默认库"`
	SQL        string `json:"sql" jsonschema:"要执行的 SQL，多条用分号隔开；MongoDB 填 Extended JSON"`
	Reason     string `json:"reason" jsonschema:"为什么这样改（显示在 Ferry 的确认框里）"`
}

func (s *server) dbEngine(ctx context.Context) *engine.Engine {
	st, _ := loadSettingsCached()
	return &engine.Engine{Rep: quiet{}, Settings: st, AcceptNewHostKey: st.Connect.AcceptNewHostKey,
		Pool: s.pool, Ctx: ctx, Getenv: s.o.Getenv}
}

func (s *server) addDBTools(m *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	addTool(s, m, &mcp.Tool{Name: "db_connections", Title: "数据库连接",
		Description: "Ferry「数据库」里保存的、用户允许 AI 查询的数据库连接：名称、种类、经哪个项目的哪台服务器连（SSH 转发）、地址、用户、默认库。" +
			"db_schema、db_query（Redis 用 redis_query）的 connection 参数从这里取。没列出来的连接要用户在 Ferry 里勾选「允许 AI 查询」。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		logged(s, "db_connections", func(ctx context.Context, in dbConnsIn) (any, error) {
			list, hidden, err := engine.AIDBConns()
			if err != nil {
				return nil, err
			}
			if in.Project != "" {
				f, err := s.resolve(in.Project)
				if err != nil {
					return nil, err
				}
				out := list[:0]
				for _, c := range list {
					if c.Config != "" && samePath(c.Config, f.Path) {
						out = append(out, c)
					}
				}
				list = out
			}
			r := map[string]any{"connections": list}
			if hidden > 0 {
				r["notAllowed"] = hidden
			}
			if len(list) == 0 {
				r["hint"] = engine.AIDBNoConnsHint
			}
			return r, nil
		}))

	addTool(s, m, &mcp.Tool{Name: "db_schema", Title: "数据库结构",
		Description: "看数据库的结构：不给 database 时列出全部库（system 是系统库）；给了 database 时列出表和视图（行数估计、大小、注释）；" +
			"给了 table 时按数据库类型返回表结构。MongoDB 列出集合和视图，table 是集合名，用来查看索引。" +
			"Redis：不给 database 时列出有键的库；给了 database（编号）时列出键和类型（最多 200 个），table 当键的模式（如 user:*）。",
		Annotations: readOnly},
		logged(s, "db_schema", func(ctx context.Context, in dbSchemaIn) (any, error) {
			cn, err := engine.AIDBFind(in.Connection)
			if err != nil {
				return nil, err
			}
			ctx, cancel := withReadTimeout(ctx)
			defer cancel()
			return s.dbEngine(ctx).AIDBSchema(ctx, cn, engine.AIDBSchemaIn{Database: in.Database, Schema: in.Schema, Table: in.Table})
		}))

	addTool(s, m, &mcp.Tool{Name: "db_query", Title: "数据库查询",
		Description: "在数据库里执行一条只读查询，返回列和行（单元格是文本或 null，二进制、很长的文本会注明长度并截断）。" +
			"最多 15 秒，默认返回 100 行、最多 500 行（more 为 true 表示还有更多）。" + engine.AIMongoQueryDesc +
			"不允许 SELECT … INTO、FOR UPDATE 等加锁、SLEEP / pg_sleep、读写服务器文件、结束别人连接的函数，也不能查账号密码表。" +
			"大表先用 db_schema 看结构和行数，查询加上条件和 LIMIT。改数据、改表结构不能用它：请用户在 Ferry 的「数据库」里做。",
		Annotations: readOnly},
		logged(s, "db_query", func(ctx context.Context, in dbQueryIn) (any, error) {
			cn, err := engine.AIDBFind(in.Connection)
			if err != nil {
				return nil, err
			}
			ctx, cancel := withReadTimeout(ctx)
			defer cancel()
			return s.dbEngine(ctx).AIDBQuery(ctx, cn, in.Database, in.SQL, in.Limit)
		}))

	addTool(s, m, &mcp.Tool{Name: "redis_query", Title: "Redis 查询", Description: engine.AIRedisQueryDesc, Annotations: readOnly},
		logged(s, "redis_query", func(ctx context.Context, in redisQueryIn) (any, error) {
			cn, err := engine.AIDBFind(in.Connection)
			if err != nil {
				return nil, err
			}
			ctx, cancel := withReadTimeout(ctx)
			defer cancel()
			return s.dbEngine(ctx).AIRedisQuery(ctx, cn, in.Database, in.Command)
		}))
}

// addDBChangeTools 改数据库：交给 Ferry 桌面版确认后执行（和部署、启停一样）。
func (s *server) addDBChangeTools(m *mcp.Server) {
	addTool(s, m, &mcp.Tool{Name: "db_execute", Title: "改数据库",
		Description: "在数据库里执行改动：插入、修改、删除数据，建表改表。" + engine.AIMongoWriteDesc +
			"只能用 db_connections 里 write 为 true 的连接（用户在连接上勾选了「允许 AI 修改」），要用户在 Ferry 设置里允许编程软件请求改动；经生产服务器连的不行。" +
			"每次都把完整操作、影响行数估算和数据库的回滚规则交给用户确认（默认在 Ferry 桌面版弹窗，也可在设置里开启在编程软件中确认，高危操作默认仍在桌面端确认）。ClickHouse 每次一条，达梦的建表改表单独确认。" +
			"一次最多 100 条；不要写 BEGIN、COMMIT；不能建库删库，不能动账号权限（GRANT、CREATE USER）、SET、CALL、LOAD DATA、COPY。" +
			"改之前先用 db_schema、db_query 看清楚结构和要改的行；UPDATE、DELETE 要写 WHERE。最多等 40 秒：没结束返回 id，再用 task_status 查。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}},
		loggedReq(s, "db_execute", func(ctx context.Context, req *mcp.CallToolRequest, in dbExecIn) (any, error) {
			if req != nil && req.Params != nil && req.Params.RequestState != "" {
				return s.resumeMRTR(ctx, req)
			}
			return s.dbChange(ctx, req, in.Connection, in.Database, in.SQL, in.Reason, false)
		}))

	addTool(s, m, &mcp.Tool{Name: "redis_execute", Title: "改 Redis",
		Description: engine.AIRedisExecDesc + "要用户在 Ferry 设置里允许编程软件请求改动；经生产服务器连的不行。最多等 40 秒：没结束返回 id，再用 task_status 查。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}},
		loggedReq(s, "redis_execute", func(ctx context.Context, req *mcp.CallToolRequest, in redisExecIn) (any, error) {
			if req != nil && req.Params != nil && req.Params.RequestState != "" {
				return s.resumeMRTR(ctx, req)
			}
			return s.dbChange(ctx, req, in.Connection, in.Database, in.Commands, in.Reason, true)
		}))
}

// dbChange db_execute、redis_execute：检查后交给用户确认。
func (s *server) dbChange(ctx context.Context, req *mcp.CallToolRequest, conn, database, src, reason string, redis bool) (any, error) {
	cn, err := engine.AIDBFind(conn)
	if err != nil {
		return nil, err
	}
	if err := engine.AIDBKindFits(cn, redis); err != nil {
		return nil, err
	}
	st, _ := settings.Load()
	f, t, err := DBChangeAllowed(st, cn)
	if err != nil {
		return nil, err
	}
	db := strings.TrimSpace(database)
	if db == "" {
		db = cn.Database
	}
	stmts, err := dbx.CheckAIWrite(cn.Kind, db, src)
	if err != nil {
		return nil, err
	}
	r := approve.Request{ID: newID(), Session: s.session, Client: clientName(req), Action: "db-exec",
		Database: db, Stmts: stmts, Reason: strings.TrimSpace(reason)}
	fillDB(&r, cn, f, t)
	return s.ask(ctx, req, r)
}

// DBChangeAllowed 这个数据库连接能不能经 MCP 改：设置里允许编程软件请求改动，连接允许 AI 修改、不是只读；
// 经服务器连时那台服务器也要允许（不是生产服务器、没设 aiChanges: false、在允许的目标里）。返回那台服务器。
func DBChangeAllowed(st settings.Settings, cn *dbx.Conn) (*config.File, *config.Target, error) {
	if !st.MCP.AllowChanges {
		return nil, nil, errors.New("Ferry 没有允许编程软件请求改动：用户可以在 Ferry「设置 → 命令行与 MCP」里打开「允许编程软件请求部署、启停、回滚等改动」，或者自己在 Ferry 的「数据库」里执行")
	}
	if err := engine.AIDBWriteAllowed(cn); err != nil {
		return nil, nil, err
	}
	if cn.Via == nil {
		return nil, nil, nil
	}
	f, err := config.LoadWithLocal(cn.Via.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("读不了数据库连接「%s」用的服务器配置：%w", cn.Name, err)
	}
	t, err := f.Target(cn.Via.Target)
	if err != nil {
		return nil, nil, err
	}
	if err := ChangeAllowed(st, f, t); err != nil {
		return nil, nil, fmt.Errorf("数据库连接「%s」经服务器 %s 连，%w", cn.Name, t.Name, err)
	}
	return f, t, nil
}

// fillDB 补上给人看的：连接、服务器、要执行的 SQL、危险的地方。
func fillDB(r *approve.Request, cn *dbx.Conn, f *config.File, t *config.Target) {
	r.DB, r.DBName, r.DBKind = cn.ID, cn.Name, cn.Kind
	r.DBCheck = cn.ApprovalKey()
	r.Project, r.Config, r.Target, r.Host = "", "", "", ""
	where := "这台电脑直接连 " + cn.Addr()
	if cn.Kind == dbx.SQLite {
		where = cn.File
	}
	if f != nil && t != nil {
		r.Project, r.Config, r.Target, r.Host = f.Project, f.Path, t.Name, t.Host
		where = "经服务器 " + t.Name + " 连"
		if cn.Container != "" {
			where += "容器 " + cn.Container
		}
	}
	db := r.Database
	if db == "" {
		db = "（默认）"
	}
	if cn.Kind == dbx.Redis {
		db = "db" + strings.TrimPrefix(strings.ToLower(orZero(r.Database)), "db")
	}
	account := ""
	if cn.AIUser != "" {
		account = "，用 AI 的账号 " + cn.AIUser
	}
	head := fmt.Sprintf("数据库 %s（%s，%s%s），库 %s。%s", cn.Name, kindName(cn.Kind), where, account, db, strings.Join(dbx.AIWriteNotes(cn.Kind, r.Stmts), "；"))
	r.Detail = []string{head, dbx.AIWriteText(cn.Kind, r.Stmts)}
	r.Danger = dbx.AIWriteDanger(cn.Kind, r.Stmts)
}

func kindName(k string) string {
	switch k {
	case dbx.MySQL:
		return "MySQL / MariaDB"
	case dbx.Postgres:
		return "PostgreSQL"
	case dbx.Redis:
		return "Redis / Valkey"
	case dbx.SQLServer:
		return "SQL Server"
	case dbx.MongoDB:
		return "MongoDB"
	case dbx.SQLite:
		return "SQLite"
	case dbx.ClickHouse:
		return "ClickHouse"
	case dbx.Dameng:
		return "达梦"
	case dbx.Kingbase:
		return "人大金仓"
	}
	return k
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// prepareDB 收到改数据库的请求、给用户看之前再查一遍，确认框里的内容按连接重新生成，并尝试 EXPLAIN 估算影响行数。
func prepareDB(r *approve.Request, pool *engine.Pool, getenv func(string) string) error {
	st, _ := settings.Load()
	if r.ClientConfirmed {
		return errors.New("此操作需要通过预检令牌或在 Ferry 里确认")
	}
	cn, err := dbx.Get(r.DB)
	if err != nil {
		return err
	}
	if r.DBKind != "" && r.DBKind != cn.Kind {
		return errors.New("数据库连接类型已改动，请重新提交")
	}
	if !cn.AI {
		return fmt.Errorf("数据库连接「%s」没有允许 AI 查询", cn.Name)
	}
	f, t, err := DBChangeAllowed(st, cn)
	if err != nil {
		return err
	}
	if err := dbx.CheckAIWriteStmts(cn.Kind, r.Database, r.Stmts); err != nil {
		return err
	}
	fillDB(r, cn, f, t)
	r.DBEstimate, r.DBEstimateMax = estimateDBRows(st, cn, r.Database, r.Stmts, pool, getenv)
	return nil
}

// estimateDBRows 尝试用 EXPLAIN 估算 MySQL / PostgreSQL 的 UPDATE / DELETE 影响行数（不执行语句，最多等 5 秒）。
func estimateDBRows(st settings.Settings, cn *dbx.Conn, database string, stmts []string, pool *engine.Pool, getenv func(string) string) (string, int64) {
	if (cn.Kind != dbx.MySQL && cn.Kind != dbx.Postgres) || len(dbx.EstimateCandidates(cn.Kind, stmts)) == 0 {
		return "", 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := &engine.Engine{Rep: quiet{}, Settings: st, AcceptNewHostKey: st.Connect.AcceptNewHostKey, Pool: pool, Ctx: ctx, Getenv: getenv, DisableAgentInstall: true}
	cl, release, err := e.OpenAIDB(cn)
	if err != nil {
		return "", 0
	}
	defer release()
	est, err := cl.EstimateRows(ctx, database, stmts)
	if err != nil {
		return "", 0
	}
	var maxRows int64
	for _, x := range est {
		if x.Rows > maxRows {
			maxRows = x.Rows
		}
	}
	return dbx.EstimateText(stmts, est), maxRows
}
