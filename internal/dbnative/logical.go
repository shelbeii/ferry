package dbnative

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/go-sql-driver/mysql"
)

const (
	pgTools      = "pg_dump / pg_restore"
	myShell      = "MySQL Shell"
	myDumper     = "mydumper / myloader"
	myDumpStream = "mysqldump / mysql（流式）"
	maDumpStream = "mariadb-dump / mariadb（流式）"
)

func findMySQLDumpPair(maria bool) (dumpBin, clientBin, method string, ok bool) {
	candidates := [][3]string{
		{"mysqldump", "mysql", myDumpStream},
		{"mariadb-dump", "mariadb", maDumpStream},
	}
	if maria {
		candidates[0], candidates[1] = candidates[1], candidates[0]
	}
	for _, c := range candidates {
		d, err1 := findTool(c[0])
		cl, err2 := findTool(c[1])
		if err1 == nil && err2 == nil {
			return d, cl, c[2], true
		}
	}
	return "", "", "", false
}

func (t *transfer) planLogical(ctx context.Context) error {
	if t.spec.Engine == "postgres" {
		t.result.Method = pgTools
		if err := requireTools("pg_dump", "pg_restore"); err != nil {
			return &proto.DBTransferProblem{Code: "missing-tools", Install: []string{"postgresql-client"}, Message: err.Error()}
		}
		t.result.Warnings = append(t.result.Warnings, "以目标账号创建数据库对象，PG 角色和原有授权由目标实例管理。")
	} else {
		var local bool
		if err := t.target.QueryRowContext(ctx, "SELECT @@global.local_infile").Scan(&local); err != nil {
			return err
		}
		_, shell := findTool("mysqlsh")
		if !t.isMaria() && shell == nil && (local || t.spec.AllowLocalInfile) {
			t.result.Method = myShell
			if !local {
				t.result.Warnings = append(t.result.Warnings, "迁移时将临时开启目标 MySQL 实例的 local_infile，任务结束后恢复为关闭。")
			}
		} else {
			t.result.Method = myDumper
			if problem := t.mysqlToolsProblem(ctx, shell == nil, local); problem != nil {
				if t.spec.AllowDumpFallback && problem.CanUseDumpStream {
					d, cl, method, ok := findMySQLDumpPair(t.isMaria())
					if ok {
						t.dumpTool, t.clientTool = d, cl
						t.result.Method = method
						t.result.Warnings = append(t.result.Warnings, "当前使用单流式管道迁移（不占临时磁盘、无需开启 local_infile）。如需更高并行速度，可安装 MySQL Shell。")
						problem = nil
					}
				}
				if problem != nil {
					return problem
				}
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
				return errors.New("原生工具可能保留对象对原库名的引用。整库包含视图、函数或触发器时，请使用同名目标库，或用本机直连迁移改库名")
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
	var selected map[proto.DBTransferTable]bool
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
		selected = seen
		t.result.Tables = len(seen)
	}
	if !t.createTarget {
		if err := t.checkTarget(ctx); err != nil {
			return err
		}
	}
	return t.checkTempSpace(ctx, selected)
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
	var target map[proto.DBTransferTable]bool
	if t.createTarget {
		target = map[proto.DBTransferTable]bool{}
	} else {
		var err error
		target, err = t.relations(ctx, t.target, t.spec.Target.Database, false)
		if err != nil {
			return err
		}
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

func (t *transfer) ensureTargetDatabase(ctx context.Context) error {
	if !t.createTarget {
		return nil
	}
	t.stage("创建目标数据库 " + t.spec.Target.Database)
	if t.spec.Engine == "postgres" {
		var exists bool
		if err := t.target.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", t.spec.Target.Database).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := t.target.ExecContext(ctx, "CREATE DATABASE "+pgIdentifier(t.spec.Target.Database)); err != nil {
				return fmt.Errorf("创建目标库失败：%w", err)
			}
		}
	} else {
		var charset, collation sql.NullString
		_ = t.source.QueryRowContext(ctx, "SELECT DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", t.spec.Source.Database).Scan(&charset, &collation)
		cs := "utf8mb4"
		if charset.Valid && charset.String != "" && !strings.ContainsAny(charset.String, " ;'\"`") {
			cs = charset.String
		}
		stmt := "CREATE DATABASE IF NOT EXISTS " + mysqlIdentifier(t.spec.Target.Database) + " DEFAULT CHARACTER SET " + cs
		if collation.Valid && collation.String != "" && !strings.ContainsAny(collation.String, " ;'\"`") {
			stmt += " COLLATE " + collation.String
		}
		if _, err := t.target.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("创建目标库失败：%w", err)
		}
	}
	t.target.Close()
	db, err := openDB(t.spec.Engine, t.spec.Target)
	if err != nil {
		return err
	}
	t.target = db
	t.createTarget = false
	return nil
}

func (t *transfer) checkTempSpace(ctx context.Context, selected map[proto.DBTransferTable]bool) error {
	if t.result.Method != pgTools && t.result.Method != myDumper {
		return nil
	}
	dir := t.spec.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return &proto.DBTransferProblem{Code: "disk-space", Message: fmt.Sprintf("执行服务器的临时目录不可用：%s，请先创建或更换临时目录", dir)}
	}
	free, err := diskFree(dir)
	if err != nil || free == 0 {
		return nil
	}
	need := t.estimateSourceBytes(ctx, selected)
	if need == 0 {
		return nil
	}
	safeMin := need + need/5 + 64<<20
	if free < safeMin {
		return &proto.DBTransferProblem{
			Code:    "disk-space",
			Message: fmt.Sprintf("执行服务器临时目录 %s 剩余空间仅 %s，预计需要约 %s（源数据约 %s）。请在高级选项中更换临时目录，或改用流式 / 本机直连迁移", dir, formatBytes(free), formatBytes(safeMin), formatBytes(need)),
		}
	}
	if need >= 512<<20 {
		t.result.Warnings = append(t.result.Warnings, fmt.Sprintf("临时导出目录 %s（可用 %s，源数据约 %s）。", dir, formatBytes(free), formatBytes(need)))
	}
	return nil
}

