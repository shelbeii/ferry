package dbx

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type MigrationTable struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
}

type MigrationOptions struct {
	TargetDB string           `json:"targetDb"`
	Whole    bool             `json:"whole"`
	DataOnly bool             `json:"dataOnly"`
	Tables   []MigrationTable `json:"tables"`
}

type MigrationTablePlan struct {
	MigrationTable
	Create bool `json:"create"`
}

type MigrationPlan struct {
	Method   string               `json:"method"`
	Tables   []MigrationTablePlan `json:"tables"`
	Warnings []string             `json:"warnings,omitempty"`
	Objects  []string             `json:"objects,omitempty"`
	checks   []migrationTableCheck
	views    []*TableInfo
	pgEnums  []migrationEnum
}

type MigrationResult struct {
	Created   []MigrationTable `json:"created,omitempty"`
	Completed []MigrationTable `json:"completed,omitempty"`
	Rows      int64            `json:"rows"`
	Phase     string           `json:"phase"`
	Table     *MigrationTable  `json:"table,omitempty"`
}

type migrationError struct {
	Result MigrationResult
	Cause  error
}

func (e *migrationError) Error() string { return fmt.Sprintf("迁移%s：%v", e.Result.Phase, e.Cause) }
func (e *migrationError) Unwrap() error { return e.Cause }
func (e *migrationError) JSONDetail() map[string]any {
	return map[string]any{"kind": "db-migration", "migration": e.Result}
}

type migrationTableCheck struct {
	MigrationTablePlan
	source, target *TableInfo
	depends        []MigrationTable
}

type migrationProgress struct {
	Step  func(string)
	Bytes func(int64)
}

func migrationTableID(t *TableInfo) MigrationTable {
	return MigrationTable{Schema: t.Schema, Name: t.Name}
}

func (t MigrationTable) String() string {
	if t.Schema != "" {
		return t.Schema + "." + t.Name
	}
	return t.Name
}

func (t MigrationTable) sqlName(kind string) string { return TableName(kind, "", t.Schema, t.Name) }

