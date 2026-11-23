package dbnative

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/go-sql-driver/mysql"
)

const (
	pgTools  = "pg_dump / pg_restore"
	myShell  = "MySQL Shell"
	myDumper = "mydumper / myloader"
)

func (t *transfer) planLogical(ctx context.Context) error {
	if t.spec.Engine == "postgres" {
		t.result.Method = pgTools
		if err := requireTools("pg_dump", "pg_restore"); err != nil {
			return err
		}
		t.result.Warnings = append(t.result.Warnings, "以目标账号创建数据库对象，PG 角色和原有授权由目标实例管理。")
	} else {
		var local bool
		if err := t.target.QueryRowContext(ctx, "SELECT @@global.local_infile").Scan(&local); err != nil {
			return err
		}
		_, shell := exec.LookPath("mysqlsh")
		if !t.isMaria() && shell == nil && (local || t.spec.AllowLocalInfile) {
			t.result.Method = myShell
			if !local {
				t.result.Warnings = append(t.result.Warnings, "迁移时将临时开启目标 MySQL 实例的 local_infile，任务结束后恢复为关闭。")
			}
		} else {
			t.result.Method = myDumper
			if problem := t.mysqlToolsProblem(ctx, shell == nil, local); problem != nil {
				return problem
			}
		}
		if t.spec.Source.Database != t.spec.Target.Database {
			var programs bool
			q := `SELECT EXISTS (SELECT 1 FROM information_schema.VIEWS WHERE TABLE_SCHEMA=?
				UNION ALL SELECT 1 FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=?
				UNION ALL SELECT 1 FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=?)`
			db := t.spec.Source.Database
			if err := t.source.QueryRowContext(ctx, q, db, db, db).Scan(&programs); err != nil {
				return err
			}
			if programs && t.spec.Scope == "database" {
				return errors.New("原生工具可能保留对象对原库名的引用。整库包含视图、函数或触发器时，请使用同名目标库，或用桌面直迁改库名")
			}
		}
		t.result.Warnings = append(t.result.Warnings, "原生迁移按任务启动时的快照复制，MySQL 定时事件由目标实例单独管理。")
	}
	if err := t.checkWritable(ctx); err != nil {
		return err
	}
	source, err := t.relations(ctx, t.source, t.spec.Source.Database, false)
	if err != nil {
		return err
	}
	if t.spec.Scope == "database" {
		t.result.Tables = len(source)
	} else {
		seen := map[proto.DBTransferTable]bool{}
		for _, table := range t.spec.Tables {
			table = t.normalize(table)
			if seen[table] || !source[table] {
				return fmt.Errorf("源库表不存在、类型不支持或选择重复：%s", table.Name)
			}
			seen[table] = true
		}
		if err := t.checkDependencies(ctx, seen); err != nil {
			return err
		}
		t.result.Tables = len(seen)
	}
	return t.checkTarget(ctx)
}

func (t *transfer) checkDependencies(ctx context.Context, selected map[proto.DBTransferTable]bool) error {
	q := `SELECT DISTINCT '', TABLE_NAME, REFERENCED_TABLE_SCHEMA, REFERENCED_TABLE_NAME
		FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA=? AND REFERENCED_TABLE_NAME IS NOT NULL`
	args := []any{t.spec.Source.Database}
	if t.spec.Engine == "postgres" {
		q = `SELECT DISTINCT ns.nspname, c.relname, np.nspname, p.relname FROM pg_constraint fk
		JOIN pg_class c ON c.oid=fk.conrelid JOIN pg_class p ON p.oid=fk.confrelid
		JOIN pg_namespace ns ON ns.oid=c.relnamespace JOIN pg_namespace np ON np.oid=p.relnamespace WHERE fk.contype='f'`
		args = nil
	}
	target, err := t.relations(ctx, t.target, t.spec.Target.Database, false)
	if err != nil {
		return err
	}
	rows, err := t.source.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var child, parent proto.DBTransferTable
		if err := rows.Scan(&child.Schema, &child.Name, &parent.Schema, &parent.Name); err != nil {
			return err
		}
		if !selected[child] {
			continue
		}
		if t.spec.Engine == "mysql" {
			if parent.Schema != t.spec.Source.Database {
				return fmt.Errorf("表 %s 有跨库外键，请先单独处理依赖库 %s", child.Name, parent.Schema)
			}
			parent.Schema = ""
		}
		if !selected[parent] && !target[parent] {
			return fmt.Errorf("表 %s 依赖 %s，请一起选中或先在目标库创建依赖表", child.Name, parent.Name)
		}
	}
	return rows.Err()
}

