package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

func migratePostgres(ctx context.Context, sc, dc *sql.Conn, srcDB string, o MigrationOptions, p *MigrationPlan, progress migrationProgress) (*MigrationResult, error) {
	result := &MigrationResult{Phase: "准备"}
	fail := func(err error) (*MigrationResult, error) { return result, &migrationError{*result, err} }
	source, err := sc.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fail(err)
	}
	defer source.Rollback()
	target, err := dc.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fail(err)
	}
	defer target.Rollback()
	settings := `SET LOCAL DateStyle = 'ISO, YMD'; SET LOCAL IntervalStyle = postgres;
		SET LOCAL TIME ZONE 'UTC'; SET LOCAL extra_float_digits = 3;
		SET LOCAL standard_conforming_strings = on; SET LOCAL row_security = off; SET LOCAL search_path = public, pg_catalog;`
	for _, tx := range []*sql.Tx{source, target} {
		if err := migrationDDL(ctx, tx, Postgres, settings); err != nil {
			return fail(err)
		}
	}
	if o.Whole {
		if err := checkMigrationEmpty(ctx, target, Postgres, o.TargetDB); err != nil {
			return fail(err)
		}
	}
	for _, t := range p.checks {
		if _, err := source.ExecContext(ctx, "LOCK TABLE "+t.sqlName(Postgres)+" IN ACCESS SHARE MODE"); err != nil {
			return fail(err)
		}
	}
	schema, err := pgMigrationSchema(ctx, source, target, srcDB, o, p)
	if err != nil {
		return fail(err)
	}
	result.Phase = "建表"
	progress.step("准备目标表")
	if err := migrationDDL(ctx, target, Postgres, schema.before); err != nil {
		return fail(err)
	}
	var created, completed []MigrationTable
	var rows int64
	count := progress.counter()
	for _, t := range p.checks {
		result.Phase, result.Table = "复制数据", &t.MigrationTable
		progress.step("复制 " + t.String())
		if !t.Create {
			if _, err := target.ExecContext(ctx, "LOCK TABLE "+t.sqlName(Postgres)+" IN ACCESS EXCLUSIVE MODE"); err != nil {
				return fail(err)
			}
			if err := migrationTableEmpty(ctx, target, t.MigrationTable, Postgres, false); err != nil {
				return fail(err)
			}
			if err := migrationExistingTriggers(ctx, target, Postgres, o.TargetDB, t.MigrationTable); err != nil {
				return fail(err)
			}
		} else {
			created = append(created, t.MigrationTable)
		}
		n, err := pgMigrationCopy(ctx, sc, dc, t.source, count)
		if err != nil {
			return fail(fmt.Errorf("%s：%w", t.String(), err))
		}
		rows += n
		completed = append(completed, t.MigrationTable)
	}
	count.flush()
	result.Phase, result.Table = "恢复索引、约束和其他对象", nil
	progress.step("恢复索引、约束和其他对象")
	if err := migrationDDL(ctx, target, Postgres, schema.after); err != nil {
		return fail(err)
	}
	if err := restorePGMigrationSequences(ctx, source, target, schema.sequences, p, o); err != nil {
		return fail(err)
	}
	result.Phase = "提交"
	if err := target.Commit(); err != nil {
		return fail(fmt.Errorf("确认目标库状态后再重试：%w", err))
	}
	result.Phase, result.Created, result.Completed, result.Rows = "完成", created, completed, rows
	return result, nil
}

type pgMigrationDefinition struct {
	before, after string
	sequences     []pgSeq
}

type migrationEnum struct {
	schema, name, labels string
}

