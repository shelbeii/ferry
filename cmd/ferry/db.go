package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/dbx"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/ui"
)

// 「数据库」：保存的数据库连接，浏览、查询、改数据、建表改表、导出（桌面版左下角）。
//
//	ferry db list | save | remove <连接> | test | close [<连接>] | creds（从服务器上读数据库的账号密码）
//	ferry db info|databases <连接>
//	ferry db tables <连接> <库>
//	ferry db table|rows|count|apply|design|op|exec|query|export|dump <连接> <库>
//	ferry db redis-keys|redis-get|redis-edit|redis-cmd <连接> <库编号>（Redis）
//	ferry db routines|routine|routine-save|backup <连接> <库>（函数、存储过程；备份整个库）
//	ferry db queries|query-load|query-save|query-rename|query-delete|backups|backup-delete <连接> <库>（这台电脑上存的查询、备份）
//
// 其余参数（表名、条件、SQL、改动……）放在环境变量 FERRY_DB_REQ 里（JSON），连接本身（save、test）放在 FERRY_DB_CONN 里。
// 新填的密码在 FERRY_DB_NEW_PASSWORD；已保存的连接没记住的密码在 FERRY_DB_PASSWORD_<连接 ID>。

type dbReq struct {
	Schema  string       `json:"schema"`
	Table   string       `json:"table"`
	Where   string       `json:"where"`
	Filters []dbx.Filter `json:"filters"`
	Sorts   []dbx.Sort   `json:"sorts"`
	Tables  []string     `json:"tables"` // dump：导出哪些表（空 = 全部）
	Data    bool         `json:"data"`   // dump：也导出数据
	Detail  bool         `json:"detail"` // tables：多读自增值、修改时间等（「对象」列表）
	Offset  int          `json:"offset"`
	Limit   int          `json:"limit"`
	Session string       `json:"session"`
	SQL     string       `json:"sql"`
	Stmts   []string     `json:"stmts"`
	Changes []dbx.Change `json:"changes"`
	Design  *dbx.Design  `json:"design"`
	Op      string       `json:"op"`
	Args    []string     `json:"args"`
	Format  string       `json:"format"`
	Out     string       `json:"out"`
	Preview bool         `json:"preview"`
	Max     int          `json:"max"`
	Stop    bool         `json:"stop"`
	// Redis
	Scan    *dbx.RedisScan `json:"scan"`
	Get     *dbx.RedisGet  `json:"get"`
	Edit    *dbx.RedisEdit `json:"edit"`
	Line    string         `json:"line"`    // redis-cmd：一行命令
	Confirm bool           `json:"confirm"` // redis-cmd：用户已经确认了危险的命令
	// 导入
	File     string         `json:"file"`     // import-sql：SQL 文件
	Continue bool           `json:"continue"` // import-sql：出错后接着执行
	CSV      *dbx.CSVImport `json:"csv"`      // csv-preview、import-csv
	// creds：从服务器上读数据库的账号密码（「软件」页连数据库时替用户填好）
	Via  *dbx.Via            `json:"via"`
	Cred *engine.DBCredQuery `json:"cred"`

	// 函数、存储过程
	Routine *dbx.RoutineRef `json:"routine"`
	// 保存的查询：名称、原来的名称（改名）、覆盖同名的
	Name      string `json:"name"`
	Old       string `json:"old"`
	Overwrite bool   `json:"overwrite"`
}

func dbCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(errors.New("用法：ferry db list | save | remove | test | info | databases | tables | table | rows | count | query | apply | design | op | exec | export | close | redis-keys | redis-get | redis-edit | redis-cmd"))
	}
	sub := c.pos[0]
	args := c.pos[1:]
	// 只给桌面版用：编程软件能执行命令，不这样的话它直接 ferry db exec 就绕过了「允许 AI 查询 / 修改」和确认
	if !cio.served && sub != "list" && sub != "close" {
		return fail(errors.New("ferry db 只能由 Ferry 桌面版调用：请在 Ferry 左下角的「数据库」里操作（编程软件请用 Ferry 的 MCP 工具 db_query、db_execute 等，改动要用户确认）"))
	}
	out := func(data any) int {
		if js != nil {
			js.Result(true, data, nil)
			return 0
		}
		b, _ := json.MarshalIndent(data, "", "  ")
		fmt.Fprintln(cio.stdout, string(b))
		return 0
	}
	var req dbReq
	if s := cio.getenv("FERRY_DB_REQ"); s != "" {
		if err := json.Unmarshal([]byte(s), &req); err != nil {
			return fail(fmt.Errorf("FERRY_DB_REQ 格式不对：%w", err))
		}
	}
	var rep engine.Reporter = con
	if js != nil {
		rep = js
	}
	// 经服务器连：用那个项目（或「新建连接」）的配置连服务器，连接池里的连接和终端、部署共用
	newEngine := func(v *dbx.Via) (*engine.Engine, error) {
		f, err := config.LoadWithLocal(v.Config)
		if err != nil {
			return nil, err
		}
		return &engine.Engine{File: f, Rep: rep, Settings: st, AcceptNewHostKey: st.Connect.AcceptNewHostKey,
			Pool: cio.pool, Ctx: cio.ctx, Getenv: cio.getenv}, nil
	}
	openTunnel := func(v *dbx.Via) (dbx.Tunnel, error) {
		e, err := newEngine(v)
		if err != nil {
			return nil, err
		}
		return e.OpenTunnel(v.Target)
	}
	// 密码：刚填的 → 这次运行里填过的 → 凭据库 → Ferry 装这个软件时保存的
	password := func(cn *dbx.Conn, fresh bool) (string, bool, error) {
		if fresh {
			if v := cio.getenv("FERRY_DB_NEW_PASSWORD"); v != "" {
				return v, true, nil
			}
		}
		if cn.ID != "" {
			if v := cio.getenv(dbx.PasswordEnv(cn.ID)); v != "" {
				return v, true, nil
			}
			if cn.SavePassword {
				v, ok, err := secret.Get(dbx.PasswordAccount(cn.ID))
				if err != nil {
					return "", false, err
				}
				if ok {
					return v, true, nil
				}
			}
		}
		if cn.Software != "" && cn.Via != nil {
			e, err := newEngine(cn.Via)
			if err == nil {
				if v, err := e.SoftwarePassword(cn.Via.Target, cn.Software); err == nil {
					return v, true, nil
				}
			}
		}
		return "", false, nil
	}
	open := func(cn *dbx.Conn, fresh bool) (*dbx.Client, func(), error) {
		pw, have, err := password(cn, fresh)
		if err != nil {
			return nil, nil, err
		}
		cl, release, err := dbx.Default.Open(cio.ctx, cn, pw, openTunnel)
		var ne *dbx.NeedPasswordError
		if errors.As(err, &ne) && !have {
			ne.Rejected = false // 没填过密码（试了空密码）：请用户填
		}
		return cl, release, err
	}
	connArg := func() (*dbx.Conn, error) {
		if len(args) == 0 {
			return nil, fmt.Errorf("用法：ferry db %s <连接 ID> …", sub)
		}
		return dbx.Get(args[0])
	}
	dbArg := func() string {
		if len(args) > 1 {
			return args[1]
		}
		return ""
	}
	connFromEnv := func() (*dbx.Conn, error) {
		var cn dbx.Conn
		if err := json.Unmarshal([]byte(cio.getenv("FERRY_DB_CONN")), &cn); err != nil {
			return nil, fmt.Errorf("FERRY_DB_CONN 格式不对：%w", err)
		}
		return &cn, nil
	}

	switch sub {
	case "list":
		cs, err := dbx.List()
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"connections": cs, "opened": dbx.Default.Opened()})
	case "save":
		cn, err := connFromEnv()
		if err != nil {
			return fail(err)
		}
		var pw, aiPw *string
		if v := cio.getenv("FERRY_DB_NEW_PASSWORD"); v != "" {
			pw = &v
		}
		if v := cio.getenv("FERRY_DB_NEW_AI_PASSWORD"); v != "" {
			aiPw = &v
		}
		saved, err := dbx.Save(*cn, pw, aiPw)
		if err != nil {
			return fail(err)
		}
		dbx.Default.Close(saved.ID)
		return out(saved)
	case "remove":
		if len(args) == 0 {
			return fail(errors.New("用法：ferry db remove <连接 ID>"))
		}
		dbx.Default.Close(args[0])
		if err := dbx.Remove(args[0]); err != nil {
			return fail(err)
		}
		return out(map[string]any{"removed": args[0]})
	case "creds":
		if req.Via == nil || req.Cred == nil {
			return fail(errors.New("FERRY_DB_REQ 里要有 via、cred"))
		}
		e, err := newEngine(req.Via)
		if err != nil {
			return fail(err)
		}
		cred, err := e.DBCredentials(req.Via.Target, *req.Cred)
		if err != nil {
			return fail(err)
		}
		return out(cred)
	case "close":
		if len(args) > 0 && req.Session != "" {
			dbx.Default.CloseSession(args[0], req.Session)
		} else if len(args) > 0 {
			dbx.Default.Close(args[0])
		} else {
			dbx.Default.Close("")
		}
		return out(map[string]any{})
	case "test":
		cn, err := connFromEnv()
		if err != nil {
			return fail(err)
		}
		if err := cn.Check(); err != nil {
			return fail(err)
		}
		probe := *cn
		probe.ID = "test" // 单独的 Manager：不进打开着的连接表，用完就关
		pw, _, err := password(cn, true)
		if err != nil {
			return fail(err)
		}
		m := &dbx.Manager{}
		cl, release, err := m.Open(cio.ctx, &probe, pw, openTunnel)
		if err != nil {
			var ne *dbx.NeedPasswordError
			if errors.As(err, &ne) {
				return fail(fmt.Errorf("密码不对：%s", ne.Msg))
			}
			return fail(err)
		}
		defer release()
		dbs, err := cl.Databases(cio.ctx)
		if err != nil {
			return fail(err)
		}
		// AI 用的账号也试一下
		if cn.AI && cn.AIUser != "" {
			aiPw := cio.getenv("FERRY_DB_NEW_AI_PASSWORD")
			if aiPw == "" && cn.ID != "" {
				if v, ok, err := secret.Get(dbx.AIPasswordAccount(cn.ID)); err == nil && ok {
					aiPw = v
				}
			}
			ai := *cn.ForAI()
			ai.ID = "test-ai"
			_, arelease, err := m.Open(cio.ctx, &ai, aiPw, openTunnel)
			if err != nil {
				var ne *dbx.NeedPasswordError
				if errors.As(err, &ne) {
					return fail(fmt.Errorf("AI 用的账号 %s 连不上，密码不对：%s", cn.AIUser, ne.Msg))
				}
				return fail(fmt.Errorf("AI 用的账号 %s 连不上：%w", cn.AIUser, err))
			}
			arelease()
		}
		return out(map[string]any{"version": cl.Version, "databases": dbs, "encrypted": cl.Encrypted, "viaSSH": cn.Via != nil})
	case "csv-preview": // 导入 CSV 前看前几行（不连数据库）
		if req.CSV == nil {
			return fail(errors.New("没有选 CSV 文件"))
		}
		p, err := dbx.PreviewCSV(req.CSV.CSVOptions)
		if err != nil {
			return fail(err)
		}
		return out(p)
	case "queries", "query-load", "query-save", "query-rename", "query-delete", "backups", "backup-delete": // 这台电脑上存的查询、备份（不连数据库）
		cn, err := connArg()
		if err != nil {
			return fail(err)
		}
		db := dbArg()
		switch sub {
		case "queries":
			items, err := dbx.ListQueries(cn.ID, db)
			if err != nil {
				return fail(err)
			}
			return out(map[string]any{"items": items, "dir": dbx.QueriesDir(cn.ID, db)})
		case "query-load":
			text, err := dbx.ReadQuery(cn.ID, db, req.Name)
			if err != nil {
				return fail(err)
			}
			return out(map[string]any{"name": req.Name, "sql": text})
		case "query-save":
			name, err := dbx.SaveQuery(cn.ID, db, req.Name, req.Old, req.SQL, req.Overwrite)
			if err != nil {
				return fail(err)
			}
			return out(map[string]any{"name": name})
		case "query-rename":
			name, err := dbx.RenameQuery(cn.ID, db, req.Old, req.Name)
			if err != nil {
				return fail(err)
			}
			return out(map[string]any{"name": name})
		case "query-delete":
			if err := dbx.DeleteQuery(cn.ID, db, req.Name); err != nil {
				return fail(err)
			}
			return out(map[string]any{})
		case "backups":
			items, err := dbx.ListBackups(cn.ID, db)
			if err != nil {
				return fail(err)
			}
			return out(map[string]any{"items": items, "dir": dbx.BackupsDir(cn.ID, db)})
		default: // backup-delete
			if err := dbx.DeleteBackup(cn.ID, db, req.Name); err != nil {
				return fail(err)
			}
			return out(map[string]any{})
		}
	case "danger": // 执行前看哪些语句危险（查询窗口、确认框用；不连数据库）
		cn, err := connArg()
		if err != nil {
			return fail(err)
		}
		// 每段都按语法再拆一遍：查询窗口把整段 SQL 当一条传来，一行里也可能写了几条
		var stmts []string
		for _, s := range append(req.Stmts, req.SQL) {
			stmts = append(stmts, dbx.Split(cn.Kind, s)...)
		}
		dangers := []string{}
		if cn.Kind != dbx.Redis {
			dangers = dbx.Dangers(cn.Kind, stmts, false)
		}
		return out(map[string]any{"dangers": dangers, "count": len(stmts)})
	}

	cn, err := connArg()
	if err != nil {
		return fail(err)
	}
	var cl *dbx.Client
	var release func()
	if sub == "ai-exec" || sub == "estimate" {
		// AI 的改动：和 AI 查询一样的账号（设了 AI 用的账号时用那个）
		e := &engine.Engine{Rep: rep, Settings: st, AcceptNewHostKey: st.Connect.AcceptNewHostKey, Pool: cio.pool, Ctx: cio.ctx, Getenv: cio.getenv}
		cl, release, err = e.OpenAIDB(cn)
	} else {
		cl, release, err = open(cn, false)
	}
	if err != nil {
		return fail(err)
	}
	defer release()
	ctx := cio.ctx
	db := dbArg()
	needRedis := func() error {
		if cn.Kind != dbx.Redis {
			return fmt.Errorf("db %s 只能用在 Redis 连接上", sub)
		}
		return nil
	}
	needDB := func() error {
		if db == "" && cn.Kind == dbx.MySQL {
			return fmt.Errorf("用法：ferry db %s <连接> <库>", sub)
		}
		return nil
	}
	needTable := func() error {
		if err := needDB(); err != nil {
			return err
		}
		if req.Table == "" {
			return errors.New("没有指定表")
		}
		return nil
	}

	switch sub {
	case "info", "databases":
		dbs, err := cl.Databases(ctx)
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"version": cl.Version, "kind": cn.Kind, "readOnly": cn.ReadOnly, "databases": dbs})
	case "tables":
		if err := needDB(); err != nil {
			return fail(err)
		}
		get := cl.Tables
		if req.Detail {
			get = cl.TablesDetail
		}
		ts, err := get(ctx, db)
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"tables": ts})
	case "table":
		if err := needTable(); err != nil {
			return fail(err)
		}
		ti, err := cl.TableInfo(ctx, db, req.Schema, req.Table)
		if err != nil {
			return fail(err)
		}
		return out(ti)
	case "rows":
		if err := needTable(); err != nil {
			return fail(err)
		}
		r, err := cl.Rows(ctx, db, req.Schema, req.Table, req.browse())
		if err != nil {
			return fail(err)
		}
		return out(r)
	case "count":
		if err := needTable(); err != nil {
			return fail(err)
		}
		n, err := cl.Count(ctx, db, req.Schema, req.Table, req.browse())
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"count": n})
	case "query":
		rs, err := cl.Query(ctx, db, req.SQL, dbx.QueryOptions{Session: req.Session, Max: req.Max, Stop: req.Stop})
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"results": rs})
	case "apply":
		if err := needTable(); err != nil {
			return fail(err)
		}
		ti, err := cl.TableInfo(ctx, db, req.Schema, req.Table)
		if err != nil {
			return fail(err)
		}
		stmts, err := dbx.ChangeSQL(cn.Kind, db, ti, req.Changes)
		if err != nil {
			return fail(err)
		}
		if req.Preview {
			sqls := []string{}
			for _, s := range stmts {
				sqls = append(sqls, s.Preview(cn.Kind))
			}
			return out(map[string]any{"sql": sqls})
		}
		r, err := cl.Apply(ctx, db, stmts)
		if err != nil {
			return fail(err)
		}
		return out(r)
	case "design":
		if err := needDB(); err != nil {
			return fail(err)
		}
		if req.Design == nil {
			return fail(errors.New("没有表的设计"))
		}
		var orig *dbx.TableInfo
		if req.Design.Orig != "" {
			if orig, err = cl.TableInfo(ctx, db, req.Design.Schema, req.Design.Orig); err != nil {
				return fail(err)
			}
		}
		stmts, err := dbx.DesignSQL(cn.Kind, db, *req.Design, orig)
		if err != nil {
			return fail(err)
		}
		if stmts == nil {
			stmts = []string{} // 没有改动
		}
		return out(map[string]any{"sql": stmts})
	case "op":
		stmts, err := dbx.OpSQL(cn.Kind, db, req.Schema, req.Op, req.Args)
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"sql": stmts})
	case "exec":
		if len(req.Stmts) == 0 {
			return fail(errors.New("没有要执行的语句"))
		}
		rs, err := cl.Exec(ctx, db, req.Stmts)
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"results": rs})
	case "estimate": // 确认框里估算 UPDATE、DELETE 会影响多少行（EXPLAIN，不执行）
		est, err := cl.EstimateRows(ctx, db, req.Stmts)
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"estimates": est, "text": dbx.EstimateText(req.Stmts, est), "candidates": len(dbx.EstimateCandidates(cn.Kind, req.Stmts))})
	case "ai-exec": // 执行编程软件请求、用户在桌面版同意了的改动（在一个事务里，失败全部撤回）
		if err := engine.AIDBWriteAllowed(cn); err != nil {
			return fail(err)
		}
		unit := "语句"
		if cn.Kind == dbx.Redis {
			unit = "命令"
		}
		rep.Step("在 %s 执行 %d 条%s", cn.Name, len(req.Stmts), unit)
		r, err := cl.AIExec(ctx, db, req.Stmts)
		if err != nil {
			return fail(err)
		}
		if !r.OK {
			return fail(engine.AIExecError(r))
		}
		for i, x := range r.Results {
			switch {
			case x.Affected != nil:
				rep.Info("第 %d 条：影响 %d 行", i+1, *x.Affected)
			case x.ReplyText != "":
				rep.Info("第 %d 条 %s：%s", i+1, x.SQL, strings.SplitN(x.ReplyText, "\n", 2)[0])
			}
		}
		return out(r)
	case "export":
		if strings.TrimSpace(req.Out) == "" {
			return fail(errors.New("没有选保存到哪里"))
		}
		r, err := cl.Export(ctx, db, dbx.ExportOptions{Format: req.Format, Out: req.Out, Schema: req.Schema, Table: req.Table, Query: req.SQL, Browse: req.browse()},
			func(n int64) { rep.Info("已导出 %d 行", n) })
		if err != nil {
			return fail(err)
		}
		return out(r)
	case "redis-keys":
		if err := needRedis(); err != nil {
			return fail(err)
		}
		var o dbx.RedisScan
		if req.Scan != nil {
			o = *req.Scan
		}
		r, err := cl.RedisKeys(ctx, db, o)
		if err != nil {
			return fail(err)
		}
		return out(r)
	case "redis-get":
		if err := needRedis(); err != nil {
			return fail(err)
		}
		if req.Get == nil {
			return fail(errors.New("没有指定键"))
		}
		r, err := cl.RedisGet(ctx, db, *req.Get)
		if err != nil {
			return fail(err)
		}
		return out(r)
	case "redis-edit":
		if err := needRedis(); err != nil {
			return fail(err)
		}
		if req.Edit == nil {
			return fail(errors.New("没有要做的改动"))
		}
		r, err := cl.RedisApply(ctx, db, *req.Edit)
		if err != nil {
			return fail(err)
		}
		return out(r)
	case "redis-cmd":
		if err := needRedis(); err != nil {
			return fail(err)
		}
		n, err := dbx.ParseRedisDB(db)
		if err != nil {
			return fail(err)
		}
		if db == "" {
			n, _ = dbx.ParseRedisDB(cn.Database)
		}
		r, err := cl.RedisCommand(ctx, req.Session, n, req.Line, req.Confirm)
		if err != nil {
			return fail(err)
		}
		return out(r)
	case "import-sql": // 执行 SQL 文件（边读边执行，同一条连接）
		if strings.TrimSpace(req.File) == "" {
			return fail(errors.New("没有选 SQL 文件"))
		}
		rep.Step("在 %s 执行 %s", cn.Name, req.File)
		r, err := cl.ImportSQL(ctx, db, dbx.ImportSQLOptions{File: req.File, Continue: req.Continue}, func(done, total, n int64) {
			rep.Progress(fmt.Sprintf("已执行 %d 条语句", n), done, total)
		})
		if err != nil {
			return fail(err)
		}
		for _, e := range r.Errors {
			rep.Warn("第 %d 条（%s）出错：%s", e.N, e.SQL, e.Error)
		}
		if r.Stopped {
			e := r.Errors[len(r.Errors)-1]
			return fail(fmt.Errorf("第 %d 条语句出错，停下了（前面 %d 条已经执行、生效）：%s\n%s", e.N, e.N-1, e.Error, e.SQL))
		}
		rep.Info("执行了 %d 条语句%s", r.Stmts, map[bool]string{true: fmt.Sprintf("，%d 条出错", r.Failed), false: ""}[r.Failed > 0])
		return out(r)
	case "import-csv": // 把 CSV 导进一张表（一个事务，失败全部撤回）
		if req.CSV == nil || req.CSV.File == "" {
			return fail(errors.New("没有选 CSV 文件"))
		}
		rep.Step("把 %s 导进 %s", req.CSV.File, req.CSV.Table)
		r, err := cl.ImportCSV(ctx, db, *req.CSV, func(done, total, n int64) {
			rep.Progress(fmt.Sprintf("已导入 %d 行", n), done, total)
		})
		if err != nil {
			return fail(err)
		}
		rep.Info("导入了 %d 行", r.Rows)
		return out(r)
	case "routines":
		rs, err := cl.Routines(ctx, db)
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"routines": rs})
	case "routine":
		if req.Routine == nil {
			return fail(errors.New("没有指定函数"))
		}
		def, err := cl.RoutineDef(ctx, db, *req.Routine)
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"sql": def})
	case "routine-save": // routine 为空是新建
		if err := cl.SaveRoutine(ctx, db, req.Routine, req.SQL); err != nil {
			return fail(err)
		}
		return out(map[string]any{})
	case "backup": // 备份整个库：导出结构和数据到这台电脑上的备份目录
		if err := needDB(); err != nil {
			return fail(err)
		}
		p, err := dbx.NewBackupPath(cn.ID, db, time.Now())
		if err != nil {
			return fail(err)
		}
		rep.Step("备份 %s / %s 到 %s", cn.Name, db, p)
		r, err := cl.Dump(ctx, db, dbx.DumpOptions{Out: p, Data: true}, func(msg string) { rep.Info("%s", msg) })
		if err != nil {
			os.Remove(p) // 没备份完的不留
			return fail(err)
		}
		return out(map[string]any{"path": p, "result": r})
	case "dump": // 导出结构（Data 为 true 时连数据一起）成 SQL 文件
		if strings.TrimSpace(req.Out) == "" {
			return fail(errors.New("没有选保存到哪里"))
		}
		if err := needDB(); err != nil {
			return fail(err)
		}
		r, err := cl.Dump(ctx, db, dbx.DumpOptions{Out: req.Out, Schema: req.Schema, Tables: req.Tables, Data: req.Data}, func(msg string) { rep.Info("%s", msg) })
		if err != nil {
			return fail(err)
		}
		return out(r)
	}
	return fail(fmt.Errorf("不认识的子命令 db %s", sub))
}

// browse 表数据的筛选、排序、翻页。
func (r *dbReq) browse() dbx.BrowseOptions {
	return dbx.BrowseOptions{Where: r.Where, Filters: r.Filters, Sorts: r.Sorts, Offset: r.Offset, Limit: r.Limit}
}