func (t *transfer) normalize(table proto.DBTransferTable) proto.DBTransferTable {
	if t.spec.Engine == "postgres" && table.Schema == "" {
		table.Schema = "public"
	}
	return table
}

func (t *transfer) checkWritable(ctx context.Context) error {
	q := "SELECT @@global.read_only"
	if t.spec.Engine == "postgres" {
		q = "SELECT current_setting('transaction_read_only')::boolean"
	}
	var readOnly bool
	if err := t.target.QueryRowContext(ctx, q).Scan(&readOnly); err != nil {
		return err
	}
	if readOnly {
		return &proto.DBTransferProblem{Code: "target-read-only", Message: "目标库是只读的"}
	}
	return nil
}

func (t *transfer) relations(ctx context.Context, db *sql.DB, name string, objects bool) (map[proto.DBTransferTable]bool, error) {
	q := `SELECT '', TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'`
	args := []any{name}
	if objects {
		q = `SELECT '', TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA=?
			UNION ALL SELECT '', ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA=?
			UNION ALL SELECT '', EVENT_NAME FROM information_schema.EVENTS WHERE EVENT_SCHEMA=?`
		args = []any{name, name, name}
	}
	if t.spec.Engine == "postgres" {
		q = `SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema' AND c.relkind IN ('p','r') AND NOT c.relispartition`
		args = nil
		if objects {
			q = `SELECT n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
				WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema' AND c.relkind IN ('p','r','f','m','v','S')
				UNION ALL SELECT n.nspname,p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
				WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
				UNION ALL SELECT n.nspname,t.typname FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
				WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema' AND t.typtype IN ('d','e','r','c')`
		}
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := map[proto.DBTransferTable]bool{}
	for rows.Next() {
		var a proto.DBTransferTable
		if err := rows.Scan(&a.Schema, &a.Name); err != nil {
			return nil, err
		}
		list[a] = true
	}
	return list, rows.Err()
}

func (t *transfer) checkTarget(ctx context.Context) error {
	target, err := t.relations(ctx, t.target, t.spec.Target.Database, true)
	if err != nil {
		return err
	}
	if t.spec.Scope == "database" && len(target) != 0 {
		return &proto.DBTransferProblem{Code: "target-not-empty", Message: "原生整库迁移需要空目标库"}
	}
	for _, table := range t.spec.Tables {
		if target[t.normalize(table)] {
			return &proto.DBTransferProblem{Code: "target-table-exists", Message: fmt.Sprintf("目标已有同名对象 %s，原生迁移只创建新表", table.Name)}
		}
	}
	return nil
}

func (t *transfer) logical(ctx context.Context) error {
	lock, err := t.target.Conn(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	var q, release string
	if t.spec.Engine == "postgres" {
		q, release = "SELECT pg_try_advisory_lock(772584132, 1)", "SELECT pg_advisory_unlock(772584132, 1)"
	} else {
		// Match the desktop migration lock for this database.
		q = `SELECT GET_LOCK(CONCAT('ferry:', LEFT(SHA2(CONCAT('ferry-migration:', LOWER(DATABASE())), 256), 48)), 0)`
		release = `SELECT RELEASE_LOCK(CONCAT('ferry:', LEFT(SHA2(CONCAT('ferry-migration:', LOWER(DATABASE())), 256), 48)))`
	}
	var locked bool
	if err := lock.QueryRowContext(ctx, q).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return &proto.DBTransferProblem{Code: "target-busy", Message: "目标库已有迁移任务在运行"}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = lock.ExecContext(cleanup, release)
	}()
	if err := t.checkTarget(ctx); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "ferry-db-native-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	switch t.result.Method {
	case pgTools:
		return t.pgLogical(ctx, work)
	case myShell:
		return t.mysqlShellWithLocalInfile(ctx, work, lock)
	default:
		return t.mydumper(ctx, work)
	}
}

func (t *transfer) mysqlToolsProblem(ctx context.Context, hasShell, local bool) *proto.DBTransferProblem {
	p := &proto.DBTransferProblem{Code: "missing-tools"}
	if err := requireTools("mydumper", "myloader"); err != nil {
		p.Message = err.Error()
		p.Install = append(p.Install, "mydumper")
	} else {
		help, err := nativeCommand(ctx, "myloader", "--help").CombinedOutput()
		if err == nil && strings.Contains(string(help), "--source-db") {
			return nil
		}
		p.Message = "执行服务器上的 myloader 不支持 --source-db。请安装兼容版本的 MyDumper，或更换执行服务器"
	}
	if t.isMaria() {
		return p
	}
	if !hasShell {
		p.Message += "。也可安装 MySQL Shell"
		p.Install = append(p.Install, "mysql-shell")
		if !local {
			p.Message += "，该工具还需要在目标实例上开启 local_infile"
		}
	} else if !local {
		p.Code = "local-infile"
		p.Message = "目标 MySQL 实例关闭了 local_infile，MySQL Shell 无法批量写入。" + p.Message
		p.CanEnableLocalInfile = true
	}
	return p
}