func (p *MigrationPlan) planPostgresEnums(ctx context.Context, src, dst *sql.DB) error {
	seen := map[MigrationTable]bool{}
	for _, table := range p.checks {
		if !table.Create {
			continue
		}
		rows, err := src.QueryContext(ctx, `SELECT ns.nspname, typ.typname, string_agg(quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder)
			FROM pg_type typ JOIN pg_namespace ns ON ns.oid = typ.typnamespace JOIN pg_enum e ON e.enumtypid = typ.oid
			WHERE typ.oid IN (SELECT COALESCE(NULLIF(t.typelem, 0), t.oid) FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
				WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped)
			GROUP BY ns.nspname, typ.typname ORDER BY 1, 2`, table.sqlName(Postgres))
		if err != nil {
			return err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var e migrationEnum
				if err := rows.Scan(&e.schema, &e.name, &e.labels); err != nil {
					return err
				}
				key := MigrationTable{e.schema, e.name}
				if seen[key] {
					continue
				}
				seen[key] = true
				var labels sql.NullString
				err := dst.QueryRowContext(ctx, `SELECT string_agg(quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder)
					FROM pg_type typ JOIN pg_namespace ns ON ns.oid = typ.typnamespace LEFT JOIN pg_enum e ON e.enumtypid = typ.oid
					WHERE ns.nspname = $1 AND typ.typname = $2 GROUP BY typ.oid`, e.schema, e.name).Scan(&labels)
				switch {
				case err == sql.ErrNoRows:
					p.pgEnums = append(p.pgEnums, e)
				case err != nil:
					return err
				case !labels.Valid || labels.String != e.labels:
					return fmt.Errorf("目标类型 %s 与源枚举不兼容", key.String())
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func pgMigrationSchema(ctx context.Context, src, dst *sql.Tx, db string, o MigrationOptions, p *MigrationPlan) (pgMigrationDefinition, error) {
	d, before := migrationDump(ctx, src, db, Postgres, o.Whole)
	d.createOnly = true
	post, after := migrationDump(ctx, src, db, Postgres, o.Whole)
	var schemas []string
	for _, t := range p.checks {
		if !slices.Contains(schemas, t.Schema) {
			schemas = append(schemas, t.Schema)
		}
	}
	for _, e := range p.pgEnums {
		if !slices.Contains(schemas, e.schema) {
			schemas = append(schemas, e.schema)
		}
	}
	if o.Whole {
		rows, err := src.QueryContext(ctx, `SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema' ORDER BY 1`)
		if err != nil {
			return pgMigrationDefinition{}, err
		}
		schemas, err = readMigrationStrings(rows)
		if err != nil {
			return pgMigrationDefinition{}, err
		}
	}
	for _, s := range schemas {
		var exists bool
		if err := dst.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", s).Scan(&exists); err != nil {
			return pgMigrationDefinition{}, err
		}
		if !exists {
			d.printf("CREATE SCHEMA IF NOT EXISTS %s;\n", Quote(Postgres, s))
		}
		if o.Whole {
			if err := d.pgEnums(s); err != nil {
				return pgMigrationDefinition{}, err
			}
		}
	}
	for _, e := range p.pgEnums {
		d.printf("CREATE TYPE %s AS ENUM (%s);\n", TableName(Postgres, "", e.schema, e.name), e.labels)
	}
	tables := migrationSourceTables(p)
	seqs, err := d.pgSequences(schemas, tables)
	if err != nil {
		return pgMigrationDefinition{}, err
	}
	for _, s := range seqs {
		if s.dep == "i" || (!o.Whole && (s.dep == "" || !migrationCreatesPGSequence(p, s))) {
			continue
		}
		cycle := "NO CYCLE"
		if s.cycle {
			cycle = "CYCLE"
		}
		d.printf("CREATE SEQUENCE %s AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s;\n",
			s.reg(), s.typ, s.inc, s.min, s.max, s.start, s.cache, cycle)
	}
	if o.Whole {
		// SQL functions may depend on tables created later in this transaction.
		d.printf("SET LOCAL check_function_bodies = off;\n")
		for _, s := range schemas {
			if err := d.pgFunctions(s); err != nil {
				return pgMigrationDefinition{}, err
			}
		}
		if err := migrationNotes(d); err != nil {
			return pgMigrationDefinition{}, err
		}
	}
	var foreign []string
	for _, t := range p.checks {
		if !t.Create {
			continue
		}
		create := *t.source
		create.Indexes, create.PrimaryKey, create.Foreign = nil, nil, nil
		d.printf("%s\n", pgDDL(&create, nil))
		constraints, err := pgMigrationConstraints(ctx, src, t.MigrationTable)
		if err != nil {
			return pgMigrationDefinition{}, err
		}
		for _, c := range constraints {
			if strings.Contains(c, " FOREIGN KEY (") {
				foreign = append(foreign, c)
			} else {
				post.printf("%s;\n", c)
			}
		}
	}
	for _, stmt := range foreign {
		post.printf("%s;\n", stmt)
	}
	if o.Whole {
		for _, view := range viewOrder(Postgres, p.views) {
			post.printf("%s;\n", strings.TrimRight(strings.TrimSpace(view.DDL), ";"))
		}
		if err := post.pgTriggers(append(tables, p.views...)); err != nil {
			return pgMigrationDefinition{}, err
		}
	}
	return pgMigrationDefinition{before.String(), after.String(), seqs}, nil
}

func readMigrationStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func pgMigrationConstraints(ctx context.Context, src *sql.Tx, t MigrationTable) ([]string, error) {
	reg := t.sqlName(Postgres)
	rows, err := src.QueryContext(ctx, `SELECT command FROM (
		SELECT 0 AS phase, conname AS name, 'ALTER TABLE ' || $2 || ' ADD CONSTRAINT ' || quote_ident(conname) || ' ' || pg_get_constraintdef(oid) AS command
		FROM pg_constraint WHERE conrelid = $1::regclass AND contype IN ('p','u','c','x','f')
		UNION ALL SELECT 1, i.indexrelid::regclass::text, pg_get_indexdef(i.indexrelid)
		FROM pg_index i WHERE i.indrelid = $1::regclass AND i.indisvalid AND NOT EXISTS (
			SELECT 1 FROM pg_constraint c WHERE c.conrelid = i.indrelid AND c.conindid = i.indexrelid AND c.contype IN ('p','u','x'))
	) d ORDER BY phase, name`, reg, reg)
	if err != nil {
		return nil, err
	}
	return readMigrationStrings(rows)
}

func migrationCreatesPGSequence(p *MigrationPlan, s pgSeq) bool {
	return slices.ContainsFunc(p.checks, func(t migrationTableCheck) bool {
		return t.Create && t.Schema == s.ownSchema && t.Name == s.ownTable
	})
}

func restorePGMigrationSequences(ctx context.Context, src, dest *sql.Tx, sequences []pgSeq, p *MigrationPlan, o MigrationOptions) error {
	for _, s := range sequences {
		created := migrationCreatesPGSequence(p, s)
		if s.dep == "" && !o.Whole {
			continue
		}
		var last string
		var called bool
		if err := src.QueryRowContext(ctx, "SELECT last_value::text, is_called FROM "+s.reg()).Scan(&last, &called); err != nil {
			return err
		}
		reg := s.reg()
		if s.dep != "" {
			table := TableName(Postgres, "", s.ownSchema, s.ownTable)
			if s.dep == "a" && created {
				if _, err := dest.ExecContext(ctx, "ALTER SEQUENCE "+reg+" OWNED BY "+table+"."+Quote(Postgres, s.col)); err != nil {
					return err
				}
			}
			var err error
			reg, err = pgMigrationSequenceName(ctx, dest, table, s.col)
			if err != nil {
				return err
			}
		}
		if created || s.dep == "" {
			if s.dep == "i" {
				cycle := "NO CYCLE"
				if s.cycle {
					cycle = "CYCLE"
				}
				q := fmt.Sprintf("ALTER SEQUENCE %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s %s", reg, s.inc, s.min, s.max, s.start, s.cache, cycle)
				if _, err := dest.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			if _, err := dest.ExecContext(ctx, "SELECT pg_catalog.setval($1::regclass, $2::bigint, $3)", reg, last, called); err != nil {
				return err
			}
			continue
		}
		// RESTART is transactional. setval on a pre-existing sequence would survive rollback.
		next, err := pgMigrationNext(s, last, called)
		if err != nil {
			return err
		}
		if _, err := dest.ExecContext(ctx, "ALTER SEQUENCE "+reg+" RESTART WITH "+next); err != nil {
			return err
		}
	}
	return nil
}

func pgMigrationSequenceName(ctx context.Context, dest *sql.Tx, table, column string) (string, error) {
	var reg sql.NullString
	err := dest.QueryRowContext(ctx, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = pg_get_serial_sequence($1, $2)::regclass`, table, column).Scan(&reg)
	if err == sql.ErrNoRows || err == nil && !reg.Valid {
		return "", fmt.Errorf("目标列 %s.%s 没有所属序列", table, column)
	}
	return reg.String, err
}

func pgMigrationNext(s pgSeq, last string, called bool) (string, error) {
	if !called {
		return last, nil
	}
	var value, increment, lower, upper big.Int
	value.SetString(last, 10)
	increment.SetString(s.inc, 10)
	lower.SetString(s.min, 10)
	upper.SetString(s.max, 10)
	value.Add(&value, &increment)
	if value.Cmp(&upper) > 0 || value.Cmp(&lower) < 0 {
		if !s.cycle {
			return "", fmt.Errorf("序列 %s 已用尽，请迁移到新表", s.reg())
		}
		if increment.Sign() > 0 {
			return lower.String(), nil
		}
		return upper.String(), nil
	}
	return value.String(), nil
}

func pgMigrationCopy(ctx context.Context, sc, dc *sql.Conn, t *TableInfo, count *migrationCounter) (int64, error) {
	var cols []string
	for _, c := range t.Columns {
		if !c.Generated {
			cols = append(cols, Quote(Postgres, c.Name))
		}
	}
	table := migrationTableID(t).sqlName(Postgres)
	if len(cols) > 0 {
		table += " (" + strings.Join(cols, ", ") + ")"
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	stopPipe := context.AfterFunc(ctx, func() {
		_ = reader.CloseWithError(ctx.Err())
		_ = writer.CloseWithError(ctx.Err())
	})
	defer stopPipe()
	type outcome struct {
		rows int64
		err  error
	}
	source := make(chan outcome, 1)
	go func() {
		n, err := pgMigrationRaw(sc, func(c *pgconn.PgConn) (pgconn.CommandTag, error) {
			return c.CopyTo(ctx, migrationCopyWriter{writer, count}, "COPY "+table+" TO STDOUT WITH (ENCODING 'UTF8')")
		})
		_ = writer.CloseWithError(err)
		source <- outcome{n, err}
	}()
	targetRows, err := pgMigrationRaw(dc, func(c *pgconn.PgConn) (pgconn.CommandTag, error) {
		return c.CopyFrom(ctx, reader, "COPY "+table+" FROM STDIN WITH (ENCODING 'UTF8')")
	})
	_ = reader.CloseWithError(err)
	if err != nil {
		cancel()
	}
	src := <-source
	if err != nil {
		if src.err != nil && !errors.Is(src.err, context.Canceled) && !errors.Is(src.err, io.ErrClosedPipe) && parent.Err() == nil {
			return 0, errors.Join(err, src.err)
		}
		return 0, err
	}
	if src.err != nil {
		return 0, src.err
	}
	if src.rows != targetRows {
		return 0, fmt.Errorf("源端读取 %d 行，目标导入 %d 行", src.rows, targetRows)
	}
	return targetRows, nil
}

func pgMigrationRaw(c *sql.Conn, copy func(*pgconn.PgConn) (pgconn.CommandTag, error)) (int64, error) {
	var rows int64
	err := c.Raw(func(driver any) error {
		conn, ok := driver.(*stdlib.Conn)
		if !ok {
			return errors.New("数据库连接不支持 COPY")
		}
		result, err := copy(conn.Conn().PgConn())
		rows = result.RowsAffected()
		return err
	})
	return rows, err
}

type migrationCopyWriter struct {
	io.Writer
	count *migrationCounter
}

func (w migrationCopyWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.count.add(n)
	return n, err
}
