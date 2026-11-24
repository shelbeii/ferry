package dbx

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type migrationQueryer interface {
	querier
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type migrationExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func checkMigrationEmpty(ctx context.Context, db migrationQueryer, kind, name string) error {
	q := `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema' AND c.relkind IN ('r','p','f','v','m','S')
		UNION ALL SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
		UNION ALL SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema' AND t.typtype IN ('d','e','r','c'))`
	var args []any
	if kind == MySQL {
		q = `SELECT EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?
			UNION ALL SELECT 1 FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?
			UNION ALL SELECT 1 FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?)`
		args = []any{name, name, name}
	}
	var exists bool
	if err := db.QueryRowContext(ctx, q, args...).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("整库迁移要求目标是空库，请选择一个空库")
	}
	return nil
}

func checkMigrationTable(ctx context.Context, q migrationQueryer, db, kind string, t *TableInfo) (warning string, err error) {
	if kind == MySQL {
		if t.Engine != "" && !strings.EqualFold(t.Engine, "InnoDB") {
			warning = fmt.Sprintf("表 %s 使用 %s 引擎，不支持事务快照，迁移期间请避免写入该表。", t.Name, t.Engine)
		}
		for _, c := range t.Columns {
			switch mysqlMigrationType(c.Type) {
			case "geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection", "geomcollection", "vector":
				return "", fmt.Errorf("表 %s 的列 %s 使用当前不支持的迁移类型 %s", t.Name, c.Name, c.Type)
			}
		}
		return warning, nil
	}
	var ordinary bool
	err = q.QueryRowContext(ctx, `SELECT c.relkind = 'r' AND NOT c.relrowsecurity AND NOT c.relispartition
		AND NOT EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = c.oid OR inhparent = c.oid)
		FROM pg_class c WHERE c.oid = $1::regclass`, migrationTableID(t).sqlName(kind)).Scan(&ordinary)
	if err == nil && !ordinary {
		err = fmt.Errorf("%s.%s 是分区表、继承表、外部表或启用了行级安全，当前无法直迁", t.Schema, t.Name)
	}
	return "", err
}

func migrationColumnsMatch(a, b *TableInfo) error {
	cols := make(map[string]ColumnInfo, len(b.Columns))
	for _, c := range b.Columns {
		cols[c.Name] = c
	}
	same := len(a.Columns) == len(b.Columns) && a.Charset == b.Charset && a.Collation == b.Collation
	for _, c := range a.Columns {
		d, ok := cols[c.Name]
		same = same && ok && c.Type == d.Type && c.Nullable == d.Nullable && c.Generated == d.Generated && c.Auto == d.Auto && c.Collation == d.Collation
	}
	if !same {
		return fmt.Errorf("目标表 %s 的列结构与源表不兼容", migrationTableID(a).String())
	}
	return nil
}

func migrationTableHasRows(ctx context.Context, q migrationQueryer, table MigrationTable, kind string, lock bool) (bool, error) {
	query := "SELECT 1 FROM " + table.sqlName(kind) + " LIMIT 1"
	if lock && kind == MySQL {
		query += " FOR UPDATE"
	}
	var one int
	err := q.QueryRowContext(ctx, query).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func migrationTableEmpty(ctx context.Context, q migrationQueryer, table MigrationTable, kind string, lock bool) error {
	has, err := migrationTableHasRows(ctx, q, table, kind, lock)
	if err != nil {
		return err
	}
	if has {
		return fmt.Errorf("目标表 %s 已有数据，迁移不会覆盖", table.String())
	}
	return nil
}

func migrationExistingTriggers(ctx context.Context, q migrationQueryer, kind, db string, t MigrationTable) error {
	query := `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal)`
	args := []any{t.sqlName(kind)}
	if kind == MySQL {
		query = `SELECT EXISTS (SELECT 1 FROM information_schema.TRIGGERS WHERE EVENT_OBJECT_SCHEMA = ? AND EVENT_OBJECT_TABLE = ?)`
		args = []any{db, t.Name}
	}
	var exists bool
	if err := q.QueryRowContext(ctx, query, args...).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("目标表 %s 有触发器，请使用新建表迁移", t.String())
	}
	return nil
}

func migrationDependencies(ctx context.Context, q migrationQueryer, kind, db string, t MigrationTable) ([]MigrationTable, error) {
	query := `SELECT DISTINCT n.nspname, c.relname FROM pg_constraint f JOIN pg_class c ON c.oid = f.confrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE f.contype = 'f' AND f.conrelid = $1::regclass ORDER BY 1, 2`
	args := []any{t.sqlName(kind)}
	if kind == MySQL {
		query = `SELECT DISTINCT REFERENCED_TABLE_SCHEMA, REFERENCED_TABLE_NAME FROM information_schema.KEY_COLUMN_USAGE
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND REFERENCED_TABLE_NAME IS NOT NULL ORDER BY 1, 2`
		args = []any{db, t.Name}
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MigrationTable
	for rows.Next() {
		var dep MigrationTable
		if err := rows.Scan(&dep.Schema, &dep.Name); err != nil {
			return nil, err
		}
		if kind == MySQL {
			if dep.Schema != db {
				return nil, fmt.Errorf("表 %s 有跨库外键 %s，当前无法直迁", t.String(), dep.String())
			}
			dep.Schema = ""
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

func (p *MigrationPlan) describeObjects(ctx context.Context, db migrationQueryer, kind, name string, o MigrationOptions) error {
	if !o.Whole {
		if !o.DataOnly {
			p.Warnings = append(p.Warnings, "指定表迁移会创建缺少的表及其索引、约束和所属序列。视图、触发器、函数和存储过程请用整库迁移。")
		}
		return nil
	}
	p.Objects = []string{"表结构、数据、索引、约束和自增值", "视图、触发器、函数和存储过程"}
	if kind == MySQL {
		var hasEvents bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?)`, name).Scan(&hasEvents); err != nil {
			return err
		}
		if hasEvents {
			p.Warnings = append(p.Warnings, "源库的 MySQL 定时事件需要单独迁移。")
		}
		p.Warnings = append(p.Warnings, "数据库账号、授权和对象 DEFINER 需在目标端管理。")
		return nil
	}
	p.Objects = append(p.Objects, "模式、枚举和序列")
	p.Warnings = append(p.Warnings, "数据库角色、授权、扩展和大对象需要单独迁移。物化视图会用目标数据重新生成。")
	for _, view := range p.views {
		if pgViewWord(view) == "MATERIALIZED VIEW" {
			p.Warnings = append(p.Warnings, "物化视图的索引需要单独迁移。")
			break
		}
	}
	return nil
}

func migrationDDL(ctx context.Context, q migrationExecutor, kind, script string) error {
	for _, stmt := range Split(kind, script) {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func migrationDump(ctx context.Context, tx *sql.Tx, db, kind string, whole bool) (*dumper, *bytes.Buffer) {
	var buf bytes.Buffer
	return &dumper{ctx: ctx, tx: tx, db: db, kind: kind, whole: whole, w: &buf}, &buf
}

func migrationNotes(d *dumper) error {
	if len(d.notes) > 0 {
		return fmt.Errorf("无法读取完整对象定义：%s", strings.Join(d.notes, "；"))
	}
	return nil
}

func migrationSourceTables(p *MigrationPlan) []*TableInfo {
	out := make([]*TableInfo, 0, len(p.checks))
	for _, t := range p.checks {
		out = append(out, t.source)
	}
	return out
}
