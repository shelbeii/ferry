package dbnative

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

const (
	pgPhysical       = "pg_basebackup"
	myPhysical       = "MySQL Clone Plugin"
	maPhysical       = "mariadb-backup"
	mariaSourceQuery = "SELECT SHA2(CONCAT_WS(CHAR(0), @@hostname, @@port, @@datadir, @@version), 256)"
)

var (
	sshTargetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]*$`)
	versionSeries    = regexp.MustCompile(`^[0-9]+\.[0-9]+`)
)

func (t *transfer) planPhysical(ctx context.Context) error {
	t.result.Directory = t.spec.Directory
	if t.spec.Engine == "postgres" || t.isMaria() {
		if t.target != nil {
			return errors.New("此引擎的物理克隆写入执行服务器上的新数据目录，请清空目标数据库连接")
		}
		if err := newDestination(t.spec.Directory); err != nil {
			return err
		}
		t.result.Warnings = append(t.result.Warnings, "克隆完成后，可用兼容的数据库版本从新目录启动实例。")
	}
	switch {
	case t.spec.Engine == "postgres":
		if t.spec.SourceSSH != "" {
			return errors.New("PostgreSQL 物理克隆通过数据库复制连接读取源实例")
		}
		t.result.Method = pgPhysical
		if err := requireTools(pgPhysical, "pg_verifybackup"); err != nil {
			return &proto.DBTransferProblem{Code: "missing-tools", Install: []string{"postgresql-client"}, Message: err.Error()}
		}
		return nil
	case t.isMaria():
		t.result.Method = maPhysical
		if err := requireTools(maPhysical, "mbstream"); err != nil {
			return err
		}
		if t.spec.SourceSSH != "" {
			if !sshTargetPattern.MatchString(t.spec.SourceSSH) {
				return errors.New("MariaDB 源 SSH 别名无效")
			}
			if err := requireTools("ssh"); err != nil {
				return err
			}
			if err := t.source.QueryRowContext(ctx, mariaSourceQuery).Scan(&t.mariaSourceID); err != nil {
				return err
			}
			return t.command(ctx, nil, "ssh", mariaSSHArgs(t.spec.SourceSSH, "command -v mariadb-backup >/dev/null && command -v mariadb >/dev/null")...)
		}
		if !localDBHost(t.spec.Source.Host) {
			return errors.New("MariaDB 物理备份需要在源数据库主机读取数据目录。请填写执行服务器已配置的源 SSH 别名")
		}
		return nil
	default:
		t.result.Method = myPhysical
		return t.mysqlCloneCheck(ctx)
	}
}

func localDBHost(s string) bool {
	return strings.EqualFold(s, "localhost") || net.ParseIP(s).IsLoopback()
}

func (t *transfer) mysqlCloneCheck(ctx context.Context) error {
	if t.target == nil {
		return errors.New("请选择已安装 Clone Plugin 的兼容目标 MySQL 实例")
	}
	if t.spec.SourceSSH != "" {
		return errors.New("MySQL 由接收实例直连源库，请清空源 SSH 别名")
	}
	if t.spec.Source.TLS == "verify" {
		return errors.New("MySQL Clone 的身份验证由接收实例的 clone_ssl_ca 等变量配置，请先配置接收实例，再在源连接选择要求加密")
	}
	if a, b := versionSeries.FindString(t.result.SourceVersion), versionSeries.FindString(t.result.TargetVersion); a == "" || a != b {
		return errors.New("源与目标 MySQL 须使用相同版本系列，具体兼容性由 Clone Plugin 校验")
	}
	if strings.Contains(t.spec.Source.Host, ":") {
		return errors.New("MySQL Clone 的源地址须使用域名或 IPv4")
	}
	var sourceID, targetID string
	for i, db := range []*sql.DB{t.source, t.target} {
		var enabled bool
		q := `SELECT EXISTS(SELECT 1 FROM information_schema.PLUGINS WHERE PLUGIN_NAME='clone' AND PLUGIN_STATUS='ACTIVE')`
		if err := db.QueryRowContext(ctx, q).Scan(&enabled); err != nil {
			return err
		}
		if !enabled {
			return fmt.Errorf("%s MySQL 实例需要启用 Clone Plugin", []string{"源", "目标"}[i])
		}
		id := &sourceID
		if i == 1 {
			id = &targetID
		}
		if err := db.QueryRowContext(ctx, "SELECT @@server_uuid").Scan(id); err != nil {
			return err
		}
	}
	if sourceID == targetID {
		return errors.New("源和目标是同一个 MySQL 实例")
	}
	var allowed string
	var currentDirectory string
	if err := t.target.QueryRowContext(ctx, "SELECT @@global.clone_valid_donor_list, @@datadir").Scan(&allowed, &currentDirectory); err != nil {
		return err
	}
	currentDirectory = path.Clean(currentDirectory)
	if t.spec.Directory == currentDirectory || strings.HasPrefix(t.spec.Directory, currentDirectory+"/") {
		return errors.New("新目录必须在接收实例正在使用的数据目录之外")
	}
	donor := net.JoinHostPort(t.spec.Source.Host, strconv.Itoa(t.spec.Source.Port))
	for _, entry := range strings.Split(allowed, ",") {
		if strings.EqualFold(strings.TrimSpace(entry), donor) {
			t.result.Warnings = append(t.result.Warnings, "目标 MySQL 写入指定的新目录，完成后可从新目录启动兼容实例。现有服务继续使用原数据目录。")
			return nil
		}
	}
	return fmt.Errorf("请在接收实例的 clone_valid_donor_list 中配置 %s", donor)
}

func (t *transfer) physical(ctx context.Context) error {
	if t.result.Method == myPhysical {
		return t.mysqlClone(ctx)
	}
	if err := os.Mkdir(t.spec.Directory, 0o700); err != nil {
		return fmt.Errorf("创建新目标目录：%w", err)
	}
	if t.result.Method == pgPhysical {
		t.stage("流式复制 PostgreSQL 实例文件和 WAL")
		if err := t.command(ctx, []string{"PGPASSWORD=" + t.spec.Source.Password}, pgPhysical, "--dbname="+pgURL(t.spec.Source).String(),
			"--pgdata="+t.spec.Directory, "--wal-method=stream", "--checkpoint=fast", "--progress", "--verbose", "--no-password"); err != nil {
			return fmt.Errorf("克隆未完成，部分文件所在目录 %s：%w", t.spec.Directory, err)
		}
		t.stage("校验 PostgreSQL 物理备份清单")
		return t.command(ctx, nil, "pg_verifybackup", t.spec.Directory)
	}
	if err := t.mariaStream(ctx); err != nil {
		return fmt.Errorf("克隆未完成，部分文件所在目录 %s：%w", t.spec.Directory, err)
	}
	t.stage("准备 MariaDB 数据文件")
	return t.command(ctx, nil, maPhysical, "--prepare", "--target-dir="+t.spec.Directory)
}

func (t *transfer) mysqlClone(ctx context.Context) error {
	t.stage("接收实例开始克隆 InnoDB 数据文件")
	dst, err := t.target.Conn(ctx)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := dst.ExecContext(ctx, "SET SESSION sql_mode = 'NO_BACKSLASH_ESCAPES'"); err != nil {
		return err
	}
	e := t.spec.Source
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	ssl := " REQUIRE SSL"
	if e.TLS == "off" {
		ssl = " REQUIRE NO SSL"
	} else if e.TLS == "" {
		ssl = ""
	}
	query := fmt.Sprintf("CLONE INSTANCE FROM %s@%s:%d IDENTIFIED BY %s DATA DIRECTORY = %s%s", quote(e.User), quote(e.Host), e.Port, quote(e.Password), quote(t.spec.Directory), ssl)
	if _, err := dst.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("MySQL 克隆失败，请检查目标实例的 clone_status 和新数据目录：%s", t.filter(err.Error()))
	}
	return nil
}

func mariaSSHArgs(host, command string) []string {
	return []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=8", "--", host, command}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

func (t *transfer) mariaStream(ctx context.Context) error {
	work, err := os.MkdirTemp("", "ferry-mariadb-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var producer *exec.Cmd
	source := t.spec.Source
	if t.spec.SourceSSH != "" {
		// The source-side process reads its own data files. Its stdout is only
		// the xbstream archive, sent over the worker's verified SSH connection.
		producer = nativeCommand(ctx, "ssh", mariaSSHArgs(t.spec.SourceSSH, "sh -s")...)
		conf := fmt.Sprintf("[client]\nuser=%s\npassword=%s\n", mysqlOptionValue(source.User), mysqlOptionValue(source.Password))
		connection := `--defaults-file="$work/connection.cnf" --host=127.0.0.1 --port=` + strconv.Itoa(source.Port)
		producer.Stdin = strings.NewReader(`set -eu