func (t *transfer) estimateSourceBytes(ctx context.Context, selected map[proto.DBTransferTable]bool) uint64 {
	if t.spec.Engine == "postgres" {
		if t.spec.Scope == "database" {
			var size int64
			if err := t.source.QueryRowContext(ctx, "SELECT pg_database_size(current_database())").Scan(&size); err == nil && size > 0 {
				return uint64(size)
			}
			return 0
		}
		var total uint64
		for tbl := range selected {
			var size int64
			if err := t.source.QueryRowContext(ctx, "SELECT pg_total_relation_size($1::regclass)", pgIdentifier(tbl.Schema)+"."+pgIdentifier(tbl.Name)).Scan(&size); err == nil && size > 0 {
				total += uint64(size)
			}
		}
		return total
	}
	rows, err := t.source.QueryContext(ctx, "SELECT TABLE_NAME, COALESCE(DATA_LENGTH, 0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'", t.spec.Source.Database)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total uint64
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			return total
		}
		if t.spec.Scope == "tables" && !selected[proto.DBTransferTable{Name: name}] {
			continue
		}
		if size > 0 {
			total += uint64(size)
		}
	}
	return total
}

func (t *transfer) logical(ctx context.Context) error {
	if err := t.ensureTargetDatabase(ctx); err != nil {
		return err
	}
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
	work, err := os.MkdirTemp(t.spec.TempDir, "ferry-db-native-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	switch t.result.Method {
	case pgTools:
		return t.pgLogical(ctx, work)
	case myShell:
		return t.mysqlShellWithLocalInfile(ctx, work, lock)
	case myDumpStream, maDumpStream:
		return t.mysqlDumpStream(ctx, work)
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
		myloaderBin, _ := findTool("myloader")
		if myloaderBin == "" {
			myloaderBin = "myloader"
		}
		help, err := nativeCommand(ctx, myloaderBin, "--help").CombinedOutput()
		if err == nil && strings.Contains(string(help), "--source-db") {
			return nil
		}
		p.Message = "执行服务器上的 myloader 不支持 --source-db。请安装兼容版本的 MyDumper，或更换执行服务器"
	}
	if _, _, _, hasDump := findMySQLDumpPair(t.isMaria()); hasDump {
		p.CanUseDumpStream = true
	}
	if t.isMaria() {
		if p.CanUseDumpStream {
			p.Message += "。也可直接使用已有的 mariadb-dump / mysqldump 流式迁移"
		}
		return p
	}
	if !hasShell {
		p.Message += "。也可安装 MySQL Shell（官方通用包，免依赖系统软件源）"
		p.Install = append(p.Install, "mysql-shell")
		if !local {
			p.Message += "，该工具还需要在目标实例上开启 local_infile"
		}
	} else if !local {
		p.Code = "local-infile"
		p.Message = "目标 MySQL 实例关闭了 local_infile，MySQL Shell 无法批量写入。" + p.Message
		p.CanEnableLocalInfile = true
	}
	if p.CanUseDumpStream {
		p.Message += "；或直接使用执行服务器上已有的 mysqldump 流式迁移（不占临时磁盘）"
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
		pgDumpBin, _ := findTool("pg_dump")
		if pgDumpBin == "" {
			pgDumpBin = "pg_dump"
		}
		help, err := nativeCommand(ctx, pgDumpBin, "--help").Output()
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
	if mydumperBin, err := findTool("mydumper"); err == nil {
		if help, err := nativeCommand(ctx, mydumperBin, "--help").CombinedOutput(); err == nil && strings.Contains(string(help), "--trx-consistency-only") {
			args = append(args, "--trx-consistency-only")
		}
	}
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

func (t *transfer) mysqlDumpStream(ctx context.Context, work string) error {
	src, err := credentialsFile(work, "source.cnf", t.spec.Source)
	if err != nil {
		return err
	}
	dst, err := credentialsFile(work, "target.cnf", t.spec.Target)
	if err != nil {
		return err
	}
	dumpBin, clientBin := t.dumpTool, t.clientTool
	if dumpBin == "" || clientBin == "" {
		var ok bool
		dumpBin, clientBin, _, ok = findMySQLDumpPair(t.isMaria())
		if !ok {
			return errors.New("执行服务器缺少 mysqldump / mysql 客户端工具")
		}
	}
	dumpHelp, _ := nativeCommand(ctx, dumpBin, "--help").CombinedOutput()
	clientHelp, _ := nativeCommand(ctx, clientBin, "--help").CombinedOutput()
	dumpHelpStr, clientHelpStr := string(dumpHelp), string(clientHelp)

	addTLS := func(args []string, e proto.DBEndpoint, help string) []string {
		if e.TLS == "" {
			return args
		}
		if strings.Contains(help, "--ssl-mode") {
			mode := map[string]string{"off": "DISABLED", "require": "REQUIRED", "verify": "VERIFY_IDENTITY"}[e.TLS]
			if mode != "" {
				return append(args, "--ssl-mode="+mode)
			}
		} else if e.TLS == "off" && strings.Contains(help, "--skip-ssl") {
			return append(args, "--skip-ssl")
		} else if (e.TLS == "require" || e.TLS == "verify") && strings.Contains(help, "--ssl") {
			return append(args, "--ssl")
		}
		return args
	}

	dumpArgs := []string{
		"--defaults-file=" + src,
		"--host=" + t.spec.Source.Host,
		"--port=" + strconv.Itoa(t.spec.Source.Port),
		"--single-transaction",
		"--quick",
		"--hex-blob",
		"--default-character-set=utf8mb4",
		"--no-create-db",
		"--skip-add-drop-table",
	}
	if strings.Contains(dumpHelpStr, "--set-gtid-purged") {
		dumpArgs = append(dumpArgs, "--set-gtid-purged=OFF")
	}
	dumpArgs = addTLS(dumpArgs, t.spec.Source, dumpHelpStr)
	if t.spec.Scope == "database" {
		dumpArgs = append(dumpArgs, "--routines", "--triggers", t.spec.Source.Database)
	} else {
		dumpArgs = append(dumpArgs, "--skip-triggers", t.spec.Source.Database)
		for _, table := range t.spec.Tables {
			dumpArgs = append(dumpArgs, table.Name)
		}
	}

	clientArgs := []string{
		"--defaults-file=" + dst,
		"--host=" + t.spec.Target.Host,
		"--port=" + strconv.Itoa(t.spec.Target.Port),
		"--default-character-set=utf8mb4",
	}
	clientArgs = addTLS(clientArgs, t.spec.Target, clientHelpStr)
	clientArgs = append(clientArgs, t.spec.Target.Database)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	env := append(os.Environ(), toolPathEnv()...)
	producer := nativeCommand(ctx, dumpBin, dumpArgs...)
	producer.Env = env
	consumer := nativeCommand(ctx, clientBin, clientArgs...)
	consumer.Env = env

	pipe, err := producer.StdoutPipe()
	if err != nil {
		return err
	}
	defer pipe.Close()
	progressPipe := &streamCountReader{r: pipe, report: func(n int64) { t.progress("流式传输", n, 0) }}
	out := &commandOutput{log: func(line string) { t.output(proto.DBTransferEvent{Kind: "info", Text: t.filter(line)}) }}
	producer.Stderr, consumer.Stderr, consumer.Stdout, consumer.Stdin = out, out, out, progressPipe

	t.stage(t.result.Method + " 管道迁移中")
	if err := consumer.Start(); err != nil {
		return err
	}
	if err := producer.Start(); err != nil {
		cancel()
		_ = consumer.Wait()
		return err
	}
	readErr := consumer.Wait()
	if readErr != nil {
		cancel()
	}
	writeErr := producer.Wait()
	progressPipe.flush()
	out.flush()
	if ctx.Err() != nil && readErr != nil && writeErr != nil {
		return ctx.Err()
	}
	if readErr != nil {
		return fmt.Errorf("%s：%w", filepath.Base(clientBin), readErr)
	}
	if writeErr != nil {
		return fmt.Errorf("%s：%w", filepath.Base(dumpBin), writeErr)
	}
	return nil
}

type streamCountReader struct {
	r      io.Reader
	total  int64
	last   time.Time
	report func(int64)
}

func (s *streamCountReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.total += int64(n)
		if s.report != nil && time.Since(s.last) >= 500*time.Millisecond {
			s.last = time.Now()
			s.report(s.total)
		}
	}
	return n, err
}

func (s *streamCountReader) flush() {
	if s.report != nil && s.total > 0 {
		s.report(s.total)
	}
}