func (t *transfer) mysqlShellWithLocalInfile(ctx context.Context, work string, conn *sql.Conn) (err error) {
	// MySQL's setting is instance-wide, including migrations into other databases.
	const lockName = "ferry:mysql-shell:local-infile"
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", lockName).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return &proto.DBTransferProblem{Code: "target-busy", Message: "目标 MySQL 实例已有 MySQL Shell 迁移在运行"}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(cleanup, "DO RELEASE_LOCK(?)", lockName)
	}()
	var enabled bool
	if err := conn.QueryRowContext(ctx, "SELECT @@global.local_infile").Scan(&enabled); err != nil {
		return err
	}
	if enabled {
		return t.mysqlShell(ctx, work)
	}
	if !t.spec.AllowLocalInfile {
		return &proto.DBTransferProblem{Code: "local-infile", Message: "目标实例的 local_infile 已关闭，请重新预检", CanEnableLocalInfile: true}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	setup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, setErr := conn.ExecContext(setup, "SET GLOBAL local_infile = ON")
	cancel()
	var sqlErr *mysql.MySQLError
	if errors.As(setErr, &sqlErr) {
		return &proto.DBTransferProblem{Code: "local-infile-denied", Install: []string{"mydumper"}, Message: fmt.Sprintf("目标 MySQL 拒绝开启 local_infile：%v", sqlErr)}
	}
	// A lost response can leave the change applied, so try to restore even
	// when the SET did not return an acknowledgement.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if _, restoreErr := conn.ExecContext(cleanup, "SET GLOBAL local_infile = OFF"); restoreErr != nil {
			message := fmt.Sprintf("无法确认目标 MySQL 已恢复 local_infile = OFF，请立即检查目标实例：%v", restoreErr)
			if err != nil {
				message = err.Error() + "。 " + message
			}
			err = &proto.DBTransferProblem{Code: "local-infile-restore", Message: message}
		} else {
			t.stage("目标 MySQL 的 local_infile 已恢复为关闭")
		}
	}()
	if setErr != nil {
		return fmt.Errorf("无法开启目标 MySQL 的 local_infile：%w", setErr)
	}
	t.stage("本次迁移已临时开启目标 MySQL 的 local_infile")
	return t.mysqlShell(ctx, work)
}

func pgIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func (t *transfer) pgLogical(ctx context.Context, work string) error {
	archive := filepath.Join(work, "dump")
	jobs := strconv.Itoa(t.spec.Threads)
	args := []string{"--dbname=" + pgURL(t.spec.Source).String(), "--format=directory", "--jobs=" + jobs, "--file=" + archive, "--no-password", "--verbose"}
	if len(t.spec.Tables) != 0 {
		help, err := nativeCommand(ctx, "pg_dump", "--help").Output()
		if err != nil {
			return err
		}
		args = append(args, "--strict-names")
		for _, table := range t.spec.Tables {
			table = t.normalize(table)
			pattern := pgIdentifier(table.Schema) + "." + pgIdentifier(table.Name)
			if strings.Contains(string(help), "--table-and-children") {
				args = append(args, "--table-and-children="+pattern)
				continue
			}
			var children bool
			if err := t.source.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_inherits WHERE inhparent = $1::regclass)`, pattern).Scan(&children); err != nil {
				return err
			}
			if children {
				return errors.New("当前 pg_dump 不支持单独导出带子表的表，请升级客户端或使用整库迁移")
			}
			args = append(args, "--table="+pattern)
		}
	}
	t.stage("并行导出 PostgreSQL 数据")
	if err := t.command(ctx, []string{"PGPASSWORD=" + t.spec.Source.Password}, "pg_dump", args...); err != nil {
		return err
	}
	if err := t.checkTarget(ctx); err != nil {
		return err
	}
	schemas := map[string]bool{}
	for _, table := range t.spec.Tables {
		name := t.normalize(table).Schema
		if schemas[name] || name == "public" {
			continue
		}
		if _, err := t.target.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgIdentifier(name)); err != nil {
			return err
		}
		schemas[name] = true
	}
	t.stage("并行恢复 PostgreSQL 数据和索引")
	return t.command(ctx, []string{"PGPASSWORD=" + t.spec.Target.Password}, "pg_restore",
		"--dbname="+pgURL(t.spec.Target).String(), "--jobs="+jobs, "--exit-on-error", "--no-data-for-failed-tables", "--no-owner", "--no-privileges", "--no-password", "--verbose", archive)
}

func (t *transfer) mysqlShell(ctx context.Context, work string) error {
	conn := func(e proto.DBEndpoint) map[string]any {
		mode := map[string]string{"": "PREFERRED", "off": "DISABLED", "require": "REQUIRED", "verify": "VERIFY_IDENTITY"}[e.TLS]
		return map[string]any{"scheme": "mysql", "host": e.Host, "port": e.Port, "user": e.User, "password": e.Password, "ssl-mode": mode}
	}
	options := map[string]any{"threads": t.spec.Threads, "showProgress": true, "schema": t.spec.Target.Database}
	var tables []string
	for _, table := range t.spec.Tables {
		tables = append(tables, table.Name)
	}
	config, err := json.Marshal(map[string]any{"source": conn(t.spec.Source), "target": conn(t.spec.Target), "db": t.spec.Source.Database, "tables": tables, "options": options})
	if err != nil {
		return err
	}
	input := filepath.Join(work, "input.json")
	if err := os.WriteFile(input, config, 0o600); err != nil {
		return err
	}
	script := `import json, sys
with open(sys.argv[1], encoding='utf-8') as file:
    job = json.load(file)
shell.connect(job['source'])
opt = job['options']
if job['tables']:
    opt['triggers'] = False
    util.copy_tables(job['db'], job['tables'], job['target'], opt)
else:
    opt['events'] = False
    util.copy_schemas([job['db']], job['target'], opt)
`
	program := filepath.Join(work, "copy.py")
	if err := os.WriteFile(program, []byte(script), 0o600); err != nil {
		return err
	}
	t.stage("MySQL Shell 并行复制")
	return t.command(ctx, []string{"MYSQLSH_USER_CONFIG_HOME=" + work}, "mysqlsh", "--py", "--no-wizard", "--quiet-start=2", "--file", program, input)
}

func mysqlOptionValue(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func credentialsFile(work, name string, e proto.DBEndpoint) (string, error) {
	path := filepath.Join(work, name)
	contents := fmt.Sprintf("[client]\nuser=%s\npassword=%s\n", mysqlOptionValue(e.User), mysqlOptionValue(e.Password))
	return path, os.WriteFile(path, []byte(contents), 0o600)
}

func (t *transfer) mydumper(ctx context.Context, work string) error {
	src, err := credentialsFile(work, "source.cnf", t.spec.Source)
	if err != nil {
		return err
	}
	dst, err := credentialsFile(work, "target.cnf", t.spec.Target)
	if err != nil {
		return err
	}
	connect := func(e proto.DBEndpoint, credentials string) []string {
		args := []string{"--defaults-file=" + credentials, "--host=" + e.Host, "--port=" + strconv.Itoa(e.Port), "--threads=" + strconv.Itoa(t.spec.Threads)}
		if e.TLS != "" {
			mode := map[string]string{"off": "DISABLED", "require": "REQUIRED", "verify": "VERIFY_IDENTITY"}[e.TLS]
			args = append(args, "--ssl-mode="+mode)
		}
		return args
	}
	archive := filepath.Join(work, "dump")
	args := append(connect(t.spec.Source, src), "--database="+t.spec.Source.Database, "--outputdir="+archive, "--verbose=3")
	if t.spec.Scope == "tables" {
		names := make([]string, 0, len(t.spec.Tables))
		for _, table := range t.spec.Tables {
			if strings.ContainsAny(t.spec.Source.Database+table.Name, ",\r\n") {
				return errors.New("mydumper 的指定表模式不支持名称中的逗号或换行")
			}
			names = append(names, t.spec.Source.Database+"."+table.Name)
		}
		args = append(args, "--tables-list="+strings.Join(names, ","))
	} else {
		args = append(args, "--routines", "--triggers")
	}
	t.stage("mydumper 并行导出")
	if err := t.command(ctx, nil, "mydumper", args...); err != nil {
		return err
	}
	if err := t.checkTarget(ctx); err != nil {
		return err
	}
	t.stage("myloader 并行恢复")
	return t.command(ctx, nil, "myloader", append(connect(t.spec.Target, dst), "--directory="+archive, "--source-db="+t.spec.Source.Database, "--database="+t.spec.Target.Database, "--verbose=3")...)
}