func (src *Client) PlanMigration(ctx context.Context, srcDB string, dst *Client, o MigrationOptions) (*MigrationPlan, error) {
	if err := migrationEndpoints(src, srcDB, dst, o); err != nil {
		return nil, err
	}
	sdb, err := src.DB(ctx, srcDB)
	if err != nil {
		return nil, err
	}
	ddb, err := dst.DB(ctx, o.TargetDB)
	if err != nil {
		return nil, err
	}
	p := &MigrationPlan{Method: "COPY", Tables: []MigrationTablePlan{}}
	if src.Conn.Kind == MySQL {
		var local, readOnly bool
		if err := ddb.QueryRowContext(ctx, "SELECT @@global.local_infile, @@global.read_only").Scan(&local, &readOnly); err != nil {
			return nil, err
		}
		if readOnly {
			return nil, errors.New("目标 MySQL 服务器是只读的")
		}
		p.Method = "LOAD DATA LOCAL INFILE"
		if !local {
			p.Method = "INSERT"
			p.Warnings = append(p.Warnings, "目标未开启 local_infile，将用多行 INSERT 传输数据。")
		}
	} else {
		var readOnly bool
		if err := ddb.QueryRowContext(ctx, "SELECT current_setting('transaction_read_only')::boolean").Scan(&readOnly); err != nil {
			return nil, err
		}
		if readOnly {
			return nil, errors.New("目标 PostgreSQL 数据库是只读的")
		}
	}
	source, err := src.Tables(ctx, srcDB)
	if err != nil {
		return nil, fmt.Errorf("读取源库：%w", err)
	}
	target, err := dst.Tables(ctx, o.TargetDB)
	if err != nil {
		return nil, fmt.Errorf("读取目标库：%w", err)
	}
	if o.Whole {
		if err := checkMigrationEmpty(ctx, ddb, src.Conn.Kind, o.TargetDB); err != nil {
			return nil, err
		}
	}
	selected := make(map[MigrationTable]bool, len(o.Tables))
	for _, table := range o.Tables {
		if table.Name == "" {
			return nil, errors.New("要迁移的表名不能为空")
		}
		if src.Conn.Kind == Postgres && table.Schema == "" {
			table.Schema = "public"
		}
		selected[table] = true
	}
	targetObjects := make(map[MigrationTable]Table, len(target))
	for _, table := range target {
		targetObjects[MigrationTable{table.Schema, table.Name}] = table
	}
	for _, table := range source {
		key := MigrationTable{table.Schema, table.Name}
		if !o.Whole && !selected[key] {
			continue
		}
		delete(selected, key)
		if table.Type == "view" && !o.Whole {
			return nil, fmt.Errorf("%s 是视图，请使用整库迁移", key.String())
		}
		info, err := src.TableInfo(ctx, srcDB, table.Schema, table.Name)
		if err != nil {
			return nil, err
		}
		if info.Type == "view" {
			p.views = append(p.views, info)
			continue
		}
		if err := checkMigrationTable(ctx, sdb, srcDB, src.Conn.Kind, info); err != nil {
			return nil, err
		}
		old, exists := targetObjects[key]
		if o.DataOnly && !exists {
			return nil, fmt.Errorf("目标没有表 %s，仅迁数据需要预先建表", key.String())
		}
		check := migrationTableCheck{MigrationTablePlan: MigrationTablePlan{key, !exists}, source: info}
		if exists {
			if old.Type != "table" {
				return nil, fmt.Errorf("目标已有同名对象 %s", key.String())
			}
			check.target, err = dst.TableInfo(ctx, o.TargetDB, table.Schema, table.Name)
			if err != nil {
				return nil, err
			}
			if err := checkMigrationTable(ctx, ddb, o.TargetDB, src.Conn.Kind, check.target); err != nil {
				return nil, err
			}
			if err := migrationColumnsMatch(info, check.target); err != nil {
				return nil, err
			}
			if err := migrationTableEmpty(ctx, ddb, key, src.Conn.Kind, false); err != nil {
				return nil, err
			}
			if err := migrationExistingTriggers(ctx, ddb, src.Conn.Kind, o.TargetDB, key); err != nil {
				return nil, err
			}
		}
		q, db := migrationQueryer(sdb), srcDB
		if exists {
			q, db = ddb, o.TargetDB
		}
		check.depends, err = migrationDependencies(ctx, q, src.Conn.Kind, db, key)
		if err != nil {
			return nil, err
		}
		p.checks = append(p.checks, check)
	}
	for table := range selected {
		return nil, fmt.Errorf("源库没有表 %s", table.String())
	}
	if len(p.checks) == 0 && len(p.views) == 0 && !o.Whole {
		return nil, errors.New("没有要迁移的表")
	}
	if err := p.orderTables(targetObjects); err != nil {
		return nil, err
	}
	if src.Conn.Kind == Postgres && !o.Whole && !o.DataOnly {
		if err := p.planPostgresEnums(ctx, sdb, ddb); err != nil {
			return nil, err
		}
	}
	if err := p.describeObjects(ctx, sdb, src.Conn.Kind, srcDB, o); err != nil {
		return nil, err
	}
	return p, nil
}

func migrationEndpoints(src *Client, srcDB string, dst *Client, o MigrationOptions) error {
	if src.Conn.Kind != dst.Conn.Kind || (src.Conn.Kind != MySQL && src.Conn.Kind != Postgres) || src.isMariaDB() != dst.isMariaDB() {
		return errors.New("请使用同引擎的 MySQL、MariaDB 或 PostgreSQL 连接")
	}
	if srcDB == "" || o.TargetDB == "" {
		return errors.New("请选择源库和目标库")
	}
	if src.Conn.ID == dst.Conn.ID && srcDB == o.TargetDB {
		return errors.New("源库和目标库相同")
	}
	if dst.Conn.ReadOnly {
		return errors.New("目标数据库连接是只读的")
	}
	if o.Whole && (o.DataOnly || len(o.Tables) != 0) || !o.Whole && len(o.Tables) == 0 {
		return errors.New("请选择整库或指定表迁移")
	}
	system := []string{"information_schema", "mysql", "performance_schema", "sys"}
	if src.Conn.Kind == Postgres {
		system = []string{"template0", "template1"}
	}
	if slices.Contains(system, strings.ToLower(srcDB)) || slices.Contains(system, strings.ToLower(o.TargetDB)) {
		return errors.New("请选择业务数据库")
	}
	return nil
}