umask 077
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT HUP INT TERM
printf '%s' ` + shellQuote(conf) + ` > "$work/connection.cnf"
actual=$(mariadb ` + connection + ` --skip-ssl --batch --skip-column-names --execute ` + shellQuote(mariaSourceQuery) + `)
if [ "$actual" != ` + shellQuote(t.mariaSourceID) + ` ]; then
  printf '%s\n' '源 SSH 别名下的 MariaDB 与所选源连接不一致' >&2
  exit 1
fi
mariadb-backup ` + connection + ` --backup --stream=xbstream --target-dir="$work/backup" --parallel=` + strconv.Itoa(t.spec.Threads) + "\n")
	} else {
		credentials, err := credentialsFile(work, "source.cnf", source)
		if err != nil {
			return err
		}
		producer = nativeCommand(ctx, maPhysical, "--defaults-file="+credentials, "--host=127.0.0.1", "--port="+strconv.Itoa(source.Port),
			"--backup", "--stream=xbstream", "--target-dir="+work+"/backup", "--parallel="+strconv.Itoa(t.spec.Threads))
	}
	pipe, err := producer.StdoutPipe()
	if err != nil {
		return err
	}
	defer pipe.Close()
	consumer := nativeCommand(ctx, "mbstream", "-x", "-C", t.spec.Directory)
	out := &commandOutput{log: func(line string) { t.output(proto.DBTransferEvent{Kind: "info", Text: t.filter(line)}) }}
	producer.Stderr, consumer.Stderr, consumer.Stdout, consumer.Stdin = out, out, out, pipe
	t.stage("流式复制 MariaDB 实例文件")
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
	out.flush()
	if readErr != nil {
		return fmt.Errorf("mbstream：%w", readErr)
	}
	if writeErr != nil {
		return fmt.Errorf("mariadb-backup：%w", writeErr)
	}
	return nil
}
