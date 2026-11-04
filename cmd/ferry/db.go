package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/dbx"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/settings"
	"github.com/captain/ferry/internal/ui"
)

// 「数据库」：保存的数据库连接，浏览、查询、改数据、建表改表、导出（桌面版左下角）。
//
//	ferry db list | save | remove <连接> | test | close [<连接>]
//	ferry db info|databases <连接>
//	ferry db tables <连接> <库>
//	ferry db table|rows|count|apply|design|op|exec|query|export|dump <连接> <库>
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
}

func dbCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, st settings.Settings, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(errors.New("用法：ferry db list | save | remove | test | info | databases | tables | table | rows | count | query | apply | design | op | exec | export | close"))
	}
	sub := c.pos[0]
	args := c.pos[1:]
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
		var pw *string
		if v := cio.getenv("FERRY_DB_NEW_PASSWORD"); v != "" {
			pw = &v
		}
		saved, err := dbx.Save(*cn, pw)
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
		return out(map[string]any{"version": cl.Version, "databases": dbs})
	}

	cn, err := connArg()
	if err != nil {
		return fail(err)
	}
	cl, release, err := open(cn, false)
	if err != nil {
		return fail(err)
	}
	defer release()
	ctx := cio.ctx
	db := dbArg()
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
		ts, err := cl.Tables(ctx, db)
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
	case "ai-exec": // 执行编程软件请求、用户在桌面版同意了的改动（在一个事务里，失败全部撤回）
		if err := engine.AIDBWriteAllowed(cn); err != nil {
			return fail(err)
		}
		rep.Step("在 %s 执行 %d 条语句", cn.Name, len(req.Stmts))
		r, err := cl.AIExec(ctx, db, req.Stmts)
		if err != nil {
			return fail(err)
		}
		if !r.OK {
			return fail(engine.AIExecError(r))
		}
		for i, x := range r.Results {
			if x.Affected != nil {
				rep.Info("第 %d 条：影响 %d 行", i+1, *x.Affected)
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