func (p *MigrationPlan) orderTables(existing map[MigrationTable]Table) error {
	tables := make(map[MigrationTable]migrationTableCheck, len(p.checks))
	for _, t := range p.checks {
		tables[t.MigrationTable] = t
	}
	var ordered []migrationTableCheck
	visiting, done := map[MigrationTable]bool{}, map[MigrationTable]bool{}
	var visit func(migrationTableCheck) error
	visit = func(t migrationTableCheck) error {
		if done[t.MigrationTable] {
			return nil
		}
		if visiting[t.MigrationTable] {
			return fmt.Errorf("目标空表有循环外键，无法按依赖导入 %s，请迁移到新表", t.String())
		}
		visiting[t.MigrationTable] = true
		for _, dep := range t.depends {
			parent, selected := tables[dep]
			if !selected && existing[dep].Type != "table" {
				return fmt.Errorf("表 %s 依赖 %s，请补选依赖表或在目标库准备该表", t.String(), dep.String())
			}
			if t.Create || !selected {
				continue // 新建表的外键会在加载完数据后建立。
			}
			if err := visit(parent); err != nil {
				return err
			}
		}
		delete(visiting, t.MigrationTable)
		done[t.MigrationTable] = true
		ordered = append(ordered, t)
		return nil
	}
	for _, t := range p.checks {
		if err := visit(t); err != nil {
			return err
		}
	}
	p.checks = ordered
	for _, t := range ordered {
		p.Tables = append(p.Tables, t.MigrationTablePlan)
	}
	return nil
}

// Migrate uses dedicated sessions so that connection settings, cancellation and the
// source snapshot stay isolated from query tabs and later migrations.
func (src *Client) Migrate(ctx context.Context, srcDB string, dst *Client, o MigrationOptions, step func(string), progress func(int64)) (*MigrationResult, error) {
	if err := migrationEndpoints(src, srcDB, dst, o); err != nil {
		return nil, err
	}
	sc, err := migrationConn(ctx, src, srcDB)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	dc, err := migrationConn(ctx, dst, o.TargetDB)
	if err != nil {
		return nil, err
	}
	defer dc.Close()
	unlock, err := lockMigration(ctx, dc, dst.Conn.Kind, o.TargetDB)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := src.PlanMigration(ctx, srcDB, dst, o)
	if err != nil {
		return nil, err
	}
	notify := migrationProgress{Step: step, Bytes: progress}
	if src.Conn.Kind == MySQL {
		return migrateMySQL(ctx, sc, dc, src, srcDB, dst, o, p, notify)
	}
	return migratePostgres(ctx, sc, dc, srcDB, o, p, notify)
}

func migrationConn(ctx context.Context, cl *Client, db string) (*sql.Conn, error) {
	pool, err := cl.pool(db, roleImport)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Conn(ctx)
	return conn, cl.explain(err)
}

func lockMigration(ctx context.Context, c *sql.Conn, kind, db string) (func(), error) {
	lock, unlock := "SELECT pg_try_advisory_lock(772584132, 1)", "SELECT pg_advisory_unlock(772584132, 1)"
	var args []any
	if kind == MySQL {
		key := sha256.Sum256([]byte("ferry-migration:" + strings.ToLower(db)))
		args = []any{fmt.Sprintf("ferry:%x", key[:24])}
		lock, unlock = "SELECT GET_LOCK(?, 0)", "SELECT RELEASE_LOCK(?)"
	}
	var ok bool
	if err := c.QueryRowContext(ctx, lock, args...).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("目标库已有迁移任务在运行")
	}
	return func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = c.ExecContext(cleanup, unlock, args...)
	}, nil
}

func (p migrationProgress) step(s string) {
	if p.Step != nil {
		p.Step(s)
	}
}

func (p migrationProgress) counter() *migrationCounter { return &migrationCounter{report: p.Bytes} }

type migrationCounter struct {
	total  int64
	last   time.Time
	report func(int64)
}

func (c *migrationCounter) add(n int) {
	c.total += int64(n)
	if c.report != nil && time.Since(c.last) >= 500*time.Millisecond {
		c.last = time.Now()
		c.report(c.total)
	}
}

func (c *migrationCounter) flush() {
	if c.report != nil {
		c.report(c.total)
	}
}
